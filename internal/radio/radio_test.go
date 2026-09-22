package radio

import (
	"context"
	"errors"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
)

// newTestBroker returns a broker pointed at a fake server that is already
// running, so nothing tries to start an rtl_tcp process.
func newTestBroker(t *testing.T) (*Broker, *fakeServer) {
	t.Helper()
	f := startFakeServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	b := New(ctx, Options{Addr: f.addr, Settle: 10 * time.Millisecond})
	// Pretend the server is ours, which it is for the purposes of every
	// test here: what is being tested is the handover, not the spawning.
	b.srv = &server{}
	t.Cleanup(func() { b.Release(); b.dropConn() })
	return b, f
}

func need(freq, rate uint32) Need {
	return Need{Mode: Samples, Tune: sdr.Config{
		CenterFreq: freq, SampleRate: rate, Gain: sdr.AutoGain,
	}}
}

// The settings a receiver asks for have to reach the radio. Without this
// the hub would appear to switch receivers while leaving the tuner where
// it was, and the only symptom would be a receiver hearing nothing.
func TestAcquireCommandsTheTuner(t *testing.T) {
	b, f := newTestBroker(t)

	h, err := b.Acquire(context.Background(), need(1_090_000_000, 2_000_000))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if h.Stream == nil {
		t.Fatal("Samples mode returned no stream")
	}
	waitFor(t, "the tuner settings", func() bool {
		s := f.sent()
		return has(s, cmdSetSampleRate, 2_000_000) && has(s, cmdSetFreq, 1_090_000_000)
	})
}

// The hazard the whole design turns on. A receive loop is parked in a
// blocking read; the radio is handed to another receiver; that read has
// to end. Cancelling a context cannot do it, because nothing downstream
// is watching one — the goroutine is inside a socket read.
func TestReleaseUnblocksAParkedRead(t *testing.T) {
	b, _ := newTestBroker(t)

	h, err := b.Acquire(context.Background(), need(98_700_000, 1_200_000))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// A receiver's loop, asking for more than the trickle will satisfy.
	parked := make(chan error, 1)
	go func() {
		buf := make([]byte, 1<<20)
		_, err := io.ReadFull(h.Stream, buf)
		parked <- err
	}()
	time.Sleep(50 * time.Millisecond)

	b.Release()

	select {
	case err := <-parked:
		if !errors.Is(err, ErrStopped) {
			t.Fatalf("read ended with %v, want ErrStopped", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the read was not released; the receiver would hold the radio for ever")
	}
}

// Releasing must not close the session. The next receiver inherits it,
// and re-establishing it means waiting for the server to come back round
// its accept loop.
func TestHandoverKeepsOneConnection(t *testing.T) {
	b, f := newTestBroker(t)

	if _, err := b.Acquire(context.Background(), need(1_090_000_000, 2_000_000)); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	after := f.connections()

	for range 5 {
		if _, err := b.Acquire(context.Background(), need(98_700_000, 1_200_000)); err != nil {
			t.Fatalf("acquire: %v", err)
		}
	}
	if got := f.connections(); got != after {
		t.Errorf("made %d connections across five handovers, want the original %d",
			got-after+after, after)
	}
}

// A revoked stream must stay dead even if the receiver holding it keeps
// trying, or a receiver that has been set aside goes on reading samples
// meant for whoever has the radio now.
func TestRevokedStreamStaysDead(t *testing.T) {
	b, _ := newTestBroker(t)

	first, err := b.Acquire(context.Background(), need(1_090_000_000, 2_000_000))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	second, err := b.Acquire(context.Background(), need(98_700_000, 1_200_000))
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}

	buf := make([]byte, 64)
	if _, err := first.Stream.Read(buf); !errors.Is(err, ErrStopped) {
		t.Errorf("the superseded stream read %v, want ErrStopped", err)
	}
	if err := first.Stream.Tune(90_000_000); !errors.Is(err, ErrStopped) {
		t.Errorf("the superseded stream tuned %v, want ErrStopped", err)
	}
	if _, err := second.Stream.Read(buf); err != nil {
		t.Errorf("the current stream cannot read: %v", err)
	}
}

// Samples captured before the tuner moved describe the wrong frequency.
// Handing those to the next receiver is how a strong carrier turns into
// a ghost one step away — the artefact the scanner's settle exists for.
func TestSwitchDropsStaleSamples(t *testing.T) {
	b, f := newTestBroker(t)

	f.setFill(0xAA) // what the old receiver was hearing
	h, err := b.Acquire(context.Background(), need(1_090_000_000, 2_000_000))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	buf := make([]byte, 1<<16)
	if _, err := io.ReadFull(h.Stream, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Let a backlog of old samples pile up while nobody is reading.
	time.Sleep(100 * time.Millisecond)

	f.setFill(0x55) // what the new receiver should hear
	h2, err := b.Acquire(context.Background(), need(98_700_000, 1_200_000))
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if _, err := io.ReadFull(h2.Stream, buf); err != nil {
		t.Fatalf("read after switch: %v", err)
	}
	for i, v := range buf {
		if v == 0xAA {
			t.Fatalf("byte %d is from before the switch — stale samples survived", i)
		}
	}
}

// Address mode is for rtl_433, which opens its own connection. Since the
// server serves one client at a time, the broker's own has to be gone.
func TestAddressModeReleasesTheSocket(t *testing.T) {
	b, f := newTestBroker(t)

	if _, err := b.Acquire(context.Background(), need(1_090_000_000, 2_000_000)); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	waitFor(t, "the broker's own connection", f.hasClient)

	h, err := b.Acquire(context.Background(), Need{Mode: Address})
	if err != nil {
		t.Fatalf("address mode: %v", err)
	}
	if h.Addr != f.addr {
		t.Errorf("handed over %q, want %q", h.Addr, f.addr)
	}
	if h.Stream != nil {
		t.Error("address mode handed over a stream as well")
	}
	waitFor(t, "the broker to let go of the socket", func() bool { return !f.hasClient() })
}

// And back again: a receiver wanting samples after rtl_433 has finished
// has to be able to get the session back.
func TestSamplesAfterAddressModeReconnects(t *testing.T) {
	b, f := newTestBroker(t)

	if _, err := b.Acquire(context.Background(), Need{Mode: Address}); err != nil {
		t.Fatalf("address mode: %v", err)
	}
	h, err := b.Acquire(context.Background(), need(1_090_000_000, 2_000_000))
	if err != nil {
		t.Fatalf("back to samples: %v", err)
	}
	if h.Stream == nil {
		t.Fatal("no stream after returning from address mode")
	}
	waitFor(t, "the tuner settings", func() bool {
		return has(f.sent(), cmdSetFreq, 1_090_000_000)
	})
}

// Switching tabs is something a person does idly and repeatedly. Leaking
// a goroutine or a socket per switch would be invisible for an hour and
// fatal for an afternoon.
func TestManySwitchesLeakNothing(t *testing.T) {
	b, f := newTestBroker(t)

	// Settle first: the fake's own writer goroutine and the initial
	// connection should not be counted as a leak.
	if _, err := b.Acquire(context.Background(), need(1_090_000_000, 2_000_000)); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	before := runtime.NumGoroutine()
	conns := f.connections()

	for i := range 100 {
		n := need(98_700_000, 1_200_000)
		if i%2 == 0 {
			n = need(1_090_000_000, 2_000_000)
		}
		h, err := b.Acquire(context.Background(), n)
		if err != nil {
			t.Fatalf("switch %d: %v", i, err)
		}
		buf := make([]byte, 1024)
		if _, err := io.ReadFull(h.Stream, buf); err != nil {
			t.Fatalf("read on switch %d: %v", i, err)
		}
	}

	b.Release()
	time.Sleep(200 * time.Millisecond)
	runtime.GC()

	if after := runtime.NumGoroutine(); after > before+5 {
		t.Errorf("goroutines went from %d to %d over 100 switches", before, after)
	}
	if got := f.connections(); got != conns {
		t.Errorf("made %d new connections over 100 switches, want 0", got-conns)
	}
}

// State is what a status endpoint reports, and it has to tell the truth
// about whether anyone actually holds the radio.
func TestStateReportsWhoHoldsTheRadio(t *testing.T) {
	b, _ := newTestBroker(t)

	if st := b.State(); st.Held {
		t.Error("state says the radio is held before anything acquired it")
	}
	if _, err := b.Acquire(context.Background(), need(1_090_000_000, 2_000_000)); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	st := b.State()
	if !st.Held || st.Mode != "samples" || st.Rate != 2_000_000 {
		t.Errorf("state = %+v, want held in samples mode at 2 Msps", st)
	}
	b.Release()
	if st := b.State(); st.Held {
		t.Error("state still says the radio is held after Release")
	}
}
