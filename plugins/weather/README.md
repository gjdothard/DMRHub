# ZIP Weather

Send a private DMR text containing a five-digit U.S. ZIP code, such as `29401`, to the plugin's DMR ID. The plugin replies to the sending radio. An example reply is `29401 68F Partly Cloudy RH 60% Wind 10 mph 1835Z`. The time is the UTC time of the station reading. Humidity and wind appear when the station reports them.

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

ZIP Weather needs DMRHub v1.3.28 or newer and accepts ZIP codes in any private text format DMRHub can decode. A compressed ETSI IP request gets a full ETSI IP reply on the same timeslot through DMRHub's private data routing. Other requests use the hub's text reply API, which requires a reproducible reply format.

To update an existing installation, open **ZIP Weather** under **Admin > Plugins** and select **Upload a new version**. This keeps its assigned DMR ID and host permissions.

## Run as a separate program

Build with `go build -o dmrhub-weather ./plugins/weather/cmd/weather` (use `dmrhub-weather.exe` on Windows). Enable the external plugin listener, add **ZIP Weather** as an external plugin, and assign it a free DMR ID. Set `DMRHUB_PLUGIN_TOKEN` and use the listener address and certificate fingerprint shown in DMRHub.

```sh
./dmrhub-weather -address hub.example.org:3006 -fingerprint 4F:2A:...
```

Omit the fingerprint when the listener uses a certificate from a trusted CA.

## Weather data

The plugin uses outbound HTTPS to `api.zippopotam.us` and `api.weather.gov` without a weather API key. Zippopotam.us receives the ZIP code, and NWS receives the resulting coordinates and requests for nearby station observations. The provider requests contain no radio ID or callsign. The plugin checks up to three nearby stations and uses a reading at most two hours old. If it cannot find a usable observation, the radio receives a weather-unavailable reply.

- [DMRHub plugin administration](../../doc/admin/plugins.md)
- [Zippopotam.us postal-code lookup](https://docs.zippopotam.us/docs/v1/)
- [NWS API and station observations](https://www.weather.gov/documentation/services-web-api)
