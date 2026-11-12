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

// Package weather answers private DMR texts containing a U.S. ZIP code.
package weather

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/DMRHub/sdk/plugin"
	pluginv1 "github.com/USA-RedDragon/DMRHub/sdk/plugin/v1"
	"github.com/USA-RedDragon/dmrgo/v2/enums"
	"github.com/USA-RedDragon/dmrgo/v2/layer2"
)

const (
	maxPending   = 4
	lookupWait   = 15 * time.Second
	requestWait  = 4 * time.Second
	replyWait    = 15 * time.Second
	duplicateTTL = 10 * time.Minute
	zipPrompt    = "Please reply with your ZIP code"
)

// Hello describes the weather plugin. The hub admin assigns its DMR ID.
func Hello() *pluginv1.Hello {
	return &pluginv1.Hello{
		Name:        "ZIP Weather",
		Version:     "0.1.5",
		Description: "Replies to a private DMR text with weather for a five-digit U.S. ZIP code. After a private call, sends local weather if a recent position is available, or asks for a ZIP code.",
		Hosts: []*pluginv1.Host{
			{Address: zipAPIHost + ":443", Reason: "Looks up the coordinates of a U.S. ZIP code."},
			{Address: nwsAPIHost + ":443", Reason: "Gets nearby National Weather Service observations."},
		},
	}
}

type weatherLookup func(context.Context, string) (observation, error)
type textReply func(context.Context, uint32, *pluginv1.Text, string) (string, error)

type responder struct {
	lookup weatherLookup
	reply  textReply
	etsi   *etsiReplies
	local  func(context.Context, uint32) (observation, error)
}

// Serve answers private text requests and private voice calls through the hub.
func Serve(ctx context.Context, conn *plugin.Conn) error {
	positions, err := newPositionAPI(os.Getenv("ZIP_WEATHER_HUB_URL"), os.Getenv("ZIP_WEATHER_POSITIONS_TOKEN"))
	if err != nil {
		return err
	}
	if positions != nil {
		defer positions.client.CloseIdleConnections()
	}
	if conn.Has(pluginv1.Feature_FEATURE_TEXTS) {
		if err := conn.Subscribe(&pluginv1.Subscribe{Texts: true}); err != nil {
			return fmt.Errorf("subscribe to weather texts: %w", err)
		}
	} else {
		slog.Warn("ZIP Weather: hub text events unavailable; listening for compressed ETSI IP bursts")
	}
	slog.Info("ZIP Weather: ready", "text_events", conn.Has(pluginv1.Feature_FEATURE_TEXTS),
		"exact_replies", conn.Has(pluginv1.Feature_FEATURE_PROMPT), "positions", positions != nil)

	client := &http.Client{
		Timeout: requestWait,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
				return conn.Dial(ctx, address)
			},
			// Two connections per host stay within the four-connection sandbox limit.
			MaxConnsPerHost: 2,
			IdleConnTimeout: 30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()
	api := weatherAPI{client: client}
	etsi := &etsiReplies{links: make(map[airRoute]*airLink), send: conn.Send, busy: make(chan struct{}, 1)}
	r := responder{
		lookup: api.lookup,
		etsi:   etsi,
		reply: func(ctx context.Context, source uint32, t *pluginv1.Text, body string) (string, error) {
			if conn.Has(pluginv1.Feature_FEATURE_PROMPT) {
				return conn.ReplyText(ctx, source, t, body)
			}
			// Older hubs choose the format and route from the radio's last
			// supported text through an owned or trusted repeater.
			return conn.SendText(ctx, source, t.GetSource(), body)
		},
	}
	if positions != nil {
		r.local = func(ctx context.Context, radio uint32) (observation, error) {
			lat, lon, err := positions.latest(ctx, radio)
			if err != nil {
				return observation{}, err
			}
			return api.lookupPoint(ctx, lat, lon)
		}
	}
	return r.receive(ctx, conn.Recv)
}

func (r responder) receive(ctx context.Context, recv func() (*pluginv1.ConnectResponse, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	var pending sync.WaitGroup
	defer func() {
		cancel()
		pending.Wait()
	}()

	active := make(chan struct{}, maxPending)
	dispatch := func(radio uint32, work func()) bool {
		select {
		case active <- struct{}{}:
			pending.Go(func() {
				defer func() { <-active }()
				work()
			})
			return true
		default:
			slog.Warn("ZIP Weather: all workers busy", "radio", radio)
			return false
		}
	}
	seen := make(map[uint64]time.Time)
	type voiceCall struct{ source, destination, stream uint32 }
	called := make(map[voiceCall]time.Time)
	var pruneAt time.Time
	for {
		message, err := recv()
		if err != nil {
			return fmt.Errorf("receive weather request: %w", err)
		}
		now := time.Now()
		if !now.Before(pruneAt) {
			for id, expires := range seen {
				if !now.Before(expires) {
					delete(seen, id)
				}
			}
			for call, expires := range called {
				if !now.Before(expires) {
					delete(called, call)
				}
			}
			pruneAt = now.Add(time.Minute)
		}
		if b := message.GetBurst(); b != nil && r.etsi != nil {
			if plugin.IsTerminator(b) {
				call := voiceCall{b.GetSource(), b.GetDestination(), b.GetStreamId()}
				if t, cc := voicePromptRoute(b, now); t != nil && !now.Before(called[call]) {
					if dispatch(t.GetSource(), func() { r.promptCaller(ctx, t, cc) }) {
						called[call] = now.Add(duplicateTTL)
					}
				}
			} else if t, cc := r.etsi.observe(b); t != nil {
				slog.Info("ZIP Weather: decoded compressed ETSI IP request", "radio", t.GetSource(), "repeater", t.GetRepeater(), "slot", t.GetSlot())
				dispatch(t.GetSource(), func() {
					r.answerWith(ctx, t, func(ctx context.Context, source uint32, text *pluginv1.Text, body string) (string, error) {
						return r.etsi.reply(ctx, source, text, body, cc)
					})
				})
			}
			continue
		}
		if refusal := message.GetRefusal(); refusal != nil {
			slog.Warn("ZIP Weather: the hub refused a reply burst", "reason", refusal.GetReason(), "stream", refusal.GetStreamId())
			continue
		}
		t := message.GetText()
		if t == nil || t.GetGroup() || t.GetRepeater() == 0 || t.GetId() == 0 {
			continue
		}

		if now.Before(seen[t.GetId()]) {
			continue
		}
		if r.etsi != nil && r.etsi.matches(t) {
			seen[t.GetId()] = now.Add(duplicateTTL)
			continue
		}
		slog.Info("ZIP Weather: received hub text", "text_id", t.GetId(), "radio", t.GetSource(), "format", t.GetFormat(), "slot", t.GetSlot())
		if dispatch(t.GetSource(), func() { r.answerWith(ctx, t, r.replyHubText) }) {
			seen[t.GetId()] = now.Add(duplicateTTL)
		}
	}
}

func voicePromptRoute(b *pluginv1.Burst, now time.Time) (*pluginv1.Text, enums.ColorCode) {
	if b.GetGroupCall() || b.GetSource() == 0 || b.GetDestination() == 0 || b.GetRepeater() == 0 || b.GetStreamId() == 0 ||
		(b.GetSlot() != 1 && b.GetSlot() != 2) || len(b.GetPayload()) != len(layer2.RawBurst{}) {
		return nil, 0
	}
	raw := layer2.RawBurst(b.GetPayload())
	slotType := raw.SlotType()
	if slotType.DataType != enums.DataTypeTerminatorWithLC {
		return nil, 0
	}
	return &pluginv1.Text{
		Source: b.GetSource(), Destination: b.GetDestination(), Repeater: b.GetRepeater(), Slot: b.GetSlot(), At: now.UnixMilli(),
	}, slotType.ColorCode
}

func (r responder) promptCaller(ctx context.Context, t *pluginv1.Text, cc enums.ColorCode) {
	slog.Info("ZIP Weather: private voice call ended", "radio", t.GetSource(), "slot", t.GetSlot())
	body := r.voiceReply(ctx, t.GetSource())
	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, replyWait)
	defer cancel()
	if _, err := r.etsi.reply(ctx, t.GetDestination(), t, body, cc); err != nil {
		slog.Warn("ZIP Weather: could not send the voice call reply", "radio", t.GetSource(), "error", err)
	}
}

func (r responder) voiceReply(ctx context.Context, radio uint32) string {
	if r.local == nil {
		return zipPrompt
	}
	ctx, cancel := context.WithTimeout(ctx, lookupWait)
	defer cancel()
	obs, err := r.local(ctx, radio)
	if err != nil {
		if !errors.Is(err, errNoPosition) && !errors.Is(err, errHTTPNotFound) {
			// Provider errors can include the coordinates in the request URL.
			slog.Warn("ZIP Weather: location-based weather unavailable", "radio", radio)
		}
		return zipPrompt
	}
	body, _ := obs.messages("Local WX")
	return body
}

func (r responder) replyHubText(ctx context.Context, source uint32, t *pluginv1.Text, body string) (string, error) {
	if r.etsi != nil && r.etsi.matches(t) {
		return "", nil
	}
	format, err := r.reply(ctx, source, t, body)
	if errors.Is(err, plugin.ErrNoFormat) && r.etsi != nil && r.etsi.matches(t) {
		return "", nil
	}
	return format, err
}

func (r responder) answer(ctx context.Context, t *pluginv1.Text) {
	r.answerWith(ctx, t, r.reply)
}

func (r responder) answerWith(ctx context.Context, t *pluginv1.Text, reply textReply) {
	zip := strings.TrimSpace(t.GetBody())
	var body, short string
	if validZIP(zip) {
		lookupCtx, cancel := context.WithTimeout(ctx, lookupWait)
		obs, err := r.lookup(lookupCtx, zip)
		cancel()
		switch {
		case err == nil:
			body, short = obs.messages(zip)
		case errors.Is(err, errUnknownZIP):
			body = "ZIP not found: " + zip
		default:
			slog.Debug("ZIP Weather: lookup failed", "zip", zip, "error", err)
			body = "Weather unavailable: " + zip
		}
	} else {
		body = "Send a 5-digit US ZIP code."
	}
	if ctx.Err() != nil {
		return
	}

	replyCtx, cancel := context.WithTimeout(ctx, replyWait)
	defer cancel()
	_, err := reply(replyCtx, t.GetDestination(), t, body)
	if errors.Is(err, plugin.ErrInvalid) && short != "" && short != body {
		_, err = reply(replyCtx, t.GetDestination(), t, short)
	}
	switch {
	case errors.Is(err, plugin.ErrNoFormat):
		slog.Warn("ZIP Weather: the hub has no usable text format for the reply", "text_id", t.GetId(), "radio", t.GetSource(), "incoming_format", t.GetFormat())
	case err != nil:
		slog.Warn("ZIP Weather: reply failed", "text_id", t.GetId(), "error", err)
	}
}

func validZIP(zip string) bool {
	if len(zip) != 5 {
		return false
	}
	for _, c := range zip {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
