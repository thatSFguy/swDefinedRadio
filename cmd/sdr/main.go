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

func main() {
	code := cli.Main(os.Args[1:])
	// Double-clicked from Explorer, the console belongs to this process
	// and closes with it, so anything just printed is never read.
	cli.PauseAtExit()
	os.Exit(code)
}
