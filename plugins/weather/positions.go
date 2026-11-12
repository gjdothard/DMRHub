// SPDX-License-Identifier: AGPL-3.0-or-later

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
	"time"
)

const maxPositionAge = 2 * time.Hour

var errNoPosition = errors.New("no usable recent radio position")

type positionAPI struct {
	base   *url.URL
	token  string
	client *http.Client
}

func newPositionAPI(baseURL, token string) (*positionAPI, error) {
	if baseURL == "" && token == "" {
		return nil, nil
	}
	if baseURL == "" || token == "" {
		return nil, errors.New("location weather requires both ZIP_WEATHER_HUB_URL and ZIP_WEATHER_POSITIONS_TOKEN")
	}
	base, err := url.Parse(baseURL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" || base.User != nil ||
		base.ForceQuery || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("ZIP_WEATHER_HUB_URL must be an HTTPS URL without credentials, query or fragment")
	}
	return &positionAPI{
		base:  base,
		token: token,
		client: &http.Client{
			Timeout: requestWait,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (a positionAPI) latest(ctx context.Context, radio uint32) (float64, float64, error) {
	endpoint := a.base.JoinPath("api", "v1", "positions", strconv.FormatUint(uint64(radio), 10))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), http.NoBody)
	if err != nil {
		return 0, 0, fmt.Errorf("build positions request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
	res, err := a.client.Do(req)
	if err != nil {
		return 0, 0, fmt.Errorf("request radio position: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return 0, 0, errNoPosition
	}
	if res.StatusCode != http.StatusOK {
		return 0, 0, upstreamStatus(res.StatusCode)
	}
	var response struct {
		Latest *struct {
			RadioID    uint32    `json:"radio_id"`
			Latitude   *float64  `json:"latitude"`
			Longitude  *float64  `json:"longitude"`
			FixTime    time.Time `json:"fix_time"`
			ReceivedAt time.Time `json:"received_at"`
		} `json:"latest"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, maxResponseBytes)).Decode(&response); err != nil {
		return 0, 0, fmt.Errorf("decode radio position: %w", err)
	}
	p := response.Latest
	if p == nil || p.RadioID != radio || p.Latitude == nil || p.Longitude == nil {
		return 0, 0, errNoPosition
	}
	lat, lon := *p.Latitude, *p.Longitude
	if !validCoordinates(lat, lon) || (lat == 0 && lon == 0) {
		return 0, 0, errNoPosition
	}
	at := p.FixTime
	if at.IsZero() {
		at = p.ReceivedAt
	}
	if age := time.Since(at); at.IsZero() || age < -5*time.Minute || age > maxPositionAge {
		return 0, 0, errNoPosition
	}
	return lat, lon, nil
}
