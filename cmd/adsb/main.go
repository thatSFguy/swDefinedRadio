// Command adsb receives ADS-B on 1090 MHz with an RTL-SDR and serves a
// live map and JSON API of the aircraft it hears.
//
// It is one subcommand of the combined sdr binary, built on its own for
// anyone who wants just this receiver.
package main

import (
	"os"

	"github.com/thatSFguy/swDefinedRadio/internal/cli"
)

func main() { cli.Adsb(os.Args[1:]) }
