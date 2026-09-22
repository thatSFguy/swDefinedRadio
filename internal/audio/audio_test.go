package audio

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/demod"
)

// TestWAVHeader checks the fields a browser reads before it will play a
// stream: getting the rate or the sample size wrong yields audio at the
// wrong pitch rather than an error.
func TestWAVHeader(t *testing.T) {
	h := WAVHeader(demod.AudioRate, 1, 16)
	if len(h) != 44 {
		t.Fatalf("header is %d bytes, want 44", len(h))
	}
	if string(h[0:4]) != "RIFF" || string(h[8:12]) != "WAVE" || string(h[36:40]) != "data" {
		t.Fatalf("chunk ids are wrong: %q", h)
	}
	if got := binary.LittleEndian.Uint16(h[20:]); got != 1 {
		t.Errorf("format = %d, want 1 (PCM)", got)
	}
	if got := binary.LittleEndian.Uint16(h[22:]); got != 1 {
		t.Errorf("channels = %d, want 1", got)
	}
	if got := binary.LittleEndian.Uint32(h[24:]); got != demod.AudioRate {
		t.Errorf("sample rate = %d, want %d", got, demod.AudioRate)
	}
	if got := binary.LittleEndian.Uint32(h[28:]); got != demod.AudioRate*2 {
		t.Errorf("byte rate = %d, want %d", got, demod.AudioRate*2)
	}
	if got := binary.LittleEndian.Uint16(h[32:]); got != 2 {
		t.Errorf("block align = %d, want 2", got)
	}
	if got := binary.LittleEndian.Uint16(h[34:]); got != 16 {
		t.Errorf("bits = %d, want 16", got)
	}
}

// TestBroadcasterFanOut checks every listener gets its own copy, and
// that unsubscribing closes the channel so a reader's range ends.
func TestBroadcasterFanOut(t *testing.T) {
	b := NewBroadcaster()
	a, c := b.Subscribe(), b.Subscribe()
	if b.Count() != 2 {
		t.Fatalf("count = %d, want 2", b.Count())
	}

	b.Send([]int16{1, 2, 3})
	for i, ch := range []chan []int16{a, c} {
		select {
		case block := <-ch:
			if len(block) != 3 || block[0] != 1 || block[2] != 3 {
				t.Errorf("listener %d got %v", i, block)
			}
		case <-time.After(time.Second):
			t.Fatalf("listener %d got nothing", i)
		}
	}

	b.Unsubscribe(a)
	if _, open := <-a; open {
		t.Error("unsubscribe left the channel open")
	}
	b.Unsubscribe(a) // must not panic on a second call
	if b.Count() != 1 {
		t.Errorf("count = %d, want 1", b.Count())
	}
}

// TestBroadcasterDropsSlowListener is the property the radio depends
// on: a listener that stops reading must not stall demodulation.
func TestBroadcasterDropsSlowListener(t *testing.T) {
	b := NewBroadcaster()
	slow := b.Subscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 1000 { // far more than the channel buffer
			b.Send([]int16{7})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("send blocked on a listener that is not reading")
	}
	if len(slow) == 0 {
		t.Error("slow listener got nothing at all")
	}
	b.Unsubscribe(slow)
}

// Dropping must end every listener, not merely stop feeding them. A
// browser holding an audio stream open against a receiver that has been
// set aside would otherwise wait on a connection nobody will ever write
// to again.
func TestDropEndsEveryListener(t *testing.T) {
	b := NewBroadcaster()
	a, c := b.Subscribe(), b.Subscribe()

	b.Drop()
	for i, ch := range []chan []int16{a, c} {
		if _, open := <-ch; open {
			t.Errorf("listener %d still open after Drop", i)
		}
	}
	if b.Count() != 0 {
		t.Errorf("count = %d after Drop, want 0", b.Count())
	}
	b.Drop() // must not panic on a second call
}

// A broadcaster must work again after being dropped. Its receiver can be
// given the radio a second time — which is the whole point of the tabs —
// and when it is, the audio has to flow again.
//
// This is the case that shipped broken: the FM tab played the first time
// it was chosen and was silent on every visit after, because dropping its
// listeners had retired the broadcaster for good.
func TestBroadcasterWorksAgainAfterDrop(t *testing.T) {
	b := NewBroadcaster()

	first := b.Subscribe()
	b.Send([]int16{1, 2, 3})
	if block := <-first; len(block) != 3 {
		t.Fatalf("first listener got %v", block)
	}
	b.Drop()

	// A second spell on the air.
	second := b.Subscribe()
	if b.Count() != 1 {
		t.Fatalf("count = %d after resubscribing, want 1", b.Count())
	}
	b.Send([]int16{4, 5, 6})
	select {
	case block := <-second:
		if len(block) != 3 || block[0] != 4 {
			t.Errorf("second listener got %v, want 4,5,6", block)
		}
	case <-time.After(time.Second):
		t.Fatal("no audio after the broadcaster was dropped and used again")
	}
}

// Sending with no listeners must be harmless.
func TestSendAfterDropIsHarmless(t *testing.T) {
	b := NewBroadcaster()
	ch := b.Subscribe()
	b.Drop()
	b.Send([]int16{1, 2, 3})
	if _, open := <-ch; open {
		t.Error("listener channel outlived Drop")
	}
}
