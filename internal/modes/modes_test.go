package modes

import (
	"encoding/hex"
	"math"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/dsp"
)

// Reference frames captured from real traffic; the expected values are
// the ones ICAO Doc 9871 and pyModeS give for them.
const (
	evenPos  = "8D40621D58C382D690C8AC2863A7"
	oddPos   = "8D40621D58C386435CC412692AD6"
	identMsg = "8D4840D6202CC371C32CE0576098"
	velMsg   = "8D485020994409940838175B284F"
)

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad test vector %q: %v", s, err)
	}
	return b
}

func TestCRCMatchesParity(t *testing.T) {
	for _, s := range []string{evenPos, oddPos, identMsg, velMsg} {
		msg := mustHex(t, s)
		if got, want := CRC(msg), Parity(msg); got != want {
			t.Errorf("%s: CRC = %06x, parity = %06x", s, got, want)
		}
	}
}

func TestCRCRejectsCorruption(t *testing.T) {
	msg := mustHex(t, evenPos)
	msg[5] ^= 0x08 // flip one bit in the payload
	if CRC(msg) == Parity(msg) {
		t.Error("corrupted frame still passes CRC")
	}
}

func TestDecodeIdentification(t *testing.T) {
	a, ok := DecodeADSB(mustHex(t, identMsg))
	if !ok {
		t.Fatal("identification squitter not decoded")
	}
	if a.ICAO != 0x4840D6 {
		t.Errorf("ICAO = %06x, want 4840d6", a.ICAO)
	}
	if a.Callsign != "KLM1023" {
		t.Errorf("callsign = %q, want %q", a.Callsign, "KLM1023")
	}
}

func TestDecodeAltitude(t *testing.T) {
	a, ok := DecodeADSB(mustHex(t, evenPos))
	if !ok || !a.HasAltitude {
		t.Fatal("no altitude decoded from airborne position")
	}
	if a.Altitude != 38000 {
		t.Errorf("altitude = %d ft, want 38000", a.Altitude)
	}
}

func TestDecodeVelocity(t *testing.T) {
	a, ok := DecodeADSB(mustHex(t, velMsg))
	if !ok || !a.HasVelocity {
		t.Fatal("no velocity decoded")
	}
	if got := math.Round(a.GroundSpeed); got != 159 {
		t.Errorf("ground speed = %v kt, want 159", got)
	}
	if got := math.Round(a.Track*100) / 100; math.Abs(got-182.88) > 0.01 {
		t.Errorf("track = %v deg, want 182.88", got)
	}
	if a.VerticalRate != -832 {
		t.Errorf("vertical rate = %d ft/min, want -832", a.VerticalRate)
	}
}

func TestDecodeCPRFields(t *testing.T) {
	e, _ := DecodeADSB(mustHex(t, evenPos))
	o, _ := DecodeADSB(mustHex(t, oddPos))
	if e.Odd {
		t.Error("even frame reported as odd")
	}
	if !o.Odd {
		t.Error("odd frame reported as even")
	}
	if e.LatCPR != 93000 || e.LonCPR != 51372 {
		t.Errorf("even CPR = (%d, %d), want (93000, 51372)", e.LatCPR, e.LonCPR)
	}
	if o.LatCPR != 74158 || o.LonCPR != 50194 {
		t.Errorf("odd CPR = (%d, %d), want (74158, 50194)", o.LatCPR, o.LonCPR)
	}
}

// synth builds the 2 Msps IQ a receiver would see for one frame: an 8 µs
// preamble with pulses in slots 0, 2, 7 and 9, then two samples per bit
// with the energy in the first half for a 1 and the second for a 0.
func synth(msg []byte, lead int) []byte {
	const hi, lo = 230, 128
	slots := make([]byte, lead+preambleLen+len(msg)*8*2+lead)
	for i := range slots {
		slots[i] = lo
	}
	for _, p := range []int{0, 2, 7, 9} {
		slots[lead+p] = hi
	}
	for i := range len(msg) * 8 {
		bit := msg[i/8]>>(7-uint(i%8))&1 == 1
		off := lead + preambleLen + i*2
		if bit {
			slots[off] = hi
		} else {
			slots[off+1] = hi
		}
	}

	iq := make([]byte, len(slots)*2)
	for i, v := range slots {
		iq[2*i], iq[2*i+1] = v, 127 // put all energy on I, Q at the DC centre
	}
	return iq
}

// TestDemodulateRoundTrip runs a synthesised frame through the whole
// chain — magnitude, preamble search, bit slicing, CRC — and checks the
// original bytes come back.
func TestDemodulateRoundTrip(t *testing.T) {
	want := mustHex(t, evenPos)
	iq := synth(want, 400)

	mag := make([]uint16, len(iq)/2)
	dsp.NewMagTable().Compute(iq, mag)

	var d Demodulator
	var got []Frame
	d.Process(mag, time.Now(), func(f Frame) { got = append(got, f) })

	if len(got) != 1 {
		t.Fatalf("recovered %d frames, want 1", len(got))
	}
	if hex.EncodeToString(got[0].Bytes) != hex.EncodeToString(want) {
		t.Errorf("recovered %x, want %x", got[0].Bytes, want)
	}
	if got[0].DF() != 17 {
		t.Errorf("DF = %d, want 17", got[0].DF())
	}
}

// TestDemodulateAcrossBlocks checks the carry-over buffer: a frame split
// between two reads must still be recovered.
func TestDemodulateAcrossBlocks(t *testing.T) {
	want := mustHex(t, oddPos)
	iq := synth(want, 200)
	mag := make([]uint16, len(iq)/2)
	dsp.NewMagTable().Compute(iq, mag)

	// Cut partway through the frame's data bits.
	split := 200 + preambleLen + 60

	var d Demodulator
	var got []Frame
	now := time.Now()
	d.Process(mag[:split], now, func(f Frame) { got = append(got, f) })
	d.Process(mag[split:], now, func(f Frame) { got = append(got, f) })

	if len(got) != 1 {
		t.Fatalf("recovered %d frames across the split, want 1", len(got))
	}
	if hex.EncodeToString(got[0].Bytes) != hex.EncodeToString(want) {
		t.Errorf("recovered %x, want %x", got[0].Bytes, want)
	}
}

func TestNoiseProducesNoFrames(t *testing.T) {
	// A deterministic pseudo-random block should not yield frames: the
	// CRC makes a false positive a roughly 1-in-16-million event.
	iq := make([]byte, 400_000)
	x := uint32(12345)
	for i := range iq {
		x = x*1664525 + 1013904223
		iq[i] = byte(x >> 24)
	}
	mag := make([]uint16, len(iq)/2)
	dsp.NewMagTable().Compute(iq, mag)

	var d Demodulator
	n := 0
	d.Process(mag, time.Now(), func(Frame) { n++ })
	if n != 0 {
		t.Errorf("noise produced %d frames, want 0", n)
	}
}
