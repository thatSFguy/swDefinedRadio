// Package cli holds each command's flags and wiring.
//
// Every receiver is reachable two ways: as a binary of its own, and as a
// subcommand of the combined sdr binary. That second one matters most on
// Windows, where the shell script that ties this repo together does not
// run and a single executable is the whole of what you need to copy.
//
// Each command parses into a flag set of its own rather than the global
// one, which is what lets them live in one binary without treading on
// each other.
package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/thatSFguy/swDefinedRadio/internal/config"
)

// Commands are the receivers, in the order they are worth offering.
var Commands = []struct {
	Name, Blurb string
	Run         func(args []string)
}{
	{"hub", "every receiver, with tabs", Hub},
	{"adsb", "aircraft, 1090 MHz", Adsb},
	{"uat", "aircraft, 978 MHz (US)", Uat},
	{"tpms", "tire sensors, 315/433 MHz", Tpms},
	{"scanner", "sweep the spectrum", Scanner},
	{"fm", "listen to broadcast FM", Fm},
}

// Route works out what an argument list means, without acting on it.
//
// Three shapes, and the second is the one worth being deliberate about: a
// leading flag means the default command with that flag on it, because
// "sdr -http :9100" plainly means run the usual thing on another port,
// not run a command called -http.
func Route(args []string) (name string, rest []string, help bool) {
	if len(args) == 0 {
		return "hub", nil, false
	}
	switch args[0] {
	case "-h", "--help", "help":
		return "", nil, true
	}
	if strings.HasPrefix(args[0], "-") {
		return "hub", args, false
	}
	return args[0], args[1:], false
}

// Main dispatches the combined binary. With no arguments it runs the hub,
// because the tabbed app is the whole radio and choosing a single
// receiver is the narrower thing to want.
func Main(args []string) int {
	name, rest, help := Route(args)
	if help {
		Usage(os.Stdout)
		return 0
	}
	for _, c := range Commands {
		if c.Name == name {
			c.Run(rest)
			return 0
		}
	}
	switch name {
	case "setpos":
		return setpos(rest)
	case "alerts":
		return alerts(rest)
	case "version", "-v", "--version":
		fmt.Println(version())
		return 0
	case "install":
		return Install(rest)
	case "uninstall":
		return Uninstall(rest)
	}
	fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", name)
	Usage(os.Stderr)
	return 2
}

func Usage(w io.Writer) {
	fmt.Fprint(w, "sdr — software-defined radio receivers.\n\n")
	fmt.Fprintln(w, "  sdr                    every receiver, with tabs")
	for _, c := range Commands {
		fmt.Fprintf(w, "  sdr %-18s %s\n", c.Name, c.Blurb)
	}
	fmt.Fprintln(w, "  sdr setpos LAT LON     save your antenna position as the default")
	fmt.Fprintln(w, "  sdr alerts [n]         show what the aircraft alerts have caught")
	fmt.Fprintln(w, "  sdr install            set this up on Windows (no admin needed)")
	fmt.Fprintln(w, "  sdr version            which build this is")
	fmt.Fprintln(w, "\nFlags go after the command: sdr adsb -lat 43.2 -lon -85.6")
	fmt.Fprintln(w, "Any command takes -h for its own flags.")
}

// setpos records the antenna position, which becomes the default for the
// aircraft receivers' -lat and -lon.
func setpos(args []string) int {
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: sdr setpos <lat> <lon>   e.g. sdr setpos 51.4779 -0.0015")
		return 2
	}
	lat, err := strconv.ParseFloat(args[0], 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "not a latitude: %s\n", args[0])
		return 2
	}
	lon, err := strconv.ParseFloat(args[1], 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "not a longitude: %s\n", args[1])
		return 2
	}
	c := config.Config{Lat: lat, Lon: lon}
	if !c.HasPosition() {
		fmt.Fprintf(os.Stderr, "%.4f, %.4f is not a position on Earth\n", lat, lon)
		return 2
	}

	path := config.DefaultPath()
	if p := os.Getenv("SDR_CONFIG"); p != "" {
		path = p
	} else if _, err := os.Stat("config.json"); err == nil {
		path = "config.json"
	}
	if err := config.Save(c, path); err != nil {
		fmt.Fprintf(os.Stderr, "cannot save %s: %v\n", path, err)
		return 1
	}
	fmt.Printf("saved %.4f, %.4f to %s\n", lat, lon, path)
	fmt.Println("  the aircraft receivers now use this unless you pass -lat/-lon")
	return 0
}

// alerts prints the alert log newest first. It is a JSON object per line,
// so this reads it rather than needing jq to be installed.
func alerts(args []string) int {
	n := 20
	if len(args) > 0 {
		if v, err := strconv.Atoi(args[0]); err == nil && v > 0 {
			n = v
		}
	}
	const path = "data/alerts.jsonl"
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nothing logged yet (%s).\n", path)
		fmt.Fprintln(os.Stderr, "Alerts appear once a receiver has heard something on its watchlist.")
		return 1
	}

	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	slices.Reverse(lines)
	for i, line := range lines {
		if i >= n {
			break
		}
		var a struct {
			At       string `json:"at"`
			Rule     string `json:"rule"`
			Note     string `json:"note"`
			Hex      string `json:"hex"`
			Callsign string `json:"callsign"`
		}
		if json.Unmarshal([]byte(line), &a) != nil {
			fmt.Println(line)
			continue
		}
		who := a.Callsign
		if who == "" {
			who = a.Hex
		}
		at := a.At
		if len(at) >= 19 {
			at = at[11:19]
		}
		fmt.Printf("%s  %-22s %-9s %s\n", at, a.Rule, who, a.Note)
	}
	return 0
}
