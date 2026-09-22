package cli

import (
	"slices"
	"testing"
)

// What an argument list means, before anything acts on it.
func TestRoute(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
		rest []string
		help bool
	}{
		{"no arguments run the tabbed app", nil, "hub", nil, false},
		{"a named receiver", []string{"adsb"}, "adsb", nil, false},
		{"a receiver with flags", []string{"fm", "-freq", "98.7"}, "fm", []string{"-freq", "98.7"}, false},
		{"help", []string{"--help"}, "", nil, true},
		{"short help", []string{"-h"}, "", nil, true},

		// A leading flag is the default command with a flag on it. Read
		// any other way, "sdr -http :9100" is an unknown command — which
		// is what it used to be, and it simply printed usage and exited.
		{"a leading flag means the default command",
			[]string{"-http", ":9100"}, "hub", []string{"-http", ":9100"}, false},
		{"a leading flag among several",
			[]string{"-device", "1", "-quiet"}, "hub", []string{"-device", "1", "-quiet"}, false},

		{"setpos keeps its arguments",
			[]string{"setpos", "43.1", "-85.6"}, "setpos", []string{"43.1", "-85.6"}, false},
		{"an unknown command is passed through to be reported",
			[]string{"wibble"}, "wibble", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			name, rest, help := Route(tt.args)
			if name != tt.want || help != tt.help {
				t.Errorf("Route(%q) = %q, help=%v; want %q, help=%v",
					tt.args, name, help, tt.want, tt.help)
			}
			if len(rest) != len(tt.rest) || !slices.Equal(rest, tt.rest) {
				t.Errorf("Route(%q) rest = %q, want %q", tt.args, rest, tt.rest)
			}
		})
	}
}

// Every command offered in the usage text has to actually run, or the
// listing is a promise the binary does not keep.
func TestEveryListedCommandRoutes(t *testing.T) {
	for _, c := range Commands {
		if c.Run == nil {
			t.Errorf("%s is listed but has nothing to run", c.Name)
		}
		if name, _, _ := Route([]string{c.Name}); name != c.Name {
			t.Errorf("Route(%q) = %q", c.Name, name)
		}
	}
}
