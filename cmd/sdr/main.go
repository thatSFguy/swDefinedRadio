// Command sdr is every receiver in this repo in one binary.
//
// It exists for the same reason the shell script does — one thing to run,
// which knows about all of them — except that a binary works on Windows,
// where the script does not.
package main

import (
	"os"

	"github.com/thatSFguy/swDefinedRadio/internal/cli"
)

func main() { os.Exit(cli.Main(os.Args[1:])) }
