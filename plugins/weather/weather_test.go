// SPDX-License-Identifier: AGPL-3.0-or-later
// DMRHub - Run a DMR network server in a single binary
// Copyright (C) 2023-2026 Jacob McSwain
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.
//
// The source code is available at <https://github.com/USA-RedDragon/DMRHub>

package weather

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/DMRHub/sdk/plugin"
	pluginv1 "github.com/USA-RedDragon/DMRHub/sdk/plugin/v1"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestZIPWeatherLookup(t *testing.T) {
	t.Parallel()
	recorded := time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	paths := []string{
		"https://api.zippopotam.us/us/02108",
		"https://api.weather.gov/points/42.3570,-71.0637",
		"https://api.weather.gov/gridpoints/BOX/70,76/stations",
		"https://api.weather.gov/stations/KBOS/observations/latest",
		"https://api.weather.gov/stations/KOWD/observations/latest",
	}
	bodies := []string{
		`{"places":[{"latitude":"42.357","longitude":"-71.0637"}]}`,
		`{"properties":{"observationStations":"https://api.weather.gov/gridpoints/BOX/70,76/stations"}}`,
		`{"features":[{"properties":{"stationIdentifier":"KBOS"}},{"properties":{"stationIdentifier":"KOWD"}}]}`,
		fmt.Sprintf(`{"properties":{"timestamp":%q,"temperature":{"unitCode":"wmoUnit:degC","value":20}}}`,
			recorded.Add(-3*time.Hour).Format(time.RFC3339)),
		fmt.Sprintf(`{"properties":{"timestamp":%q,"textDescription":"Partly Cloudy",
			"temperature":{"unitCode":"wmoUnit:degC","value":20},
			"relativeHumidity":{"unitCode":"wmoUnit:percent","value":60},
			"windSpeed":{"unitCode":"wmoUnit:km_h-1","value":16.09344}}}`, recorded.Format(time.RFC3339)),
	}

	calls := 0
	api := weatherAPI{client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if calls >= len(paths) || req.URL.String() != paths[calls] {
			return nil, fmt.Errorf("unexpected request %d: %s", calls, req.URL)
		}
		if req.URL.Host == nwsAPIHost && (req.Header.Get("User-Agent") == "" || req.Header.Get("Accept") != "application/geo+json") {
			return nil, fmt.Errorf("NWS request headers missing")
		}
		body := bodies[calls]
		calls++
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	obs, err := api.lookup(t.Context(), "02108")
	if err != nil {
		t.Fatal(err)
	}
	full, short := obs.messages("02108")
	want := "02108 68F Partly Cloudy RH 60% Wind 10 mph " + recorded.Format("1504Z")
	if full != want || short != "02108 68F" || calls != len(paths) {
		t.Fatalf("got full=%q short=%q calls=%d, want full=%q short=%q calls=%d", full, short, calls, want, "02108 68F", len(paths))
	}
}

func TestReplyUsesOriginalText(t *testing.T) {
	t.Parallel()
	temp := 20.0
	request := &pluginv1.Text{Id: 12, Source: 1234567, Destination: 5060, Repeater: 11, Slot: 2, Body: " 02108 "}
	var replies []string
	r := responder{
		lookup: func(_ context.Context, zip string) (observation, error) {
			if zip != "02108" {
				t.Fatalf("lookup ZIP = %q, want 02108", zip)
			}
			return observation{Timestamp: time.Now(), Description: "Partly Cloudy", Temperature: nwsValue{Value: &temp}}, nil
		},
		reply: func(_ context.Context, source uint32, text *pluginv1.Text, body string) (string, error) {
			if source != request.GetDestination() || text != request {
				t.Fatalf("reply source=%d text=%v, want source=%d and original request", source, text, request.GetDestination())
			}
			replies = append(replies, body)
			if len(replies) == 1 {
				return "", plugin.ErrInvalid
			}
			return "TMS", nil
		},
	}
	r.answer(t.Context(), request)
	if len(replies) != 2 || !strings.HasPrefix(replies[0], "02108 68F Partly Cloudy ") || replies[1] != "02108 68F" {
		t.Fatalf("unexpected replies: %q", replies)
	}
}

func TestZIPReplyErrors(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		body string
		err  error
		want string
	}{
		{"29401-1234", nil, "Send a 5-digit US ZIP code."},
		{"1234x", nil, "Send a 5-digit US ZIP code."},
		{"00000", errUnknownZIP, "ZIP not found: 00000"},
		{"29401", context.DeadlineExceeded, "Weather unavailable: 29401"},
	} {
		t.Run(tt.body, func(t *testing.T) {
			t.Parallel()
			var replied bool
			r := responder{
				lookup: func(context.Context, string) (observation, error) {
					if tt.err == nil {
						t.Fatal("invalid ZIP triggered an API call")
					}
					return observation{}, tt.err
				},
				reply: func(_ context.Context, _ uint32, _ *pluginv1.Text, body string) (string, error) {
					replied = true
					if body != tt.want {
						t.Errorf("reply = %q, want %q", body, tt.want)
					}
					return "TMS", nil
				},
			}
			r.answer(t.Context(), &pluginv1.Text{Body: tt.body})
			if !replied {
				t.Error("missing reply")
			}
		})
	}
}
