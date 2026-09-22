// Command fm receives broadcast FM and serves it as live audio a browser
// can play.
//
// It is one subcommand of the combined sdr binary, built on its own for
// anyone who wants just this receiver.
package main

import (
	"os"

	"github.com/thatSFguy/swDefinedRadio/internal/cli"
)

func main() { cli.Fm(os.Args[1:]) }
