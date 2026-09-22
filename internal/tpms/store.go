package tpms

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Clustering constants. Sensors on one car transmit in a loose burst as
// the wheels wake up, so co-occurrence in time is the signal that they
// belong together.
const (
	// CoWindow is how close two transmissions must be to count as
	// possibly coming from the same vehicle.
	CoWindow = 90 * time.Second

	// MinShared is how many co-occurrences are needed before two sensors
	// are linked, so a single coincidental overlap cannot merge two cars.
	MinShared = 3

	// MinRatio is the fraction of the rarer sensor's sightings that must
	// be shared. Four wheels on one car approach 1.0; two cars that
	// occasionally pass at the same moment stay well below it.
	MinRatio = 0.4
)

// Sensor is the accumulated history of one TPMS sensor.
type Sensor struct {
	Key   string `json:"key"`
	ID    string `json:"id"`
	Model string `json:"model"`
	Label string `json:"label,omitempty"` // user-assigned, e.g. "front left"

	First time.Time `json:"first_seen"`
	Last  time.Time `json:"last_seen"`
	Count int       `json:"count"`

	PressureKPa    float64 `json:"pressure_kpa,omitempty"`
	HasPressure    bool    `json:"has_pressure"`
	TemperatureC   float64 `json:"temperature_c,omitempty"`
	HasTemperature bool    `json:"has_temperature"`
	BatteryOK      *bool   `json:"battery_ok,omitempty"`
	Integrity      string  `json:"integrity,omitempty"`

	// Vehicle is filled in by clustering, not stored.
	Vehicle string `json:"vehicle,omitempty"`
}

// PSI converts the last pressure for display.
func (s Sensor) PSI() float64 { return s.PressureKPa / 6.894757 }

// Vehicle is a set of sensors that consistently transmit together, which
// in practice means the wheels of one car.
type Vehicle struct {
	ID      string   `json:"id"`
	Label   string   `json:"label,omitempty"`
	Sensors []string `json:"sensors"`
	Models  []string `json:"models"`

	First time.Time `json:"first_seen"`
	Last  time.Time `json:"last_seen"`
}

// state is the on-disk form of everything worth surviving a restart.
type state struct {
	Sensors map[string]*Sensor        `json:"sensors"`
	Cooccur map[string]map[string]int `json:"cooccur"`
	Labels  map[string]string         `json:"labels"`
}

// Store holds every sensor heard, the co-occurrence counts that drive
// clustering, and the user's labels. It is safe for concurrent use.
type Store struct {
	mu      sync.RWMutex
	sensors map[string]*Sensor
	cooccur map[string]map[string]int
	labels  map[string]string

	recent  []Reading // ring of the newest readings, for the live feed
	total   int64
	dir     string
	logFile *os.File
	dirty   bool
}

// NewStore opens (or creates) a store backed by dir. Readings are
// appended to readings.jsonl, and the sensor table to state.json.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", dir, err)
	}
	s := &Store{
		sensors: make(map[string]*Sensor),
		cooccur: make(map[string]map[string]int),
		labels:  make(map[string]string),
		dir:     dir,
	}
	if err := s.load(); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(filepath.Join(dir, "readings.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open reading log: %w", err)
	}
	s.logFile = f
	return s, nil
}

func (s *Store) statePath() string { return filepath.Join(s.dir, "state.json") }

func (s *Store) load() error {
	b, err := os.ReadFile(s.statePath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state: %w", err)
	}
	var st state
	if err := json.Unmarshal(b, &st); err != nil {
		return fmt.Errorf("parse state: %w", err)
	}
	if st.Sensors != nil {
		s.sensors = st.Sensors
	}
	if st.Cooccur != nil {
		s.cooccur = st.Cooccur
	}
	if st.Labels != nil {
		s.labels = st.Labels
	}
	return nil
}

// Save writes the sensor table atomically, so a crash mid-write cannot
// leave a truncated state file behind.
func (s *Store) Save() error {
	s.mu.Lock()
	st := state{Sensors: s.sensors, Cooccur: s.cooccur, Labels: s.labels}
	b, err := json.MarshalIndent(st, "", "  ")
	s.dirty = false
	s.mu.Unlock()
	if err != nil {
		return err
	}

	tmp := s.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.statePath())
}

// Close flushes state and closes the reading log.
func (s *Store) Close() error {
	err := s.Save()
	if s.logFile != nil {
		s.logFile.Close()
	}
	return err
}

// Add folds one reading into the store and appends it to the log.
func (s *Store) Add(r Reading) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := r.Key()
	sen := s.sensors[key]
	if sen == nil {
		sen = &Sensor{Key: key, ID: r.ID, Model: r.Model, First: r.At}
		s.sensors[key] = sen
	}

	// Note every other sensor heard recently: those are the candidates
	// for being on the same vehicle.
	for other, o := range s.sensors {
		if other == key {
			continue
		}
		if d := r.At.Sub(o.Last); d < CoWindow && d > -CoWindow {
			s.link(key, other)
		}
	}

	sen.Last = r.At
	sen.Count++
	if r.HasPressure {
		sen.PressureKPa, sen.HasPressure = r.PressureKPa, true
	}
	if r.HasTemperature {
		sen.TemperatureC, sen.HasTemperature = r.TemperatureC, true
	}
	if r.BatteryOK != nil {
		sen.BatteryOK = r.BatteryOK
	}
	if r.Integrity != "" {
		sen.Integrity = r.Integrity
	}

	s.total++
	s.dirty = true
	s.recent = append(s.recent, r)
	if len(s.recent) > 200 {
		s.recent = s.recent[len(s.recent)-200:]
	}

	if s.logFile != nil {
		if b, err := json.Marshal(r); err == nil {
			fmt.Fprintf(s.logFile, "%s\n", b)
		}
	}
}

func (s *Store) link(a, b string) {
	if s.cooccur[a] == nil {
		s.cooccur[a] = make(map[string]int)
	}
	if s.cooccur[b] == nil {
		s.cooccur[b] = make(map[string]int)
	}
	s.cooccur[a][b]++
	s.cooccur[b][a]++
}

// SetLabel names a sensor ("front left") or a vehicle. Vehicle keys are
// prefixed so the two namespaces cannot collide.
func (s *Store) SetLabel(kind, id, label string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.labels[kind+":"+id] = label
	if kind == "sensor" {
		if sen := s.sensors[id]; sen != nil {
			sen.Label = label
		}
	}
	s.dirty = true
}

// Dirty reports whether there are unsaved changes.
func (s *Store) Dirty() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dirty
}

// Snapshot returns the current sensors and the vehicles they cluster
// into, newest activity first.
func (s *Store) Snapshot() ([]Sensor, []Vehicle, int64) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	vehicles := s.cluster()
	owner := make(map[string]string, len(s.sensors))
	for _, v := range vehicles {
		for _, k := range v.Sensors {
			owner[k] = v.ID
		}
	}

	out := make([]Sensor, 0, len(s.sensors))
	for k, sen := range s.sensors {
		c := *sen
		c.Vehicle = owner[k]
		c.Label = s.labels["sensor:"+k]
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Last.After(out[j].Last) })
	sort.Slice(vehicles, func(i, j int) bool { return vehicles[i].Last.After(vehicles[j].Last) })
	return out, vehicles, s.total
}

// Recent returns the newest readings, oldest first.
func (s *Store) Recent(n int) []Reading {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if n > len(s.recent) {
		n = len(s.recent)
	}
	out := make([]Reading, n)
	copy(out, s.recent[len(s.recent)-n:])
	return out
}

// cluster groups sensors into vehicles by union-find over the pairs whose
// co-occurrence is both frequent enough and consistent enough. Callers
// must hold at least a read lock.
func (s *Store) cluster() []Vehicle {
	parent := make(map[string]string, len(s.sensors))
	var find func(string) string
	find = func(x string) string {
		if parent[x] == "" || parent[x] == x {
			parent[x] = x
			return x
		}
		parent[x] = find(parent[x])
		return parent[x]
	}
	union := func(a, b string) {
		ra, rb := find(a), find(b)
		if ra == rb {
			return
		}
		// Keep the lexicographically smaller root so vehicle IDs stay
		// stable as new sensors arrive.
		if rb < ra {
			ra, rb = rb, ra
		}
		parent[rb] = ra
	}

	for k := range s.sensors {
		find(k)
	}
	for a, peers := range s.cooccur {
		sa := s.sensors[a]
		if sa == nil {
			continue
		}
		for b, shared := range peers {
			sb := s.sensors[b]
			if sb == nil || shared < MinShared {
				continue
			}
			rarer := min(sa.Count, sb.Count)
			if rarer > 0 && float64(shared)/float64(rarer) >= MinRatio {
				union(a, b)
			}
		}
	}

	groups := make(map[string][]string)
	for k := range s.sensors {
		r := find(k)
		groups[r] = append(groups[r], k)
	}

	out := make([]Vehicle, 0, len(groups))
	for root, members := range groups {
		// A lone sensor is not yet a vehicle; it may be a passing car
		// heard once, or a wheel whose siblings have not been heard.
		if len(members) < 2 {
			continue
		}
		sort.Strings(members)
		v := Vehicle{ID: root, Sensors: members, Label: s.labels["vehicle:"+root]}
		seen := map[string]bool{}
		for _, k := range members {
			sen := s.sensors[k]
			if v.First.IsZero() || sen.First.Before(v.First) {
				v.First = sen.First
			}
			if sen.Last.After(v.Last) {
				v.Last = sen.Last
			}
			if !seen[sen.Model] {
				seen[sen.Model] = true
				v.Models = append(v.Models, sen.Model)
			}
		}
		sort.Strings(v.Models)
		out = append(out, v)
	}
	return out
}
