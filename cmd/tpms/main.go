// Command tpms logs tire-pressure sensor transmissions and serves a live
// view of them.
//
// It is one subcommand of the combined sdr binary, built on its own for
// anyone who wants just this receiver.
package main

import (
	"os"

	"github.com/thatSFguy/swDefinedRadio/internal/cli"
)

func main() { cli.Tpms(os.Args[1:]) }
