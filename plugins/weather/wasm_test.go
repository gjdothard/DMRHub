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
	"os"
	"testing"

	"github.com/USA-RedDragon/DMRHub/internal/plugins/wasm"
)

func TestUploadableManifest(t *testing.T) {
	t.Parallel()
	path := os.Getenv("ZIP_WEATHER_WASM")
	if path == "" {
		t.Skip("set ZIP_WEATHER_WASM to the built module's absolute path")
	}
	module, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
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
