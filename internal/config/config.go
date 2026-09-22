// Package config holds the settings shared by the receivers, chiefly the
// antenna's own position.
//
// Values loaded from a config file become the *defaults* for command-line
// flags, so anything given explicitly on the command line still wins.
package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// DefaultHTTPAddr is the address every receiver's web UI listens on. They
// share one port because only one can hold the dongle at a time, so there
// is only ever one UI to visit.
const DefaultHTTPAddr = ":9999"

// URL renders a listen address as something clickable in a terminal. A
// bare ":9999" or a wildcard bind is shown as localhost, since that is
// where the person reading the log is.
func URL(addr string) string {
	host, port, ok := strings.Cut(addr, ":")
	if !ok {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "[::]" {
		host = "localhost"
	}
	return "http://" + host + ":" + port
}

// Config is the on-disk settings file.
type Config struct {
	// Lat and Lon are the receiver's own position. Setting them enables
	// single-frame position fixes and a range for every contact.
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// HasPosition reports whether a usable position is set. Zero is treated as
// unset: the point it names is in the Atlantic, so nobody's antenna is
// legitimately there.
func (c Config) HasPosition() bool {
	if c.Lat == 0 && c.Lon == 0 {
		return false
	}
	return math.Abs(c.Lat) <= 90 && math.Abs(c.Lon) <= 180
}

// Search lists the config paths that are consulted, most specific first.
// SDR_CONFIG overrides everything, then a config.json beside the binary's
// working directory, then the usual per-user location.
func Search() []string {
	var out []string
	if p := os.Getenv("SDR_CONFIG"); p != "" {
		out = append(out, p)
	}
	out = append(out, "config.json")
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".config", "sdr", "config.json"))
	}
	return out
}

// DefaultPath is where Save writes when no file exists yet.
func DefaultPath() string {
	if p := os.Getenv("SDR_CONFIG"); p != "" {
		return p
	}
	return "config.json"
}

// Load returns the first config file found and the path it came from. A
// missing file is not an error — it just means no defaults.
func Load() (Config, string, error) {
	for _, p := range Search() {
		b, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return Config{}, p, fmt.Errorf("read %s: %w", p, err)
		}
		var c Config
		if err := json.Unmarshal(b, &c); err != nil {
			return Config{}, p, fmt.Errorf("parse %s: %w", p, err)
		}
		return c, p, nil
	}
	return Config{}, "", nil
}

// Save writes the config to path, creating the directory if needed.
func Save(c Config, path string) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
