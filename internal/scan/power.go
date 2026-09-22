package scan

import (
	"bytes"
	"math"
	"strconv"
)

// Power is a spectrum in dBFS. Bins no segment covered are NaN, which
// encoding/json refuses outright — it fails the whole response rather
// than emitting anything — so they marshal as null instead.
type Power []float64

// MarshalJSON writes the spectrum as a JSON array, with gaps as null.
func (p Power) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.Grow(len(p) * 8)
	b.WriteByte('[')
	for i, v := range p {
		if i > 0 {
			b.WriteByte(',')
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			b.WriteString("null")
			continue
		}
		// One decimal is plenty for a display in decibels, and it keeps
		// the payload a third the size of full precision.
		b.WriteString(strconv.FormatFloat(v, 'f', 1, 64))
	}
	b.WriteByte(']')
	return b.Bytes(), nil
}

// Decimate reduces a spectrum to at most target points by keeping the
// strongest bin in each group. A mean would average narrow carriers into
// the noise, which is exactly what a scanner must not do; the peak is
// what stays visible.
//
// Gaps only survive if every bin in the group is a gap.
func Decimate(p Power, target int) Power {
	if target <= 0 || len(p) <= target {
		out := make(Power, len(p))
		copy(out, p)
		return out
	}
	out := make(Power, target)
	for i := range target {
		lo := i * len(p) / target
		hi := (i + 1) * len(p) / target
		if hi <= lo {
			hi = lo + 1
		}
		best := math.NaN()
		for _, v := range p[lo:min(hi, len(p))] {
			if math.IsNaN(v) {
				continue
			}
			if math.IsNaN(best) || v > best {
				best = v
			}
		}
		out[i] = best
	}
	return out
}
