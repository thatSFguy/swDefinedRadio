// Command scanner sweeps the tuner across a range of frequencies and
// serves a live spectrum, a waterfall and the signals it finds.
//
// It is one subcommand of the combined sdr binary, built on its own for
// anyone who wants just this receiver.
package main

import (
	"os"

	"github.com/thatSFguy/swDefinedRadio/internal/cli"
)

func main() { cli.Scanner(os.Args[1:]) }
