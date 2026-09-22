package scan

import (
	"context"
	"testing"
	"time"
)

// countingSource is a source that can retune and can drain, recording how
// often each was asked for.
type countingSource struct {
	tunes  int
	drains int
	fill   byte
}

func (c *countingSource) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = c.fill
	}
	return len(p), nil
}

func (c *countingSource) Tune(uint32) error { c.tunes++; return nil }
func (c *countingSource) Close() error      { return nil }

func (c *countingSource) Drain(time.Duration, int) (int, error) {
	c.drains++
	return 0, nil
}

// plainSource can retune but has no Drain, which is the fallback path.
type plainSource struct {
	tunes int
	reads int
}

func (p *plainSource) Read(b []byte) (int, error) {
	p.reads++
	for i := range b {
		b[i] = 0x80
	}
	return len(b), nil
}

func (p *plainSource) Tune(uint32) error { p.tunes++; return nil }
func (p *plainSource) Close() error      { return nil }

// testConfig is a sweep small enough to run in a test: two steps, a short
// dwell, and no real settling delay to wait through.
func testConfig() Config {
	return Config{
		Start:      100_000_000,
		Stop:       102_000_000,
		SampleRate: 2_000_000,
		BinCount:   64,
		Dwell:      time.Millisecond,
		Settle:     time.Millisecond,
		Crop:       0.75,
	}
}

// The sweeper reaches for Drain through an unexported interface, matched
// by shape rather than by name. Nothing else in the tree notices if a
// source stops satisfying it — the sweep simply gets quietly slower,
// falling back to reading and discarding a Settle's worth of samples at
// every step. This test is what notices.
func TestSweeperUsesDrain(t *testing.T) {
	src := &countingSource{fill: 0x80}
	sw := New(src, testConfig())

	if _, err := sw.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if src.tunes == 0 {
		t.Fatal("the sweep never retuned")
	}
	if src.drains != src.tunes {
		t.Errorf("drained %d times for %d retunes; every step must drop the "+
			"samples captured before it moved", src.drains, src.tunes)
	}
}

// A source without Drain must still sweep — correctly, just more slowly.
func TestSweeperFallsBackWithoutDrain(t *testing.T) {
	src := &plainSource{}
	sw := New(src, testConfig())

	if _, err := sw.Sweep(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if src.tunes == 0 {
		t.Fatal("the sweep never retuned")
	}
	if src.reads == 0 {
		t.Fatal("the sweep never read any samples")
	}
}

// The sweeper is handed an sdr.Source, so the drainer match has to survive
// being passed through that interface rather than only working on the
// concrete type.
func TestDrainerMatchesThroughSourceInterface(t *testing.T) {
	var src interface {
		Read([]byte) (int, error)
		Tune(uint32) error
		Close() error
	} = &countingSource{fill: 0x80}

	if _, ok := src.(drainer); !ok {
		t.Fatal("a source with Drain is not recognised as a drainer; " +
			"the sweep would silently take the slow path")
	}
}
