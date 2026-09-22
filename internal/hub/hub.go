// Package hub serves every receiver from one process, with a tab bar to
// choose between them.
//
// Only one receiver can be on the air at a time — one tuner, one
// converter — so the tabs are mutually exclusive and selecting one hands
// it the radio. What the hub adds over running the receivers separately
// is that it outlives any of them: the aircraft table, the sensor store
// and the alert history go on existing while the radio is doing something
// else, so leaving a tab and coming back does not start from nothing.
//
// Each receiver keeps its own page and its own API, unchanged from when
// it was a command of its own. The shell holds them in a frame and the
// hub sends each request to the receiver it belongs to.
package hub

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/radio"
	"github.com/thatSFguy/swDefinedRadio/internal/web"
)

//go:embed web
var webFS embed.FS

// Meta is how a receiver presents itself on the tab bar.
type Meta struct {
	ID    string `json:"id"`    // "adsb" — the URL segment
	Title string `json:"title"` // "Aircraft" — what the tab says
	Band  string `json:"band"`  // "1090 MHz"
}

// App is one tab.
//
// The two halves of an App's life are what make the hub worth having.
// Handler is built once, at startup, and answers for as long as the
// process runs. Run is called and cancelled every time the tab is
// selected and left. So a receiver that is not on the air still serves
// what it heard earlier, and a poll arriving from a tab that has just
// been switched away from gets a sane answer rather than an error.
type App interface {
	Meta() Meta

	// Need is asked immediately before every activation, so a receiver
	// retuned from its own page asks for where it actually is.
	Need() radio.Need

	// Handler is this receiver's page and API. ctx is the hub's lifetime.
	Handler(ctx context.Context) (http.Handler, error)

	// Run holds the radio until ctx is cancelled. It must return
	// promptly once it is: nothing else can have the radio until it does.
	Run(ctx context.Context, h radio.Handle) error
}

// Background is implemented by receivers with work that carries on while
// they are off the air — expiring an aircraft table, saving a store. An
// aircraft last heard four minutes ago has gone whether or not anyone was
// listening, so that timer keeps running.
type Background interface {
	Background(ctx context.Context)
}

// Phase is what the hub is doing, for the shell to show.
const (
	PhaseIdle     = "idle"     // nothing on the air
	PhaseStopping = "stopping" // taking the radio off the last receiver
	PhaseArming   = "arming"   // reconfiguring for the next one
	PhaseOnAir    = "on_air"
	PhaseFailed   = "failed"
)

// Radio is the part of the broker the hub uses.
//
// Narrow on purpose: the hub decides *which* receiver should have the
// radio and knows nothing about how one is arranged, which keeps the
// tab logic testable without a dongle or an rtl_tcp to talk to.
type Radio interface {
	Acquire(ctx context.Context, n radio.Need) (radio.Handle, error)
	Release()
	State() radio.State
}

// Hub holds the receivers and decides which one has the radio.
type Hub struct {
	broker Radio
	ctx    context.Context

	apps  []App
	byID  map[string]App
	muxes map[string]http.Handler

	// switchMu serialises handovers. It is held for as long as a switch
	// takes, which is why the state below has a lock of its own: the
	// shell polls for progress *during* a switch, and would be answering
	// its own question if that poll had to wait for the switch to finish.
	switchMu sync.Mutex

	stateMu sync.RWMutex
	active  string
	phase   string
	target  string
	lastErr string
	since   time.Time

	cancel context.CancelFunc
	done   chan struct{}
}

// New builds the hub. ctx is the process's lifetime: it bounds the
// receivers' background work and the radio itself, and must not be tied
// to any one activation.
func New(ctx context.Context, b Radio, apps []App) (*Hub, error) {
	h := &Hub{
		broker: b, ctx: ctx, apps: apps,
		byID:  make(map[string]App, len(apps)),
		muxes: make(map[string]http.Handler, len(apps)),
		phase: PhaseIdle, since: time.Now(),
	}
	for _, a := range apps {
		id := a.Meta().ID
		if _, dup := h.byID[id]; dup {
			return nil, fmt.Errorf("hub: two receivers both called %q", id)
		}
		// Each receiver gets a mux of its own. A shared one would panic
		// on registration, because the two aircraft receivers serve the
		// same paths — and prefixing them apart would mean editing the
		// pages, which is the thing worth not doing.
		mux, err := a.Handler(ctx)
		if err != nil {
			return nil, fmt.Errorf("hub: %s: %w", id, err)
		}
		h.byID[id], h.muxes[id] = a, mux

		if bg, ok := a.(Background); ok {
			go bg.Background(ctx)
		}
	}
	return h, nil
}

// Select puts a receiver on the air, taking the radio off whichever had
// it. Selecting the one already on the air does nothing.
func (h *Hub) Select(id string) error {
	app, ok := h.byID[id]
	if !ok {
		return fmt.Errorf("no receiver called %q", id)
	}

	h.switchMu.Lock()
	defer h.switchMu.Unlock()

	if h.Active() == id && h.Phase() == PhaseOnAir {
		return nil
	}
	h.stop(id)

	h.setPhase(PhaseArming, id, "")
	hnd, err := h.broker.Acquire(h.ctx, app.Need())
	if err != nil {
		h.setPhase(PhaseFailed, id, err.Error())
		return err
	}

	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	h.cancel, h.done = cancel, done

	go func() {
		defer close(done)
		defer func() {
			// One receiver failing is not a reason to lose the other
			// four, along with everything they have heard.
			if p := recover(); p != nil {
				log.Printf("hub: %s panicked: %v", id, p)
				h.setPhase(PhaseFailed, id, fmt.Sprint(p))
			}
		}()
		if err := app.Run(ctx, hnd); err != nil && ctx.Err() == nil {
			log.Printf("hub: %s stopped: %v", id, err)
			h.setPhase(PhaseFailed, id, err.Error())
		}
	}()

	h.setActive(id)
	return nil
}

// Stop takes the radio off whatever has it.
func (h *Hub) Stop() {
	h.switchMu.Lock()
	defer h.switchMu.Unlock()
	h.stop("")
	h.setPhase(PhaseIdle, "", "")
}

// stopTimeout is how long a receiver is given to notice it has lost the
// radio. It should take microseconds — the read it is parked in is
// interrupted directly — so reaching this means something is wrong and
// is worth saying rather than hiding.
const stopTimeout = 3 * time.Second

// stop ends the current activation. switchMu must be held.
func (h *Hub) stop(next string) {
	if h.cancel == nil {
		return
	}
	h.setPhase(PhaseStopping, next, "")

	// Cancel and revoke both. Cancelling alone cannot wake a receiver
	// parked in a socket read; revoking alone would leave its loop
	// spinning on failed reads, because its context is still live.
	h.cancel()
	h.broker.Release()

	select {
	case <-h.done:
	case <-time.After(stopTimeout):
		log.Printf("hub: %s did not stop within %v; carrying on without it",
			h.Active(), stopTimeout)
	}
	h.cancel, h.done = nil, nil
	h.setActive("")
}

func (h *Hub) setPhase(phase, target, errMsg string) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	h.phase, h.target, h.lastErr, h.since = phase, target, errMsg, time.Now()
}

func (h *Hub) setActive(id string) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	h.active = id
	if id != "" {
		h.phase, h.target, h.lastErr, h.since = PhaseOnAir, "", "", time.Now()
	}
}

// Active is the receiver currently holding the radio, or "".
func (h *Hub) Active() string {
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()
	return h.active
}

// Phase is what the hub is doing.
func (h *Hub) Phase() string {
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()
	return h.phase
}

// State is everything the shell needs to draw itself.
type State struct {
	Tabs    []Meta      `json:"tabs"`
	Active  string      `json:"active"`
	Phase   string      `json:"phase"`
	Target  string      `json:"target,omitempty"`
	Error   string      `json:"error,omitempty"`
	SinceMs int64       `json:"since_ms"`
	Radio   radio.State `json:"radio"`
}

func (h *Hub) State() State {
	h.stateMu.RLock()
	st := State{
		Active: h.active, Phase: h.phase, Target: h.target, Error: h.lastErr,
		SinceMs: time.Since(h.since).Milliseconds(),
	}
	h.stateMu.RUnlock()

	st.Tabs = make([]Meta, 0, len(h.apps))
	for _, a := range h.apps {
		st.Tabs = append(st.Tabs, a.Meta())
	}
	st.Radio = h.broker.State()
	return st
}

// Handler is everything the hub serves.
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()

	// The shell, and only the shell. Without {$} this pattern is a
	// catch-all and would swallow every API path below it.
	shell, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Printf("hub: web assets: %v", err)
	} else {
		mux.Handle("GET /{$}", http.FileServer(http.FS(shell)))
	}

	mux.HandleFunc("GET /hub/state", func(w http.ResponseWriter, r *http.Request) {
		web.WriteJSON(w, h.State())
	})

	mux.HandleFunc("POST /hub/select", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if err := h.Select(id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		web.WriteJSON(w, h.State())
	})

	mux.HandleFunc("POST /hub/stop", func(w http.ResponseWriter, r *http.Request) {
		h.Stop()
		web.WriteJSON(w, h.State())
	})

	// Every receiver's assets, whether or not it is on the air, so a tab
	// can draw itself before it has the radio. The pages load their own
	// files relatively — Leaflet, the offline basemap — so serving each
	// under its own prefix keeps them apart with no edits to any of them.
	mux.Handle("GET /app/{id}/", http.StripPrefix("/app/", h.assets()))

	mux.Handle("/", h.dispatch())
	return mux
}

// assets serves /app/<id>/... from that receiver's own handler.
func (h *Hub) assets() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		mux, ok := h.muxes[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/" + rest
		mux.ServeHTTP(w, r2)
	})
}

// dispatch sends a request to the receiver it belongs to.
//
// The pages ask for absolute paths — /api/aircraft, /audio.wav — and they
// are not edited, so the path alone cannot say which receiver is meant.
// Two of them, the 1090 MHz and 978 MHz aircraft pages, use the *same*
// paths, so answering with whichever receiver happens to be on the air
// would quietly show one page the other's aircraft.
//
// The frame's Referer says which page asked, because the page is served
// from /app/<id>/. When there is none — someone with curl, or a
// bookmarked API URL — the active receiver is what they meant.
func (h *Hub) dispatch() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		active := h.Active()
		want := appFromReferer(r.Referer())
		if want == "" {
			want = active
		}
		if want == "" {
			http.Error(w, "no receiver is on the air", http.StatusServiceUnavailable)
			return
		}
		mux, ok := h.muxes[want]
		if !ok {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

// appFromReferer picks the receiver id out of a /app/<id>/ referer.
func appFromReferer(ref string) string {
	if ref == "" {
		return ""
	}
	i := strings.Index(ref, "/app/")
	if i < 0 {
		return ""
	}
	id, _, _ := strings.Cut(ref[i+len("/app/"):], "/")
	return id
}
