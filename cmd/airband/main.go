// Command airband receives aircraft voice on the AM channels between 118
// and 137 MHz.
//
// It is one subcommand of the combined sdr binary, built on its own for
// anyone who wants just this receiver.
package main

import (
	"os"

	"github.com/thatSFguy/swDefinedRadio/internal/cli"
)

func main() { cli.Airband(os.Args[1:]) }
