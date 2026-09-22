package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/radio"
)

// fakeApp is a receiver that records what happened to it. Both aircraft
// receivers serve /api/aircraft, so the tests can use that path to check
// a request reached the right one.
type fakeApp struct {
	meta    Meta
	running atomic.Bool
	runs    atomic.Int32
	stops   atomic.Int32

	// blockStop holds Run open after its context is cancelled, standing
	// in for a receiver that will not let go.
	blockStop time.Duration
}

func (f *fakeApp) Meta() Meta      { return f.meta }
func (f *fakeApp) Need() radio.Need { return radio.Need{Mode: radio.Samples} }

func (f *fakeApp) Handler(context.Context) (http.Handler, error) {
	mux := http.NewServeMux()
	// The path both aircraft receivers share, which is the one that must
	// not be answered by the wrong receiver.
	mux.HandleFunc("GET /api/aircraft", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, f.meta.ID)
	})
	mux.HandleFunc("GET /index.html", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "page for %s", f.meta.ID)
	})
	return mux, nil
}

func (f *fakeApp) Run(ctx context.Context, h radio.Handle) error {
	f.runs.Add(1)
	f.running.Store(true)
	defer func() { f.running.Store(false); f.stops.Add(1) }()
	<-ctx.Done()
	if f.blockStop > 0 {
		time.Sleep(f.blockStop)
	}
	return ctx.Err()
}

// testHub builds a hub over a broker with no real radio behind it.
func testHub(t *testing.T, apps ...App) *Hub {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	h, err := New(ctx, &fakeRadio{}, apps)
	if err != nil {
		t.Fatalf("new hub: %v", err)
	}
	t.Cleanup(h.Stop)
	return h
}

func twoAircraft() (*fakeApp, *fakeApp) {
	return &fakeApp{meta: Meta{"adsb", "Aircraft", "1090 MHz"}},
		&fakeApp{meta: Meta{"uat", "UAT", "978 MHz"}}
}

// Two receivers cannot both be called the same thing, or a tab would be
// unreachable and a request ambiguous.
func TestDuplicateIDsRefused(t *testing.T) {
	a, _ := twoAircraft()
	b := &fakeApp{meta: Meta{"adsb", "Other", "x"}}
	ctx := context.Background()
	if _, err := New(ctx, &fakeRadio{}, []App{a, b}); err == nil {
		t.Fatal("two receivers with the same id were accepted")
	}
}

// A receiver that is not on the air still answers for what it heard
// earlier. That is the whole reason the hub is worth having, and it is
// what stops a poll arriving mid-switch from getting an error.
func TestInactiveReceiverStillServes(t *testing.T) {
	a, u := twoAircraft()
	h := testHub(t, a, u)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	// Nothing has been selected, so nothing is on the air.
	body := getWithReferer(t, srv, "/api/aircraft", srv.URL+"/app/uat/")
	if body != "uat" {
		t.Errorf("got %q from an idle receiver, want its own answer", body)
	}
}

// The failure worth designing against: the two aircraft receivers serve
// identical paths, so answering with whichever happens to hold the radio
// would quietly render one page the other's aircraft — no error, just
// wrong data.
func TestDispatchUsesTheAskingPageNotTheActiveOne(t *testing.T) {
	a, u := twoAircraft()
	h := testHub(t, a, u)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	if err := h.Select("adsb"); err != nil {
		t.Fatalf("select: %v", err)
	}

	// A poll from the UAT page, while ADS-B holds the radio.
	if got := getWithReferer(t, srv, "/api/aircraft", srv.URL+"/app/uat/"); got != "uat" {
		t.Errorf("the UAT page was answered by %q — it would draw the wrong aircraft", got)
	}
	// And the active one still answers for itself.
	if got := getWithReferer(t, srv, "/api/aircraft", srv.URL+"/app/adsb/"); got != "adsb" {
		t.Errorf("the ADS-B page was answered by %q", got)
	}
}

// Without a referer — curl, a bookmarked API URL — the active receiver is
// what was meant.
func TestDispatchFallsBackToTheActiveReceiver(t *testing.T) {
	a, u := twoAircraft()
	h := testHub(t, a, u)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	if err := h.Select("uat"); err != nil {
		t.Fatalf("select: %v", err)
	}
	if got := getWithReferer(t, srv, "/api/aircraft", ""); got != "uat" {
		t.Errorf("got %q, want the active receiver's answer", got)
	}
}

// Every receiver's page is reachable whether or not it has the radio, so
// a tab can draw itself before being given it.
func TestAssetsServedForEveryReceiver(t *testing.T) {
	a, u := twoAircraft()
	h := testHub(t, a, u)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	for _, id := range []string{"adsb", "uat"} {
		got := getWithReferer(t, srv, "/app/"+id+"/index.html", "")
		if got != "page for "+id {
			t.Errorf("/app/%s/ served %q", id, got)
		}
	}
}

// The shell must not swallow the API paths beneath it.
func TestShellServesOnlyTheRoot(t *testing.T) {
	a, u := twoAircraft()
	h := testHub(t, a, u)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	r, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	if len(b) < 500 {
		t.Fatalf("the shell is %d bytes — that is not the page", len(b))
	}

	// and an API path still reaches a receiver, not the shell
	if got := getWithReferer(t, srv, "/api/aircraft", srv.URL+"/app/adsb/"); got != "adsb" {
		t.Errorf("/api/aircraft was answered with %q", got)
	}
}

// Selecting a tab starts that receiver and stops the one that had the
// radio. Only one can be running, because there is only one tuner.
func TestSelectStartsOneAndStopsTheOther(t *testing.T) {
	a, u := twoAircraft()
	h := testHub(t, a, u)

	if err := h.Select("adsb"); err != nil {
		t.Fatalf("select adsb: %v", err)
	}
	waitFor(t, "adsb to be running", func() bool { return a.running.Load() })

	if err := h.Select("uat"); err != nil {
		t.Fatalf("select uat: %v", err)
	}
	waitFor(t, "uat to be running", func() bool { return u.running.Load() })
	waitFor(t, "adsb to have stopped", func() bool { return !a.running.Load() })

	if h.Active() != "uat" || h.Phase() != PhaseOnAir {
		t.Errorf("active=%q phase=%q, want uat on the air", h.Active(), h.Phase())
	}
}

// Selecting the tab already on the air should do nothing, rather than
// pointlessly taking the radio away and giving it back.
func TestSelectingTheActiveTabIsANoOp(t *testing.T) {
	a, u := twoAircraft()
	h := testHub(t, a, u)

	if err := h.Select("adsb"); err != nil {
		t.Fatalf("select: %v", err)
	}
	waitFor(t, "adsb to be running", func() bool { return a.running.Load() })
	runs := a.runs.Load()

	if err := h.Select("adsb"); err != nil {
		t.Fatalf("reselect: %v", err)
	}
	if got := a.runs.Load(); got != runs {
		t.Errorf("reselecting restarted the receiver (%d runs, was %d)", got, runs)
	}
}

func TestSelectUnknownReceiver(t *testing.T) {
	a, u := twoAircraft()
	h := testHub(t, a, u)
	if err := h.Select("nope"); err == nil {
		t.Fatal("selecting a receiver that does not exist was accepted")
	}
}

// Stop takes the radio off everything and leaves the hub up, so the tab
// bar is still there to choose from.
func TestStopLeavesTheHubRunning(t *testing.T) {
	a, u := twoAircraft()
	h := testHub(t, a, u)

	if err := h.Select("adsb"); err != nil {
		t.Fatalf("select: %v", err)
	}
	waitFor(t, "adsb to be running", func() bool { return a.running.Load() })

	h.Stop()
	waitFor(t, "adsb to have stopped", func() bool { return !a.running.Load() })
	if h.Active() != "" || h.Phase() != PhaseIdle {
		t.Errorf("active=%q phase=%q, want idle", h.Active(), h.Phase())
	}
}

// A receiver that will not let go must not wedge the hub. It is a bug
// worth complaining about, not one worth hanging over.
func TestAStuckReceiverDoesNotWedgeTheHub(t *testing.T) {
	slow := &fakeApp{meta: Meta{"slow", "Slow", "x"}, blockStop: 5 * time.Second}
	_, u := twoAircraft()
	h := testHub(t, slow, u)

	if err := h.Select("slow"); err != nil {
		t.Fatalf("select: %v", err)
	}
	waitFor(t, "the slow receiver to start", func() bool { return slow.running.Load() })

	done := make(chan error, 1)
	go func() { done <- h.Select("uat") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("switching away from a stuck receiver failed: %v", err)
		}
	case <-time.After(stopTimeout + 3*time.Second):
		t.Fatal("the hub hung waiting for a receiver that would not stop")
	}
	waitFor(t, "uat to take over", func() bool { return u.running.Load() })
}

// The state endpoint is what the shell draws itself from.
func TestStateEndpoint(t *testing.T) {
	a, u := twoAircraft()
	h := testHub(t, a, u)
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()

	r, err := http.Get(srv.URL + "/hub/state")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer r.Body.Close()

	var st State
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(st.Tabs) != 2 || st.Tabs[0].ID != "adsb" || st.Tabs[1].ID != "uat" {
		t.Errorf("tabs = %+v, want adsb then uat in order", st.Tabs)
	}
	if st.Phase != PhaseIdle {
		t.Errorf("phase = %q, want idle before anything is selected", st.Phase)
	}
}

// The shell polls for progress while a switch is happening, so asking
// must not wait for the switch it is asking about.
func TestStateAnswersDuringASwitch(t *testing.T) {
	slow := &fakeApp{meta: Meta{"slow", "Slow", "x"}, blockStop: 2 * time.Second}
	_, u := twoAircraft()
	h := testHub(t, slow, u)

	if err := h.Select("slow"); err != nil {
		t.Fatalf("select: %v", err)
	}
	waitFor(t, "the slow receiver to start", func() bool { return slow.running.Load() })

	go h.Select("uat")
	time.Sleep(100 * time.Millisecond)

	answered := make(chan string, 1)
	go func() { answered <- h.Phase() }()
	select {
	case <-answered:
	case <-time.After(time.Second):
		t.Fatal("asking for the phase blocked on the switch in progress")
	}
}

func getWithReferer(t *testing.T, srv *httptest.Server, path, referer string) string {
	t.Helper()
	req, err := http.NewRequest("GET", srv.URL+path, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", path, err)
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeRadio stands in for the broker. What the hub does with the radio is
// tested where the broker lives; what is tested here is which receiver
// gets it, and that needs no hardware at all.
type fakeRadio struct {
	acquires atomic.Int32
	releases atomic.Int32
	fail     error
}

func (f *fakeRadio) Acquire(context.Context, radio.Need) (radio.Handle, error) {
	if f.fail != nil {
		return radio.Handle{}, f.fail
	}
	f.acquires.Add(1)
	return radio.Handle{}, nil
}

func (f *fakeRadio) Release()          { f.releases.Add(1) }
func (f *fakeRadio) State() radio.State { return radio.State{Running: true} }

// A radio that cannot be arranged leaves the hub saying so rather than
// pretending a receiver is on the air.
func TestSelectReportsARadioThatWillNotArrange(t *testing.T) {
	a, u := twoAircraft()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h, err := New(ctx, &fakeRadio{fail: errNoRadio}, []App{a, u})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := h.Select("adsb"); err == nil {
		t.Fatal("selecting a tab succeeded with no radio to give it")
	}
	if h.Phase() != PhaseFailed {
		t.Errorf("phase = %q, want failed", h.Phase())
	}
	if a.running.Load() {
		t.Error("the receiver was started even though the radio was not arranged")
	}
}

var errNoRadio = fmt.Errorf("no radio")
