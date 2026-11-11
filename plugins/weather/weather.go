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
		Version:     "0.1.2",
		Description: "Replies to a private DMR text containing a five-digit U.S. ZIP code with current weather observations.",
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
}

// Serve subscribes to private text requests and answers through the hub.
func Serve(ctx context.Context, conn *plugin.Conn) error {
	if !conn.Has(pluginv1.Feature_FEATURE_TEXTS) ||
		(!conn.Has(pluginv1.Feature_FEATURE_PROMPT) && !conn.Has(pluginv1.Feature_FEATURE_SEND_TEXT)) {
		slog.Warn("ZIP Weather: the hub does not support receiving and sending plugin texts; use DMRHub v1.3.28 or newer")
		for {
			if _, err := conn.Recv(); err != nil {
				return fmt.Errorf("receive from hub: %w", err)
			}
		}
	}
	if err := conn.Subscribe(&pluginv1.Subscribe{Texts: true}); err != nil {
		return fmt.Errorf("subscribe to weather texts: %w", err)
	}

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
	etsi := &etsiReplies{links: make(map[airRoute]*airLink), send: conn.Send, busy: make(chan struct{}, 1)}
	r := responder{
		lookup: weatherAPI{client: client}.lookup,
		etsi:   etsi,
		reply: func(ctx context.Context, source uint32, t *pluginv1.Text, body string) (string, error) {
			if cc, ok := etsi.match(t); ok {
				return etsi.reply(ctx, source, t, body, cc)
			}
			if conn.Has(pluginv1.Feature_FEATURE_PROMPT) {
				format, err := conn.ReplyText(ctx, source, t, body)
				if errors.Is(err, plugin.ErrNoFormat) {
					if cc, ok := etsi.match(t); ok {
						return etsi.reply(ctx, source, t, body, cc)
					}
				}
				return format, err
			}
			// Older hubs choose the format and route from the radio's last
			// supported text through an owned or trusted repeater.
			return conn.SendText(ctx, source, t.GetSource(), body)
		},
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
	seen := make(map[uint64]time.Time)
	var pruneAt time.Time
	for {
		message, err := recv()
		if err != nil {
			return fmt.Errorf("receive weather text: %w", err)
		}
		if b := message.GetBurst(); b != nil && r.etsi != nil {
			r.etsi.observe(b)
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
		slog.Debug("ZIP Weather: the hub has no usable text format for the reply", "incoming_format", t.GetFormat())
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
