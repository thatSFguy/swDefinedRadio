package sdr

import (
	"context"
	"io"
	"math"
	"os"
	"testing"
	"time"
)

// These tests need a dongle attached, so they are skipped unless asked
// for: SDR_HARDWARE=1 go test ./internal/sdr/ -run Hardware -v
//
// They exist because the whole design of switching receivers without
// restarting the radio rests on assumptions about what rtl_tcp will do
// on real hardware, and those cannot be established against a fake.
func requireRadio(t *testing.T) {
	t.Helper()
	if os.Getenv("SDR_HARDWARE") != "1" {
		t.Skip("needs a dongle; set SDR_HARDWARE=1 to run")
	}
}

// measureRate reports the observed sample rate, counting two bytes per
// sample. What the tuner was told is not evidence; what arrives is.
//
// It reads flat out for a warm-up period first and throws that away.
// Whatever had queued up — in the socket, and in the fifteen buffers
// rtl_tcp allocates — arrives as fast as the link will carry it, so
// measuring immediately reports the speed of the backlog draining rather
// than the speed of the radio. Draining first does not help: Drain gives
// up when the stream goes quiet, and a stream running at four megabytes a
// second never does. Only once the reader has caught up is the rate that
// arrives the rate the tuner is running at.
func measureRate(t *testing.T, src io.Reader, warmup, d time.Duration) float64 {
	t.Helper()
	buf := make([]byte, 1<<16)

	until := time.Now().Add(warmup)
	for time.Now().Before(until) {
		if _, err := src.Read(buf); err != nil {
			t.Fatalf("read during warm-up: %v", err)
		}
	}

	var total int64
	start := time.Now()
	until = start.Add(d)
	for time.Now().Before(until) {
		n, err := src.Read(buf)
		total += int64(n)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	return float64(total) / 2 / time.Since(start).Seconds()
}

// The assumption everything rests on: that a sample rate commanded over
// the rtl_tcp protocol is honoured by a server that is already streaming.
//
// If this fails, switching receivers cannot be a reconfiguration and the
// broker has to restart rtl_tcp whenever the rate changes — which works,
// but costs seconds rather than milliseconds on every such switch.
func TestHardwareSampleRateChangeMidStream(t *testing.T) {
	requireRadio(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const (
		adsbRate = 2_000_000 // what the Mode S slicer needs
		fmRate   = 1_200_000 // what the FM demodulator needs
	)

	src, stop, err := EnsureRTLTCP(ctx, "127.0.0.1:1234", Config{
		CenterFreq: 1_090_000_000, SampleRate: adsbRate, Gain: AutoGain,
	})
	if err != nil {
		t.Fatalf("rtl_tcp: %v", err)
	}
	defer stop()
	defer src.Close()
	t.Logf("tuner: %s", src.Tuner)

	// Let the server settle before believing anything it sends.
	time.Sleep(time.Second)
	if _, err := src.Drain(50*time.Millisecond, 8<<20); err != nil {
		t.Fatalf("drain: %v", err)
	}

	first := measureRate(t, src, 2*time.Second, 3*time.Second)
	t.Logf("commanded %d Hz, measured %.0f Hz", adsbRate, first)
	if rel := math.Abs(first-adsbRate) / adsbRate; rel > 0.05 {
		t.Fatalf("baseline rate is %.0f Hz, %.1f%% off the commanded %d",
			first, rel*100, adsbRate)
	}

	// The switch a tab change makes: command a new rate on the running
	// session, clear what was captured under the old one, and look again.
	if err := src.command(cmdSetSampleRate, fmRate); err != nil {
		t.Fatalf("set sample rate: %v", err)
	}
	time.Sleep(time.Second)
	src.ResetBuffer()
	if _, err := src.Drain(50*time.Millisecond, 16<<20); err != nil {
		t.Fatalf("drain after retune: %v", err)
	}

	second := measureRate(t, src, 2*time.Second, 3*time.Second)
	t.Logf("commanded %d Hz, measured %.0f Hz", fmRate, second)
	if rel := math.Abs(second-fmRate) / fmRate; rel > 0.05 {
		t.Errorf("rate did not take: measured %.0f Hz, %.1f%% off the commanded %d\n"+
			"\tthe broker will need RateChange=RestartServer", second, rel*100, fmRate)
	}

	// And back again, because a tab switch goes both ways.
	if err := src.command(cmdSetSampleRate, adsbRate); err != nil {
		t.Fatalf("set sample rate back: %v", err)
	}
	time.Sleep(time.Second)
	src.ResetBuffer()
	if _, err := src.Drain(50*time.Millisecond, 16<<20); err != nil {
		t.Fatalf("drain: %v", err)
	}
	third := measureRate(t, src, 2*time.Second, 3*time.Second)
	t.Logf("commanded %d Hz, measured %.0f Hz", adsbRate, third)
	if rel := math.Abs(third-adsbRate) / adsbRate; rel > 0.05 {
		t.Errorf("rate did not come back: measured %.0f Hz, %.1f%% off %d",
			third, rel*100, adsbRate)
	}
}

// Retuning mid-stream is what the scanner does thousands of times a
// sweep, and what a tab switch does once. Both need the frequency to
// actually move.
func TestHardwareRetuneMidStream(t *testing.T) {
	requireRadio(t)

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	src, stop, err := EnsureRTLTCP(ctx, "127.0.0.1:1234", Config{
		CenterFreq: 98_700_000, SampleRate: 1_200_000, Gain: AutoGain,
	})
	if err != nil {
		t.Fatalf("rtl_tcp: %v", err)
	}
	defer stop()
	defer src.Close()

	time.Sleep(time.Second)
	for _, hz := range []uint32{98_700_000, 1_090_000_000, 315_000_000, 98_700_000} {
		if err := src.Tune(hz); err != nil {
			t.Fatalf("tune %d: %v", hz, err)
		}
		time.Sleep(300 * time.Millisecond)
		src.ResetBuffer()
		if _, err := src.Drain(50*time.Millisecond, 8<<20); err != nil {
			t.Fatalf("drain: %v", err)
		}
		// Samples must keep arriving after every move.
		buf := make([]byte, 1<<16)
		if _, err := io.ReadFull(src, buf); err != nil {
			t.Fatalf("no samples after tuning to %d: %v", hz, err)
		}
	}
}

// rtl_tcp serves one client at a time. A second one is accepted by the
// kernel into the listen backlog and then never spoken to, so a dial that
// waits indefinitely for the greeting hangs for as long as the first
// client stays connected. This is that situation, against the real
// server, and it must end in an error rather than a wedged goroutine.
func TestHardwareSecondClientIsRefusedNotHung(t *testing.T) {
	requireRadio(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	held, stop, err := EnsureRTLTCP(ctx, "127.0.0.1:1234", Config{
		CenterFreq: 433_920_000, SampleRate: 250_000, Gain: AutoGain,
	})
	if err != nil {
		t.Fatalf("rtl_tcp: %v", err)
	}
	defer stop()
	defer held.Close()

	// Keep the session busy, the way an active receiver would.
	go func() {
		buf := make([]byte, 1<<16)
		for ctx.Err() == nil {
			if _, err := held.Read(buf); err != nil {
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond)

	done := make(chan error, 1)
	start := time.Now()
	go func() {
		second, err := DialRTLTCP("127.0.0.1:1234", Config{CenterFreq: 98_700_000})
		if second != nil {
			second.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		took := time.Since(start)
		if err == nil {
			t.Fatal("a second client got a greeting while the first was being served")
		}
		t.Logf("refused after %v: %v", took.Round(time.Millisecond), err)
		if took > greetingTimeout+2*time.Second {
			t.Errorf("took %v to give up, want about %v", took, greetingTimeout)
		}
	case <-time.After(greetingTimeout + 8*time.Second):
		t.Fatal("dialling a busy rtl_tcp hung — this is the bug the greeting deadline fixes")
	}
}
