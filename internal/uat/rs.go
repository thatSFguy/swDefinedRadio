package uat

// Reed-Solomon over GF(256), as UAT uses it. Every UAT frame is
// protected: a basic ADS-B message is RS(30,18), a long one RS(48,34)
// and each block of a ground uplink is RS(92,72). All three are
// shortened codes from the same RS(255,·) family, with the field
// polynomial and first root DO-282B specifies.
//
// Without this a frame is either perfect or lost. With it, a handful
// of wrong bytes — which is what a marginal signal delivers — still
// decodes, and the difference in range is large.

// fieldPoly is x^8 + x^7 + x^2 + x + 1, the primitive polynomial UAT's
// Galois field is built on.
const fieldPoly = 0x187

// firstRoot is the first consecutive root of the generator polynomial.
// It is 120 for every UAT code, which is unusual enough to be worth
// stating: most codes start at 0 or 1.
const firstRoot = 120

const symbols = 255 // symbols in the unshortened code

// a0 marks the logarithm of zero, which does not exist.
const a0 = symbols

// The field tables are built by a function rather than in init(),
// because the codes below are package-level variables too: Go orders
// variable initialisation by dependency, and an init() function would
// run after the codes had already been built from an empty table.
var alphaTo, indexOf = buildField()

func buildField() (alpha [symbols + 1]byte, index [symbols + 1]int) {
	index[0] = a0
	alpha[a0] = 0
	sr := 1
	for i := 0; i < symbols; i++ {
		index[sr] = i
		alpha[i] = byte(sr)
		sr <<= 1
		if sr&0x100 != 0 {
			sr ^= fieldPoly
		}
		sr &= symbols
	}
	return alpha, index
}

// mod255 reduces an exponent into [0,254] without a division.
func mod255(x int) int {
	for x >= symbols {
		x -= symbols
		x = (x >> 8) + (x & symbols)
	}
	return x
}

// code is one shortened Reed-Solomon code: total symbols, of which
// roots are parity.
type code struct {
	total int // symbols in a frame, data and parity together
	roots int // parity symbols; it corrects roots/2 errors
	gen   []byte
}

// The three codes UAT uses.
var (
	adsbShort = newCode(30, 12) // RS(30,18), corrects 6 bytes
	adsbLong  = newCode(48, 14) // RS(48,34), corrects 7 bytes
	uplink    = newCode(92, 20) // RS(92,72), corrects 10 bytes
)

func newCode(total, roots int) *code {
	c := &code{total: total, roots: roots}
	// Generator polynomial: the product of (x - alpha**(firstRoot+i)),
	// kept in index form for the encoder.
	gen := make([]byte, roots+1)
	gen[0] = 1
	for i := 0; i < roots; i++ {
		root := alphaTo[mod255(firstRoot+i)]
		for j := i + 1; j > 0; j-- {
			gen[j] = gen[j-1] ^ mul(gen[j], root)
		}
		gen[0] = mul(gen[0], root)
	}
	c.gen = gen
	return c
}

func mul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return alphaTo[mod255(indexOf[a]+indexOf[b])]
}

// parity returns the parity symbols for a data block, which is what the
// tests need to build a frame a receiver would actually see.
func (c *code) parity(data []byte) []byte {
	out := make([]byte, c.roots)
	for _, d := range data {
		feedback := d ^ out[0]
		copy(out, out[1:])
		out[c.roots-1] = 0
		if feedback != 0 {
			for i := 0; i < c.roots; i++ {
				out[i] ^= mul(c.gen[c.roots-1-i], feedback)
			}
		}
	}
	return out
}

// correct repairs a frame in place and reports how many symbols were
// wrong. It returns false when the damage is past what the code can
// repair, which is the only honest answer: a frame with too many errors
// cannot be told from a different valid frame.
//
// The method is the usual three steps — Berlekamp-Massey for the error
// locator, a Chien search for where the errors are, and Forney for what
// they are.
func (c *code) correct(frame []byte) (corrected int, ok bool) {
	if len(frame) != c.total {
		return 0, false
	}
	// A shortened code is the full 255-symbol code with zeros in front.
	// Those zeros are not transmitted, but the error positions are
	// still numbered as if they were.
	pad := symbols - c.total

	// Syndromes: the received frame evaluated at each root of the
	// generator. All zero means the frame arrived intact, which is the
	// common case and worth leaving early for.
	syn := make([]int, c.roots)
	{
		s := make([]byte, c.roots)
		for i := range s {
			s[i] = frame[0]
		}
		for j := 1; j < c.total; j++ {
			for i := 0; i < c.roots; i++ {
				if s[i] == 0 {
					s[i] = frame[j]
				} else {
					s[i] = frame[j] ^ alphaTo[mod255(indexOf[s[i]]+firstRoot+i)]
				}
			}
		}
		clean := true
		for i, v := range s {
			if v != 0 {
				clean = false
			}
			syn[i] = indexOf[v]
		}
		if clean {
			return 0, true
		}
	}

	// Berlekamp-Massey. lambda is kept in value form while it is being
	// built and b, the previous locator, in index form.
	lambda := make([]byte, c.roots+1)
	lambda[0] = 1
	b := make([]int, c.roots+1)
	for i := range b {
		b[i] = a0
	}
	b[0] = 0 // index form of 1
	t := make([]byte, c.roots+1)

	el := 0
	for r := 1; r <= c.roots; r++ {
		// How far the current locator is from explaining the syndromes.
		var d byte
		for i := 0; i < r; i++ {
			if lambda[i] != 0 && syn[r-i-1] != a0 {
				d ^= alphaTo[mod255(indexOf[lambda[i]]+syn[r-i-1])]
			}
		}
		discr := indexOf[d]
		if discr == a0 {
			shiftIndex(b) // b(x) <- x*b(x)
			continue
		}

		// t(x) <- lambda(x) - discr * x * b(x)
		t[0] = lambda[0]
		for i := 0; i < c.roots; i++ {
			t[i+1] = lambda[i+1]
			if b[i] != a0 {
				t[i+1] ^= alphaTo[mod255(discr+b[i])]
			}
		}
		if 2*el <= r-1 {
			el = r - el
			for i := 0; i <= c.roots; i++ {
				if lambda[i] == 0 {
					b[i] = a0
				} else {
					b[i] = mod255(indexOf[lambda[i]] - discr + symbols)
				}
			}
		} else {
			shiftIndex(b)
		}
		copy(lambda, t)
	}

	// Convert the locator to index form; its degree is how many errors
	// it claims to have found.
	degree := 0
	lam := make([]int, c.roots+1)
	for i := 0; i <= c.roots; i++ {
		lam[i] = indexOf[lambda[i]]
		if lam[i] != a0 {
			degree = i
		}
	}
	if degree == 0 || degree > c.roots/2 {
		return 0, false
	}

	// Chien search: every power of alpha at which the locator vanishes
	// is an error position.
	root := make([]int, 0, degree)
	loc := make([]int, 0, degree)
	reg := make([]int, c.roots+1)
	copy(reg, lam)
	for i := 1; i <= symbols; i++ {
		q := byte(1) // lambda[0] is always 1
		for j := degree; j > 0; j-- {
			if reg[j] != a0 {
				reg[j] = mod255(reg[j] + j)
				q ^= alphaTo[reg[j]]
			}
		}
		if q != 0 {
			continue
		}
		root = append(root, i)
		loc = append(loc, i-1)
		if len(root) == degree {
			break
		}
	}
	if len(root) != degree {
		// Fewer roots than the locator's degree: the error pattern is
		// not one this code could have produced, so the frame is too
		// damaged to trust.
		return 0, false
	}

	// omega = syndromes * lambda, truncated to the parity length.
	omega := make([]int, c.roots+1)
	omega[c.roots] = a0
	degOmega := 0
	for i := 0; i < c.roots; i++ {
		var tmp byte
		j := degree
		if i < j {
			j = i
		}
		for ; j >= 0; j-- {
			if syn[i-j] != a0 && lam[j] != a0 {
				tmp ^= alphaTo[mod255(syn[i-j]+lam[j])]
			}
		}
		if tmp != 0 {
			degOmega = i
		}
		omega[i] = indexOf[tmp]
	}

	// Forney: the value of each error.
	for j := degree - 1; j >= 0; j-- {
		var num1 byte
		for i := degOmega; i >= 0; i-- {
			if omega[i] != a0 {
				num1 ^= alphaTo[mod255(omega[i]+i*root[j])]
			}
		}
		if num1 == 0 {
			continue // a zero error value changes nothing
		}
		num2 := alphaTo[mod255(root[j]*(firstRoot-1)+symbols)]

		// The formal derivative of lambda keeps only its odd terms,
		// which in this field is lambda[i+1] for even i.
		var den byte
		for i := min(degree, c.roots-1) & ^1; i >= 0; i -= 2 {
			if lam[i+1] != a0 {
				den ^= alphaTo[mod255(lam[i+1]+i*root[j])]
			}
		}
		if den == 0 {
			return 0, false
		}

		at := loc[j] - pad
		if at < 0 || at >= c.total {
			return 0, false // an error outside the frame is not a real fix
		}
		frame[at] ^= alphaTo[mod255(indexOf[num1]+indexOf[num2]+symbols-indexOf[den])]
	}
	return degree, true
}

func shiftIndex(b []int) {
	copy(b[1:], b)
	b[0] = a0
}
