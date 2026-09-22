package modes

import (
	"math"
	"time"
)

const (
	// SampleRate is the only rate this demodulator accepts. At 2 MHz a
	// 1 µs Mode S bit is exactly two samples, so slicing a bit is a
	// single comparison of its two halves.
	SampleRate = 2_000_000

	// preambleLen covers the 8 µs preamble, whose four pulses fall at
	// 0, 1.0, 3.5 and 4.5 µs — samples 0, 2, 7 and 9.
	preambleLen = 16

	shortBits = 56
	longBits  = 112
	longFrame = preambleLen + longBits*2
)

// Frame is one Mode S transmission that passed its CRC.
type Frame struct {
	Bytes  []byte
	Signal float64 // preamble power in dBFS, a crude RSSI
	At     time.Time
}

// DF returns the downlink format, the first five bits of the frame.
func (f Frame) DF() byte { return f.Bytes[0] >> 3 }

// Demodulator turns a magnitude stream into frames. Feed it successive
// blocks; it retains the tail of each so a frame straddling a block
// boundary is still recovered.
type Demodulator struct {
	buf []uint16
}

// Process scans one block of magnitudes, calling emit for every frame
// found. at is the wall-clock time of the block's first sample.
func (d *Demodulator) Process(mag []uint16, at time.Time, emit func(Frame)) {
	carried := len(d.buf)
	d.buf = append(d.buf, mag...)

	// Only offsets with a whole frame ahead of them can be examined.
	for j := 0; j+longFrame <= len(d.buf); j++ {
		if !preambleAt(d.buf[j:]) {
			continue
		}
		f, n, ok := decodeAt(d.buf[j:])
		if !ok {
			continue
		}
		// Date the frame from its position in the block, ignoring the
		// samples carried over from last time.
		f.At = at.Add(time.Duration(j-carried) * time.Second / SampleRate)
		emit(f)
		j += n - 1 // the loop's j++ takes us past the frame
	}

	// Carry the tail so the next block can complete a frame that started
	// near the end of this one.
	keep := longFrame - 1
	if len(d.buf) > keep {
		d.buf = d.buf[:copy(d.buf, d.buf[len(d.buf)-keep:])]
	}
}

// preambleAt reports whether m opens with a Mode S preamble: pulses in
// slots 0, 2, 7 and 9, quiet in the slots between them.
func preambleAt(m []uint16) bool {
	if !(m[0] > m[1] && m[1] < m[2] && m[2] > m[3] && m[3] < m[0] &&
		m[4] < m[0] && m[5] < m[0] && m[6] < m[0] &&
		m[7] > m[8] && m[8] < m[9] && m[9] > m[6]) {
		return false
	}
	// The four pulses must stand clear of the quiet slots around them.
	// Dividing their sum by six rather than four sets the floor at two
	// thirds of the mean pulse height, which is dump1090's heuristic.
	high := (uint32(m[0]) + uint32(m[2]) + uint32(m[7]) + uint32(m[9])) / 6
	if uint32(m[4]) >= high || uint32(m[5]) >= high {
		return false
	}
	// Slots 11-14 are the quiet run before the first data bit.
	return !(uint32(m[11]) >= high || uint32(m[12]) >= high ||
		uint32(m[13]) >= high || uint32(m[14]) >= high)
}

// decodeAt slices the bits following a preamble at the head of m and
// returns the frame if its CRC checks out, along with its length in
// samples.
func decodeAt(m []uint16) (Frame, int, bool) {
	// Slice the full long-frame's worth; the downlink format in the
	// first five bits then tells us how many were really transmitted.
	var msg [longBits / 8]byte
	for i := range longBits {
		// Pulse-position modulation: energy in the first half of the
		// bit period means 1, in the second half means 0.
		if m[preambleLen+i*2] > m[preambleLen+i*2+1] {
			msg[i/8] |= 1 << (7 - uint(i%8))
		}
	}

	n := msgLenBits(msg[0] >> 3)
	if n == 0 {
		return Frame{}, 0, false
	}
	b := msg[:n/8]
	if CRC(b) != Parity(b) {
		return Frame{}, 0, false
	}

	out := make([]byte, len(b))
	copy(out, b)
	return Frame{Bytes: out, Signal: signalDBFS(m)}, preambleLen + n*2, true
}

// signalDBFS estimates received power from the four preamble pulses.
func signalDBFS(m []uint16) float64 {
	var sum float64
	for _, i := range [4]int{0, 2, 7, 9} {
		v := float64(m[i]) / 65535
		sum += v * v
	}
	p := sum / 4
	if p <= 0 {
		return -100
	}
	return 10 * math.Log10(p)
}

// msgLenBits gives the transmitted length for a downlink format, or 0
// for formats this receiver skips.
//
// Only DF11, DF17 and DF18 are accepted. The other formats overlay the
// aircraft address on their parity, so their CRC cannot be verified
// without already knowing who sent them — checking those against the
// roster of addresses we have seen is a natural later addition.
func msgLenBits(df byte) int {
	switch df {
	case 11:
		return shortBits
	case 17, 18:
		return longBits
	}
	return 0
}
