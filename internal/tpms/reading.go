// Package tpms logs tyre-pressure sensor transmissions and groups the
// sensors into vehicles.
//
// Demodulation is left to rtl_433, which carries decoders for around
// twenty-five proprietary TPMS protocols. This package parses its output,
// keeps the history, and works out which sensors belong together.
package tpms

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Reading is one decoded TPMS transmission.
type Reading struct {
	At    time.Time `json:"at"`
	Model string    `json:"model"` // rtl_433 decoder name, a hint at the make
	ID    string    `json:"id"`

	PressureKPa    float64 `json:"pressure_kpa,omitempty"`
	HasPressure    bool    `json:"has_pressure"`
	TemperatureC   float64 `json:"temperature_c,omitempty"`
	HasTemperature bool    `json:"has_temperature"`
	BatteryOK      *bool   `json:"battery_ok,omitempty"`

	// Integrity is rtl_433's check field: CRC, CHECKSUM or PARITY. A
	// decoder that verified a CRC is far less likely to be a false
	// positive than one that only checked parity.
	Integrity string `json:"integrity,omitempty"`
}

// Key identifies a sensor. Two decoders can emit the same bare ID, so
// the model is part of the identity.
func (r Reading) Key() string { return r.Model + "/" + r.ID }

// PSI converts the reading's pressure for display.
func (r Reading) PSI() float64 { return r.PressureKPa / 6.894757 }

// ParseRTL433 converts one JSON line from rtl_433 into a Reading. It
// reports false for lines that are not TPMS reports — rtl_433 also
// decodes weather stations, doorbells and much else.
func ParseRTL433(line []byte) (Reading, bool) {
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		return Reading{}, false
	}

	model := str(m, "model")
	if !strings.EqualFold(str(m, "type"), "TPMS") &&
		!strings.Contains(strings.ToUpper(model), "TPMS") {
		return Reading{}, false
	}

	id, ok := identity(m)
	if !ok {
		return Reading{}, false
	}

	r := Reading{
		At:        readingTime(m),
		Model:     model,
		ID:        id,
		Integrity: str(m, "mic"),
	}

	// Decoders report pressure in whichever unit the protocol carries.
	switch {
	case has(m, "pressure_kPa"):
		r.PressureKPa, r.HasPressure = num(m, "pressure_kPa"), true
	case has(m, "pressure_PSI"):
		r.PressureKPa, r.HasPressure = num(m, "pressure_PSI")*6.894757, true
	case has(m, "pressure_bar"):
		r.PressureKPa, r.HasPressure = num(m, "pressure_bar")*100, true
	}

	switch {
	case has(m, "temperature_C"):
		r.TemperatureC, r.HasTemperature = num(m, "temperature_C"), true
	case has(m, "temperature_F"):
		r.TemperatureC, r.HasTemperature = (num(m, "temperature_F")-32)*5/9, true
	}

	if v, present := m["battery_ok"]; present {
		ok := num(map[string]any{"v": v}, "v") != 0
		r.BatteryOK = &ok
	}
	return r, true
}

// identity extracts the sensor ID, which decoders emit as either a JSON
// string or a number depending on the protocol.
func identity(m map[string]any) (string, bool) {
	v, present := m["id"]
	if !present {
		return "", false
	}
	switch t := v.(type) {
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s, s != ""
	case float64:
		if t != math.Trunc(t) {
			return strconv.FormatFloat(t, 'f', -1, 64), true
		}
		return strconv.FormatInt(int64(t), 10), true
	default:
		return fmt.Sprint(t), true
	}
}

// readingTime prefers rtl_433's own timestamp but falls back to now,
// since the field's format depends on how rtl_433 was invoked.
func readingTime(m map[string]any) time.Time {
	s := str(m, "time")
	// rtl_433's -M time:iso emits a local time with no zone, and adding
	// :usec appends microseconds; the plain -M default uses a space.
	for _, layout := range []string{
		time.RFC3339Nano, time.RFC3339,
		"2006-01-02T15:04:05.999999",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Now()
}

func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func has(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

func num(m map[string]any, k string) float64 {
	switch t := m[k].(type) {
	case float64:
		return t
	case bool:
		if t {
			return 1
		}
	case string:
		if f, err := strconv.ParseFloat(t, 64); err == nil {
			return f
		}
	}
	return 0
}
