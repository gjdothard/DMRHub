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
	"os"
	"testing"
	"time"

	"github.com/USA-RedDragon/DMRHub/internal/plugins/wasm"
	pluginv1 "github.com/USA-RedDragon/DMRHub/sdk/plugin/v1"
)

func readModule(t *testing.T) []byte {
	t.Helper()
	path := os.Getenv("ZIP_WEATHER_WASM")
	if path == "" {
		t.Skip("set ZIP_WEATHER_WASM to the built module's absolute path")
	}
	module, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return module
}

func TestUploadableManifest(t *testing.T) {
	t.Parallel()
	module := readModule(t)
	engine := wasm.NewEngine(wasm.DefaultLimits())
	defer func() { _ = engine.Close(t.Context()) }()
	manifest, err := engine.Inspect(t.Context(), module)
	if err != nil {
		t.Fatal(err)
	}
	hosts := manifest.GetHosts()
	if manifest.GetName() != "ZIP Weather" || manifest.GetVersion() == "" ||
		len(hosts) != 2 || hosts[0].GetAddress() != "api.zippopotam.us:443" || hosts[1].GetAddress() != "api.weather.gov:443" {
		t.Fatalf("unexpected DMRHub plugin manifest: %v", manifest)
	}
}

func TestUploadableTextReplies(t *testing.T) {
	t.Parallel()
	module := readModule(t)
	engine := wasm.NewCachedEngine(wasm.DefaultLimits())
	defer func() { _ = engine.Close(t.Context()) }()

	for _, tt := range []struct {
		name       string
		exactReply bool
	}{
		{name: "v1.3.28", exactReply: false},
		{name: "v1.3.29 or newer", exactReply: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			instance, err := engine.Start(ctx, tt.name, module, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer instance.Stop()

			stream := instance.Stream(ctx)
			if _, err := stream.Recv(); err != nil {
				t.Fatal(err)
			}
			features := []pluginv1.Feature{pluginv1.Feature_FEATURE_TEXTS, pluginv1.Feature_FEATURE_SEND_TEXT}
			if tt.exactReply {
				features = append(features, pluginv1.Feature_FEATURE_PROMPT)
			}
			if err := stream.Send(&pluginv1.ConnectResponse{Message: &pluginv1.ConnectResponse_Welcome{
				Welcome: &pluginv1.Welcome{Name: "ZIP Weather", Features: features},
			}}); err != nil {
				t.Fatal(err)
			}
			sub, err := stream.Recv()
			if err != nil || !sub.GetSubscribe().GetTexts() {
				t.Fatalf("expected a text subscription, got %v, %v", sub, err)
			}

			// An invalid ZIP exercises the reply path without contacting a provider.
			incoming := &pluginv1.Text{Id: 12, Source: 1234567, Destination: 5060, Repeater: 11, Slot: 2, Body: "hello"}
			if err := stream.Send(&pluginv1.ConnectResponse{Message: &pluginv1.ConnectResponse_Text{Text: incoming}}); err != nil {
				t.Fatal(err)
			}
			request, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			reply := request.GetRequest().GetSendText()
			if reply.GetSource() != incoming.GetDestination() || reply.GetRadio() != incoming.GetSource() ||
				reply.GetBody() != "Send a 5-digit US ZIP code." {
				t.Fatalf("unexpected text reply: %v", reply)
			}
			if tt.exactReply && reply.GetReplyTo() != incoming.GetId() || !tt.exactReply && reply.ReplyTo != nil {
				t.Fatalf("unexpected reply mode: %v", reply)
			}
		})
	}
}
