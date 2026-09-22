package track

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/modes"
)

// pairWindow is how long an even and odd report may be apart and still
// be combined. ICAO allows ten seconds; a fast aircraft moves far enough
// in longer than that to land in the wrong zone.
const pairWindow = 10 * time.Second

// Aircraft is the accumulated state for one ICAO address.
type Aircraft struct {
	ICAO     uint32 `json:"-"`
	Hex      string `json:"hex"`
	Callsign string `json:"callsign,omitempty"`

	HasPosition bool    `json:"has_position"`
	Lat         float64 `json:"lat,omitempty"`
	Lon         float64 `json:"lon,omitempty"`
	DistanceNM  float64 `json:"distance_nm,omitempty"`

	HasAltitude  bool    `json:"has_altitude"`
	Altitude     int     `json:"altitude,omitempty"`
	GroundSpeed  float64 `json:"speed,omitempty"`
	Track        float64 `json:"track,omitempty"`
	HasTrack     bool    `json:"has_track"`
	VerticalRate int     `json:"vertical_rate,omitempty"`
	OnGround     bool    `json:"on_ground"`

	// Squawk and Emergency come from an aircraft status squitter, which
	// is rare: most aircraft never send one, and an empty Emergency
	// means "not declared" rather than "all well".
	Squawk    string `json:"squawk,omitempty"`
	Emergency string `json:"emergency,omitempty"`

	Messages  int       `json:"messages"`
	Signal    float64   `json:"signal_dbfs"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`

	even, odd CPR
}

// Stats summarises receiver health.
type Stats struct {
	Frames    int64 `json:"frames"`    // CRC-valid frames
	Positions int64 `json:"positions"` // successful position fixes
	Tracked   int   `json:"tracked"`   // aircraft currently in the table
	WithPos   int   `json:"with_position"`
}

// Tracker maintains the live aircraft table. It is safe for concurrent
// use: the receive loop calls Update while HTTP handlers call Snapshot.
type Tracker struct {
	mu     sync.RWMutex
	craft  map[uint32]*Aircraft
	frames int64
	fixes  int64

	ttl time.Duration

	// A reference position lets a single frame be placed by local CPR,
	// which gives a fix sooner than waiting for an even/odd pair, and
	// gives every aircraft a range from the antenna.
	refLat, refLon float64
	hasRef         bool
}

// New returns a tracker that forgets aircraft unheard for ttl.
func New(ttl time.Duration) *Tracker {
	return &Tracker{craft: make(map[uint32]*Aircraft), ttl: ttl}
}

// SetReference records the receiver's own position.
func (t *Tracker) SetReference(lat, lon float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.refLat, t.refLon, t.hasRef = lat, lon, true
}

// Reference returns the receiver's own position and whether one is set.
func (t *Tracker) Reference() (lat, lon float64, ok bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.refLat, t.refLon, t.hasRef
}

// CountFrame records a CRC-valid frame that carried nothing we decode,
// so the frame rate reflects real receiver performance.
func (t *Tracker) CountFrame() {
	t.mu.Lock()
	t.frames++
	t.mu.Unlock()
}

// Report is a decoded message from a link that gives position outright,
// such as UAT on 978 MHz. Mode S needs two frames and a projection to
// get there; UAT sends latitude and longitude in every message, so
// there is nothing to resolve and the report goes straight in.
type Report struct {
	ICAO     uint32
	Callsign string

	HasPosition bool
	Lat, Lon    float64

	HasAltitude bool
	Altitude    int

	HasVelocity  bool
	GroundSpeed  float64
	Track        float64
	HasTrack     bool
	VerticalRate int

	OnGround bool
}

// UpdateReport folds an already-positioned report into the table.
func (t *Tracker) UpdateReport(r Report, signal float64, at time.Time) Aircraft {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.frames++
	ac := t.seen(r.ICAO, signal, at)
	if r.Callsign != "" {
		ac.Callsign = r.Callsign
	}
	if r.HasAltitude {
		ac.Altitude, ac.HasAltitude = r.Altitude, true
	}
	if r.HasVelocity {
		ac.GroundSpeed = r.GroundSpeed
		ac.VerticalRate = r.VerticalRate
		if r.HasTrack {
			ac.Track, ac.HasTrack = r.Track, true
		}
	}
	ac.OnGround = r.OnGround
	if r.HasPosition {
		t.setFix(ac, r.Lat, r.Lon)
	}
	return *ac
}

// seen finds or creates an aircraft and records that a message arrived.
func (t *Tracker) seen(icao uint32, signal float64, at time.Time) *Aircraft {
	ac := t.craft[icao]
	if ac == nil {
		ac = &Aircraft{ICAO: icao, Hex: fmt.Sprintf("%06x", icao), FirstSeen: at}
		t.craft[icao] = ac
	}
	ac.LastSeen = at
	ac.Messages++
	ac.Signal = signal
	return ac
}

// Update folds one decoded squitter into the table and returns the
// aircraft's state afterwards, which is what a caller watching for
// something in particular needs to test.
func (t *Tracker) Update(a modes.ADSB, signal float64, at time.Time) Aircraft {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.frames++
	ac := t.seen(a.ICAO, signal, at)

	if a.Callsign != "" {
		ac.Callsign = a.Callsign
	}
	if a.HasAltitude {
		ac.Altitude, ac.HasAltitude = a.Altitude, true
	}
	if a.HasVelocity {
		ac.GroundSpeed = a.GroundSpeed
		ac.Track, ac.HasTrack = a.Track, true
		ac.VerticalRate = a.VerticalRate
	}
	if a.HasPosition {
		ac.OnGround = a.OnGround
		t.position(ac, a, at)
	}
	if a.HasStatus {
		ac.Squawk = a.Squawk
		// An emergency stays on the record for as long as the aircraft
		// is tracked: it is cleared only by the crew saying so.
		if a.Emergency != modes.NoEmergency {
			ac.Emergency = a.Emergency.String()
		} else {
			ac.Emergency = ""
		}
	}
	return *ac
}

// position stores the new report and tries to resolve a fix from it.
func (t *Tracker) position(ac *Aircraft, a modes.ADSB, at time.Time) {
	c := CPR{Lat: a.LatCPR, Lon: a.LonCPR, Odd: a.Odd, Surface: a.OnGround, At: at}
	if a.Odd {
		ac.odd = c
	} else {
		ac.even = c
	}

	// Prefer a global fix from a fresh even/odd pair: it needs no prior
	// knowledge and cannot be thrown off by a distant reference.
	if !a.OnGround && !ac.even.At.IsZero() && !ac.odd.At.IsZero() {
		if gap := ac.even.At.Sub(ac.odd.At); gap < pairWindow && gap > -pairWindow {
			if lat, lon, ok := GlobalAirborne(ac.even, ac.odd); ok {
				t.setFix(ac, lat, lon)
				return
			}
		}
	}

	// Otherwise fall back to a local fix, which needs only this frame
	// but assumes the aircraft is near the receiver.
	if t.hasRef {
		lat, lon := Local(t.refLat, t.refLon, c)
		t.setFix(ac, lat, lon)
	}
}

func (t *Tracker) setFix(ac *Aircraft, lat, lon float64) {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return
	}
	ac.Lat, ac.Lon, ac.HasPosition = lat, lon, true
	if t.hasRef {
		ac.DistanceNM = DistanceNM(t.refLat, t.refLon, lat, lon)
	}
	t.fixes++
}

// Expire drops aircraft unheard for longer than the tracker's TTL and
// returns how many were removed.
func (t *Tracker) Expire(now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for k, ac := range t.craft {
		if now.Sub(ac.LastSeen) > t.ttl {
			delete(t.craft, k)
			n++
		}
	}
	return n
}

// Snapshot returns a copy of the table, most recently heard first.
func (t *Tracker) Snapshot() ([]Aircraft, Stats) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	out := make([]Aircraft, 0, len(t.craft))
	withPos := 0
	for _, ac := range t.craft {
		if ac.HasPosition {
			withPos++
		}
		out = append(out, *ac)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastSeen.After(out[j].LastSeen)
	})
	return out, Stats{
		Frames:    t.frames,
		Positions: t.fixes,
		Tracked:   len(t.craft),
		WithPos:   withPos,
	}
}
