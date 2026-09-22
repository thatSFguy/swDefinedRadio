package radio

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// fakeServer stands in for rtl_tcp: the greeting, a stream of samples,
// and a record of the commands it was sent.
//
// It serves one client at a time, because that is what rtl_tcp does and
// it is the constraint the whole handover design is shaped around. A
// second connection is accepted by the listener and then left in silence,
// exactly as the real thing leaves it.
type fakeServer struct {
	addr string
	ln   net.Listener

	mu       sync.Mutex
	cmds     []command
	fill     byte
	accepted int
	busy     bool
}

type command struct {
	op    byte
	param uint32
}

const (
	cmdSetFreq       = 0x01
	cmdSetSampleRate = 0x02
	cmdSetGainMode   = 0x03
)

func startFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeServer{addr: ln.Addr().String(), ln: ln, fill: 0x11}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.accepted++
			busy := f.busy
			if !busy {
				f.busy = true
			}
			f.mu.Unlock()

			if busy {
				// One client at a time: hold it open and say nothing.
				go func() { io.Copy(io.Discard, conn); conn.Close() }()
				continue
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeServer) serve(conn net.Conn) {
	defer func() {
		conn.Close()
		f.mu.Lock()
		f.busy = false
		f.mu.Unlock()
	}()

	var hdr [12]byte
	copy(hdr[:4], "RTL0")
	binary.BigEndian.PutUint32(hdr[4:8], 5) // R820T
	binary.BigEndian.PutUint32(hdr[8:12], 29)
	if _, err := conn.Write(hdr[:]); err != nil {
		return
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Samples reach a real client in bursts — librtlsdr fills one of
		// its buffers and hands the lot over — so there are gaps between
		// them even at full rate. Writing an even trickle instead would
		// make this fake the one source that never falls silent, and
		// would hide anything that depends on noticing when it does.
		buf := make([]byte, 16<<10)
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
			time.Sleep(6 * time.Millisecond)
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

func (f *fakeServer) sent() []command {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]command(nil), f.cmds...)
}

func (f *fakeServer) setFill(b byte) {
	f.mu.Lock()
	f.fill = b
	f.mu.Unlock()
}

func (f *fakeServer) connections() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepted
}

func (f *fakeServer) hasClient() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.busy
}

func has(cmds []command, op byte, param uint32) bool {
	for _, c := range cmds {
		if c.op == op && c.param == param {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
