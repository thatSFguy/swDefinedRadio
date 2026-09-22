package uat

import (
	"bytes"
	"math"
	"math/rand/v2"
	"os"
	"testing"
	"time"
)

// deviation is the frequency shift either side of the carrier, from
// DO-282B. The modulator below uses it to build the signal a receiver
// would actually see, so the demodulator is tested against the real
// thing rather than against its own assumptions.
const deviation = 312_500

// modulate turns bits into 8-bit IQ at SampleRate, the way a UAT
// transmitter does: continuous phase, shifted up for a one and down for
// a zero. noise is the amplitude of the noise added on top, 0 for none.
func modulate(bits []bool, noise float64, rng *rand.Rand) []byte {
	step := 2 * math.Pi * deviation / SampleRate
	out := make([]byte, 0, len(bits)*2*2)
	phase := 0.0
	put := func(b bool) {
		if b {
			phase += step
		} else {
			phase -= step
		}
		i := math.Cos(phase)
		q := math.Sin(phase)
		if noise > 0 {
			i += rng.NormFloat64() * noise
			q += rng.NormFloat64() * noise
		}
		out = append(out, sample(i), sample(q))
	}
	for _, b := range bits {
		put(b) // two samples a symbol
		put(b)
	}
	return out
}

func sample(v float64) byte {
	x := math.Round(v*127.5 + 127.5)
	return byte(math.Max(0, math.Min(255, x)))
}

// silence is the noise a receiver hears with nothing transmitting.
func silence(n int, rng *rand.Rand) []byte {
	out := make([]byte, n*2)
	for i := range out {
		out[i] = sample(rng.NormFloat64() * 0.3)
	}
	return out
}

func bitsOf(sync uint64, frame []byte) []bool {
	bits := make([]bool, 0, syncBits+len(frame)*8)
	for i := syncBits - 1; i >= 0; i-- {
		bits = append(bits, sync>>uint(i)&1 == 1)
	}
	for _, b := range frame {
		for i := 7; i >= 0; i-- {
			bits = append(bits, b>>uint(i)&1 == 1)
		}
	}
	return bits
}

// adsbFrame builds a transmittable ADS-B frame around a message.
func adsbFrame(msg []byte) []bool {
	c := adsbShort
	if len(msg) > shortFrame-adsbShort.roots {
		c = adsbLong
	}
	return bitsOf(syncADSB, append(append([]byte{}, msg...), c.parity(msg)...))
}

// uplinkFrameBits builds a ground uplink: six blocks, each coded, then
// interleaved byte by byte.
func uplinkFrameBits(payload []byte) []bool {
	blocks := make([][]byte, uplinkParts)
	for p := range blocks {
		data := payload[p*uplinkData : (p+1)*uplinkData]
		blocks[p] = append(append([]byte{}, data...), uplink.parity(data)...)
	}
	raw := make([]byte, uplinkFrame)
	for i := 0; i < uplinkBlock; i++ {
		for p := 0; p < uplinkParts; p++ {
			raw[i*uplinkParts+p] = blocks[p][i]
		}
	}
	return bitsOf(syncUplink, raw)
}

func collect(t *testing.T, iq []byte) []Frame {
	t.Helper()
	var got []Frame
	var d Demodulator
	d.Process(iq, time.Now(), func(f Frame) { got = append(got, f) })
	// A second, empty call flushes nothing — the point is that it must
	// not panic or invent frames.
	d.Process(nil, time.Now(), func(f Frame) { got = append(got, f) })
	return got
}

func TestReceivesLongADSB(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	msg := make([]byte, longFrame-adsbLong.roots)
	msg[0] = 1 << 3 // message type 1: a long frame
	for i := 1; i < len(msg); i++ {
		msg[i] = byte(i * 3)
	}

	iq := append(silence(4000, rng), modulate(adsbFrame(msg), 0, rng)...)
	iq = append(iq, silence(20000, rng)...)

	got := collect(t, iq)
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1", len(got))
	}
	if got[0].Uplink {
		t.Error("an ADS-B frame was reported as a ground uplink")
	}
	if !bytes.Equal(got[0].Payload, msg) {
		t.Errorf("payload came back wrong:\n got %x\nwant %x", got[0].Payload, msg)
	}
	if got[0].Errors != 0 {
		t.Errorf("a clean frame needed %d corrections", got[0].Errors)
	}
}

func TestReceivesShortADSB(t *testing.T) {
	rng := rand.New(rand.NewPCG(2, 2))
	msg := make([]byte, shortFrame-adsbShort.roots)
	msg[0] = 0 // message type 0 is the only short one
	for i := 1; i < len(msg); i++ {
		msg[i] = byte(200 - i)
	}

	iq := append(silence(3000, rng), modulate(adsbFrame(msg), 0, rng)...)
	iq = append(iq, silence(20000, rng)...)

	got := collect(t, iq)
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1", len(got))
	}
	if !bytes.Equal(got[0].Payload, msg) {
		t.Errorf("payload came back wrong:\n got %x\nwant %x", got[0].Payload, msg)
	}
}

func TestReceivesGroundUplink(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 3))
	payload := make([]byte, uplinkParts*uplinkData)
	for i := range payload {
		payload[i] = byte(i)
	}

	iq := append(silence(2000, rng), modulate(uplinkFrameBits(payload), 0, rng)...)
	iq = append(iq, silence(2000, rng)...)

	got := collect(t, iq)
	if len(got) != 1 {
		t.Fatalf("got %d frames, want 1", len(got))
	}
	if !got[0].Uplink {
		t.Error("a ground uplink was reported as an aircraft message")
	}
	if !bytes.Equal(got[0].Payload, payload) {
		t.Errorf("payload came back wrong (%d bytes vs %d)", len(got[0].Payload), len(payload))
	}
}

// TestSurvivesNoise is what decides the receiver's range: how much
// noise a frame can carry and still come out whole. Integrating across
// each symbol and letting Reed-Solomon repair what is left holds the
// link together well past the point where individual bits are
// unreliable, so the thresholds here are the receiver's sensitivity
// written down.
func TestSurvivesNoise(t *testing.T) {
	msg := make([]byte, longFrame-adsbLong.roots)
	msg[0] = 2 << 3
	for i := 1; i < len(msg); i++ {
		msg[i] = byte(i * 7)
	}

	for _, c := range []struct {
		noise   float64 // noise amplitude per component
		wantPct int     // frames that must still be recovered
	}{
		{0.20, 100}, // 11 dB SNR: comfortable
		{0.30, 100}, // 7.4 dB
		{0.40, 80},  // 4.9 dB: marginal, and still mostly readable
	} {
		heard := 0
		const trials = 40
		for trial := range trials {
			rng := rand.New(rand.NewPCG(uint64(trial), 9))
			iq := append(silence(1500, rng), modulate(adsbFrame(msg), c.noise, rng)...)
			iq = append(iq, silence(2000, rng)...)

			for _, f := range collect(t, iq) {
				if bytes.Equal(f.Payload, msg) {
					heard++
					break
				}
			}
		}
		snr := 10 * math.Log10(1/(2*c.noise*c.noise))
		if got := heard * 100 / trials; got < c.wantPct {
			t.Errorf("noise %.2f (SNR %.1f dB): %d%% of frames recovered, want %d%%",
				c.noise, snr, got, c.wantPct)
		} else {
			t.Logf("noise %.2f (SNR %.1f dB): %d%% recovered", c.noise, snr, got)
		}
	}
}

// TestNoiseAloneIsQuiet: sync can appear in noise, and the check that
// stops it becoming a phantom aircraft is Reed-Solomon. A second of
// static should produce nothing at all.
func TestNoiseAloneIsQuiet(t *testing.T) {
	rng := rand.New(rand.NewPCG(5, 5))
	got := collect(t, silence(SampleRate, rng))
	if len(got) != 0 {
		t.Errorf("%d frames were decoded out of pure noise", len(got))
	}
}

// TestFrameSplitAcrossReads: the radio delivers fixed-size blocks with
// no regard for where a frame starts, so one landing on the boundary
// must still be found.
func TestFrameSplitAcrossReads(t *testing.T) {
	rng := rand.New(rand.NewPCG(6, 6))
	msg := make([]byte, longFrame-adsbLong.roots)
	msg[0] = 3 << 3
	for i := 1; i < len(msg); i++ {
		msg[i] = byte(i + 10)
	}

	iq := append(silence(1000, rng), modulate(adsbFrame(msg), 0, rng)...)
	iq = append(iq, silence(20000, rng)...)

	// Cut in the middle of the frame itself.
	cut := (1000 + longFrame*8) * 2
	var got []Frame
	var d Demodulator
	start := time.Now()
	d.Process(iq[:cut], start, func(f Frame) { got = append(got, f) })
	d.Process(iq[cut:], start.Add(time.Duration(cut/2)*time.Second/SampleRate),
		func(f Frame) { got = append(got, f) })

	if len(got) != 1 {
		t.Fatalf("got %d frames across the split, want 1", len(got))
	}
	if !bytes.Equal(got[0].Payload, msg) {
		t.Error("the payload did not survive the split")
	}
	// The frame began about 1000 samples in, not wherever the second
	// read happened to start.
	want := start.Add(1000 * time.Second / SampleRate)
	if off := got[0].At.Sub(want); off > 2*time.Millisecond || off < -2*time.Millisecond {
		t.Errorf("frame timed %v from where it started", off)
	}
}

func BenchmarkDemodulate(b *testing.B) {
	rng := rand.New(rand.NewPCG(7, 7))
	msg := make([]byte, longFrame-adsbLong.roots)
	msg[0] = 1 << 3
	iq := append(silence(50_000, rng), modulate(adsbFrame(msg), 0.2, rng)...)
	iq = append(iq, silence(50_000, rng)...)

	b.SetBytes(int64(len(iq)))
	b.ResetTimer()
	for range b.N {
		var d Demodulator
		d.Process(iq, time.Now(), func(Frame) {})
	}
}

// TestWriteSampleCapture writes a synthetic capture to the path in
// UATOUT: a few aircraft and a ground station, modulated as the air
// would carry them. It is how the whole receiver — file, demodulator,
// decoder, table, map — can be exercised somewhere the band is quiet.
//
//	UATOUT=/tmp/sample.iq go test ./internal/uat -run TestWriteSampleCapture
//	./bin/uat -file /tmp/sample.iq
func TestWriteSampleCapture(t *testing.T) {
	path := os.Getenv("UATOUT")
	if path == "" {
		t.Skip("set UATOUT to write a synthetic capture")
	}
	rng := rand.New(rand.NewPCG(11, 12))

	// Three aircraft around the receiver, and a ground station.
	type plane struct {
		addr          uint32
		callsign      string
		lat, lon      float64
		alt           int
		ns, ew, vrate int
	}
	planes := []plane{
		{0xa1b2c3, "N123AB", 43.05, -85.80, 4500, 120, 90, 1216},
		{0xa4d5e6, "N9077Q", 43.30, -85.40, 2500, -80, 140, -512},
		{0xa7f8a9, "N54321", 42.90, -85.95, 8500, 200, -60, 0},
	}

	var iq []byte
	iq = append(iq, silence(20_000, rng)...)
	for _, p := range planes {
		msg := encodeSV(p.lat, p.lon, p.alt, false, p.ns, p.ew, p.vrate)
		msg[1], msg[2], msg[3] = byte(p.addr>>16), byte(p.addr>>8), byte(p.addr)
		putCallsign(msg, 1, p.callsign)
		iq = append(iq, modulate(adsbFrame(msg), 0.15, rng)...)
		iq = append(iq, silence(30_000, rng)...)
	}

	// A ground station at Gerald R. Ford, with an empty product set.
	payload := make([]byte, uplinkParts*uplinkData)
	const step = 360.0 / (1 << 24)
	rawLat := uint32(math.Round(40.6892 / step))
	rawLon := uint32(math.Round((-74.0445 + 360) / step))
	payload[0], payload[1] = byte(rawLat>>15), byte(rawLat>>7)
	payload[2] = byte(rawLat<<1)&0xfe | byte(rawLon>>23&1)
	payload[3], payload[4] = byte(rawLon>>15), byte(rawLon>>7)
	payload[5] = byte(rawLon<<1)&0xfe | 1
	payload[6] = 0x80 | 0x20 | 4
	payload[7] = 2 << 4
	iq = append(iq, modulate(uplinkFrameBits(payload), 0.15, rng)...)
	iq = append(iq, silence(40_000, rng)...)

	if err := os.WriteFile(path, iq, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s: %d samples, %.2f s", path, len(iq)/2, float64(len(iq)/2)/SampleRate)
}
