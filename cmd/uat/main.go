// Command uat receives UAT on 978 MHz and serves the same live map and
// JSON API the 1090 MHz receiver does.
//
// It is one subcommand of the combined sdr binary, built on its own for
// anyone who wants just this receiver.
package main

import (
	"os"

	"github.com/thatSFguy/swDefinedRadio/internal/cli"
)

func main() { cli.Uat(os.Args[1:]) }
