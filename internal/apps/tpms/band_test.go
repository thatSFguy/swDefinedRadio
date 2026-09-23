package tpms

import (
	"slices"
	"testing"
)

func testApp(t *testing.T, freqs ...string) *App {
	t.Helper()
	a, err := New(Config{Freqs: freqs, HopSeconds: 30, Dir: t.TempDir(), Quiet: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

// Hopping is the default, because without knowing what you are waiting
// for, hearing half of each band beats hearing none of one.
func TestStartsHopping(t *testing.T) {
	a := testApp(t, "315M", "433.92M")
	if a.Band() != Hop {
		t.Errorf("band = %q, want %q", a.Band(), Hop)
	}
	cfg, _ := a.sourceFor()
	if !slices.Equal(cfg.Freqs, []string{"315M", "433.92M"}) {
		t.Errorf("hopping over %v, want both", cfg.Freqs)
	}
}

// Parking narrows rtl_433 to the one frequency, which is the whole point.
func TestParkingOnOneBand(t *testing.T) {
	a := testApp(t, "315M", "433.92M")
	if err := a.SetBand("315M"); err != nil {
		t.Fatalf("SetBand: %v", err)
	}
	if a.Band() != "315M" {
		t.Errorf("band = %q", a.Band())
	}
	cfg, _ := a.sourceFor()
	if !slices.Equal(cfg.Freqs, []string{"315M"}) {
		t.Errorf("listening on %v, want only 315M", cfg.Freqs)
	}
	// and the hop argument goes with it, since there is nowhere to hop
	if slices.Contains(cfg.Args(), "-H") {
		t.Errorf("parked but still told to hop: %v", cfg.Args())
	}
}

func TestGoingBackToHopping(t *testing.T) {
	a := testApp(t, "315M", "433.92M")
	if err := a.SetBand("433.92M"); err != nil {
		t.Fatalf("SetBand: %v", err)
	}
	if err := a.SetBand(Hop); err != nil {
		t.Fatalf("back to hop: %v", err)
	}
	cfg, _ := a.sourceFor()
	if len(cfg.Freqs) != 2 {
		t.Errorf("hopping over %v, want both again", cfg.Freqs)
	}
}

// A frequency the receiver was never given is refused rather than
// silently restarting rtl_433 on something it cannot hear.
func TestRefusesAnUnconfiguredBand(t *testing.T) {
	a := testApp(t, "315M", "433.92M")
	if err := a.SetBand("868M"); err == nil {
		t.Fatal("an unconfigured frequency was accepted")
	}
	if a.Band() != Hop {
		t.Errorf("band changed to %q despite being refused", a.Band())
	}
}

// Changing the band has to bump the version, or the run loop cannot tell
// a deliberate restart from rtl_433 falling over.
func TestChangingBandMarksARestart(t *testing.T) {
	a := testApp(t, "315M", "433.92M")
	_, before := a.sourceFor()
	if err := a.SetBand("315M"); err != nil {
		t.Fatalf("SetBand: %v", err)
	}
	if _, after := a.sourceFor(); after == before {
		t.Error("the version did not change, so the restart looks like a crash")
	}
	// setting the same band again changes nothing
	if err := a.SetBand("315M"); err != nil {
		t.Fatalf("SetBand: %v", err)
	}
	_, same := a.sourceFor()
	if _, again := a.sourceFor(); again != same {
		t.Error("setting the band it is already on restarted it")
	}
}
