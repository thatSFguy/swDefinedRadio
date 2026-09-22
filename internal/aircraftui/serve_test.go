package aircraftui

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/alert"
	"github.com/thatSFguy/swDefinedRadio/internal/modes"
	"github.com/thatSFguy/swDefinedRadio/internal/track"
)

// TestEmbeddedUI checks the web assets are compiled into the binary and
// served from the root, which an embed directive gets wrong silently.
func TestEmbeddedUI(t *testing.T) {
	sub, err := fsSub()
	if err != nil {
		t.Fatalf("fsSub: %v", err)
	}
	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("index.html not embedded: %v", err)
	}
	for _, want := range []string{"/api/aircraft", "leaflet", "<title>"} {
		if !strings.Contains(strings.ToLower(string(b)), want) {
			t.Errorf("index.html is missing %q", want)
		}
	}
}

// tileURL is a stand-in street map for the tests: the selector only
// offers the online option when one is configured.
const tileURL = "https://example.invalid/{z}/{x}/{y}.png"

// TestAlertsAPI drives the endpoint the UI polls, through a real
// watcher, so the shape the page depends on is checked end to end.
func TestAlertsAPI(t *testing.T) {
	w := alert.New(alert.Builtin(), time.Hour, nil)
	tr := track.New(time.Minute)

	// A US military address and an emergency squawk: one of each kind
	// of rule, both raised through the tracker rather than by hand.
	for _, a := range []modes.ADSB{
		{ICAO: 0xAE1234, Callsign: "RCH271"},
		{ICAO: 0xA00001, HasStatus: true, Squawk: "7700", Emergency: modes.GeneralEmergency},
	} {
		w.Check(tr.Update(a, -20, time.Now()), time.Now())
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, ln, tr, w, Options{TileURL: tileURL, ConfigPath: filepath.Join(t.TempDir(), "config.json"), AllowSetPosition: true})

	resp, err := http.Get("http://" + ln.Addr().String() + "/api/alerts")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	var got struct {
		Watching bool          `json:"watching"`
		Rules    []alert.Rule  `json:"rules"`
		Alerts   []alert.Alert `json:"alerts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Watching || len(got.Rules) == 0 {
		t.Fatalf("watching=%v with %d rules", got.Watching, len(got.Rules))
	}
	// Newest first: the emergency was raised last.
	if len(got.Alerts) < 3 {
		t.Fatalf("got %d alerts, want the military address, its callsign and both emergency rules", len(got.Alerts))
	}
	if got.Alerts[0].Hex != "a00001" || !got.Alerts[0].Urgent {
		t.Errorf("newest alert = %+v, want the urgent emergency", got.Alerts[0])
	}
	if got.Alerts[0].Squawk != "7700" || got.Alerts[0].Emergency != "general emergency" {
		t.Errorf("the emergency detail did not survive the round trip: %+v", got.Alerts[0])
	}

	// With no watcher the endpoint must still answer, so the UI can
	// tell "nothing is being watched" from "the request failed".
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	go Serve(ctx, ln2, tr, nil, Options{ConfigPath: filepath.Join(t.TempDir(), "config.json"), AllowSetPosition: true})
	resp2, err := http.Get("http://" + ln2.Addr().String() + "/api/alerts")
	if err != nil {
		t.Fatalf("get without a watcher: %v", err)
	}
	defer resp2.Body.Close()
	got.Watching = true
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Watching {
		t.Error("watching is true with no watcher")
	}
}

// TestMapWorksOffline guards the ways the map quietly stops working
// without a network — a library or a tile server reached over the
// internet — all of which are invisible until you are somewhere with no
// signal. Everything the map needs has to be inside the binary.
func TestMapWorksOffline(t *testing.T) {
	sub, err := fsSub()
	if err != nil {
		t.Fatalf("fsSub: %v", err)
	}
	for _, p := range []string{"vendor/leaflet/leaflet.js", "vendor/leaflet/leaflet.css", "basemap.json"} {
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			t.Errorf("%s is not embedded: %v", p, err)
		} else if len(b) < 1000 {
			t.Errorf("%s is only %d bytes — that is not the real file", p, len(b))
		}
	}

	b, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		t.Fatalf("index.html: %v", err)
	}
	for _, bad := range []string{"unpkg.com", "cdn.jsdelivr", "openstreetmap.org"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("index.html reaches for %s, so the map needs a network", bad)
		}
	}
	if !strings.Contains(string(b), "basemap.json") {
		t.Error("index.html does not draw the built-in basemap")
	}
	// The online street map is an option, and must not be the default:
	// the stored map is the one that is on the map at load, and the
	// tile URL is only ever the one the receiver hands over.
	if !strings.Contains(string(b), "const stored = L.layerGroup().addTo(map)") {
		t.Error("the stored map is not the base layer the page starts with")
	}
	if i := strings.Index(string(b), "L.tileLayer("); i >= 0 {
		if !strings.Contains(string(b)[i:i+40], "opts.tile_url") {
			t.Error("the page builds a tile layer from something other than the receiver's setting")
		}
	}
}

// TestBasemap checks the map data itself: the shape the page expects,
// and enough of it to be a map rather than an empty sea.
func TestBasemap(t *testing.T) {
	sub, err := fsSub()
	if err != nil {
		t.Fatalf("fsSub: %v", err)
	}
	raw, err := fs.ReadFile(sub, "basemap.json")
	if err != nil {
		t.Fatalf("basemap.json: %v", err)
	}

	var bm struct {
		Land    [][][2]float64 `json:"land"`
		Borders [][][2]float64 `json:"borders"`
		States  [][][2]float64 `json:"states"`
		Lakes   [][][2]float64 `json:"lakes"`
		Places  []struct {
			Name string     `json:"n"`
			At   [2]float64 `json:"p"`
			Rank int        `json:"r"`
		} `json:"places"`
		Airports []struct {
			At       [2]float64 `json:"p"`
			Code     string     `json:"c"`
			Name     string     `json:"n"`
			Rank     int        `json:"r"`
			Military int        `json:"m"`
		} `json:"airports"`
	}
	if err := json.Unmarshal(raw, &bm); err != nil {
		t.Fatalf("basemap.json does not parse: %v", err)
	}
	for _, c := range []struct {
		name string
		n    int
		min  int
	}{
		{"land", len(bm.Land), 50},
		{"borders", len(bm.Borders), 100},
		{"states", len(bm.States), 100},
		{"lakes", len(bm.Lakes), 5},
		{"places", len(bm.Places), 200},
		{"airports", len(bm.Airports), 500},
	} {
		if c.n < c.min {
			t.Errorf("%s has %d features, want at least %d", c.name, c.n, c.min)
		}
	}

	// Coordinates are stored the way Leaflet takes them, latitude
	// first. Swapped pairs would still parse and would put the whole
	// map in the wrong place, so check the range.
	for _, ring := range bm.Land {
		for _, p := range ring {
			if p[0] < -90 || p[0] > 90 || p[1] < -180 || p[1] > 180 {
				t.Fatalf("point %v is not lat,lon — the pairs may be swapped", p)
			}
		}
	}

	// Airports are the point of an aircraft map, so check a few known
	// ones are where they should be, with the code the label shows.
	want := map[string][2]float64{
		"ORD": {41.98, -87.91}, // Chicago O'Hare
		"DTW": {42.23, -83.35}, // Detroit Metro
		"GRR": {42.88, -85.53}, // the receiver's local field
	}
	for _, a := range bm.Airports {
		if a.Code == "" || a.Name == "" {
			t.Fatalf("airport with no code or name: %+v", a)
		}
		if at, ok := want[a.Code]; ok {
			if math.Abs(a.At[0]-at[0]) > 0.02 || math.Abs(a.At[1]-at[1]) > 0.02 {
				t.Errorf("%s is at %v, want about %v", a.Code, a.At, at)
			}
			delete(want, a.Code)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing airports: %v", want)
	}

	// Somewhere identifiable, to catch data that parses but is not a
	// map of this planet: land within a degree of Grand Rapids.
	near := 0
	for _, ring := range bm.Land {
		for _, p := range ring {
			if p[0] > 24 && p[0] < 50 && p[1] > -125 && p[1] < -66 {
				near++
			}
		}
	}
	if near < 10 {
		t.Errorf("only %d land points in the continental US — is this the right data?", near)
	}
}

// TestMapAssetsAreServed checks everything the page pulls in comes
// from this receiver. A wrong path here is invisible online — the
// browser would fall back to nothing and the map would simply be blank
// in the air.
func TestMapAssetsAreServed(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, ln, track.New(time.Minute), nil, Options{TileURL: tileURL, ConfigPath: filepath.Join(t.TempDir(), "config.json"), AllowSetPosition: true})

	base := "http://" + ln.Addr().String()
	// The map selector reads this to decide whether to offer the
	// online option at all.
	resp, err := http.Get("http://" + ln.Addr().String() + "/api/map")
	if err != nil {
		t.Fatalf("get /api/map: %v", err)
	}
	var opts map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&opts); err != nil {
		t.Fatalf("decode /api/map: %v", err)
	}
	resp.Body.Close()
	if opts["tile_url"] != tileURL {
		t.Errorf("tile_url = %q, want %q", opts["tile_url"], tileURL)
	}

	for _, path := range []string{"/", "/vendor/leaflet/leaflet.js", "/vendor/leaflet/leaflet.css", "/basemap.json"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("get %s: %v", path, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(b) < 500 {
			t.Errorf("%s came back %d, %d bytes", path, resp.StatusCode, len(b))
		}
	}
}

// TestAPIShape drives the handler through a real tracker so the JSON the
// UI depends on is verified end to end.
func TestAPIShape(t *testing.T) {
	tr := track.New(time.Minute)
	tr.SetReference(52.0, 4.0)
	tr.Update(modes.ADSB{ICAO: 0x40621D, Callsign: "TEST123"}, -20, time.Now())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		craft, stats := tr.Snapshot()
		_ = json.NewEncoder(w).Encode(struct {
			Now      time.Time        `json:"now"`
			Stats    track.Stats      `json:"stats"`
			Aircraft []track.Aircraft `json:"aircraft"`
		}{time.Now(), stats, craft})
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var got struct {
		Stats    track.Stats      `json:"stats"`
		Aircraft []track.Aircraft `json:"aircraft"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, body)
	}
	if got.Stats.Tracked != 1 || len(got.Aircraft) != 1 {
		t.Fatalf("tracked = %d, aircraft = %d, want 1 and 1", got.Stats.Tracked, len(got.Aircraft))
	}
	if a := got.Aircraft[0]; a.Hex != "40621d" || a.Callsign != "TEST123" {
		t.Errorf("aircraft = %+v, want hex 40621d callsign TEST123", a)
	}
}
