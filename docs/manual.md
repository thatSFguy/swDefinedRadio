# The manual

Everything in detail: every flag of every receiver, what the scanner's
settings actually do, reading a waterfall, shortwave with direct sampling
or an upconverter, and what is and is not decodable on 1090 MHz.

The [README](../README.md) is the short version.


Software-defined radio receivers in Go, built around an RTL-SDR dongle.

Everything is pure Go apart from the IQ sample source, which comes from
the `rtl-sdr` command-line tools. There is no cgo and no `librtlsdr`
linkage, so `go build` works with nothing but the Go toolchain.

## Layout

```
cmd/
  hub/          Every receiver at once, with a tab bar to choose between
  adsb/         ADS-B receiver: live aircraft map and JSON API
  tpms/         Tire-pressure sensor logger with vehicle clustering
  uat/          978 MHz UAT receiver (US): the same map, other band
  scanner/      Spectrum sweeper: live plot, waterfall, signal list
  fm/           Broadcast FM receiver: browser player and speaker output
internal/
  apps/         The receivers themselves — what each cmd/ is a way to run
    adsb/       1090 MHz: demodulate, decode, track
    uat/        978 MHz, plus the ground stations only it hears
    tpms/       rtl_433 as a child process, its JSON parsed here
    scanner/    The sweep loop, its settings, and the waterfall store
    fm/         Demodulation and the live audio stream
                (each holds its own web/ — Leaflet and the vector
                 basemap are vendored, so no CDN and no tile server)
  hub/          The tab bar, and which receiver currently has the radio
  radio/        Owns the dongle and hands it between receivers
  sdr/          IQ sources — rtl_sdr subprocess, or an rtl_tcp client
  dsp/          FFT, windows, magnitude — shared signal processing
  audio/        Fans decoded sound out to the browser and the speaker
  web/          Writing JSON, serving until cancelled, printing the URL
  config/       Shared settings: antenna position, listen address
  modes/        Mode S demodulation and ADS-B message decoding
  track/        CPR position recovery and the aircraft state table
  tpms/         TPMS reading parsing, sensor store, vehicle clustering
  scan/         Sweep engine, band identification, peak detection
  alert/        Rules for the aircraft worth interrupting you for
  uat/          978 MHz: FSK demodulation, Reed-Solomon, UAT messages
  aircraftui/   The map, table and alerts both aircraft receivers serve
  demod/        FM demodulation: discriminator, de-emphasis, decimation
```

`internal/sdr` and `internal/dsp` are deliberately receiver-agnostic so
the next receiver can reuse them. Each `cmd/` is a thin wrapper: flags,
and a call into the package that does the work. That is what lets one
process serve all of them while each still runs on its own.

## Quick start

`./sdr` builds what it needs and runs it:

```sh
./sdr                             # every receiver, with tabs — the usual way
./sdr tpms                        # just the tire-pressure logger, foreground
./sdr adsb -lat 43.2 -lon -85.6   # just the aircraft receiver, foreground
./sdr fm 98.7                     # listen to a broadcast FM station
./sdr uat                         # 978 MHz UAT, United States only
./sdr start hub                   # background, logging to logs/
./sdr status                      # what is running, and is the radio usable
./sdr logs hub                    # follow a background log
./sdr stop                        # stop whatever is running
./sdr setpos 43.16 -85.67         # save your antenna position
./sdr alerts                      # what the alert rules have caught
./sdr build                       # rebuild every binary
./sdr build windows               # cross-compile into dist/windows/
```

With no arguments `./sdr` runs the tabbed app, which serves every
receiver and hands the radio to whichever tab is chosen. Naming one
receiver runs just that receiver, which is the simpler thing when it is
all you want. Arguments after the name are passed straight through.

The script exists because three things reliably go wrong otherwise:

* **Go is not on `PATH`** in a non-login shell. It looks in the usual
  places, including `~/.local/lib/go`.
* **Only one process can hold the dongle.** Starting one receiver stops
  whatever else has it, rather than failing with a libusb error.
* **The udev permission resets** every time the device is re-attached
  under WSL. `./sdr status` checks the device node and prints the fix.

To run it from anywhere, either add the repo to `PATH` or symlink it:

```sh
ln -s "$PWD/sdr" ~/.local/bin/sdr
```

## Your antenna's position

`adsb` wants to know where the receiver is. Save it once:

```sh
./sdr setpos 51.477928 -0.0015450
```

That writes `config.json`, which supplies the **defaults** for `-lat` and
`-lon`. Passing either flag still overrides it, so a one-off run needs no
edit to the file:

```sh
./bin/adsb                                  # 51.4779, -0.0015 (from config.json)
./bin/adsb -lat 51.4700 -lon -0.4543        # 51.4700, -0.4543 (from -lat/-lon)
./bin/adsb -lat 40.0                        # latitude overridden, longitude from the file
```

The receiver logs which source it used at startup, because a stale config
would otherwise put the antenna somewhere surprising without saying so.

### Setting it from the browser

The aircraft map has a **Use my location** button beside the position
in its header. It asks the browser where it is, sends that to the
receiver, and writes it to the config file, so it survives a restart —
the same thing `./sdr setpos` does, without reading coordinates off
another map.

Two things are worth knowing before pressing it:

* **It sets the *receiver's* position, not the viewer's.** That is the
  same thing when the browser is on the machine the dongle is plugged
  into, and wrong when you have opened the page from somewhere else.
* **A desktop has no GPS.** The browser locates it by wifi or by IP
  address, which can be a few hundred metres or a few tens of
  kilometres out. The accuracy comes back with the fix: anything vaguer
  than 5 km is accepted with a warning, since ranges will be out by
  about that much, and anything vaguer than 100 km is refused outright.

Browsers only share a location with a page served over https or from
localhost, so the button reports plainly that it cannot help if the UI
has been opened over the LAN by IP address. `-no-position-api` removes
the endpoint altogether, and the button with it.

The same endpoint is there for anything else that knows where it is:

```sh
curl -X POST localhost:9999/api/position \
  -d '{"lat":51.477928,"lon":-0.001545,"accuracy_m":38}'
```

`config.json` is deliberately not in the repository: it is where you
live. `config.example.json` shows the shape.

Config is looked for in this order, first match winning:

1. `$SDR_CONFIG`
2. `./config.json`
3. `~/.config/sdr/config.json`

Setting a position enables single-frame position fixes — aircraft appear
without waiting for an even/odd pair — and gives every contact a range,
which is the number that tells you how the antenna is performing.

## The web UI

Everything is served on **<http://localhost:9999>**. Opening it gives you
a tab bar with every receiver on it; choosing a tab hands that receiver
the radio.

The tabs are mutually exclusive, because the radio is. There is one tuner
and one converter, and the receivers want incompatible settings — ADS-B
needs exactly 2 Msps at 1090 MHz because its slicer assumes two samples
per bit, FM wants 1.2 Msps, the scanner sweeps at 2.4. So choosing a tab
takes the radio off whichever had it. That takes about 200 ms: the dongle
is held by one long-lived `rtl_tcp` for the life of the process, and
switching commands the tuner rather than restarting anything.

What one process buys over five is that it outlives any of them. The
aircraft table, the sensor store and the alert history belong to the
receiver, not to its turn on the radio, so leaving a tab and coming back
does not start from nothing — which is what happened when switching
receivers meant stopping one program and starting another. Aircraft last
heard several minutes ago still age out while you are away: they have
gone whether or not anyone was listening, and a table frozen mid-flight
would come back showing a map full of things that are not there.

Each receiver keeps exactly the page it had as a command of its own, held
in a frame. Every request is answered by the receiver whose page asked,
not by whichever one currently holds the radio — the two aircraft
receivers serve identical paths, so the difference matters.

Running a single receiver instead (`./sdr adsb`) serves that receiver's
page at the same address with no tab bar, exactly as before.

The port is bound before the radio is touched, so a clash fails
immediately with a clear message rather than leaving something running
with no UI. Override with `-http` if 9999 is taken:

```sh
./sdr -http :9100
./sdr tpms -http :9100
```

## Requirements

* Go 1.25 or newer, and nothing else from Go's side — there are no
  third-party dependencies and no cgo
* `rtl-sdr` (`sudo apt install rtl-sdr`), which provides `rtl_tcp` and
  `rtl_sdr`
* `rtl_433` (`sudo apt install rtl-433`), for the tire sensors only
* `sox` (`sudo apt install sox`), only if you want FM through this
  machine's speakers rather than the browser
* An RTL-SDR dongle the current user can open — see Permissions below

## adsb

Receives 1090 MHz extended squitters and serves a live map.

```sh
go build -o bin/adsb ./cmd/adsb
./bin/adsb -lat 37.7749 -lon -122.4194
```

Then open <http://localhost:9999>.

Passing your own `-lat`/`-lon` is optional but worth doing: it enables
single-frame position fixes, which show aircraft sooner, and it gives
every contact a range so you can tell how well the antenna is doing.
Without it, an aircraft only appears once an even and an odd position
frame have both arrived.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-http` | `:9999` | Address for the web UI and JSON API |
| `-lat`, `-lon` | unset | Receiver position |
| `-gain` | auto | Tuner gain in dB |
| `-ppm` | `0` | Crystal frequency correction |
| `-device` | `0` | RTL-SDR device index |
| `-rtltcp` | unset | Use an `rtl_tcp` server, e.g. `localhost:1234` |
| `-ttl` | `60s` | Forget aircraft unheard for this long |
| `-raw` | off | Print every CRC-valid frame as hex |
| `-freq` | `1090000000` | Centre frequency |
| `-alerts`, `-alert-*` | | Alerts on interesting aircraft, see below |
| `-tile-url` | OpenStreetMap | Street map for the map selector; empty removes the option |
| `-no-position-api` | off | Refuse to set the receiver position over HTTP |

### The map

The map is drawn from vector data compiled into the binary — land,
country borders, state and province lines, large lakes, about a
thousand cities and 890 airports — rather than fetched as tiles from a
tile server. It
needs no network, ever: it looks the same on a plane as it does at
home, and there is no cache to fill, no provider to sign up with and no
usage policy to fall foul of.

The straight lines across the middle of a large lake are not a drawing
fault: state boundaries really do run through the water.

It is coarse on purpose. An aircraft at 35,000 ft is somewhere over a
state, not on a street, so coastlines, borders and a few names are all
the context the icons need. The exception is airports, which are what
the traffic is actually doing: each is a dot and its code, with
military fields in the same amber the alerts use, so a descent into
GRR or a tanker orbiting its home field reads at a glance.

Labels appear as the map is zoomed in, by the rank Natural Earth
assigns them, and only while they are on screen — a wide view is not a
smear of names, and the two thousand of them are not all in the page at
once.

The data is [Natural Earth](https://www.naturalearthdata.com), which is
public domain — no permission, no attribution, no restrictions. It is
built into `internal/aircraftui/web/basemap.json` (about 530 KB) by
`tools/build-basemap.py`, which takes the published GeoJSON, keeps the
six layers above, rounds coordinates to two decimal places — around a
kilometre, a fraction of a pixel at these zooms, though airports keep
four because a point costs nothing — and drops the properties:

```sh
python3 tools/build-basemap.py path/to/natural-earth-geojson internal/aircraftui/web/basemap.json
```

Leaflet itself is vendored in `internal/aircraftui/web/vendor/leaflet` and
compiled in too, so nothing the map needs is loaded from a CDN.

### Choosing the map

The control at the top left of the map switches between:

* **Stored map** — the vector data above. The default, and the one that
  works with no network.
* **Street map (online)** — tiles from a tile server, inverted to sit
  next to a dark panel. Useful on the ground for the detail the stored
  map deliberately lacks.

The choice is remembered, and `?map=street` or `?map=stored` overrides
it, so a particular view can be bookmarked. City and airport labels
belong to the stored map and go with it, since the street tiles carry
their own.

The page fetches street tiles **directly from the tile server**, the
way any Leaflet page does — this receiver neither proxies nor caches
them. `-tile-url` sets the server and defaults to OpenStreetMap;
pointing it at a provider you hold a key for works the same way, and
`-tile-url ""` removes the option from the selector entirely.

**On caching raster tiles.** An earlier version cached OpenStreetMap
tiles on disk and prefetched an area before a flight. It worked, and it
was the wrong thing to do: dragging the map asks for fifty tiles at
once, prefetching an area is bulk downloading by any reading of their
usage policy, and OpenStreetMap quite reasonably answered `403
Forbidden`. A tile server run on donations is not a free CDN for a
hobby receiver. The street option above is an ordinary browser map with
no cache and no prefetching, which is the use their policy contemplates;
everything offline comes from the vector data instead.

### Alerts

Most of what flies over is an airliner going somewhere dull. The
receiver watches for the rest and says so: a row turns amber in the
table, the aircraft gets a pulsing ring on the map, a line appears in
the alerts panel, and — if you ask for them — a beep and a desktop
notification.

The built-in rules are deliberately few, because an alert that fires on
ordinary traffic teaches you to ignore it:

| Rule | What it catches |
| --- | --- |
| emergency squawk | 7500 unlawful interference, 7600 radio failure, 7700 general emergency |
| declared emergency | the crew set an emergency state in a status squitter |
| US military | ICAO address in `ae0000-afffff` |
| UK military | ICAO address in `43c000-43cfff` |
| military callsign | `RCH` (Reach), `CNV`, `SPAR`, `DOOM`, `GRIM`, `JAKE`, `POLO` |
| government VIP | `SAM`, `VENUS`, `EXEC1`, `AF1` |
| air ambulance | `LIFE`, `MEDEVAC`, `MERCY`, `ANGEL` |
| very high | above 50,000 ft |

An aircraft that matches is left alone for 30 minutes afterwards, since
one in range sends several messages a second.

### Your own rules

`./sdr alerts-example` writes a starter `alerts.json`. Rules there are
added to the built-in ones unless `"builtin": false` replaces them:

```json
{
  "cooldown": "30m",
  "rules": [
    { "name": "the neighbour's Cessna", "hex": ["a1b2c3"] },
    { "name": "police helicopter", "callsign": ["N911*"], "urgent": true },
    { "name": "low and close", "below_ft": 3000, "within_nm": 10 },
    { "name": "a country's block", "hex": ["4b0000-4bffff"] }
  ]
}
```

| Condition | Matches |
| --- | --- |
| `hex` | ICAO address: `4b1814`, a prefix `ae*`, or a range `ae0000-afffff` |
| `callsign` | flight ID, with `*` at either end: `RCH*`, `*LIFE*` |
| `squawk` | the Mode A code exactly, e.g. `7700` |
| `emergency` | any declared emergency state |
| `above_ft`, `below_ft` | altitude |
| `above_kts` | ground speed |
| `within_nm` | range from your antenna |
| `urgent` | worth a noise: this is what the beep and the notification are for |

Conditions within one rule are combined — `below_ft` with `within_nm`
means low **and** close — and a rule with no conditions is rejected
rather than quietly matching everything. A broken rules file is
reported and the built-in rules are used, so a typo cannot leave a
receiver silently watching for nothing.

### Alerts elsewhere

Every alert is appended to `data/alerts.jsonl`, one JSON object per
line, so a receiver left running all day can be asked afterwards what
came over:

```sh
./sdr alerts            # the last 20, newest first
./sdr alerts 100
```

`-alert-cmd` runs a shell command for each one, which is how this
reaches a notification on the desktop while the browser is closed:

```sh
./sdr start adsb -alert-cmd 'notify-send "$ALERT_RULE" "$ALERT_TEXT"'
```

The alert arrives in the environment — `ALERT_RULE`, `ALERT_TEXT`,
`ALERT_HEX`, `ALERT_CALLSIGN`, `ALERT_SQUAWK`, `ALERT_EMERGENCY`,
`ALERT_ALTITUDE`, `ALERT_DISTANCE_NM`, `ALERT_LAT`, `ALERT_LON`,
`ALERT_URGENT` — never on the command line, so a callsign from the air
cannot become part of the command. Commands run detached with a 30
second deadline: one that hangs must not take the radio down with it.

| Flag | Default | Meaning |
| --- | --- | --- |
| `-alerts` | `alerts.json` | Rules file; built-in rules are used if it is missing |
| `-alerts-example` | off | Write a starter rules file and exit |
| `-alert-log` | `data/alerts.jsonl` | Where raised alerts are appended |
| `-alert-cmd` | unset | Shell command run for each alert |
| `-no-alerts` | off | Watch for nothing |

### What the alerts cannot see

Squawk and emergency state come from the **aircraft status** squitter,
type code 28. It is the only ADS-B message that carries them, and it is
rare: plenty of aircraft never send one, and an aircraft squawking 7700
on a transponder without ADS-B out will not appear here at all. The
Mode S replies that would otherwise carry a squawk — DF5 and DF21 —
overlay the aircraft address on the parity field, so they cannot be
CRC-checked without already knowing the sender, and this receiver drops
them. An emergency alert is therefore a bonus, not a guarantee.

Address blocks are a much better signal, because every position message
carries the address. The two military blocks built in are the ones that
are unambiguous and well documented; ranges for other countries exist
and are easy to add, but a wrong range means alerts on scheduled
traffic, which is worse than no rule at all.

### API

`GET /api/aircraft` returns the current table:

```json
{
  "now": "2026-09-10T12:00:00Z",
  "stats": { "frames": 18422, "positions": 3310, "tracked": 14, "with_position": 11 },
  "aircraft": [
    {
      "hex": "a1b2c3", "callsign": "UAL422",
      "lat": 37.62, "lon": -122.38, "distance_nm": 12.4,
      "altitude": 8175, "speed": 271.5, "track": 118.2, "vertical_rate": -1216,
      "messages": 214, "signal_dbfs": -18.3,
      "first_seen": "...", "last_seen": "..."
    }
  ]
}
```

## uat

Receives UAT on 978 MHz, which is the other half of ADS-B in the United
States and is not used this way anywhere else.

```sh
./sdr uat
./bin/uat -file capture.iq        # replay a recording instead
```

It serves the same map, table, alerts and position controls as `adsb` —
they share `internal/aircraftui` rather than being two pages that drift
apart — with ground stations added.

### Why a second receiver

Airliners carry 1090 MHz transponders. Light aircraft below 18,000 ft
may instead carry UAT, and ground stations broadcast weather, traffic
and notices on it, so a 1090 MHz receiver never sees any of that. The
two cannot run at once on one dongle — different frequency, different
modulation — so `./sdr` treats them as it treats any two receivers and
lets one hold the radio at a time.

### How it is received

Where 1090 MHz is pulse-position modulation with a parity check, UAT is
continuous-phase FSK at 1.041667 Mbps, framed by a 36-bit sync word,
with every frame Reed-Solomon coded:

* The tuner runs at 2.083334 MS/s, two samples a symbol. The sign of
  the phase advance between consecutive samples is the bit, and since a
  symbol spans two samples the pair is added rather than one being
  picked — the whole of the evidence for a symbol is worth about 3 dB.
* Two interleaved bit streams are tracked, because one lands on the
  symbol centres and the other half a symbol out. Whichever finds the
  sync word wins.
* The sync word says whether an aircraft message or a ground uplink
  follows. The two words are bitwise complements, so a demodulator with
  its polarity inverted would see every message as the other kind —
  worth knowing when nothing decodes at all.
* **Reed-Solomon.** A basic message is RS(30,18), a long one RS(48,34),
  a ground uplink six interleaved RS(92,72) blocks. Those repair six,
  seven and ten wrong bytes, which is most of the receiver's range:
  frames arrive damaged far more often than they arrive clean. Every
  frame survives 7 dB SNR in synthetic tests and about nine in ten
  survive 5 dB.

Position needs no tricks: UAT sends latitude and longitude outright in
every message, so an aircraft appears the moment it is heard rather
than after an even/odd pair as on 1090 MHz.

### Ground stations

`GET /api/ground` lists the ground stations heard and where each says
it is; they appear on the map in green. What they are *sending* —
weather imagery, text, traffic — is a protocol of its own that this
does not decode. Knowing which are audible is still useful: they
transmit constantly, so they are the quickest confirmation that the
antenna is working on this band at all.

### Flags

`uat` takes the same flags as `adsb` — `-http`, `-lat`/`-lon`, `-gain`,
`-ppm`, `-device`, `-ttl`, `-raw`, `-quiet`, the `-alert-*` family and
`-tile-url` — plus:

| Flag | Default | Meaning |
| --- | --- | --- |
| `-freq` | `978000000` | Centre frequency |
| `-file` | unset | Replay a raw IQ capture instead of opening the radio |

### When the band is quiet

978 MHz can be silent for long stretches: ground stations are
line-of-sight and light aircraft are mostly daytime traffic. Before
suspecting the receiver, check whether anything is transmitting at all
— a capture of the band next to one of 1090 MHz settles it quickly,
since 1090 is never quiet.

A capture can be replayed rather than waiting at the antenna:

```sh
rtl_sdr -f 978000000 -s 2083334 -g 0 capture.iq      # record
./bin/uat -file capture.iq                           # and replay it
```

The UI stays up when a recording ends, so what it held can be looked at
rather than flashing past. With nothing to record, a synthetic capture —
three aircraft and a ground station, modulated as the air would carry
them — exercises the whole chain:

```sh
UATOUT=/tmp/sample.iq go test ./internal/uat -run TestWriteSampleCapture
./bin/uat -file /tmp/sample.iq
```

## tpms

Logs tire-pressure sensor transmissions, groups the sensors into
vehicles, and lets you name them.

```sh
go build -o bin/tpms ./cmd/tpms
./bin/tpms
```

Then open <http://localhost:9999>.

Unlike `adsb`, this one does not demodulate anything itself. ADS-B is a
single documented protocol, so writing it out was worthwhile; TPMS is
roughly twenty-five proprietary, undocumented, manufacturer-specific
protocols, and `rtl_433` already implements them all. So `rtl_433` does
the demodulation and this package does everything above it — logging,
clustering, labelling, persistence and the UI.

### Identifying vehicles

A TPMS packet carries a sensor ID, pressure, temperature and some status
flags. There is no make, model or registration in it. Two things get you
to a vehicle anyway:

* The **decoder name** hints at the manufacturer (`Toyota`, `Ford`,
  `Renault`), though `Schrader` alone supplies dozens of makes.
* **Co-occurrence** identifies the actual vehicle. Sensors that keep
  transmitting within seconds of each other are the wheels of one car.
  The store counts these co-occurrences and unions sensors into a
  vehicle once a pair has been heard together at least `MinShared`
  times *and* those shared sightings are at least `MinRatio` of the
  rarer sensor's total. You then name the group once in the UI and it
  sticks.

The thresholds are deliberately conservative: a single coincidental
overlap never merges two cars. Two vehicles that genuinely always travel
together are the one case clustering cannot separate — split them by
hand if it happens.

### Hopping, or staying put

One dongle has 2.4 MHz of bandwidth and the two TPMS bands are 119 MHz
apart, so it cannot cover both at once. The default is to hop, dwelling
`-hop` seconds on each, which hears roughly half of what transmits on
either.

The buttons at the top of the page park it on one band instead:

* **Hop** — both in turn, the default, and the right answer when you do
  not know what you are waiting for.
* **315M** — North American sensors, which is most Toyota, Ford and GM.
* **433.92M** — European sensors, and Hyundai/Kia.

Parking doubles the chance of catching what transmits on that band, at
the cost of hearing nothing from the other. It restarts rtl_433, because
the frequencies are command line arguments, so there is a second or two
of deafness on either side of the change.

Worth knowing if a make is not turning up: sensors transmit while the
wheels are turning, and only rarely when parked. A vehicle has to
actually drive past during a listening window, so hearing nothing from a
make says more about what has gone by than about what is supported.

### When the guess is wrong

Co-occurrence is a guess, and it fails in both directions. Two vehicles
that always travel together — parked side by side, leaving together every
morning — are heard together often enough to be indistinguishable from
one, and they merge. A wheel whose siblings are never heard at the same
moment joins nothing and sits unassigned.

Neither corrects itself with more listening: co-occurrence counts only
ever accumulate, so a merge is permanent. The **vehicle** column in the
sensor table is therefore a choice, not a readout:

* **automatic** — leave it to the clustering, which is the default.
* **a named vehicle** — put this sensor on that vehicle, whatever the
  counts say.
* **on its own** — take it out of the group it was put in. It becomes a
  vehicle of its own, which other sensors can then be moved onto.

A sensor assigned by hand stops taking part in the automatic clustering
altogether, since union-find can merge but never split; it could not be
pulled back out otherwise. Assignments are saved with the sensor table
and survive a restart. Setting a sensor back to **automatic** returns it
to the clustering's judgement.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-freq` | `315M,433.92M` | Frequencies to listen on |
| `-hop` | `30` | Seconds per frequency when more than one is given |
| `-data` | `data/tpms` | Where the log and sensor table live |
| `-http` | `:9999` | Web UI and JSON API address |
| `-gain` | auto | Tuner gain in dB |
| `-ppm` | `0` | Frequency correction |
| `-other` | off | Also report non-TPMS devices `rtl_433` decodes |
| `-quiet` | off | Do not print each reading |

North America uses 315 MHz and Europe 433.92 MHz, but plenty of cars
sold in the US use either, so both are covered by default. The dongle
cannot span them at once — 2.4 MHz of bandwidth against 119 MHz of
separation — so it hops. Pin `-freq` to one band once you know which
yours use, and you will miss fewer transmissions.

### Output

Readings append to `data/tpms/readings.jsonl`, one JSON object per line,
which is the durable log. The sensor table, co-occurrence counts and
your labels live in `data/tpms/state.json`, written atomically every 15
seconds and on exit.

`GET /api/state` returns sensors, vehicles and the most recent readings.
`POST /api/label` with `{"kind":"sensor"|"vehicle","id":...,"label":...}`
names one.

### What to expect

Most sensors only transmit while the wheel is turning above roughly
25 km/h, and then go quiet to save battery. So this records **arrivals
and departures**, not continuous presence — treat a recent sighting as
"the car is here". Expect nothing at all from a parked street.

## scanner

Sweeps the tuner across a range and shows what is transmitting.

```sh
./sdr scanner                                   # 88 MHz to 1090 MHz
./sdr scanner -start 88M -stop 108M             # just the FM band
./bin/scanner -once -start 400M -stop 500M      # one sweep, print and exit
```

Then open <http://localhost:9999> for a live spectrum, a waterfall, and a
ranked list of signals with the band each falls in.

This is the tool `internal/sdr`'s rtl_tcp client was written for: sweeping
means retuning hundreds of times per pass, which an `rtl_sdr` pipe cannot
do. A server is started automatically if none is listening.

### How a sweep works

The tuner sees 2.4 MHz at a time, so covering a gigahertz means stepping
across it. At each step the scanner discards the samples still in flight
from the previous frequency, integrates for the dwell time, takes a
windowed FFT of each block, averages the power per bin, and folds the
result onto one absolute-frequency grid.

Only the middle of each segment is kept — the tuner's response rolls off
at the edges of its passband, so the outer part reads artificially low.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-start`, `-stop` | `88M`, `1090M` | Range to sweep; `k`/`M`/`G` suffixes work |
| `-bins` | `1024` | FFT size; bin width is `rate/bins` |
| `-dwell` | `40ms` | Integration time per step — longer finds weaker signals |
| `-settle` | `60ms` | Samples discarded after each retune |
| `-crop` | `0.75` | Fraction of each segment kept |
| `-offset` | `0` | Move the oscillator off segment centre (see below) |
| `-threshold` | `10` | dB above the noise floor to count as a signal |
| `-sep` | `150000` | Merge signals closer than this many Hz |
| `-once` | off | One sweep, print the table, exit |
| `-rate`, `-gain`, `-ppm`, `-device` | | Tuner settings |

### Changing the range from the browser

The controls above the spectrum set the start and stop frequency without
restarting anything. Presets cover the bands worth jumping to — FM,
airband, 2 m, 70 cm, the ISM segments, UHF TV, ADS-B — and the estimated
sweep time updates as you type, so the cost of a wider span is visible
before committing to it.

Narrowing the range is by far the most effective speed-up: 88-1090 MHz
is 557 steps and about a minute a pass, while the airband alone is 11
steps and under three seconds. A change interrupts the sweep in progress
rather than waiting for it to finish, and clears the waterfall, whose
rows no longer describe the span being measured.

The same settings are available over HTTP:

```sh
curl -s localhost:9999/api/config
curl -s -X POST localhost:9999/api/config \
  -H 'Content-Type: application/json' \
  -d '{"start_hz":118000000,"stop_hz":137000000}'
```

Out-of-range requests are refused with a message saying why, and leave
the running sweep alone.

### How long a sweep takes

Each step costs `settle + dwell`, so the time is simply
`steps x (settle + dwell)`. Sweeping 400-960 MHz at the defaults is 312
steps of 100 ms, which measures out at just over 31 seconds.

Settling dominates, and it cannot be cut much. A retune does not affect
samples already captured: those sit in librtlsdr's USB buffers, and no
amount of draining the socket reaches them — only elapsed time does.

Set it too low and every strong carrier acquires a ghost **exactly one
tuning step away**, because stale samples get folded into the next
segment at the same oscillator-relative offset. On this hardware the
effect appears below roughly 150 ms, which is where the 180 ms default
comes from:

```
-settle 60ms      488.311  (real)   490.111  (ghost, +1.800 = one step)
                  500.311  (real)   502.111  (ghost)
-settle 180ms     488.311            500.311            518.311
```

So narrowing the range is the lever that actually works, and the browser
controls make that a click.

### Two things that will otherwise mislead you

**One transmission is not one peak.** A broadcast FM carrier spans about
200 kHz and its stereo subcarriers dip below the detection threshold
between them, so a single station reads as five or six separate signals.
`-sep` merges runs closer together than the given spacing; the default of
150 kHz is tuned for FM. Lower it when hunting narrowband signals.

**The receiver invents a signal at every step.** The RTL2832U puts a
large DC offset spike wherever its oscillator sits. That is an artefact,
not a transmitter, and if it is not blanked widely enough its skirt shows
up as a fake narrow peak at each segment centre — which looks entirely
plausible until you notice the frequencies are exactly your own step
spacing. The scanner blanks a proportional guard band around it.

Blanking leaves a narrow blind spot at each segment centre. To remove it
entirely, move the oscillator out of the kept window:

```sh
./bin/scanner -crop 0.4 -offset 600000
```

The window must then be under half the sample rate, so the sweep takes
about twice as many steps. Worth it when examining a specific frequency,
unnecessary for a general survey.

**The receiver invents signals of its own.** Beyond the DC spike, the
dongle's 28.8 MHz crystal produces mixing products on exact multiples of
4.8 MHz — 460.8, 480.0, 489.6, 691.2 MHz and so on. They are narrow,
strong and perfectly stable, which makes them look more like real
transmitters than real transmitters do. The scanner flags them rather
than removing them, since a genuine signal can sit on one of those
frequencies:

```
   480.000 MHz    -17.8 dB   16.4 kHz  * likely receiver spur
```

### Telling a real signal from an artefact

Sweep the same span twice with the start frequency shifted by a fraction
of a step. A real transmitter stays at the same absolute frequency; an
artefact tied to the oscillator moves with it.

```sh
./bin/scanner -once -start 380M   -stop 390M
./bin/scanner -once -start 380.3M -stop 390.3M
```

Note this catches artefacts tied to the *oscillator*, but not crystal
spurs — those sit at fixed absolute frequencies and survive the test,
which is why they are flagged by arithmetic instead.

### A sanity check that the frequencies are right

Sweep the UHF TV band. Every ATSC station puts a strong, narrow pilot
carrier exactly 0.3094 MHz above its channel's lower edge, and channel 14
begins at 470 MHz. If the numbers come back as 488.309, 500.309, 512.309
and so on, the whole chain — tuning, FFT, folding onto the frequency
grid — is correct to about a kilohertz.

```sh
./bin/scanner -once -start 470M -stop 600M -threshold 14
```

## airband

Aircraft voice, on the AM channels between 118 and 137 MHz. The 1090 MHz
receiver shows where aircraft are; this one lets you hear them. One
dongle cannot do both, so they are separate tabs.

### Finding channels

Tower, ground and approach frequencies differ at every airport, so the
receiver ships only with the ones that mean the same thing everywhere —
121.5 guard, unicom, CTAF. **Find channels** sweeps the band and offers
what it heard.

It offers rather than adds, and the difference matters. A sweep proves
something was transmitting on a frequency; it does not prove the
frequency is worth keeping. An intermittent noise source, a harmonic, or
a distant airport heard once all look identical on the one pass it gets.
So each result gets a **Listen** button: tune to it, and if there is
speech, **Keep** it under a name. If there is hiss, **Discard** it.

Three kinds of rubbish are filtered out before anything is offered:

* **Spurs.** The receiver's own 4.8 MHz clock produces peaks at
  multiples of itself, and they are often the strongest things in the
  band. The sweeper already recognises them.
* **Off-grid peaks.** Airband sits on a 25 kHz grid and a sweep resolves
  a few kHz, so results are snapped to the nearest channel. One sitting
  halfway between two is either mismeasured or one of the 8.33 kHz
  channels, and is dropped rather than guessed at.
* **Peaks too wide to be voice.** An AM channel is about 8 kHz of speech
  in a 25 kHz slot. Anything far broader is not somebody talking.

### Scanning

The band is silent between transmissions, so scanning matters more than
tuning. It moves along the list, stops on whoever keys up, and holds the
channel for a moment after they stop — the reply comes back on the same
frequency, and moving on between the two halves of an exchange is the
most annoying thing a scanner can do.

Choosing a channel parks on it and stops the scan.

### Squelch

The one control that matters here. It compares the carrier level against
a threshold, which works well on AM because an idle airband channel has
no carrier at all. The meter shows the current level and marks where the
threshold sits; put it just above the noise.

Recovered audio is divided by the carrier, so a distant aircraft is as
loud as a close one — what you hear is modulation depth, not how
strongly the signal arrived.

## fm

Receives one broadcast FM station and plays it, in the browser or
through this machine's speakers.

```sh
./sdr fm 98.7                     # tune 98.7 MHz, play in the browser
./sdr fm 98.7 -speaker            # and through this machine's audio too
./bin/fm -freq 107.9M -deemph eu  # European de-emphasis
```

Open <http://localhost:9999> for a player with a signal meter, a dial
you can retune from, and presets. Retuning does not restart anything —
the tuner is moved under the running demodulator, so a station change
takes about as long as one audio block.

The audio is served as an endless WAV stream at `/audio.wav`, which
every browser plays from a plain `<audio>` element with no client-side
decoding. Any listener that falls behind has blocks dropped rather than
being allowed to stall the radio, so a browser tab left paused in the
background cannot break reception for anything else.

### How the demodulation works

The tuner runs at 1.2 MS/s. A 63-tap low-pass keeps the 200 kHz the
station occupies and rejects its neighbours, decimating by five to a
240 kHz IF. Multiplying each sample by the conjugate of the one before
it gives a value whose angle is the change in phase, and that phase
advance *is* the audio — this is the quadrature discriminator, and it
is four lines of Go. A second filter keeps 15 kHz and decimates by five
again to 48 kHz, then a one-pole de-emphasis filter undoes the treble
boost the broadcaster applied.

The output is mono. The 15 kHz audio filter deliberately cuts below the
19 kHz stereo pilot, so the L−R subcarrier at 38 kHz — and the RDS data
at 57 kHz — are filtered away rather than decoded.

Get `-deemph` wrong and nothing fails, it just sounds off: `us` where
`eu` was wanted is dull, the other way round is hissy.

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-freq` | `98.7M` | Station; a bare number under 200 is read as MHz |
| `-deemph` | `us` | `us` (75 µs) or `eu` (50 µs) |
| `-speaker` | off | Also play through this machine's audio device |
| `-volume` | `1.0` | Output gain multiplier |
| `-gain` | `-1` | Tuner gain in dB, or `-1` for automatic |
| `-ppm`, `-device`, `-rtltcp`, `-http` | | Tuner and server settings |

A frequency outside 87.5-108 MHz is rejected before the tuner is asked
for it, which catches `987` typed for `98.7`.

### Speakers, and speakers under WSL

`-speaker` pipes the audio to `play` from `sox` (`sudo apt install
sox`). Under WSL that reaches Windows' audio through WSLg's PulseAudio
socket, which the tool sets up itself. If `sox` is missing or the audio
device cannot be opened the failure is logged and reception continues —
the browser stream is the primary output, not a fallback.

### What to expect

A local station on a bad indoor antenna is still perfectly listenable:
FM is a strong-signal service and the capture effect means the loudest
station in the passband wins outright. If audio is noisy the fix is
almost always the antenna, or `-gain` fixed somewhere near 30 dB when
automatic gain hunts on a strong signal. Silence with a moving signal
meter means the wrong `-deemph` or a `-volume` of zero; silence with a
dead meter means the tuner is not seeing the station.

## Shortwave

The R820T tuner cannot go below about 24 MHz, so almost all of shortwave
(1.8-30 MHz) is out of reach as the hardware ships. There are two ways
round it, and one of them does not work on most dongles.

### Direct sampling

`-direct 2` bypasses the tuner and takes the ADC straight from the
antenna pin, reaching DC to 14.4 MHz. It is one flag, and it costs
nothing to try:

```sh
./bin/scanner -once -direct 2 -start 4M -stop 14M -threshold 8
```

But it only works if the board routes an antenna to that pin, and most
do not — including the Nooelec NESDR SMArt. The mode still *enables*, so
it is easy to believe it is working. The way to tell is to look for
signals that must be there rather than signals that might be:

* **WWV** transmits continuously on 5, 10, 15 and 20 MHz and is
  receivable across North America. If it is absent, HF is not working.
* The **49 m band** (5.8-6.3 MHz) carries many broadcasters after dark.
  Complete silence there means the same thing.
* A genuine broadcast is a stable carrier on the 5 kHz channel grid. A
  wide blob that moves tens of kHz between sweeps is not a station.

On an unmodified dongle you may still see something — stray coupling
reaches the ADC pins — but it will be one or two unstable artefacts, not
a band full of stations.

### An upconverter

The approach that works with an unmodified dongle. A converter such as a
Ham It Up or SpyVerter shifts everything up by 125 MHz, putting the whole
of HF comfortably inside the tuner's range.

```sh
./sdr scanner -upconvert 125M -start 5M -stop 15M
```

`-start` and `-stop` stay the frequencies you actually want; the shift is
added when tuning and removed when reporting, so peaks read as real
shortwave frequencies. The reachable range shown at startup accounts for
the converter.

### The antenna matters more than the receiver

A quarter wave at 10 MHz is 7.5 metres. A telescopic whip is
electrically invisible at that frequency, so no amount of receiver will
help. Ten to twenty metres of wire and a 9:1 unun costs little and makes
more difference than anything else on this page.

## Permissions

The `rtl-sdr` package ships a udev rule granting the `plugdev` group
access to the dongle. If the device was plugged in *before* the package
was installed the rule never fired, and `rtl_test` reports
`usb_open error -3`. Re-apply it with:

```sh
sudo udevadm control --reload-rules
sudo udevadm trigger --action=add --subsystem-match=usb
```

Confirm with `rtl_test -t`, which should print the tuner type instead of
an error. Under WSL, re-attaching the device with `usbipd` has the same
effect.

## What is and is not decoded on 1090 MHz

Received and CRC-checked: **DF11** all-call replies and **DF17/DF18**
extended squitters. Within those, aircraft identification (type codes
1-4), airborne and surface position (5-8, 9-18, 20-22), airborne
velocity (19) and aircraft status (28 subtype 1: emergency state and
the Mode A squawk).

Not handled:

* **DF0/4/5/16/20/21.** These overlay the aircraft address on the parity
  field, so the CRC cannot be verified without already knowing the
  sender. Checking them against the addresses already in the tracker
  would recover altitude and squawk from non-ADS-B transponders.
* **Gillham-coded altitude** (the Q=0 encoding), which only applies
  above 50,000 ft. The `very high` alert rule therefore fires on the
  aircraft that report altitude normally up there, not on every one.
* **Airspeed velocity subtypes 3 and 4**, rare outside military traffic.
* **Error correction.** dump1090 brute-forces single-bit errors against
  the CRC; here a frame either checks out or is dropped.

## Windows, and other machines

Nothing here uses cgo, so one command cross-compiles with no toolchain
beyond Go itself:

```sh
./sdr build windows     # dist/windows/sdr.exe — one file, every receiver
./sdr build darwin
./sdr build linux
```

That is a single binary. The tabbed app with no arguments, one receiver
when you name one, and the parts of this script that have nothing to do
with holding the dongle:

```
sdr                    every receiver, with tabs
sdr adsb               aircraft, 1090 MHz
sdr fm 98.7            listen to broadcast FM
sdr setpos LAT LON     save your antenna position
sdr alerts [n]         what the alerts have caught
sdr install            set it up on Windows
```

Ready-made binaries are on the [releases
page](https://github.com/thatSFguy/swDefinedRadio/releases), built by CI
from a tag.

### Installing on Windows

Copy `sdr.exe` over and run:

```
sdr.exe install
```

It puts itself under `%LOCALAPPDATA%\Programs\sdr`, adds that to your
PATH, makes a Start Menu shortcut, and downloads the `rtl-sdr` programs
every receiver drives as a child process — `rtl_tcp`, `rtl_sdr` and
`rtl_433`. None of that needs administrator rights, because none of it
belongs to the machine rather than to you. `sdr uninstall` puts it back,
PATH included.

Those programs are downloaded from their own projects rather than carried
in this repository: they are other people's work under their own licence,
and a copy kept here would ship stale.

The one thing the installer will not do is the driver, and it is the part
that actually stops people. Windows binds an RTL2832U stick to its
television driver, which will not let anything else open it;
[Zadig](https://zadig.akeo.ie/) rebinds it to WinUSB. That needs
administrator rights and a choice only a person should make, because the
same dialog can just as easily unbind something quite different. The
installer checks which driver is bound and says so.

Running natively on Windows is worth considering if the dongle is plugged
into a Windows machine and forwarded into WSL with `usbipd`, because it
removes the forwarding entirely. Release it from WSL first:

```
usbipd detach --busid <id>
```

`./sdr` itself is a shell script and does not run on Windows, but nearly
everything it does is in the binary. What is missing there is `start`,
`stop`, `status` and `logs` — running something in the background and
keeping track of it, which Windows does its own way.

## Notes on WSL

Both aircraft receivers stream about 4 MB/s continuously over USB/IP. This works,
but if the message rate looks low or `rtl_sdr` reports dropped samples,
the USB/IP link is the first thing to suspect rather than the receiver.

## Releases

Every push is built, vetted and tested by CI, including a cross-compile
of every platform — a build tag that only works on Linux is easy to add
by accident and hard to notice.

A release is cut from a tag:

```sh
git tag -a v0.1.0 -m "what changed"
git push origin v0.1.0
```

which builds one self-contained binary per platform, with the version
compiled in, and attaches them to the release along with `SHA256SUMS`.
`sdr version` says which build a binary is; without a tag it falls back
to the commit it came from, and says so if the working copy was dirty.

## Testing

```sh
go test ./...
go test ./internal/modes -bench=. -benchtime=3x -run=XXX

SDR_HARDWARE=1 go test ./internal/sdr/ ./internal/radio/ -run Hardware -v
```

The ordinary run needs no dongle. That includes the parts that hand the
radio between receivers, which are tested against a fake `rtl_tcp` — a
greeting, a stream of samples in bursts the way real ones arrive, and a
record of the commands it was sent. That is enough to establish that a
sample rate is really commanded, that revoking a receiver releases one
parked in a blocking read, that stale samples captured before a switch
are dropped, and that a hundred switches leak no goroutines or sockets.

The tests that do need a dongle say so and skip unless `SDR_HARDWARE=1`.
They exist because a few things cannot be established against a fake: on
this hardware a sample rate commanded mid-stream is honoured to within a
few hundredths of a percent, a handover between receivers costs about
205 ms, and a second client dialling a busy `rtl_tcp` is refused in two
seconds rather than waiting for ever.

The UAT tests do the same as the Mode S ones from the transmitting
end: a frame is Reed-Solomon coded, modulated as FSK with noise on it,
and pushed back through the demodulator, so the receiver's sensitivity
is a number the test suite asserts rather than a hope. The Reed-Solomon
decoder is checked against its own encoder over every code UAT uses,
damaged to exactly the limit each can repair and then past it.

The Mode S tests synthesise the IQ a receiver would see for a known
frame and push it through the whole chain — magnitude, preamble search,
bit slicing, CRC — so the demodulator is covered without a radio
attached. CPR decoding is checked against the worked example in ICAO
Doc 9871.

The FM tests do the same trick from the transmitting end: a 1 kHz tone
is modulated onto a synthetic carrier, pushed through the whole
receiver, and the recovered audio is measured to confirm it comes back
out at 1 kHz.

On a 2018 i7-8550U the full pipeline costs about 27 ms of CPU per
second of radio, roughly 3% of one core.
