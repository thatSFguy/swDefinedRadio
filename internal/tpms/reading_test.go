package tpms

import (
	"math"
	"testing"
)

// Lines in the shape rtl_433 actually emits, including the spacing it
// puts around colons.
const (
	lineSchrader = `{"time" : "2026-09-10T13:15:22", "model" : "Schrader", "type" : "TPMS", "flags" : "03", "id" : "1A2B3C4", "pressure_kPa" : 220.000, "temperature_C" : 24.000, "mic" : "CRC"}`
	lineFordPSI  = `{"time" : "2026-09-10T13:15:24", "model" : "Ford", "type" : "TPMS", "id" : 12345678, "pressure_PSI" : 32.500, "temperature_F" : 75.000}`
	lineBar      = `{"time" : "2026-09-10T13:15:25", "model" : "Citroen", "type" : "TPMS", "id" : "ab12", "pressure_bar" : 2.2, "battery_ok" : 0}`
	lineWeather  = `{"time" : "2026-09-10T13:15:26", "model" : "Acurite-Tower", "id" : 1234, "temperature_C" : 21.5, "humidity" : 55}`
	lineNoID     = `{"time" : "2026-09-10T13:15:27", "model" : "Mystery", "type" : "TPMS", "pressure_kPa" : 200}`
)

func TestParseKPa(t *testing.T) {
	r, ok := ParseRTL433([]byte(lineSchrader))
	if !ok {
		t.Fatal("Schrader line not parsed as TPMS")
	}
	if r.Model != "Schrader" || r.ID != "1a2b3c4" {
		t.Errorf("model/id = %q/%q, want Schrader/1a2b3c4 (id lowercased)", r.Model, r.ID)
	}
	if !r.HasPressure || r.PressureKPa != 220 {
		t.Errorf("pressure = %v kPa, want 220", r.PressureKPa)
	}
	if got := r.PSI(); math.Abs(got-31.9) > 0.1 {
		t.Errorf("PSI = %.2f, want about 31.9", got)
	}
	if !r.HasTemperature || r.TemperatureC != 24 {
		t.Errorf("temperature = %v C, want 24", r.TemperatureC)
	}
	if r.Integrity != "CRC" {
		t.Errorf("integrity = %q, want CRC", r.Integrity)
	}
	if r.Key() != "Schrader/1a2b3c4" {
		t.Errorf("key = %q", r.Key())
	}
}

// TestParseImperial covers the decoders that report PSI and Fahrenheit;
// both must be normalised so sensors stay comparable.
func TestParseImperial(t *testing.T) {
	r, ok := ParseRTL433([]byte(lineFordPSI))
	if !ok {
		t.Fatal("Ford line not parsed")
	}
	if r.ID != "12345678" {
		t.Errorf("numeric id = %q, want 12345678", r.ID)
	}
	if math.Abs(r.PressureKPa-224.08) > 0.1 {
		t.Errorf("pressure = %.2f kPa, want about 224.08", r.PressureKPa)
	}
	if math.Abs(r.TemperatureC-23.89) > 0.1 {
		t.Errorf("temperature = %.2f C, want about 23.89", r.TemperatureC)
	}
}

func TestParseBarAndBattery(t *testing.T) {
	r, ok := ParseRTL433([]byte(lineBar))
	if !ok {
		t.Fatal("Citroen line not parsed")
	}
	if math.Abs(r.PressureKPa-220) > 1e-6 {
		t.Errorf("pressure = %v kPa, want 220 (2.2 bar)", r.PressureKPa)
	}
	if r.BatteryOK == nil || *r.BatteryOK {
		t.Error("battery_ok 0 should decode as a low battery")
	}
}

func TestRejectsNonTPMS(t *testing.T) {
	if _, ok := ParseRTL433([]byte(lineWeather)); ok {
		t.Error("a weather station was accepted as TPMS")
	}
	if _, ok := ParseRTL433([]byte(lineNoID)); ok {
		t.Error("a TPMS line with no sensor id was accepted")
	}
	if _, ok := ParseRTL433([]byte("not json")); ok {
		t.Error("garbage was accepted")
	}
}

func TestParseKeepsTimestamp(t *testing.T) {
	r, _ := ParseRTL433([]byte(lineSchrader))
	if r.At.Year() != 2026 || r.At.Minute() != 15 {
		t.Errorf("timestamp = %v, want rtl_433's own 2026-09-10T13:15:22", r.At)
	}
}
