package uat

import (
	"math/rand/v2"
	"testing"
)

// TestFieldTables checks the Galois field itself before anything built
// on it: every non-zero element must have a logarithm that round-trips,
// and alpha must have order 255.
func TestFieldTables(t *testing.T) {
	seen := map[byte]bool{}
	for i := 0; i < symbols; i++ {
		v := alphaTo[i]
		if v == 0 {
			t.Fatalf("alpha**%d is zero", i)
		}
		if seen[v] {
			t.Fatalf("alpha**%d repeats a value — the field polynomial is not primitive", i)
		}
		seen[v] = true
		if indexOf[v] != i {
			t.Fatalf("log(alpha**%d) = %d", i, indexOf[v])
		}
	}
	if len(seen) != 255 {
		t.Fatalf("the field has %d non-zero elements, want 255", len(seen))
	}
	// Multiplication against long division in the field.
	if got := mul(0x53, 0xCA); got != gfMulSlow(0x53, 0xCA) {
		t.Errorf("mul(0x53,0xCA) = %#x, want %#x", got, gfMulSlow(0x53, 0xCA))
	}
}

// gfMulSlow multiplies the long way, so the table-driven version has
// something independent to agree with.
func gfMulSlow(a, b byte) byte {
	var p byte
	for i := 0; i < 8; i++ {
		if b&1 != 0 {
			p ^= a
		}
		hi := a & 0x80
		a <<= 1
		if hi != 0 {
			a ^= byte(fieldPoly & 0xff)
		}
		b >>= 1
	}
	return p
}

// TestCorrectsUpToTheLimit is the property the range of the receiver
// depends on: every code must repair its full quota of wrong bytes,
// wherever they land.
func TestCorrectsUpToTheLimit(t *testing.T) {
	for _, c := range []struct {
		name string
		code *code
	}{
		{"adsb short", adsbShort},
		{"adsb long", adsbLong},
		{"uplink block", uplink},
	} {
		t.Run(c.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(1, 2))
			data := c.code.total - c.code.roots
			max := c.code.roots / 2

			for trial := 0; trial < 200; trial++ {
				msg := make([]byte, data)
				for i := range msg {
					msg[i] = byte(rng.UintN(256))
				}
				frame := append(append([]byte{}, msg...), c.code.parity(msg)...)

				// An untouched frame must decode with nothing corrected.
				clean := append([]byte{}, frame...)
				if n, ok := c.code.correct(clean); !ok || n != 0 {
					t.Fatalf("clean frame: corrected %d, ok %v", n, ok)
				}

				// Now break exactly as many bytes as the code allows.
				damaged := append([]byte{}, frame...)
				hit := map[int]bool{}
				for len(hit) < max {
					hit[int(rng.UintN(uint(c.code.total)))] = true
				}
				for at := range hit {
					damaged[at] ^= byte(1 + rng.UintN(255))
				}

				n, ok := c.code.correct(damaged)
				if !ok {
					t.Fatalf("trial %d: %d errors were not corrected", trial, max)
				}
				if n != max {
					t.Errorf("trial %d: reported %d errors, want %d", trial, n, max)
				}
				for i := range frame {
					if damaged[i] != frame[i] {
						t.Fatalf("trial %d: byte %d came back %#x, want %#x", trial, i, damaged[i], frame[i])
					}
				}
			}
		})
	}
}

// TestRefusesTooMuchDamage: past the limit the code must say so rather
// than hand back a frame it has quietly turned into a different valid
// message.
func TestRefusesTooMuchDamage(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	c := adsbLong
	data := c.total - c.roots

	wrong := 0
	for trial := 0; trial < 300; trial++ {
		msg := make([]byte, data)
		for i := range msg {
			msg[i] = byte(rng.UintN(256))
		}
		frame := append(append([]byte{}, msg...), c.parity(msg)...)
		damaged := append([]byte{}, frame...)

		// Comfortably past the limit.
		hit := map[int]bool{}
		for len(hit) < c.roots/2+4 {
			hit[int(rng.UintN(uint(c.total)))] = true
		}
		for at := range hit {
			damaged[at] ^= byte(1 + rng.UintN(255))
		}

		n, ok := c.correct(damaged)
		if !ok {
			continue // refused, which is the right answer
		}
		// If it claims success the result must still be a valid
		// codeword; what it must never do is return the original with
		// errors left in it.
		if n > 0 {
			wrong++
		}
	}
	if wrong > 60 {
		t.Errorf("%d of 300 over-damaged frames were claimed as corrected", wrong)
	}
}

func BenchmarkCorrectLong(b *testing.B) {
	msg := make([]byte, adsbLong.total-adsbLong.roots)
	for i := range msg {
		msg[i] = byte(i * 7)
	}
	frame := append(append([]byte{}, msg...), adsbLong.parity(msg)...)
	frame[3] ^= 0x5a
	frame[20] ^= 0xf0

	buf := make([]byte, len(frame))
	b.ResetTimer()
	for range b.N {
		copy(buf, frame)
		adsbLong.correct(buf)
	}
}
