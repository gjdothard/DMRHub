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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	zipAPIHost        = "api.zippopotam.us"
	nwsAPIHost        = "api.weather.gov"
	maxResponseBytes  = 2 * 1024 * 1024
	maxStationChecks  = 3
	maxObservationAge = 2 * time.Hour
)

var (
	errUnknownZIP    = errors.New("unknown U.S. ZIP code")
	errHTTPNotFound  = errors.New("upstream resource not found")
	errNoObservation = errors.New("no recent weather observation")
)

type weatherAPI struct {
	client *http.Client
}

type upstreamStatus int

func (s upstreamStatus) Error() string { return fmt.Sprintf("upstream returned HTTP %d", s) }

type zipPlace struct {
	Latitude  string `json:"latitude"`
	Longitude string `json:"longitude"`
}

type nwsValue struct {
	Value *float64 `json:"value"`
	Unit  string   `json:"unitCode"`
}

type observation struct {
	Timestamp   time.Time `json:"timestamp"`
	Description string    `json:"textDescription"`
	Temperature nwsValue  `json:"temperature"`
	Humidity    nwsValue  `json:"relativeHumidity"`
	Wind        nwsValue  `json:"windSpeed"`
}

func (a weatherAPI) lookup(ctx context.Context, zip string) (observation, error) {
	var location struct {
		Places []zipPlace `json:"places"`
	}
	err := a.get(ctx, "https://"+zipAPIHost+"/us/"+zip, &location)
	if errors.Is(err, errHTTPNotFound) {
		return observation{}, errUnknownZIP
	}
	if err != nil {
		return observation{}, err
	}
	if len(location.Places) == 0 {
		return observation{}, errUnknownZIP
	}

	place := location.Places[0]
	lat, latErr := strconv.ParseFloat(place.Latitude, 64)
	lon, lonErr := strconv.ParseFloat(place.Longitude, 64)
	if latErr != nil || lonErr != nil || !validCoordinates(lat, lon) {
		return observation{}, errors.New("invalid ZIP coordinates")
	}
	return a.lookupPoint(ctx, lat, lon)
}

func validCoordinates(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

func (a weatherAPI) lookupPoint(ctx context.Context, lat, lon float64) (observation, error) {
	var point struct {
		Properties struct {
			StationsURL string `json:"observationStations"`
		} `json:"properties"`
	}
	endpoint := fmt.Sprintf("https://%s/points/%.4f,%.4f", nwsAPIHost, lat, lon)
	if err := a.get(ctx, endpoint, &point); err != nil {
		return observation{}, err
	}

	stationsURL, err := url.Parse(point.Properties.StationsURL)
	if err != nil || stationsURL.Scheme != "https" || stationsURL.Host != nwsAPIHost ||
		stationsURL.User != nil || !strings.HasPrefix(stationsURL.Path, "/gridpoints/") ||
		!strings.HasSuffix(stationsURL.Path, "/stations") {
		return observation{}, errors.New("invalid NWS observation stations link")
	}

	var stations struct {
		Features []struct {
			Properties struct {
				ID string `json:"stationIdentifier"`
			} `json:"properties"`
		} `json:"features"`
	}
	if err := a.get(ctx, stationsURL.String(), &stations); err != nil {
		return observation{}, err
	}

	var stationErr error
	var receivedObservation bool
	for _, station := range stations.Features[:min(maxStationChecks, len(stations.Features))] {
		id := station.Properties.ID
		if id == "" {
			continue
		}
		endpoint := "https://" + nwsAPIHost + "/stations/" + url.PathEscape(id) + "/observations/latest"
		var latest struct {
			Properties observation `json:"properties"`
		}
		if err := a.get(ctx, endpoint, &latest); err != nil {
			if ctx.Err() != nil {
				return observation{}, err
			}
			stationErr = err
			continue
		}
		receivedObservation = true
		if latest.Properties.usable(time.Now()) {
			return latest.Properties, nil
		}
	}
	if !receivedObservation && stationErr != nil {
		return observation{}, stationErr
	}
	return observation{}, errNoObservation
}

func (a weatherAPI) get(ctx context.Context, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, http.NoBody)
	if err != nil {
		return fmt.Errorf("build weather request: %w", err)
	}
	req.Header.Set("User-Agent", "DMRHub-ZIP-Weather/0.1 (https://github.com/USA-RedDragon/DMRHub)")
	if req.URL.Host == nwsAPIHost {
		req.Header.Set("Accept", "application/geo+json")
	}

	res, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", req.URL.Host, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return errHTTPNotFound
	}
	if res.StatusCode != http.StatusOK {
		return upstreamStatus(res.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxResponseBytes)).Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", req.URL.Host, err)
	}
	return nil
}

func (o observation) usable(now time.Time) bool {
	age := now.Sub(o.Timestamp)
	return age >= -5*time.Minute && age <= maxObservationAge &&
		o.Temperature.Value != nil && o.Temperature.Unit == "wmoUnit:degC" &&
		*o.Temperature.Value >= -100 && *o.Temperature.Value <= 70
}

func (o observation) messages(label string) (full, short string) {
	fahrenheit := *o.Temperature.Value*9/5 + 32
	short = fmt.Sprintf("%s %.0fF", label, fahrenheit)
	condition := strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if r < ' ' || r > '~' {
			return ' '
		}
		return r
	}, o.Description)), " ")
	full = short
	if condition != "" {
		full += " " + condition[:min(len(condition), 32)]
	}
	if o.Humidity.Value != nil && o.Humidity.Unit == "wmoUnit:percent" && *o.Humidity.Value >= 0 && *o.Humidity.Value <= 100 {
		full += fmt.Sprintf(" RH %.0f%%", *o.Humidity.Value)
	}
	if o.Wind.Value != nil && *o.Wind.Value >= 0 && *o.Wind.Value <= 500 {
		switch o.Wind.Unit {
		case "wmoUnit:km_h-1":
			full += fmt.Sprintf(" Wind %.0f mph", *o.Wind.Value/1.609344)
		case "wmoUnit:m_s-1":
			full += fmt.Sprintf(" Wind %.0f mph", *o.Wind.Value*2.236936)
		}
	}
	full += o.Timestamp.UTC().Format(" 1504Z")
	return full, short
}
