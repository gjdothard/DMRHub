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
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/DMRHub/sdk/plugin"
	pluginv1 "github.com/USA-RedDragon/DMRHub/sdk/plugin/v1"
)

const (
	maxPending   = 4
	lookupWait   = 15 * time.Second
	requestWait  = 4 * time.Second
	replyWait    = 15 * time.Second
	duplicateTTL = 10 * time.Minute
)

// Hello describes the weather plugin. The hub admin assigns its DMR ID.
func Hello() *pluginv1.Hello {
	return &pluginv1.Hello{
		Name:        "ZIP Weather",
		Version:     "0.1.0",
		Description: "Replies to a private DMR text containing a five-digit U.S. ZIP code with current weather observations.",
	}
}

type weatherLookup func(context.Context, string) (observation, error)
type textReply func(context.Context, uint32, *pluginv1.Text, string) (string, error)

type responder struct {
	lookup weatherLookup
	reply  textReply
}

// Serve subscribes to private text requests and answers through the hub.
func Serve(ctx context.Context, conn *plugin.Conn) error {
	if !conn.Has(pluginv1.Feature_FEATURE_TEXTS) || !conn.Has(pluginv1.Feature_FEATURE_PROMPT) {
		return fmt.Errorf("ZIP Weather needs the hub's text and reply features: %w", plugin.ErrUnsupported)
	}
	if err := conn.Subscribe(&pluginv1.Subscribe{Texts: true}); err != nil {
		return fmt.Errorf("subscribe to weather texts: %w", err)
	}

	client := &http.Client{
		Timeout: requestWait,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	r := responder{lookup: weatherAPI{client: client}.lookup, reply: conn.ReplyText}
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
	seen := make(map[uint64]time.Time)
	var pruneAt time.Time
	for {
		message, err := recv()
		if err != nil {
			return fmt.Errorf("receive weather text: %w", err)
		}
		t := message.GetText()
		if t == nil || t.GetGroup() || t.GetRepeater() == 0 || t.GetId() == 0 {
			continue
		}

		now := time.Now()
		if !now.Before(pruneAt) {
			for id, expires := range seen {
				if !now.Before(expires) {
					delete(seen, id)
				}
			}
			pruneAt = now.Add(time.Minute)
		}
		if now.Before(seen[t.GetId()]) {
			continue
		}
		select {
		case active <- struct{}{}:
			seen[t.GetId()] = now.Add(duplicateTTL)
			pending.Go(func() {
				defer func() { <-active }()
				r.answer(ctx, t)
			})
		default:
			slog.Debug("ZIP Weather: all workers busy", "text_id", t.GetId())
		}
	}
}

func (r responder) answer(ctx context.Context, t *pluginv1.Text) {
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
	_, err := r.reply(replyCtx, t.GetDestination(), t, body)
	if errors.Is(err, plugin.ErrInvalid) && short != "" && short != body {
		_, err = r.reply(replyCtx, t.GetDestination(), t, short)
	}
	switch {
	case errors.Is(err, plugin.ErrNoFormat):
		slog.Debug("ZIP Weather: hub cannot reply in the incoming format", "format", t.GetFormat())
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
