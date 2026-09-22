package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasPosition(t *testing.T) {
	for _, c := range []struct {
		name string
		cfg  Config
		want bool
	}{
		{"unset", Config{}, false},
		{"valid", Config{Lat: 43.06, Lon: -85.67}, true},
		{"southern", Config{Lat: -33.86, Lon: 151.2}, true},
		{"latitude out of range", Config{Lat: 91, Lon: 0}, false},
		{"longitude out of range", Config{Lat: 0, Lon: 181}, false},
		{"longitude only", Config{Lon: -85.67}, true},
	} {
		if got := c.cfg.HasPosition(); got != c.want {
			t.Errorf("%s: HasPosition() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "config.json")
	want := Config{Lat: 51.4779, Lon: -0.0015}
	if err := Save(want, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	t.Setenv("SDR_CONFIG", path)
	got, from, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if from != path {
		t.Errorf("loaded from %q, want %q", from, path)
	}
	if got != want {
		t.Errorf("config = %+v, want %+v", got, want)
	}
}

// TestMissingFileIsNotAnError keeps a fresh checkout from failing to start.
func TestMissingFileIsNotAnError(t *testing.T) {
	t.Setenv("SDR_CONFIG", filepath.Join(t.TempDir(), "absent.json"))
	t.Chdir(t.TempDir()) // so a stray ./config.json cannot be picked up

	c, from, err := Load()
	if err != nil {
		t.Fatalf("Load with no file: %v", err)
	}
	if from != "" || c.HasPosition() {
		t.Errorf("expected empty config, got %+v from %q", c, from)
	}
}

func TestMalformedFileReportsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SDR_CONFIG", path)
	if _, _, err := Load(); err == nil {
		t.Error("malformed config loaded without error")
	}
}

// TestSDRConfigWins checks precedence: the environment beats ./config.json.
func TestSDRConfigWins(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if err := Save(Config{Lat: 1, Lon: 1}, filepath.Join(dir, "config.json")); err != nil {
		t.Fatal(err)
	}
	env := filepath.Join(dir, "override.json")
	if err := Save(Config{Lat: 2, Lon: 2}, env); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SDR_CONFIG", env)

	got, _, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Lat != 2 {
		t.Errorf("lat = %v, want 2 (SDR_CONFIG should win)", got.Lat)
	}
}

func TestURL(t *testing.T) {
	for _, c := range []struct{ addr, want string }{
		{":9999", "http://localhost:9999"},
		{"0.0.0.0:9999", "http://localhost:9999"},
		{"127.0.0.1:8080", "http://127.0.0.1:8080"},
		{"radiobox:9999", "http://radiobox:9999"},
	} {
		if got := URL(c.addr); got != c.want {
			t.Errorf("URL(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}
