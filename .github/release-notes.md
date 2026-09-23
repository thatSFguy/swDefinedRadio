One file per platform. Nothing to unpack — every receiver, its page, the
offline basemap and Leaflet are compiled in.

Run it with no arguments for the tabbed app, which serves every receiver
and hands the radio to whichever tab you choose. Or name a single one:

```
sdr                    every receiver, with tabs
sdr adsb               aircraft, 1090 MHz
sdr fm 98.7            listen to broadcast FM
sdr alerts             what the alerts have caught
```

**On Windows**, copy `sdr-*-windows-amd64.exe` somewhere and run
`sdr install`. That puts it under `%LOCALAPPDATA%`, adds it to your PATH,
and downloads the `rtl-sdr` programs the receivers drive as child
processes. It needs no administrator rights, and `sdr uninstall` puts
everything back.

The dongle still needs the WinUSB driver, which
[Zadig](https://zadig.akeo.ie/) installs. That is the one step that
cannot be automated — it needs administrator rights and a choice only a
person should make. `sdr install` checks which driver is bound and tells
you.

**On Linux and macOS**, install `rtl-sdr` (and `rtl-433` for the tire
sensors) from your package manager, then put the binary on your PATH.

Verify a download against `SHA256SUMS`.
