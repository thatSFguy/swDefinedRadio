// Package modes demodulates and decodes Mode S / ADS-B transmissions on
// 1090 MHz.
package modes

// crcPoly is the Mode S CRC-24 generator polynomial
// x^24+x^23+x^22+x^21+x^20+x^19+x^18+x^17+x^16+x^15+x^14+x^13+x^12+x^10+x^3+1
// with the implicit top bit dropped.
const crcPoly = 0xFFF409

var crcTable [256]uint32

func init() {
	for i := range crcTable {
		c := uint32(i) << 16
		for range 8 {
			if c&0x800000 != 0 {
				c = (c << 1) ^ crcPoly
			} else {
				c <<= 1
			}
		}
		crcTable[i] = c & 0xFFFFFF
	}
}

// CRC returns the checksum over every byte of msg but the trailing three
// parity bytes.
func CRC(msg []byte) uint32 {
	var c uint32
	for _, b := range msg[:len(msg)-3] {
		c = ((c << 8) ^ crcTable[byte(c>>16)^b]) & 0xFFFFFF
	}
	return c
}

// Parity returns the checksum carried in the last three bytes of msg.
func Parity(msg []byte) uint32 {
	n := len(msg)
	return uint32(msg[n-3])<<16 | uint32(msg[n-2])<<8 | uint32(msg[n-1])
}
