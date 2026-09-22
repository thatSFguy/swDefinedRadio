package alert

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"
)

// Builtin is the ruleset a receiver starts with. It is deliberately
// short. Every entry here is something publicly documented and hard to
// get wrong; guessing at address blocks produces alerts that are worse
// than no alerts, because they teach you to ignore the thing.
//
// Add your own in the alerts file rather than editing this: see
// LoadOrBuiltin.
func Builtin() []Rule {
	return []Rule{{
		Name:   "emergency squawk",
		Note:   "7500 unlawful interference, 7600 radio failure, 7700 general emergency",
		Squawk: []string{"7500", "7600", "7700"},
		Urgent: true,
	}, {
		Name:      "declared emergency",
		Note:      "the crew set an emergency state in an aircraft status squitter",
		Emergency: true,
		Urgent:    true,
	}, {
		// The United States military block. This one is unambiguous and
		// large, and it is most of what makes a scanner interesting away
		// from an airport.
		Name: "US military",
		Note: "ICAO address in the US military block",
		Hex:  []string{"ae0000-afffff"},
	}, {
		Name: "UK military",
		Note: "ICAO address in the UK military block",
		Hex:  []string{"43c000-43cfff"},
	}, {
		// Callsigns catch the aircraft whose addresses are civil: a
		// chartered airlifter, a government VIP flight, a medical
		// helicopter. These prefixes are published and stable.
		Name:     "military callsign",
		Note:     "air mobility, tanker and support callsigns",
		Callsign: []string{"RCH*", "CNV*", "SPAR*", "DOOM*", "GRIM*", "JAKE*", "POLO*"},
	}, {
		Name:     "government VIP",
		Note:     "special air mission and executive flights",
		Callsign: []string{"SAM*", "VENUS*", "EXEC1*", "AF1*"},
		Urgent:   true,
	}, {
		Name:     "air ambulance",
		Note:     "medical flights, which are usually local and low",
		Callsign: []string{"LIFE*", "MEDEVAC*", "MERCY*", "ANGEL*"},
	}, {
		// Airliners cruise in the thirties and low forties. Above this
		// is business jets at the top of their range, weather and
		// research aircraft, and the occasional thing with no callsign.
		Name:    "very high",
		Note:    "above 50,000 ft",
		AboveFt: 50_000,
	}}
}

// File is the on-disk ruleset: rules of your own, and whether to keep
// the built-in ones alongside them.
type File struct {
	// Builtin includes the built-in rules. It defaults to true, so a
	// file that only adds a watchlist keeps the military and emergency
	// rules working.
	Builtin *bool `json:"builtin,omitempty"`

	// Cooldown is how long one aircraft is left alone after matching a
	// rule, as a Go duration such as "30m".
	Cooldown string `json:"cooldown,omitempty"`

	Rules []Rule `json:"rules"`
}

// LoadOrBuiltin reads a rules file. A missing file is not an error —
// it means the built-in rules, which is the sensible default — so the
// second result says whether a file was actually read.
func LoadOrBuiltin(path string) (rules []Rule, cooldown time.Duration, found bool, err error) {
	if path == "" {
		return Builtin(), DefaultCooldown, false, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Builtin(), DefaultCooldown, false, nil
	}
	if err != nil {
		return Builtin(), DefaultCooldown, false, err
	}

	var f File
	if err := json.Unmarshal(b, &f); err != nil {
		// A broken rules file must not take the receiver down with it,
		// but it must be loud: silently watching for nothing is worse
		// than not watching at all.
		return Builtin(), DefaultCooldown, false, fmt.Errorf("%s: %w", path, err)
	}

	cooldown = DefaultCooldown
	if f.Cooldown != "" {
		d, err := time.ParseDuration(f.Cooldown)
		if err != nil {
			return Builtin(), DefaultCooldown, false, fmt.Errorf("%s: cooldown %q: %w", path, f.Cooldown, err)
		}
		cooldown = d
	}

	rules = f.Rules
	if f.Builtin == nil || *f.Builtin {
		rules = append(Builtin(), rules...)
	}
	for i, r := range rules {
		if r.Name == "" {
			return Builtin(), DefaultCooldown, false, fmt.Errorf("%s: rule %d has no name", path, i+1)
		}
		if r.empty() {
			return Builtin(), DefaultCooldown, false,
				fmt.Errorf("%s: rule %q has no conditions, so it would match every aircraft", path, r.Name)
		}
	}
	return rules, cooldown, true, nil
}

// Example is a starter rules file, written out by -alerts-example. It
// shows the shape of each condition rather than trying to be useful.
const Example = `{
  "builtin": true,
  "cooldown": "30m",
  "rules": [
    {
      "name": "the neighbour's Cessna",
      "note": "N12345, seen most weekends",
      "hex": ["a1b2c3"]
    },
    {
      "name": "police helicopter",
      "callsign": ["N911*", "POLICE*"],
      "urgent": true
    },
    {
      "name": "low and close",
      "note": "anything below 3,000 ft within 10 NM — likely overhead",
      "below_ft": 3000,
      "within_nm": 10
    },
    {
      "name": "a whole country's block",
      "note": "hex takes exact values, ae* prefixes, or ranges",
      "hex": ["4b0000-4bffff"]
    }
  ]
}
`
