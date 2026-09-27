package alert

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/track"
)

// Watchlist is the flights someone is waiting for: a relative's
// arrival, a particular airframe's daily run. It differs from a rule in
// what happens on a match. A rule raises one alert and moves on; a
// watched flight raises an alarm that stays raised — and keeps making a
// noise in the page — until someone acknowledges it or the aircraft
// drops out of range.
//
// The list is edited from the page and kept in a file of its own, so the
// hand-written rules file is never rewritten by a program. One list can
// be shared by several watchers, which is how the two aircraft receivers
// in the hub agree on it.
type Watchlist struct {
	path string

	mu      sync.Mutex
	flights []string
}

// watchlistFile is the on-disk shape.
type watchlistFile struct {
	Flights []string `json:"flights"`
}

// maxFlights bounds the list. It is edited over HTTP, and a list of
// thousands is a mistake rather than a watchlist.
const maxFlights = 100

// OpenWatchlist reads the list at path. A missing file is an empty list,
// and an empty path is a list that is never saved.
func OpenWatchlist(path string) (*Watchlist, error) {
	l := &Watchlist{path: path}
	if path == "" {
		return l, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	var f watchlistFile
	if err := json.Unmarshal(b, &f); err != nil {
		return l, fmt.Errorf("%s: %w", path, err)
	}
	flights, err := normaliseFlights(f.Flights)
	if err != nil {
		return l, fmt.Errorf("%s: %w", path, err)
	}
	l.flights = flights
	return l, nil
}

// Flights is the list as it stands.
func (l *Watchlist) Flights() []string {
	if l == nil {
		return []string{}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string{}, l.flights...)
}

// Set replaces the list and saves it, returning the list as kept —
// upper-cased, de-duplicated and sorted.
func (l *Watchlist) Set(flights []string) ([]string, error) {
	norm, err := normaliseFlights(flights)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.path != "" {
		if err := saveWatchlist(l.path, norm); err != nil {
			return nil, err
		}
	}
	l.flights = norm
	return append([]string{}, norm...), nil
}

func saveWatchlist(path string, flights []string) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(watchlistFile{Flights: flights}, "", "  ")
	if err != nil {
		return err
	}
	// Written beside and renamed over, so a crash mid-write cannot
	// leave a half list that fails to parse on the next start.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Match returns the watchlist entry a callsign answers to, if any.
func (l *Watchlist) Match(callsign string) (string, bool) {
	if l == nil || callsign == "" {
		return "", false
	}
	cs := canonFlight(strings.ToUpper(strings.TrimSpace(callsign)))
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.flights {
		if flightMatches(f, cs) {
			return f, true
		}
	}
	return "", false
}

// normaliseFlights cleans a list typed by a person: case, spaces,
// duplicates. Anything that could not be a callsign is refused rather
// than kept, since an entry that can never match looks exactly like a
// flight that has not turned up yet.
func normaliseFlights(in []string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, f := range in {
		f = strings.ToUpper(strings.Join(strings.Fields(f), ""))
		if f == "" || seen[f] {
			continue
		}
		if len(f) > 8 {
			return nil, fmt.Errorf("%q is longer than a callsign can be (8 characters)", f)
		}
		for _, c := range f {
			if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '*') {
				return nil, fmt.Errorf("%q: a flight is letters and digits, with * as a wildcard", f)
			}
		}
		if strings.Trim(f, "*") == "" {
			return nil, fmt.Errorf("%q would match every aircraft", f)
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) > maxFlights {
		return nil, fmt.Errorf("%d flights is more than the %d a watchlist holds", len(out), maxFlights)
	}
	sort.Strings(out)
	return out, nil
}

// flightMatches compares one watchlist entry with a callsign already in
// canonical form.
func flightMatches(entry, cs string) bool {
	if strings.Contains(entry, "*") {
		return matchPattern(entry, cs)
	}
	if canonFlight(entry) == cs {
		return true
	}
	icao, ok := iataToICAO(entry)
	return ok && canonFlight(icao) == cs
}

// canonFlight drops leading zeros from the flight number, so DAL0042
// and DAL42 are the same flight: airlines disagree about padding, and
// the person typing it will not know which this one uses.
func canonFlight(s string) string {
	i := 0
	for i < len(s) && s[i] >= 'A' && s[i] <= 'Z' {
		i++
	}
	if i == 0 || i == len(s) {
		return s
	}
	num := strings.TrimLeft(s[i:], "0")
	if num == "" || num[0] < '0' || num[0] > '9' {
		// All zeros, or zeros before a letter: not a flight number to
		// tidy, so leave it exactly as it was.
		return s
	}
	return s[:i] + num
}

// iataToICAO turns the flight number printed on a ticket — DL1234 —
// into the callsign the aircraft actually transmits, DAL1234. ADS-B
// carries only the ICAO form, so without this the obvious thing to type
// never matches.
func iataToICAO(f string) (string, bool) {
	if len(f) < 3 || f[2] < '0' || f[2] > '9' {
		return "", false
	}
	code, ok := airlines[f[:2]]
	if !ok {
		return "", false
	}
	return code + f[2:], true
}

// airlines maps IATA airline designators to ICAO ones, for the carriers
// someone in North America is most likely to be meeting. It is short on
// purpose: every entry is stable and published, and anything missing
// can be typed in its ICAO form instead.
//
// A regional flight sold under a mainline number is flown under the
// regional operator's callsign — DL5123 is transmitted as EDV5123 — so
// no table can catch those; "*5123" can.
var airlines = map[string]string{
	"AA": "AAL", // American
	"AS": "ASA", // Alaska
	"B6": "JBU", // JetBlue
	"DL": "DAL", // Delta
	"F9": "FFT", // Frontier
	"G4": "AAY", // Allegiant
	"HA": "HAL", // Hawaiian
	"MX": "MXY", // Breeze
	"NK": "NKS", // Spirit
	"SY": "SCX", // Sun Country
	"UA": "UAL", // United
	"WN": "SWA", // Southwest
	"9E": "EDV", // Endeavor
	"MQ": "ENY", // Envoy
	"OH": "JIA", // PSA
	"OO": "SKW", // SkyWest
	"YV": "ASH", // Mesa
	"YX": "RPA", // Republic
	"AC": "ACA", // Air Canada
	"WS": "WJA", // WestJet
	"AM": "AMX", // Aeroméxico
	"BA": "BAW", // British Airways
	"LH": "DLH", // Lufthansa
	"AF": "AFR", // Air France
	"KL": "KLM", // KLM
	"EK": "UAE", // Emirates
	"QR": "QTR", // Qatar
	"FX": "FDX", // FedEx
	"5X": "UPS", // UPS
}

// Alarm is a watched flight currently in range.
type Alarm struct {
	Hex        string    `json:"hex"`
	Callsign   string    `json:"callsign"`
	Flight     string    `json:"flight"` // the watchlist entry it matched
	Since      time.Time `json:"since"`
	LastSeen   time.Time `json:"last_seen"`
	Acked      bool      `json:"acked"`
	Altitude   int       `json:"altitude,omitempty"`
	DistanceNM float64   `json:"distance_nm,omitempty"`
}

// WatchFlights attaches a watchlist. An alarm ends when its aircraft has
// not been heard for gone, which should be the tracker's own time to
// live: the alarm clears when the aircraft leaves the table, not before
// and not after.
func (w *Watcher) WatchFlights(l *Watchlist, gone time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.list = l
	w.gone = gone
	if w.alarms == nil {
		w.alarms = make(map[string]*Alarm)
	}
}

// Watchlist is the attached list, or nil.
func (w *Watcher) Watchlist() *Watchlist {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.list
}

// checkWatchlist raises or refreshes the alarm for one aircraft. It
// returns an alert only when the alarm is new, so the log and the
// alert command hear about each visit once, however long it lasts.
// Called with w.mu held.
func (w *Watcher) checkWatchlist(ac track.Aircraft, now time.Time) (Alert, bool) {
	if w.list == nil {
		return Alert{}, false
	}
	flight, ok := w.list.Match(ac.Callsign)
	if !ok {
		return Alert{}, false
	}
	al := w.alarms[ac.Hex]
	if al != nil && w.ended(al, now) {
		// Gone and back again is a new visit, and worth a new noise
		// even if the last one was acknowledged.
		al = nil
	}
	fresh := al == nil
	if fresh {
		al = &Alarm{Hex: ac.Hex, Since: now}
		w.alarms[ac.Hex] = al
	}
	al.Callsign, al.Flight, al.LastSeen = ac.Callsign, flight, now
	if ac.HasAltitude {
		al.Altitude = ac.Altitude
	}
	if ac.HasPosition {
		al.DistanceNM = ac.DistanceNM
	}
	if !fresh {
		return Alert{}, false
	}
	a := alertFor(Rule{
		Name:   "watchlist",
		Note:   "watched flight " + flight + " is in range",
		Urgent: true,
	}, ac, now)
	return a, true
}

func (w *Watcher) ended(al *Alarm, now time.Time) bool {
	return w.gone > 0 && now.Sub(al.LastSeen) > w.gone
}

// Alarms returns the watched flights in range now, newest first, and
// forgets the ones that have left or been taken off the list.
func (w *Watcher) Alarms(now time.Time) []Alarm {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := []Alarm{}
	for hex, al := range w.alarms {
		if _, still := w.list.Match(al.Callsign); !still || w.ended(al, now) {
			delete(w.alarms, hex)
			continue
		}
		out = append(out, *al)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	return out
}

// Ack silences the alarm for one aircraft, or every alarm when hex is
// empty. The alarm stays listed until the aircraft leaves; it just stops
// asking for attention. It reports whether there was such an alarm.
func (w *Watcher) Ack(hex string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	hex = strings.ToLower(strings.TrimSpace(hex))
	found := false
	for h, al := range w.alarms {
		if hex == "" || h == hex {
			al.Acked, found = true, true
		}
	}
	return found
}
