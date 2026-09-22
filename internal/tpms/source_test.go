package tpms

import (
	"slices"
	"strings"
	"testing"
)

// The rtl_433 command line is the most breakable string in the package:
// nothing downstream checks it, and a wrong flag shows up as a receiver
// that runs and simply never hears anything. These cases pin the shape of
// it so a change to how the radio is reached has to be deliberate.
func TestSourceConfigArgs(t *testing.T) {
	tests := []struct {
		name string
		cfg  SourceConfig
		want []string
	}{
		{
			name: "one frequency, no hopping",
			cfg:  SourceConfig{Freqs: []string{"433.92M"}, HopSeconds: 30},
			want: []string{"-d", "0", "-f", "433.92M", "-M", "time:iso", "-F", "json"},
		},
		{
			name: "two frequencies hop between them",
			cfg:  SourceConfig{Freqs: []string{"315M", "433.92M"}, HopSeconds: 30},
			want: []string{"-d", "0", "-f", "315M", "-f", "433.92M", "-H", "30",
				"-M", "time:iso", "-F", "json"},
		},
		{
			name: "gain and ppm appear only when set",
			cfg: SourceConfig{
				Freqs: []string{"315M"}, Gain: "40.2", PPM: -3, Device: "1",
			},
			want: []string{"-d", "1", "-f", "315M", "-g", "40.2", "-p", "-3",
				"-M", "time:iso", "-F", "json"},
		},
		{
			name: "extra arguments come before the output format",
			cfg: SourceConfig{
				Freqs: []string{"315M"}, ExtraArgs: []string{"-R", "60"},
			},
			want: []string{"-d", "0", "-f", "315M", "-R", "60",
				"-M", "time:iso", "-F", "json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.cfg.Args()
			if !slices.Equal(got, tt.want) {
				t.Errorf("Args() =\n  %s\nwant\n  %s",
					strings.Join(got, " "), strings.Join(tt.want, " "))
			}
		})
	}
}

// Hopping is only meaningful with somewhere to hop to, and passing -H with
// a single frequency makes rtl_433 wait pointlessly.
// Reaching the radio through rtl_tcp is what lets this receiver share a
// dongle rather than taking it, so the device argument has to carry the
// address through untouched.
func TestSourceConfigCanNameAnRTLTCPServer(t *testing.T) {
	args := SourceConfig{
		Freqs:  []string{"315M", "433.92M"},
		Device: "rtl_tcp:127.0.0.1:1234",
	}.Args()
	want := []string{"-d", "rtl_tcp:127.0.0.1:1234", "-f", "315M", "-f", "433.92M",
		"-M", "time:iso", "-F", "json"}
	if !slices.Equal(args, want) {
		t.Errorf("Args() =\n  %s\nwant\n  %s",
			strings.Join(args, " "), strings.Join(want, " "))
	}
}

// An unset device still means the first dongle, as it always did.
func TestSourceConfigDefaultsToDeviceZero(t *testing.T) {
	args := SourceConfig{Freqs: []string{"315M"}}.Args()
	if len(args) < 2 || args[0] != "-d" || args[1] != "0" {
		t.Errorf("Args() starts %v, want -d 0", args[:min(2, len(args))])
	}
}

func TestSourceConfigNoHopForOneFrequency(t *testing.T) {
	args := SourceConfig{Freqs: []string{"433.92M"}, HopSeconds: 30}.Args()
	if slices.Contains(args, "-H") {
		t.Errorf("-H passed for a single frequency: %s", strings.Join(args, " "))
	}
}

// JSON on stdout is what the reading parser expects; losing it turns every
// reading into an unparseable line.
func TestSourceConfigAlwaysRequestsJSON(t *testing.T) {
	args := SourceConfig{Freqs: []string{"315M"}}.Args()
	if len(args) < 4 {
		t.Fatalf("too few arguments: %v", args)
	}
	tail := strings.Join(args[len(args)-4:], " ")
	if tail != "-M time:iso -F json" {
		t.Errorf("arguments end with %q, want %q", tail, "-M time:iso -F json")
	}
}
