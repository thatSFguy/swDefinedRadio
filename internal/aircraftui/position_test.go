package aircraftui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/config"
	"github.com/thatSFguy/swDefinedRadio/internal/track"
)

// serveFor starts the receiver's HTTP side on a free port and returns
// its base URL, the tracker behind it and the config path it writes.
func serveFor(t *testing.T, setPos bool) (string, *track.Tracker, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tr := track.New(time.Minute)
	cfg := filepath.Join(t.TempDir(), "config.json")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go Serve(ctx, ln, tr, nil, Options{ConfigPath: cfg, AllowSetPosition: setPos})
	return "http://" + ln.Addr().String(), tr, cfg
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestPositionFromBrowser is the whole flow: the page offers what the
// browser knows, the receiver starts using it, and it survives a
// restart because it was written to the config file.
func TestPositionFromBrowser(t *testing.T) {
	base, tr, cfgPath := serveFor(t, true)

	if _, _, ok := tr.Reference(); ok {
		t.Fatal("the tracker started with a position it was never given")
	}

	code, body := post(t, base+"/api/position", `{"lat":51.477928,"lon":-0.001545,"accuracy_m":38}`)
	if code != 200 {
		t.Fatalf("post = %d: %s", code, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if got["saved"] != true {
		t.Errorf("not saved: %v", got)
	}
	if w, ok := got["warning"]; ok {
		t.Errorf("a 38 m fix was warned about: %v", w)
	}

	lat, lon, ok := tr.Reference()
	if !ok || lat != 51.477928 || lon != -0.001545 {
		t.Errorf("tracker reference = %v, %v, %v", lat, lon, ok)
	}

	// On disk, in the form the next start reads.
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read back %s: %v", cfgPath, err)
	}
	var saved config.Config
	if err := json.Unmarshal(b, &saved); err != nil {
		t.Fatalf("%s is not valid config: %v: %s", cfgPath, err, b)
	}
	if !saved.HasPosition() || saved.Lat != 51.477928 || saved.Lon != -0.001545 {
		t.Errorf("config.json holds %+v", saved)
	}

	// And the page can read it back.
	resp, err := http.Get(base + "/api/position")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p struct {
		Lat, Lon float64
		Set      bool
		Settable bool
	}
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if !p.Set || !p.Settable || p.Lat != 51.477928 {
		t.Errorf("GET /api/position = %+v", p)
	}
}

// TestPositionRejectsNonsense: a bad position is worse than none, since
// local decoding trusts it and every range is measured from it.
func TestPositionRejectsNonsense(t *testing.T) {
	base, tr, _ := serveFor(t, true)

	for _, c := range []struct{ what, body string }{
		{"off the globe", `{"lat":91,"lon":0}`},
		{"off the globe", `{"lat":0,"lon":181}`},
		{"null island", `{"lat":0,"lon":0}`},
		{"a fix good to 400 km", `{"lat":43.1,"lon":-85.6,"accuracy_m":400000}`},
		{"not json", `nope`},
	} {
		if code, body := post(t, base+"/api/position", c.body); code != http.StatusBadRequest {
			t.Errorf("%s: code = %d, want 400 (%s)", c.what, code, body)
		}
	}
	if _, _, ok := tr.Reference(); ok {
		t.Error("a rejected position was applied anyway")
	}
}

// TestVagueFixWarns: a desktop with no GPS is located by wifi or IP,
// which can be a city out. It is still usable, but the page has to say
// so rather than quietly reporting wrong ranges.
func TestVagueFixWarns(t *testing.T) {
	base, tr, _ := serveFor(t, true)

	code, body := post(t, base+"/api/position", `{"lat":43.1,"lon":-85.6,"accuracy_m":42000}`)
	if code != 200 {
		t.Fatalf("code = %d: %s", code, body)
	}
	var got map[string]any
	_ = json.Unmarshal([]byte(body), &got)
	w, _ := got["warning"].(string)
	if w == "" {
		t.Error("a 42 km fix was accepted with no warning")
	}
	if _, _, ok := tr.Reference(); !ok {
		t.Error("the position was warned about but not applied")
	}
}

func TestPositionAPICanBeDisabled(t *testing.T) {
	base, tr, cfgPath := serveFor(t, false)

	if code, _ := post(t, base+"/api/position", `{"lat":43.1,"lon":-85.6}`); code != http.StatusForbidden {
		t.Errorf("code = %d, want 403", code)
	}
	if _, _, ok := tr.Reference(); ok {
		t.Error("the position was set with the API disabled")
	}
	if _, err := os.Stat(cfgPath); err == nil {
		t.Error("the config file was written with the API disabled")
	}

	// The page still needs to read the position, and to know not to
	// offer the button.
	resp, err := http.Get(base + "/api/position")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p struct{ Settable bool }
	_ = json.NewDecoder(resp.Body).Decode(&p)
	if p.Settable {
		t.Error("the page is told it may set the position when it may not")
	}
}
