package cli

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version is set at build time from the tag being released:
//
//	go build -ldflags "-X github.com/thatSFguy/swDefinedRadio/internal/cli.Version=v1.2.3"
//
// A build from a working copy leaves it alone and falls back to what the
// toolchain recorded, so a binary can always say where it came from.
var Version = ""

// version reports what this binary is, which matters most when someone
// has an executable and no idea which build it is.
func version() string {
	v := Version
	revision, modified := "", false
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if len(s.Value) >= 12 {
					revision = s.Value[:12]
				}
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	if v == "" {
		if revision != "" {
			v = revision
		} else {
			v = "dev"
		}
		revision = ""
	}

	out := "sdr " + v
	if revision != "" {
		out += " (" + revision + ")"
	}
	if modified {
		out += " with local changes"
	}
	return fmt.Sprintf("%s, %s/%s, %s", out, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
