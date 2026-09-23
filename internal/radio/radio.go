// Package radio owns the dongle on behalf of every receiver.
//
// Only one receiver can use the radio at a time. That is a fact of the
// hardware — one tuner, one converter — and not a limitation of this
// code, so the broker hands out one activation at a time and ends the
// previous one before it starts the next.
//
// What it buys is that handing the radio between receivers is a matter of
// commanding the tuner rather than restarting anything. An rtl_tcp server
// holds the USB device for the life of the process and a single client
// connection is kept open across handovers, so a receiver that is set
// aside and brought back costs a few hundred milliseconds rather than the
// seconds a USB re-claim takes.
//
// Receivers say what they need, never how to arrange it. Three ways of
// needing the radio are enough to cover everything here: most want a
// stream of samples, rtl_433 wants to open its own connection, and if
// that ever stops working it wants the USB device to itself.
package radio

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/thatSFguy/swDefinedRadio/internal/sdr"
)

// Mode is how a receiver reaches the radio.
type Mode int

const (
	// Samples: the broker tunes the radio and hands over a Stream. This
	// is adsb, uat, fm and scanner.
	Samples Mode = iota

	// Address: the broker stands clear of rtl_tcp and hands over its
	// address, for a receiver that spawns a subprocess which connects on
	// its own — rtl_433's -d rtl_tcp:host:port. The broker holds no
	// client socket for the duration, because rtl_tcp serves exactly one
	// client and a second is accepted but never spoken to.
	Address

	// Exclusive: the broker stops rtl_tcp altogether and hands over the
	// device index, leaving the USB device free for a receiver that
	// insists on opening it directly.
	Exclusive
)

func (m Mode) String() string {
	switch m {
	case Samples:
		return "samples"
	case Address:
		return "address"
	case Exclusive:
		return "exclusive"
	}
	return "unknown"
}

// Need is what a receiver requires of the radio.
//
// It is asked for immediately before every activation rather than once at
// startup, so a receiver that has been retuned from its own UI asks for
// where it actually is instead of where it began.
type Need struct {
	Mode Mode

	// Tune is the tuner state for Samples. For Address and Exclusive only
	// DeviceIndex is read: the subprocess chooses its own rate and
	// frequency, and arguing with it about them would achieve nothing.
	Tune sdr.Config
}

// Handle is what an active receiver is given. Exactly one field is
// populated, chosen by the Mode that was asked for, so a receiver reads
// the one field answering its own question and never has to know how the
// radio was arranged.
type Handle struct {
	Stream *Stream // Samples
	Addr   string  // Address: an rtl_tcp with no client attached
	Device int     // Exclusive: rtl_tcp stopped, the USB device free
}

// ErrStopped is what a Stream reports once its activation has ended.
//
// Deliberately not io.EOF: an EOF from the radio means the dongle stopped
// delivering samples and is worth complaining about, whereas this is the
// ordinary end of a receiver's turn.
var ErrStopped = errors.New("radio: this receiver is no longer the active one")

// Stream is one activation's view of the radio.
//
// The connection underneath outlives the activation, because the next
// receiver wants the same rtl_tcp session and re-establishing it means
// waiting for the server to come back round its accept loop. So ending an
// activation cannot close the socket. It sets a read deadline in the past
// instead, which is the only way to get a goroutine out of a blocking
// read on a connection that is meant to survive.
type Stream struct {
	mu      sync.Mutex
	conn    *sdr.RTLTCP
	revoked bool
}

func (s *Stream) stopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revoked
}

// revoke interrupts whatever read is in progress and refuses further use.
func (s *Stream) revoke() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked {
		return
	}
	s.revoked = true
	// A deadline in the past interrupts a read already under way.
	_ = s.conn.SetReadDeadline(time.Now())
}

func (s *Stream) Read(p []byte) (int, error) {
	if s.stopped() {
		return 0, ErrStopped
	}
	n, err := s.conn.Read(p)
	if err != nil && s.stopped() {
		// The deadline set to interrupt the read, not a fault.
		return n, ErrStopped
	}
	return n, err
}

// Tune moves the centre frequency, which fm does from an HTTP handler
// while the receive loop is reading.
func (s *Stream) Tune(hz uint32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revoked {
		return ErrStopped
	}
	return s.conn.Tune(hz)
}

// Close ends the receiver's use of the stream. The broker owns the
// connection's lifetime, so this does not close the socket.
func (s *Stream) Close() error { return nil }

// Drain forwards to the connection.
//
// Without this method the band scanner's search for a drainer fails and
// it silently falls back to reading and discarding a settle's worth of
// samples at every tuning step — which works, but is the slow path the
// drainer exists to avoid.
func (s *Stream) Drain(quiet time.Duration, maxBytes int) (int, error) {
	if s.stopped() {
		return 0, ErrStopped
	}
	return s.conn.Drain(quiet, maxBytes)
}

// Compile-time proof that a Stream is still both a source and a drainer.
// The scanner matches the drainer on shape rather than by name, so this
// is the only thing standing between a rename and a silent slowdown.
var (
	_ sdr.Source = (*Stream)(nil)
	_ interface {
		Drain(time.Duration, int) (int, error)
	} = (*Stream)(nil)
)

// RatePolicy says how to change the sample rate.
type RatePolicy int

const (
	// InBand commands the new rate on the running server. Measured to
	// work on an R820T: a rate commanded mid-stream is honoured to within
	// a fraction of a percent. See internal/sdr's hardware tests.
	InBand RatePolicy = iota

	// RestartServer stops and restarts rtl_tcp whenever the rate changes.
	// Costs seconds rather than milliseconds, and exists for hardware
	// where InBand turns out not to hold.
	RestartServer
)

// Options configure a Broker.
type Options struct {
	Addr   string // rtl_tcp address; defaults to 127.0.0.1:1234
	Device int

	// Settle is how long to wait after reconfiguring before trusting what
	// arrives. Samples already in flight were captured under the old
	// settings, and feeding those to the next receiver is how a strong
	// carrier turns into a ghost one tuning step away.
	Settle time.Duration

	RateChange RatePolicy
}

// drainQuiet is how long the stream must be silent before the backlog is
// considered cleared. Samples arrive from the dongle in bursts, so there
// are gaps between them even while it is running flat out.
const drainQuiet = 3 * time.Millisecond

// drainBudget bounds how long clearing the backlog may take.
//
// Draining stops as soon as the stream falls quiet, which is the usual
// case and takes a few milliseconds. The budget is for the other case: a
// socket left unread while a receiver was stopped can hold far more than
// a settle's worth, and reading all of it could take seconds. Better to
// give up and let a few stale samples through than to make someone wait
// — a receiver that has just been handed the radio will resynchronise on
// its own, whereas a slow tab switch is felt every time.
const drainBudget = 250 * time.Millisecond

// handoverPause is how long to let rtl_tcp return to accepting after our
// own connection is dropped, before telling a receiver to connect.
const handoverPause = 150 * time.Millisecond

const (
	defaultAddr   = "127.0.0.1:1234"
	defaultSettle = 180 * time.Millisecond

	// serverBoot is how long rtl_tcp is given to claim the device and
	// open its socket.
	serverBoot = 8 * time.Second
)

// Broker owns the radio.
type Broker struct {
	mu   sync.Mutex
	o    Options
	ctx  context.Context
	srv  *server     // the rtl_tcp child; nil when stopped
	conn *sdr.RTLTCP // our client connection; nil in Address and Exclusive
	cur  *Stream     // the outstanding activation
	mode Mode
	rate uint32 // what the tuner was last told
	held bool   // an activation is outstanding
}

// New returns a Broker. ctx bounds the rtl_tcp child, so it must be the
// process's lifetime and not any one receiver's.
func New(ctx context.Context, o Options) *Broker {
	if o.Addr == "" {
		o.Addr = defaultAddr
	}
	if o.Settle == 0 {
		o.Settle = defaultSettle
	}
	return &Broker{o: o, ctx: ctx}
}

// Acquire ends whatever activation is outstanding, arranges the radio for
// n, and returns the handle. It blocks until the radio is genuinely ready
// — reconfigured, settled and drained — so that the first sample a
// receiver reads is one it can trust.
func (b *Broker) Acquire(ctx context.Context, n Need) (Handle, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.releaseLocked()

	switch n.Mode {
	case Samples:
		h, err := b.acquireSamples(ctx, n)
		if err != nil {
			return Handle{}, err
		}
		b.mode, b.held = Samples, true
		return h, nil

	case Address:
		if err := b.ensureServer(n.Tune); err != nil {
			return Handle{}, err
		}
		// Starting the server is not the same as the server listening,
		// and handing over an address nothing is bound to yet means the
		// receiver's subprocess is refused and gives up. Connecting
		// once proves it is ready; the connection is then dropped,
		// because rtl_tcp serves one client at a time and that one has
		// to be the receiver's.
		if _, err := b.ensureConn(n.Tune); err != nil {
			return Handle{}, err
		}
		b.dropConn()
		// Let the server reap that connection and get back to accepting
		// before the receiver's subprocess tries. The kernel's backlog
		// covers this in practice, but a client that does not retry —
		// rtl_433 gives up on the first refusal — deserves the margin.
		time.Sleep(handoverPause)
		b.mode, b.held = Address, true
		return Handle{Addr: b.o.Addr}, nil

	case Exclusive:
		b.stopServer()
		b.mode, b.held = Exclusive, true
		return Handle{Device: b.o.Device}, nil
	}
	return Handle{}, fmt.Errorf("radio: unknown mode %d", n.Mode)
}

func (b *Broker) acquireSamples(ctx context.Context, n Need) (Handle, error) {
	cfg := n.Tune
	cfg.DeviceIndex = b.o.Device

	// A rate change means a restart only where the hardware needs one.
	if b.o.RateChange == RestartServer && b.srv != nil &&
		cfg.SampleRate != 0 && cfg.SampleRate != b.rate {
		log.Printf("radio: sample rate %d to %d, restarting rtl_tcp", b.rate, cfg.SampleRate)
		b.stopServer()
	}

	if err := b.ensureServer(cfg); err != nil {
		return Handle{}, err
	}
	conn, err := b.ensureConn(cfg)
	if err != nil {
		return Handle{}, err
	}

	// Whatever is buffered was captured under the old settings, and bufio
	// remembers the error that ended an interrupted read. Both have to go
	// before the new settings are applied.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return Handle{}, fmt.Errorf("radio: clear deadline: %w", err)
	}
	conn.ResetBuffer()

	if err := conn.Apply(cfg); err != nil {
		return Handle{}, fmt.Errorf("radio: configure: %w", err)
	}
	b.rate = cfg.SampleRate

	// Let the change work through, then drop what was captured while it
	// did. Draining alone is not enough: the stale samples are still
	// arriving until the tuner has actually moved.
	select {
	case <-ctx.Done():
		return Handle{}, ctx.Err()
	case <-time.After(b.o.Settle):
	}
	conn.ResetBuffer()
	if _, err := conn.DrainFor(drainQuiet, drainBudget); err != nil {
		return Handle{}, fmt.Errorf("radio: drain: %w", err)
	}

	b.cur = &Stream{conn: conn}
	return Handle{Stream: b.cur}, nil
}

// Release ends the current activation. The rtl_tcp server stays up.
func (b *Broker) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.releaseLocked()
}

func (b *Broker) releaseLocked() {
	if b.cur != nil {
		b.cur.revoke()
		b.cur = nil
	}
	b.held = false
}

// ensureServer starts rtl_tcp if it is not running.
func (b *Broker) ensureServer(cfg sdr.Config) error {
	if b.srv != nil {
		return nil
	}
	cfg.DeviceIndex = b.o.Device
	srv, err := startServer(b.ctx, b.o.Addr, cfg)
	if err != nil {
		return err
	}
	b.srv = srv
	b.rate = cfg.SampleRate
	return nil
}

func (b *Broker) stopServer() {
	b.dropConn()
	if b.srv != nil {
		b.srv.stop()
		b.srv = nil
		b.rate = 0
	}
}

func (b *Broker) dropConn() {
	if b.conn != nil {
		_ = b.conn.Close()
		b.conn = nil
	}
}

// ensureConn dials the server, retrying while it is still coming up or
// while a previous client is still letting go.
func (b *Broker) ensureConn(cfg sdr.Config) (*sdr.RTLTCP, error) {
	if b.conn != nil {
		return b.conn, nil
	}
	deadline := time.Now().Add(serverBoot)
	var last error
	for time.Now().Before(deadline) {
		c, err := sdr.DialRTLTCP(b.o.Addr, cfg)
		if err == nil {
			b.conn = c
			return c, nil
		}
		last = err
		select {
		case <-b.ctx.Done():
			return nil, b.ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("radio: no rtl_tcp at %s: %w", b.o.Addr, last)
}

// Close stops everything the broker started.
func (b *Broker) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.releaseLocked()
	b.stopServer()
}

// State is what the radio is doing, for a status endpoint.
type State struct {
	Running bool   `json:"running"` // is rtl_tcp up
	Held    bool   `json:"held"`    // is a receiver using it
	Mode    string `json:"mode"`
	Addr    string `json:"addr"`
	Rate    uint32 `json:"sample_rate"`
}

func (b *Broker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return State{
		Running: b.srv != nil,
		Held:    b.held,
		Mode:    b.mode.String(),
		Addr:    b.o.Addr,
		Rate:    b.rate,
	}
}

// server is the rtl_tcp child process.
type server struct {
	cmd *exec.Cmd
}

func startServer(ctx context.Context, addr string, cfg sdr.Config) (*server, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("radio: bad rtl_tcp address %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	args := []string{"-a", host, "-p", port, "-d", strconv.Itoa(cfg.DeviceIndex)}
	if cfg.CenterFreq != 0 {
		args = append(args, "-f", strconv.FormatUint(uint64(cfg.CenterFreq), 10))
	}
	if cfg.SampleRate != 0 {
		args = append(args, "-s", strconv.FormatUint(uint64(cfg.SampleRate), 10))
	}

	cmd := exec.CommandContext(ctx, "rtl_tcp", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("radio: start rtl_tcp (is rtl-sdr installed?): %w", err)
	}
	return &server{cmd: cmd}, nil
}

func (s *server) stop() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
}
