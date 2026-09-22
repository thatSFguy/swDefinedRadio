// Package audio carries decoded sound from a receiver to whoever is
// listening — a browser, the machine's own speaker, or both at once.
package audio

import (
	"encoding/binary"
	"sync"
)

// subscriberBacklog is how many blocks a listener may fall behind before
// blocks start being dropped for it. At 48 kHz a block is a few tens of
// milliseconds, so this is a second or two of slack: enough to ride out a
// browser being busy, short enough that a listener which has genuinely
// stopped reading does not hold that much audio in memory.
const subscriberBacklog = 32

// Broadcaster fans one stream of audio out to every listener.
//
// Nothing here is allowed to block the radio. A listener that has fallen
// behind has blocks dropped for it rather than being waited for, because
// the alternative is back-pressure reaching the demodulator and stalling
// reception for everybody.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan []int16]struct{}
}

func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[chan []int16]struct{})}
}

// Subscribe returns a channel of audio blocks. The channel is closed when
// the listener unsubscribes or the broadcaster is closed, so a reader can
// range over it and stop when the audio does.
func (b *Broadcaster) Subscribe() chan []int16 {
	ch := make(chan []int16, subscriberBacklog)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[ch] = struct{}{}
	return ch
}

// Unsubscribe stops a listener and closes its channel.
func (b *Broadcaster) Unsubscribe(ch chan []int16) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.subs[ch]; ok {
		delete(b.subs, ch)
		close(ch)
	}
}

// Send copies a block to every listener, skipping any that has fallen
// behind.
func (b *Broadcaster) Send(block []int16) {
	if len(block) == 0 {
		return
	}
	cp := make([]int16, len(block))
	copy(cp, block)

	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- cp:
		default: // listener is behind; drop rather than stall
		}
	}
}

// Count is how many listeners there are.
func (b *Broadcaster) Count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Drop ends every listener, and leaves the broadcaster ready for more.
//
// This is what happens when a receiver is set aside rather than shut
// down: the audio stream a browser is holding open has to end, or the
// page sits there with a connection to a receiver that has stopped
// producing anything, waiting for sound that will never come. Closing
// each subscriber's channel is what lets those handlers return.
//
// It deliberately does not retire the broadcaster. The receiver it
// belongs to can be given the radio again — that is the whole point of
// the tabs — and when it is, the same broadcaster carries the audio.
// Making this terminal meant the FM tab played once and was silent every
// time after, with nothing in the logs to say why.
func (b *Broadcaster) Drop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		delete(b.subs, ch)
		close(ch)
	}
}

// WAVHeader writes a RIFF header for a stream of unknown length.
//
// The sizes are left at their maximum because the length is not knowable
// in advance — the radio keeps producing audio for as long as anyone is
// listening. Players accept this and simply keep reading, which is what
// makes a live stream playable by an ordinary <audio> element with no
// client-side decoding at all.
func WAVHeader(rate, channels, bits int) []byte {
	const maxSize = 0xFFFFFFFF
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], maxSize)
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16) // PCM header length
	binary.LittleEndian.PutUint16(h[20:], 1)  // PCM
	binary.LittleEndian.PutUint16(h[22:], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	byteRate := rate * channels * bits / 8
	binary.LittleEndian.PutUint32(h[28:], uint32(byteRate))
	binary.LittleEndian.PutUint16(h[32:], uint16(channels*bits/8))
	binary.LittleEndian.PutUint16(h[34:], uint16(bits))
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], maxSize)
	return h
}

// LittleEndianPCM appends block to buf as the 16-bit little-endian bytes
// a WAV stream and a sound device both expect. buf is returned so the
// caller can keep one scratch slice rather than allocating per block.
func LittleEndianPCM(buf []byte, block []int16) []byte {
	for _, s := range block {
		buf = append(buf, byte(s), byte(s>>8))
	}
	return buf
}
