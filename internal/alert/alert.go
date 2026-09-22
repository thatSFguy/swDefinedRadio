// Package alert watches the aircraft table for the ones worth looking
// up from: military transports, declared emergencies, anything on a
// personal watchlist. The rules are data, not code, so a receiver can
// be told what counts as interesting without rebuilding it.
package alert

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/track"
)

// Rule is one reason to raise an alert. Every condition that is set has
// to match, so a rule can be narrowed by combining them; a ruleset fires
// when any of its rules match.
type Rule struct {
	Name string `json:"name"`
	Note string `json:"note,omitempty"`

	// Hex matches the ICAO address: an exact "4b1814", a prefix "ae*",
	// or a range "ae0000-afffff". Address blocks are allocated by
	// country and by operator, which is what makes this useful.
	Hex []string `json:"hex,omitempty"`

	// Callsign matches the flight identification, with * allowed at
	// either end: "RCH*" catches RCH271 and RCH4077.
	Callsign []string `json:"callsign,omitempty"`

	// Squawk matches the Mode A code exactly, e.g. "7700".
	Squawk []string `json:"squawk,omitempty"`

	// Emergency matches any declared emergency state.
	Emergency bool `json:"emergency,omitempty"`

	AboveFt  int     `json:"above_ft,omitempty"`
	BelowFt  int     `json:"below_ft,omitempty"`
	AboveKts float64 `json:"above_kts,omitempty"`
	WithinNM float64 `json:"within_nm,omitempty"`

	// Urgent marks a rule worth interrupting someone for. The UI makes
	// a noise for these and stays quiet for the rest.
	Urgent bool `json:"urgent,omitempty"`
}

// Alert is one raised match, kept for the UI and written to the log.
type Alert struct {
	At         time.Time `json:"at"`
	Rule       string    `json:"rule"`
	Note       string    `json:"note,omitempty"`
	Urgent     bool      `json:"urgent,omitempty"`
	Hex        string    `json:"hex"`
	Callsign   string    `json:"callsign,omitempty"`
	Squawk     string    `json:"squawk,omitempty"`
	Emergency  string    `json:"emergency,omitempty"`
	Altitude   int       `json:"altitude,omitempty"`
	Speed      float64   `json:"speed,omitempty"`
	DistanceNM float64   `json:"distance_nm,omitempty"`
	Lat        float64   `json:"lat,omitempty"`
	Lon        float64   `json:"lon,omitempty"`
}

// Line is a one-line description, for a log or a notification.
func (a Alert) Line() string {
	who := a.Hex
	if a.Callsign != "" {
		who = a.Callsign + " (" + a.Hex + ")"
	}
	var extra []string
	if a.Squawk != "" {
		extra = append(extra, "squawk "+a.Squawk)
	}
	if a.Emergency != "" {
		extra = append(extra, a.Emergency)
	}
	if a.Altitude != 0 {
		extra = append(extra, strconv.Itoa(a.Altitude)+" ft")
	}
	if a.DistanceNM > 0 {
		extra = append(extra, fmt.Sprintf("%.0f NM", a.DistanceNM))
	}
	s := a.Rule + ": " + who
	if len(extra) > 0 {
		s += " — " + strings.Join(extra, ", ")
	}
	return s
}

// DefaultCooldown is how long the same aircraft is left alone after
// matching a rule. An aircraft in range sends several messages a second,
// and one alert per message would be useless.
const DefaultCooldown = 30 * time.Minute

// keep is how many recent alerts are held for the UI.
const keep = 200

// Watcher applies a ruleset to aircraft updates.
type Watcher struct {
	rules    []Rule
	cooldown time.Duration
	sink     func(Alert)

	mu     sync.Mutex
	fired  map[string]time.Time
	recent []Alert // newest first
}

// New returns a watcher. sink is called once per raised alert, from
// whichever goroutine called Check, and may be nil.
func New(rules []Rule, cooldown time.Duration, sink func(Alert)) *Watcher {
	if cooldown <= 0 {
		cooldown = DefaultCooldown
	}
	return &Watcher{
		rules:    rules,
		cooldown: cooldown,
		sink:     sink,
		fired:    make(map[string]time.Time),
	}
}

// Rules is the ruleset in force, for the UI to show.
func (w *Watcher) Rules() []Rule { return w.rules }

// Check tests one aircraft against every rule and returns whatever it
// raised — usually nothing.
func (w *Watcher) Check(ac track.Aircraft, now time.Time) []Alert {
	var raised []Alert
	for _, r := range w.rules {
		if !r.matches(ac) {
			continue
		}
		key := ac.Hex + "|" + r.Name
		w.mu.Lock()
		last, seen := w.fired[key]
		if seen && now.Sub(last) < w.cooldown {
			w.mu.Unlock()
			continue
		}
		w.fired[key] = now
		a := alertFor(r, ac, now)
		w.recent = append([]Alert{a}, w.recent...)
		if len(w.recent) > keep {
			w.recent = w.recent[:keep]
		}
		w.prune(now)
		w.mu.Unlock()

		raised = append(raised, a)
		if w.sink != nil {
			w.sink(a)
		}
	}
	return raised
}

// Recent returns the alerts raised so far, newest first.
func (w *Watcher) Recent(limit int) []Alert {
	w.mu.Lock()
	defer w.mu.Unlock()
	if limit <= 0 || limit > len(w.recent) {
		limit = len(w.recent)
	}
	out := make([]Alert, limit)
	copy(out, w.recent[:limit])
	return out
}

// prune drops cooldown entries that can no longer suppress anything,
// so a receiver left running for weeks does not accumulate them.
func (w *Watcher) prune(now time.Time) {
	if len(w.fired) < 1000 {
		return
	}
	for k, t := range w.fired {
		if now.Sub(t) > w.cooldown {
			delete(w.fired, k)
		}
	}
}

func alertFor(r Rule, ac track.Aircraft, now time.Time) Alert {
	a := Alert{
		At: now, Rule: r.Name, Note: r.Note, Urgent: r.Urgent,
		Hex: ac.Hex, Callsign: ac.Callsign, Squawk: ac.Squawk,
		Speed: ac.GroundSpeed, DistanceNM: ac.DistanceNM,
	}
	if ac.HasAltitude {
		a.Altitude = ac.Altitude
	}
	if ac.HasPosition {
		a.Lat, a.Lon = ac.Lat, ac.Lon
	}
	if ac.Emergency != "" && ac.Emergency != "none" {
		a.Emergency = ac.Emergency
	}
	return a
}

// matches reports whether every condition the rule sets is satisfied.
func (r Rule) matches(ac track.Aircraft) bool {
	if len(r.Hex) > 0 && !matchHex(r.Hex, ac.Hex) {
		return false
	}
	if len(r.Callsign) > 0 && !matchAny(r.Callsign, ac.Callsign) {
		return false
	}
	if len(r.Squawk) > 0 && !matchAny(r.Squawk, ac.Squawk) {
		return false
	}
	if r.Emergency && (ac.Emergency == "" || ac.Emergency == "none") {
		return false
	}
	if r.AboveFt != 0 && (!ac.HasAltitude || ac.Altitude <= r.AboveFt) {
		return false
	}
	if r.BelowFt != 0 && (!ac.HasAltitude || ac.Altitude >= r.BelowFt) {
		return false
	}
	if r.AboveKts != 0 && ac.GroundSpeed <= r.AboveKts {
		return false
	}
	if r.WithinNM != 0 && (!ac.HasPosition || ac.DistanceNM > r.WithinNM) {
		return false
	}
	// A rule with no conditions at all would match every aircraft, which
	// is never what anyone means.
	return !r.empty()
}

func (r Rule) empty() bool {
	return len(r.Hex) == 0 && len(r.Callsign) == 0 && len(r.Squawk) == 0 &&
		!r.Emergency && r.AboveFt == 0 && r.BelowFt == 0 &&
		r.AboveKts == 0 && r.WithinNM == 0
}

// matchAny matches a value against patterns that may have a * at either
// end. Matching is case-insensitive: callsigns arrive upper-case but
// nobody types them that way.
func matchAny(pats []string, v string) bool {
	if v == "" {
		return false
	}
	v = strings.ToUpper(strings.TrimSpace(v))
	for _, p := range pats {
		if matchPattern(strings.ToUpper(strings.TrimSpace(p)), v) {
			return true
		}
	}
	return false
}

func matchPattern(p, v string) bool {
	switch {
	case p == "":
		return false
	case p == "*":
		return true
	case strings.HasPrefix(p, "*") && strings.HasSuffix(p, "*") && len(p) > 1:
		return strings.Contains(v, strings.Trim(p, "*"))
	case strings.HasSuffix(p, "*"):
		return strings.HasPrefix(v, strings.TrimSuffix(p, "*"))
	case strings.HasPrefix(p, "*"):
		return strings.HasSuffix(v, strings.TrimPrefix(p, "*"))
	}
	return p == v
}

// matchHex also understands "ae0000-afffff", because address blocks are
// allocated as ranges and writing one out as prefixes is error-prone.
func matchHex(pats []string, hex string) bool {
	if hex == "" {
		return false
	}
	hex = strings.ToLower(hex)
	n, err := strconv.ParseUint(hex, 16, 32)
	for _, p := range pats {
		p = strings.ToLower(strings.TrimSpace(p))
		lo, hi, isRange := strings.Cut(p, "-")
		if isRange && err == nil {
			a, err1 := strconv.ParseUint(strings.TrimSpace(lo), 16, 32)
			b, err2 := strconv.ParseUint(strings.TrimSpace(hi), 16, 32)
			if err1 == nil && err2 == nil && n >= a && n <= b {
				return true
			}
			continue
		}
		if matchPattern(p, hex) {
			return true
		}
	}
	return false
}
