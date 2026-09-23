<div align="center">

# swDefinedRadio

**Five radio receivers, one dongle, one binary.**

Aircraft · tyre sensors · spectrum · broadcast FM — in a browser, with tabs.

[![ci](https://github.com/thatSFguy/swDefinedRadio/actions/workflows/ci.yml/badge.svg)](https://github.com/thatSFguy/swDefinedRadio/actions/workflows/ci.yml)
[![release](https://img.shields.io/github/v/release/thatSFguy/swDefinedRadio?color=blue)](https://github.com/thatSFguy/swDefinedRadio/releases)
[![go](https://img.shields.io/badge/go-1.25-00ADD8)](https://go.dev)
[![deps](https://img.shields.io/badge/dependencies-none-success)](go.mod)

</div>

---

Point a cheap RTL-SDR dongle at the sky and this decodes what it hears.
Everything is pure Go — no cgo, no `librtlsdr` linkage, and not one
third-party module — so `go build` works with nothing but the toolchain.

| | | |
|---|---|---|
| ✈️ **Aircraft** | 1090 MHz | ADS-B: live map, range, alerts for what's worth looking up for |
| 🛩️ **UAT** | 978 MHz | The other half of ADS-B in the US, plus ground stations |
| 🚗 **Tyres** | 315 / 433 MHz | Tyre-pressure sensors, clustered into vehicles as they pass |
| 📡 **Spectrum** | 24 – 1766 MHz | Sweep, waterfall, and what each signal probably is |
| 📻 **FM** | 87.5 – 108 MHz | Broadcast radio, streamed to the browser |

## Try it

```sh
git clone https://github.com/thatSFguy/swDefinedRadio && cd swDefinedRadio
./sdr
```

Then open **<http://localhost:9999>** and pick a tab.

Or take a [release binary](https://github.com/thatSFguy/swDefinedRadio/releases)
— one file per platform, nothing to unpack.

## One radio, five receivers

There is one tuner and one converter, and the receivers want incompatible
settings: ADS-B needs exactly 2 Msps because its slicer assumes two
samples per bit, FM wants 1.2, the scanner sweeps at 2.4. So the tabs are
mutually exclusive — choosing one hands it the radio.

That handover takes about **200 ms**. A single `rtl_tcp` holds the dongle
for the life of the process and switching commands the tuner, rather than
stopping one program and starting another.

And because the process outlives any one receiver, what each has heard
survives being switched away from. Leave the aircraft tab for ten minutes
and the table is still there when you come back — ageing honestly, since
an aircraft last heard four minutes ago has gone whether or not anyone
was listening.

## Commands

`./sdr` builds what it needs and runs it. Naming one receiver runs just
that receiver, which is simpler when it is all you want.

```sh
./sdr                             # every receiver, with tabs
./sdr adsb -lat 51.48 -lon -0.001 # just the aircraft receiver
./sdr fm 98.7                     # listen to a station
./sdr start hub                   # background, logging to logs/
./sdr status                      # what is running, and is the radio usable
./sdr stop
./sdr setpos 51.4779 -0.0015      # save your antenna position
./sdr alerts                      # what the alert rules have caught
./sdr build windows               # cross-compile a single sdr.exe
```

The script exists because three things reliably go wrong otherwise: Go is
not on `PATH` in a non-login shell, only one process can hold the dongle,
and the udev permission resets every time the device is re-attached under
WSL. `./sdr status` checks all three.

## Windows

`./sdr build windows` produces **one** `sdr.exe`. Copy it over and:

```
sdr.exe install
```

It installs under `%LOCALAPPDATA%`, adds itself to your PATH, and
downloads the `rtl-sdr` programs the receivers drive as child processes.
No administrator rights — none of it belongs to the machine rather than
to you. `sdr uninstall` puts it all back.

The one thing it cannot do is the driver. Windows binds an RTL2832U stick
to its television driver, which will not let anything else open it;
[Zadig](https://zadig.akeo.ie/) rebinds it to WinUSB. That needs
administrator rights and a choice only a person should make, since the
same dialog can just as easily unbind something quite different. The
installer checks, and says so.

## Alerts

The aircraft receivers watch for what is worth interrupting you for, and
ship with rules for emergency squawks, military and government blocks,
air ambulances and anything unusually high. Add your own:

```sh
./sdr alerts-example     # writes a starter alerts.json to edit
```

```json
{ "name": "police helicopter", "callsign": ["N911*", "POLICE*"], "urgent": true }
{ "name": "low and close", "below_ft": 3000, "within_nm": 10 }
```

Alerts appear in the page, can beep or raise a desktop notification, and
append to `data/alerts.jsonl`. `-alert-cmd` runs anything you like, with
the details in the environment rather than on the command line — so a
callsign can never become another command.

## The map works offline

Aircraft are drawn on vendored [Leaflet](https://leafletjs.com) over a
[Natural Earth](https://www.naturalearthdata.com) basemap compiled into
the binary. No CDN, no tile server, no API key, and nothing about where
you are leaving the machine. Street tiles are an option, not a
requirement.

## Requirements

* **Go 1.25+** and nothing else from Go's side
* **`rtl-sdr`** — `sudo apt install rtl-sdr`
* **`rtl_433`** — only for the tyre sensors
* **`sox`** — only for FM through this machine's speakers
* An RTL-SDR dongle you can open ([permissions](#permissions))

## How it works

```
cmd/sdr            one binary, every receiver
internal/
  hub/             the tab bar, and which receiver has the radio
  radio/           owns the dongle; hands it over without restarting it
  apps/            the five receivers, each with its own page
  sdr/             IQ sources — an rtl_sdr pipe, or an rtl_tcp client
  modes/ uat/      Mode S and UAT demodulation and decoding
  track/           CPR position recovery and the aircraft table
  scan/ dsp/       sweeping, FFT, windows, peak detection
  demod/ audio/    FM demodulation, and fanning sound out to listeners
```

Receivers say what they need of the radio and never how to arrange it.
Decoding is separate from holding the dongle, which is what lets a tab be
left and come back to something.

## Permissions

The dongle is a USB device, so it needs a udev rule to be openable
without root. Under WSL this resets every time it is re-attached:

```sh
sudo udevadm control --reload-rules
sudo udevadm trigger --action=add --subsystem-match=usb
```

`./sdr status` checks the device node and prints this when it is wrong.

## Testing

```sh
go test ./...                                              # no dongle needed
SDR_HARDWARE=1 go test ./internal/{sdr,radio} -run Hardware -v
```

Demodulators are tested from the transmitting end: a frame is coded,
modulated with noise on it, and pushed back through, so sensitivity is a
number the suite asserts rather than a hope. CPR decoding is checked
against the worked example in ICAO Doc 9871. Handing the radio between
receivers is tested against a fake `rtl_tcp`, so none of it needs
hardware — the few tests that genuinely do skip themselves.

## Going further

The [manual](docs/manual.md) has every flag of every receiver, what the
sweep settings actually do, how to read a waterfall, reaching shortwave
with direct sampling or an upconverter, and what is and is not decodable
on 1090 MHz.

## Licence

[MIT](LICENSE) — do what you like with it.

The receivers drive `rtl_tcp`, `rtl_sdr` and `rtl_433` as separate
processes, over a socket and a pipe, which is what the GPL FAQ calls
communicating at arm's length; they are downloaded from their own
projects rather than carried here, so none of their code is
redistributed. [Leaflet](internal/aircraftui/web/vendor/leaflet/LICENSE)
is vendored under BSD-2-Clause, and the
[Natural Earth](https://www.naturalearthdata.com) basemap is public
domain.
