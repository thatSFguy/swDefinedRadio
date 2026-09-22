package radio

import (
	"context"
	"io"
	"math"
	"os"
	"testing"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
)

// Needs a dongle: SDR_HARDWARE=1 go test ./internal/radio/ -run Hardware -v
func requireRadio(t *testing.T) {
	t.Helper()
	if os.Getenv("SDR_HARDWARE") != "1" {
		t.Skip("needs a dongle; set SDR_HARDWARE=1 to run")
	}
}

// observedRate reports the sample rate actually arriving, after reading
// flat out long enough to be past whatever had queued up. What the tuner
// was told is not evidence; what arrives is.
func observedRate(t *testing.T, r io.Reader, warmup, window time.Duration) float64 {
	t.Helper()
	buf := make([]byte, 1<<16)
	until := time.Now().Add(warmup)
	for time.Now().Before(until) {
		if _, err := r.Read(buf); err != nil {
			t.Fatalf("read during warm-up: %v", err)
		}
	}
	var total int64
	start := time.Now()
	until = start.Add(window)
	for time.Now().Before(until) {
		n, err := r.Read(buf)
		total += int64(n)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	return float64(total) / 2 / time.Since(start).Seconds()
}

// The whole point of the broker, against real hardware: handing the radio
// from one receiver to another leaves the tuner running at what the new
// receiver asked for, without restarting anything.
//
// ADS-B and FM are the pair worth testing because their rates differ by
// enough to be unmistakable — if a switch silently left the radio at the
// other one's rate, FM would play at 1.67 times speed and ADS-B would
// decode nothing at all.
func TestHardwareBrokerSwitchesBetweenReceivers(t *testing.T) {
	requireRadio(t)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	b := New(ctx, Options{Settle: 200 * time.Millisecond})
	defer b.Close()

	want := []struct {
		name string
		freq uint32
		rate uint32
	}{
		{"adsb", 1_090_000_000, 2_000_000},
		{"fm", 98_700_000, 1_200_000},
		{"scanner", 100_000_000, 2_400_000},
		{"adsb again", 1_090_000_000, 2_000_000},
	}

	for _, w := range want {
		start := time.Now()
		h, err := b.Acquire(ctx, Need{Mode: Samples, Tune: sdr.Config{
			CenterFreq: w.freq, SampleRate: w.rate, Gain: sdr.AutoGain,
		}})
		if err != nil {
			t.Fatalf("%s: acquire: %v", w.name, err)
		}
		took := time.Since(start)

		got := observedRate(t, h.Stream, 1500*time.Millisecond, 2*time.Second)
		rel := math.Abs(got-float64(w.rate)) / float64(w.rate)
		t.Logf("%-11s switch in %-6v  commanded %7d Hz  measured %9.0f Hz  (%.2f%% off)",
			w.name, took.Round(time.Millisecond), w.rate, got, rel*100)
		if rel > 0.05 {
			t.Errorf("%s: radio is at %.0f Hz, not the %d it was given", w.name, got, w.rate)
		}
	}
}

// A receiver parked in a read must be released when the radio is handed
// on — against the real server, where the samples are real and the socket
// is a real socket.
func TestHardwareReleaseUnblocksParkedRead(t *testing.T) {
	requireRadio(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b := New(ctx, Options{Settle: 200 * time.Millisecond})
	defer b.Close()

	h, err := b.Acquire(ctx, Need{Mode: Samples, Tune: sdr.Config{
		CenterFreq: 98_700_000, SampleRate: 1_200_000, Gain: sdr.AutoGain,
	}})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	parked := make(chan error, 1)
	go func() {
		buf := make([]byte, 64<<20) // far more than will ever arrive
		_, err := io.ReadFull(h.Stream, buf)
		parked <- err
	}()
	time.Sleep(500 * time.Millisecond)

	start := time.Now()
	b.Release()
	select {
	case err := <-parked:
		t.Logf("released in %v: %v", time.Since(start).Round(time.Millisecond), err)
		if err != ErrStopped {
			t.Errorf("read ended with %v, want ErrStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked read was never released")
	}
}

// rtl_433 opens its own connection, so the broker has to let go of the
// one it holds. Against the real server this is the thing that would
// otherwise leave the TPMS tab silently receiving nothing.
func TestHardwareAddressModeHandsOverTheSession(t *testing.T) {
	requireRadio(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	b := New(ctx, Options{Settle: 200 * time.Millisecond})
	defer b.Close()

	if _, err := b.Acquire(ctx, Need{Mode: Samples, Tune: sdr.Config{
		CenterFreq: 1_090_000_000, SampleRate: 2_000_000, Gain: sdr.AutoGain,
	}}); err != nil {
		t.Fatalf("samples: %v", err)
	}

	h, err := b.Acquire(ctx, Need{Mode: Address})
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	if h.Addr == "" {
		t.Fatal("address mode handed over no address")
	}

	// The broker has let go, so a fresh client must be able to get in —
	// which is exactly what rtl_433 will be doing.
	c, err := sdr.DialRTLTCP(h.Addr, sdr.Config{
		CenterFreq: 433_920_000, SampleRate: 250_000, Gain: sdr.AutoGain,
	})
	if err != nil {
		t.Fatalf("a client could not take over the session: %v", err)
	}
	defer c.Close()
	buf := make([]byte, 1<<16)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("no samples for the new client: %v", err)
	}
	t.Log("a second client took over the session and is receiving")
}
