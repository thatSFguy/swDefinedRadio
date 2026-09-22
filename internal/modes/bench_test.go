package modes

import (
	"math/rand"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/dsp"
)

// benchBlock is one second of IQ at the demodulator's sample rate, so a
// result of 1000 ms/op means the receiver exactly keeps up with the
// radio and anything less is headroom.
func benchBlock() []byte {
	iq := make([]byte, SampleRate*2)
	r := rand.New(rand.NewSource(1))
	r.Read(iq)
	return iq
}

func BenchmarkMagnitude(b *testing.B) {
	iq := benchBlock()
	mag := make([]uint16, len(iq)/2)
	t := dsp.NewMagTable()
	b.SetBytes(int64(len(iq)))
	b.ResetTimer()
	for b.Loop() {
		t.Compute(iq, mag)
	}
}

// BenchmarkPipeline covers the whole real-time path for one second of
// noise: magnitude conversion plus the preamble search across every
// sample offset.
func BenchmarkPipeline(b *testing.B) {
	iq := benchBlock()
	mag := make([]uint16, len(iq)/2)
	t := dsp.NewMagTable()
	now := time.Now()
	b.SetBytes(int64(len(iq)))
	b.ResetTimer()
	for b.Loop() {
		var d Demodulator
		t.Compute(iq, mag)
		d.Process(mag, now, func(Frame) {})
	}
}
