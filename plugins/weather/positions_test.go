// SPDX-License-Identifier: AGPL-3.0-or-later

package weather

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLatestRadioPosition(t *testing.T) {
	t.Parallel()
	recent := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	stale := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	for _, tt := range []struct {
		name   string
		latest string
		status int
		ok     bool
	}{
		{"recent fix", fmt.Sprintf(`{"radio_id":1234567,"latitude":32.9,"longitude":-80,"fix_time":%q}`, recent), 200, true},
		{"receive time", fmt.Sprintf(`{"radio_id":1234567,"latitude":32.9,"longitude":-80,"fix_time":null,"received_at":%q}`, recent), 200, true},
		{"stale fix received recently", fmt.Sprintf(`{"radio_id":1234567,"latitude":32.9,"longitude":-80,"fix_time":%q,"received_at":%q}`, stale, recent), 200, false},
		{"different radio", fmt.Sprintf(`{"radio_id":1234568,"latitude":32.9,"longitude":-80,"received_at":%q}`, recent), 200, false},
		{"missing position", "null", 200, false},
		{"not visible", "null", 404, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			api, err := newPositionAPI("https://hub.example", "test-token")
			if err != nil {
				t.Fatal(err)
			}
			api.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodGet || req.URL.String() != "https://hub.example/api/v1/positions/1234567" ||
					req.Header.Get("Authorization") != "Bearer test-token" {
					return nil, errors.New("wrong positions request")
				}
				body := `{"latest":` + tt.latest + `}`
				return &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			lat, lon, err := api.latest(t.Context(), 1234567)
			if tt.ok {
				if err != nil || lat != 32.9 || lon != -80 {
					t.Fatalf("unexpected position: %v, %v, %v", lat, lon, err)
				}
			} else if !errors.Is(err, errNoPosition) {
				t.Fatalf("expected no position, got %v", err)
			}
		})
	}
}

func TestVoiceReplyFromPosition(t *testing.T) {
	t.Parallel()
	const radio = 1234567
	temp := 20.0
	r := responder{local: func(_ context.Context, id uint32) (observation, error) {
		if id != radio {
			t.Fatalf("looked up the wrong radio: %d", id)
		}
		return observation{
			Timestamp:   time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC),
			Description: "Clear",
			Temperature: nwsValue{Value: &temp},
		}, nil
	}}
	if got := r.voiceReply(t.Context(), radio); got != "Local WX 68F Clear 1800Z" {
		t.Fatalf("unexpected local weather reply: %q", got)
	}
	r.local = func(context.Context, uint32) (observation, error) { return observation{}, errNoPosition }
	if got := r.voiceReply(t.Context(), radio); got != zipPrompt {
		t.Fatalf("unexpected missing-position reply: %q", got)
	}
}

func TestLocationFailureReason(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		err  error
		want string
	}{
		{upstreamStatus(http.StatusUnauthorized), "upstream returned HTTP 401"},
		{errNoObservation, "no recent observation from nearby stations"},
		{&url.Error{URL: "https://weather.example/points/32.9,-80", Err: context.DeadlineExceeded}, "request timed out"},
		{&url.Error{URL: "https://weather.example/points/32.9,-80", Err: errors.New("failed")}, "HTTP connection failed"},
	} {
		if got := locationFailureReason(tt.err); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}
