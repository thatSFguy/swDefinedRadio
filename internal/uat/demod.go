// Package uat receives UAT, the 978 MHz datalink used in the United
// States. Light aircraft carry it instead of 1090 MHz ADS-B, and
// ground stations use it to broadcast weather and traffic, so a 978
// receiver sees a layer of activity that a 1090 one never does.
//
// The link is nothing like Mode S: continuous-phase FSK at 1.041667
// Mbps rather than pulse-position modulation at 1 Mbps, framed by a
// 36-bit sync word, and every frame is Reed-Solomon coded.
package uat

import (
	"math"
	"time"
)

// SymbolRate is the UAT bit rate. SampleRate is two samples a symbol,
// which is the least that can recover the clock and keeps the USB
// bandwidth the same as the 1090 MHz receiver's.
const (
	SymbolRate = 1_041_667
	SampleRate = 2 * SymbolRate
)

// The 36-bit sync words. They are bitwise complements of each other,
// which is worth knowing: if the demodulator's idea of which way is up
// were inverted, every ADS-B frame would arrive looking like a ground
// uplink.
const (
	syncADSB   uint64 = 0xEACDDA4E2
	syncUplink uint64 = 0x153225B1D
	syncBits          = 36
	syncMask   uint64 = 1<<syncBits - 1
)

// syncTolerance is how many of the 36 sync bits may be wrong. Four
// leaves the sync findable on a weak signal while still being rare
// enough in noise to cost only a few wasted decodes a second.
const syncTolerance = 4

// Frame lengths in bytes, before Reed-Solomon is stripped.
const (
	shortFrame  = 30  // RS(30,18): 18 bytes of message
	longFrame   = 48  // RS(48,34): 34 bytes of message
	uplinkFrame = 552 // six interleaved RS(92,72) blocks: 432 bytes
	uplinkParts = 6
	uplinkBlock = 92
	uplinkData  = 72
)

// Frame is one message recovered from the air.
type Frame struct {
	At      time.Time
	Payload []byte // message bytes, parity removed and errors repaired
	Uplink  bool   // true for a ground station broadcast
	Errors  int    // symbols Reed-Solomon had to repair
	Signal  float64
}

// Demodulator recovers frames from a stream of 8-bit IQ samples. It
// keeps enough state between calls that a frame split across two reads
// is still found.
type Demodulator struct {
	// The phase advance between consecutive samples, whose sign is the
	// bit the transmitter sent. Kept as a value rather than a bit
	// because a symbol spans two samples and the two are added: the
	// total phase change across a symbol is the whole of the evidence
	// for it, and using half of it throws away 3 dB of sensitivity.
	//
	// At two samples a symbol one of the two interleaved streams lands
	// on symbol boundaries and the other half a symbol out, so both are
	// tracked and whichever syncs wins.
	cross []float64
	mag   []float64

	regs [2]uint64
	have [2]int

	prevI, prevQ float64
	primed       bool

	// at is the time of bits[0], so a frame that straddles two reads is
	// still timed from where it actually started.
	at time.Time

	// skip suppresses sync detection inside a frame already being read.
	skip int
}

// How much of the stream has to be in hand before a frame can be read,
// in samples. Sync detection only needs room for the longest aircraft
// message; a ground uplink is eleven times longer, and waiting for that
// much every time would hold every aircraft message back with it.
const (
	needADSB   = longFrame*8*2 + 2
	needUplink = uplinkFrame*8*2 + 2
)

// Process demodulates a block of interleaved unsigned 8-bit IQ and
// calls emit for every frame that survives Reed-Solomon. at is the time
// of the first sample.
func (d *Demodulator) Process(iq []byte, at time.Time, emit func(Frame)) {
	// Whatever was held back from last time sits immediately before
	// this block, so the retained tail is timed by counting backwards
	// from the caller's timestamp rather than by accumulating.
	d.at = at.Add(-time.Duration(len(d.cross)) * time.Second / SampleRate)

	n := len(iq) / 2
	for i := 0; i < n; i++ {
		qi := (float64(iq[2*i]) - 127.5) / 127.5
		qq := (float64(iq[2*i+1]) - 127.5) / 127.5
		if !d.primed {
			d.prevI, d.prevQ, d.primed = qi, qq, true
			continue
		}
		// The imaginary part of s[k] * conj(s[k-1]) has the sign of the
		// phase advance, which for two-level FSK is the bit. No arctan
		// is needed to know which side of zero it fell.
		cross := qq*d.prevI - qi*d.prevQ
		d.prevI, d.prevQ = qi, qq

		d.cross = append(d.cross, cross)
		d.mag = append(d.mag, math.Hypot(qi, qq))
	}

	// Walk the bit stream, leaving a frame's worth of lookahead behind
	// for the next call.
	i := 0
	for ; i+needADSB <= len(d.cross); i++ {
		s := i & 1
		d.regs[s] = (d.regs[s]<<1 | boolBit(d.bit(i))) & syncMask
		if d.have[s] < syncBits {
			d.have[s]++
			continue
		}
		if d.skip > 0 {
			d.skip--
			continue
		}

		uplink := false
		switch {
		case bitsWrong(d.regs[s], syncADSB) <= syncTolerance:
		case bitsWrong(d.regs[s], syncUplink) <= syncTolerance:
			uplink = true
		default:
			continue
		}

		if uplink && i+2+needUplink > len(d.cross) {
			// A ground uplink has started but has not all arrived.
			// Rewind to before its sync word and wait: the same sync
			// will be found again next time with the whole frame
			// behind it.
			i -= syncBits * 2
			if i < 0 {
				i = 0
			}
			d.regs = [2]uint64{}
			d.have = [2]int{}
			break
		}

		f, used, ok := d.readFrame(i+2, uplink)
		if !ok {
			continue
		}
		f.At = d.at.Add(time.Duration(i) * time.Second / SampleRate)
		emit(f)
		// Do not look for another frame inside this one.
		d.skip = used
	}

	// Keep the unprocessed tail, and the sync registers with it.
	d.cross = append(d.cross[:0:0], d.cross[i:]...)
	d.mag = append(d.mag[:0:0], d.mag[i:]...)
}

// bit is the symbol starting at sample i: the phase change across the
// whole symbol, which is what a non-coherent FSK detector integrates.
func (d *Demodulator) bit(i int) bool {
	return d.cross[i]+d.cross[i+1] > 0
}

// readFrame slices the symbols following a sync word into a frame and
// hands it to Reed-Solomon. from is the sample the payload starts at.
func (d *Demodulator) readFrame(from int, uplink bool) (Frame, int, bool) {
	if uplink {
		raw, used, ok := d.take(from, uplinkFrame)
		if !ok {
			return Frame{}, 0, false
		}
		payload, errs, ok := decodeUplink(raw)
		if !ok {
			return Frame{}, 0, false
		}
		return Frame{Payload: payload, Uplink: true, Errors: errs,
			Signal: d.signal(from, used)}, used, true
	}

	// An ADS-B frame is either 18 or 34 bytes of message, and nothing
	// in the sync says which. Try the long form first: a short frame
	// read as a long one fails Reed-Solomon, because the extra bytes
	// are whatever was on the air next.
	if raw, used, ok := d.take(from, longFrame); ok {
		if n, ok := adsbLong.correct(raw); ok && plausible(raw, longFrame) {
			return Frame{Payload: raw[:longFrame-adsbLong.roots], Errors: n,
				Signal: d.signal(from, used)}, used, true
		}
	}
	if raw, used, ok := d.take(from, shortFrame); ok {
		if n, ok := adsbShort.correct(raw); ok && plausible(raw, shortFrame) {
			return Frame{Payload: raw[:shortFrame-adsbShort.roots], Errors: n,
				Signal: d.signal(from, used)}, used, true
		}
	}
	return Frame{}, 0, false
}

// plausible rejects a frame whose length disagrees with what its own
// header says it should be. Reed-Solomon can succeed on noise often
// enough to matter, and the message type is a free second opinion:
// type 0 is the only one that fits in a short frame.
func plausible(raw []byte, length int) bool {
	messageType := raw[0] >> 3
	if length == shortFrame {
		return messageType == 0
	}
	return messageType != 0 && messageType <= 10
}

// take packs n bytes' worth of symbols starting at sample from, most
// significant bit first, and reports how many samples that consumed.
func (d *Demodulator) take(from, n int) ([]byte, int, bool) {
	need := n * 8 * 2
	if from+need+1 > len(d.cross) {
		return nil, 0, false
	}
	out := make([]byte, n)
	for i := 0; i < n*8; i++ {
		if d.bit(from + i*2) {
			out[i/8] |= 0x80 >> (i % 8)
		}
	}
	return out, need, true
}

// decodeUplink un-interleaves a ground uplink and corrects each of its
// six blocks. The blocks are interleaved byte by byte so that a burst
// of interference damages a little of each rather than destroying one.
func decodeUplink(raw []byte) ([]byte, int, bool) {
	out := make([]byte, 0, uplinkParts*uplinkData)
	total := 0
	for part := 0; part < uplinkParts; part++ {
		block := make([]byte, uplinkBlock)
		for i := 0; i < uplinkBlock; i++ {
			block[i] = raw[i*uplinkParts+part]
		}
		n, ok := uplink.correct(block)
		if !ok {
			return nil, 0, false
		}
		total += n
		out = append(out, block[:uplinkData]...)
	}
	return out, total, true
}

func (d *Demodulator) signal(from, n int) float64 {
	if from+n > len(d.mag) {
		n = len(d.mag) - from
	}
	if n <= 0 {
		return -100
	}
	var sum float64
	for _, m := range d.mag[from : from+n] {
		sum += m * m
	}
	rms := math.Sqrt(sum / float64(n))
	if rms <= 0 {
		return -100
	}
	return 20 * math.Log10(rms)
}

func boolBit(b bool) uint64 {
	if b {
		return 1
	}
	return 0
}

// bitsWrong counts how many of the sync bits disagree.
func bitsWrong(got, want uint64) int {
	n := 0
	for x := got ^ want; x != 0; x &= x - 1 {
		n++
	}
	return n
}
