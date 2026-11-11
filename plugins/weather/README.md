# ZIP Weather

Send a private DMR text containing a five-digit U.S. ZIP code, such as `29401`, to the plugin's DMR ID. The plugin replies to the sending radio. An example reply is `29401 68F Partly Cloudy RH 60% Wind 10 mph 1835Z`. The time is the UTC time of the station reading. Humidity and wind appear when the station reports them.

## Build and run

From the repository root:

```sh
go build -o dmrhub-weather ./plugins/weather/cmd/weather
```

On Windows, use `-o dmrhub-weather.exe`.

1. In DMRHub, enable **Accept plugins that run as their own programs** in **Admin > Setup > Plugins**, and restart the hub to start its plugin listener.
2. In **Admin > Plugins**, add and enable an external plugin called **ZIP Weather**. Give it a free DMR ID and copy its token, listener address, and certificate fingerprint from the plugin's page.
3. Set `DMRHUB_PLUGIN_TOKEN` in the plugin process's environment. Run the binary with the listener address and fingerprint. Omit the fingerprint when the listener uses a certificate from a trusted CA.

```sh
./dmrhub-weather -address hub.example.org:3006 -fingerprint 4F:2A:...
```

4. Send `29401` as a private text to the assigned ID from a radio on the owner's hotspot or an admin-trusted repeater. DMRHub uses the original radio's text format, timeslot, and repeater for the reply. The hub can reply only in a format it can reproduce. For example, it cannot reply to UDT texts.

ZIP Weather runs as an external plugin. Its host needs outbound HTTPS access to `api.zippopotam.us` and `api.weather.gov`. No weather API key is required. Zippopotam.us receives the ZIP code, and NWS receives the resulting coordinates and requests for nearby station observations. The provider requests contain no radio ID or callsign. The plugin checks up to three nearby stations and uses a reading at most two hours old. If it cannot find a usable observation, the radio receives a weather-unavailable reply.

- [DMRHub plugin administration](../../doc/admin/plugins.md)
- [Zippopotam.us postal-code lookup](https://docs.zippopotam.us/docs/v1/)
- [NWS API and station observations](https://www.weather.gov/documentation/services-web-api)
