// Package dsp holds the small signal-processing primitives shared by the
// receivers in this repo.
package dsp

import "math"

// magScale maps the largest possible IQ vector length (127.5 * sqrt(2))
// onto the top of the uint16 range.
const magScale = 65535.0 / 180.312

// MagTable converts an interleaved unsigned-8-bit IQ pair to a magnitude.
// The RTL2832U centres both components on 127.5, so each is recentred
// before taking the vector length. Precomputing all 65536 combinations
// keeps the hot loop to one indexed load per sample.
type MagTable [256 * 256]uint16

// NewMagTable builds the lookup table. It costs a few milliseconds once.
func NewMagTable() *MagTable {
	t := new(MagTable)
	for i := range 256 {
		for q := range 256 {
			fi := float64(i) - 127.5
			fq := float64(q) - 127.5
			t[i<<8|q] = uint16(math.Round(math.Hypot(fi, fq) * magScale))
		}
	}
	return t
}

// Compute writes the magnitude of each IQ pair in src to dst, which must
// hold at least len(src)/2 elements. It returns the number written.
func (t *MagTable) Compute(src []byte, dst []uint16) int {
	n := min(len(src)/2, len(dst))
	for i := range n {
		dst[i] = t[uint16(src[2*i])<<8|uint16(src[2*i+1])]
	}
	return n
}
