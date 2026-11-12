# ZIP Weather

Send a private DMR text containing a five-digit U.S. ZIP code, such as `29401`, to the plugin's DMR ID. The plugin replies to the sending radio. An example reply is `29401 68F Partly Cloudy RH 60% Wind 10 mph 1835Z`. The time is the UTC time of the station reading. Humidity and wind appear when the station reports them.

Make a private voice call to the plugin's DMR ID and release PTT. When location weather is configured, ZIP Weather uses the calling radio's last DMRHub position to send current conditions. Otherwise it sends `Please reply with your ZIP code`. Send the ZIP as a private text to the same ID. Voice-call replies use full ETSI IP.

## Upload to DMRHub

Download the **dmrhub-weather-wasm** artifact from a successful **Package ZIP Weather** workflow run and extract `dmrhub-weather.wasm`. To build the same file from the repository root, run:

```sh
GOOS=wasip1 GOARCH=wasm go build -trimpath -ldflags='-s -w' -o dmrhub-weather.wasm ./plugins/weather/cmd/weather
```

In PowerShell:

```powershell
$env:GOOS = 'wasip1'
$env:GOARCH = 'wasm'
go build -trimpath -ldflags='-s -w' -o dmrhub-weather.wasm ./plugins/weather/cmd/weather
Remove-Item Env:GOOS, Env:GOARCH
```

1. In DMRHub, make sure **Run plugins inside DMRHub** is on in **Admin > Setup > Plugins**.
2. Under **Admin > Plugins > Upload a plugin**, select `dmrhub-weather.wasm`.
3. Enter one free DMR ID, select both requested HTTPS hosts, and click **Upload and start**.
4. Send `29401` as a private text to that ID from a radio on the owner's hotspot or an admin-trusted repeater.

ZIP Weather accepts ZIP codes in any private text format DMRHub can decode. The plugin decodes compressed ETSI IP requests directly and sends a full ETSI IP reply on the same timeslot through DMRHub's private data routing. Other formats require DMRHub v1.3.28 or newer and a reply format the hub can reproduce.

To update an existing installation, open **ZIP Weather** under **Admin > Plugins** and select **Upload a new version**. This keeps its assigned DMR ID and host permissions.

## Run as a separate program

Build with `go build -o dmrhub-weather ./plugins/weather/cmd/weather` (use `dmrhub-weather.exe` on Windows). Enable the external plugin listener, add **ZIP Weather** as an external plugin, and assign it a free DMR ID. Set `DMRHUB_PLUGIN_TOKEN` and use the listener address and certificate fingerprint shown in DMRHub.

```sh
./dmrhub-weather -address hub.example.org:3006 -fingerprint 4F:2A:...
```

Omit the fingerprint when the listener uses a certificate from a trusted CA.

### Weather from the existing Location plugin

This option runs ZIP Weather as a separate program and uses the existing DMRHub Positions API. The built-in Location plugin continues collecting fixes as usual.

1. Create a personal token in **Settings > API tokens**. Select **Only what I pick** and grant `positions:read`. The token can read positions for the account's radios and radios shared with that account.
2. Set `ZIP_WEATHER_HUB_URL` to the hub's HTTPS web address and `ZIP_WEATHER_POSITIONS_TOKEN` to the personal API token in the external program's environment. Keep the separate `DMRHUB_PLUGIN_TOKEN` for the plugin listener connection.
3. If replacing an uploaded ZIP Weather instance, turn that instance off before assigning its ID to the external instance. Start the external program using the listener address and fingerprint from its plugin page.
4. Make a private voice call to ZIP Weather from a radio whose recent fix is visible on **Positions**. The program uses a fix up to two hours old. A missing, stale or inaccessible fix or a failed weather lookup produces the ZIP prompt.

The **dmrhub-weather-linux** workflow artifact contains Linux executables for amd64 and arm64. Use the binary for the host running the external plugin, or build from source as above.

## Weather data

The plugin uses outbound HTTPS to `api.zippopotam.us` and `api.weather.gov` without a weather API key. Zippopotam.us receives the ZIP code for ZIP requests. NWS receives the lookup coordinates, including the radio's last fix for location requests, and requests for nearby station observations. The weather providers receive no radio ID or callsign. The plugin checks up to three nearby stations and uses a reading at most two hours old. For a ZIP request with no usable observation, the radio receives a weather-unavailable reply.

- [DMRHub plugin administration](../../doc/admin/plugins.md)
- [Zippopotam.us postal-code lookup](https://docs.zippopotam.us/docs/v1/)
- [NWS API and station observations](https://www.weather.gov/documentation/services-web-api)
