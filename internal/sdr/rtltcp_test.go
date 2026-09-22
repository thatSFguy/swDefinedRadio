package sdr

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// command is one thing a client asked the server to do.
type command struct {
	op    byte
	param uint32
}

// fakeRTLTCP is an rtl_tcp server in a goroutine: the greeting, a stream
// of samples, and a record of the commands it was sent.
//
// It exists because every interesting property of talking to rtl_tcp —
// that a sample rate is really commanded, that a blocked read can be
// interrupted, that a stale buffer is dropped — can be established
// without a dongle attached, and so belongs in the ordinary test run
// rather than in a manual check someone has to remember to do.
type fakeRTLTCP struct {
	addr string

	mu   sync.Mutex
	cmds []command

	// greet is false for a server that accepts a connection and then says
	// nothing, which is what a real rtl_tcp does to a second client.
	greet bool
	// fill is the byte streamed as samples, so a test can tell samples
	// captured before a retune from samples captured after one.
	fill byte
}

func startFake(t *testing.T, greet bool) *fakeRTLTCP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	f := &fakeRTLTCP{addr: ln.Addr().String(), greet: greet, fill: 0x11}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeRTLTCP) serve(conn net.Conn) {
	defer conn.Close()
	if !f.greet {
		// Accept and stay silent, holding the connection open.
		io.Copy(io.Discard, conn)
		return
	}
	var hdr [12]byte
	copy(hdr[:4], "RTL0")
	binary.BigEndian.PutUint32(hdr[4:8], uint32(TunerR820T))
	binary.BigEndian.PutUint32(hdr[8:12], 29)
	if _, err := conn.Write(hdr[:]); err != nil {
		return
	}

	// Stream samples until the client goes away.
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			f.mu.Lock()
			b := f.fill
			f.mu.Unlock()
			for i := range buf {
				buf[i] = b
			}
			if _, err := conn.Write(buf); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var c [5]byte
	for {
		if _, err := io.ReadFull(conn, c[:]); err != nil {
			<-done
			return
		}
		f.mu.Lock()
		f.cmds = append(f.cmds, command{c[0], binary.BigEndian.Uint32(c[1:])})
		f.mu.Unlock()
	}
}

// sent reports the commands received so far.
func (f *fakeRTLTCP) sent() []command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]command(nil), f.cmds...)
}

// setFill changes the byte being streamed, standing in for the samples
// changing character after the tuner is moved.
func (f *fakeRTLTCP) setFill(b byte) {
	f.mu.Lock()
	f.fill = b
	f.mu.Unlock()
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func has(cmds []command, op byte, param uint32) bool {
	for _, c := range cmds {
		if c.op == op && c.param == param {
			return true
		}
	}
	return false
}

// The tuner settings a receiver asks for must actually reach the server.
// This is the assumption the whole hub rests on: that switching receivers
// is a matter of commanding the radio rather than restarting it.
func TestDialAppliesConfig(t *testing.T) {
	f := startFake(t, true)
	c, err := DialRTLTCP(f.addr, Config{
		CenterFreq: 1_090_000_000,
		SampleRate: 2_000_000,
		Gain:       AutoGain,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	if c.Tuner != TunerR820T {
		t.Errorf("tuner = %v, want R820T", c.Tuner)
	}
	// Wait for every command being asserted on, not just the first ones.
	// They are written in order — rate, frequency, then gain mode — so
	// waiting for an earlier one says nothing about a later one having
	// arrived yet.
	waitFor(t, "the tuner settings to arrive", func() bool {
		s := f.sent()
		return has(s, cmdSetSampleRate, 2_000_000) &&
			has(s, cmdSetFreq, 1_090_000_000) &&
			has(s, cmdSetGainMode, 0)
	})
}

// A second client is accepted into the listen backlog and then never
// spoken to. Dialling must give up rather than block forever, which is
// what it used to do.
func TestDialGivesUpWithoutGreeting(t *testing.T) {
	f := startFake(t, false)

	type result struct {
		err error
	}
	ch := make(chan result, 1)
	go func() {
		_, err := DialRTLTCP(f.addr, Config{CenterFreq: 100_000_000})
		ch <- result{err}
	}()

	select {
	case r := <-ch:
		if r.err == nil {
			t.Fatal("dial succeeded against a server that never greeted")
		}
		if !strings.Contains(r.err.Error(), "greeting") {
			t.Errorf("error does not mention the greeting: %v", r.err)
		}
	case <-time.After(greetingTimeout + 3*time.Second):
		t.Fatal("dial blocked past the greeting timeout")
	}
}

// Setting a deadline in the past must release a receiver parked in a
// blocking read, without closing the connection the next receiver wants.
func TestSetReadDeadlineInterruptsBlockedRead(t *testing.T) {
	f := startFake(t, true)
	c, err := DialRTLTCP(f.addr, Config{CenterFreq: 100_000_000, SampleRate: 1_024_000})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// A receiver's read loop: a large read that the trickle of samples
	// above will not satisfy quickly.
	blocked := make(chan error, 1)
	go func() {
		buf := make([]byte, 1<<20)
		_, err := io.ReadFull(c, buf)
		blocked <- err
	}()

	// Let it get into the read, then take the radio away.
	time.Sleep(50 * time.Millisecond)
	if err := c.SetReadDeadline(time.Now()); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	select {
	case err := <-blocked:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("read ended with %v, want a timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read was not interrupted by the deadline")
	}

	// The connection itself must still be usable: the point of a deadline
	// rather than a close is that the next receiver inherits the session.
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear deadline: %v", err)
	}
	if err := c.Tune(98_700_000); err != nil {
		t.Fatalf("connection unusable after interrupting a read: %v", err)
	}
	waitFor(t, "the retune to arrive", func() bool {
		return has(f.sent(), cmdSetFreq, 98_700_000)
	})
}

// After an interrupted read, bufio still holds the old samples and the
// error that ended the read. Both have to go, or the next receiver is fed
// samples captured at the previous frequency.
func TestResetBufferDropsStaleSamplesAndError(t *testing.T) {
	f := startFake(t, true)
	c, err := DialRTLTCP(f.addr, Config{CenterFreq: 100_000_000, SampleRate: 1_024_000})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Let samples of the old character accumulate, then interrupt.
	time.Sleep(50 * time.Millisecond)
	if err := c.SetReadDeadline(time.Now()); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	one := make([]byte, 1)
	if _, err := c.Read(one); err == nil {
		t.Fatal("expected the interrupted read to report the deadline")
	}

	// The radio moves, and everything captured before now is stale.
	f.setFill(0x22)
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear deadline: %v", err)
	}
	c.ResetBuffer()

	// Without the reset this first read returns the remembered timeout
	// rather than samples, which is what would make a following Drain
	// believe the socket had already gone quiet.
	if _, err := c.Drain(100*time.Millisecond, 1<<20); err != nil {
		t.Fatalf("drain after reset: %v", err)
	}
	buf := make([]byte, 64)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read after reset: %v", err)
	}
	for i, b := range buf {
		if b != 0x22 {
			t.Fatalf("byte %d is %#02x, want %#02x — stale samples survived the reset", i, b, 0x22)
		}
	}
}
