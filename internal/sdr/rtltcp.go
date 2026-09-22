package sdr

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// rtl_tcp command opcodes. Each command is one opcode byte followed by a
// big-endian uint32 argument.
const (
	cmdSetFreq       = 0x01
	cmdSetSampleRate = 0x02
	cmdSetGainMode   = 0x03
	cmdSetGain       = 0x04
	cmdSetFreqCorr   = 0x05
	cmdSetDirectSamp = 0x09
	cmdSetBiasTee    = 0x0e
)

// greetingTimeout bounds the wait for an rtl_tcp server's opening bytes.
// It arrives immediately or not at all, so this only has to be long
// enough to cross a loopback connection.
const greetingTimeout = 2 * time.Second

// Direct sampling modes. Normally the tuner chip mixes the wanted band
// down to an intermediate frequency, which is why an R820T cannot go
// below about 24 MHz. Direct sampling bypasses the tuner and feeds the
// ADC straight from the antenna pin, reaching from DC up to half the
// 28.8 MHz reference — the shortwave range.
//
// It only works if the hardware routes an antenna to that pin. Most
// dongles need a soldering modification; a few are wired for it. On an
// unmodified dongle the mode still enables, but there is nothing
// connected, so the result is the receiver's own noise.
const (
	DirectOff = 0
	DirectI   = 1
	DirectQ   = 2 // the usual choice where the mod is fitted
)

// TunerType is the tuner chip reported in the rtl_tcp greeting. It is
// worth surfacing because the chips differ in tuning range: an R820T
// stops around 1.76 GHz, an E4000 has a gap around 1.1 GHz.
type TunerType uint32

const (
	TunerUnknown TunerType = iota
	TunerE4000
	TunerFC0012
	TunerFC0013
	TunerFC2580
	TunerR820T
	TunerR828D
)

func (t TunerType) String() string {
	switch t {
	case TunerE4000:
		return "E4000"
	case TunerFC0012:
		return "FC0012"
	case TunerFC0013:
		return "FC0013"
	case TunerFC2580:
		return "FC2580"
	case TunerR820T:
		return "R820T/R820T2"
	case TunerR828D:
		return "R828D"
	}
	return "unknown"
}

// RTLTCP streams IQ from an rtl_tcp server.
type RTLTCP struct {
	conn net.Conn
	br   *bufio.Reader

	Tuner     TunerType
	GainCount uint32
}

// DialRTLTCP connects to an rtl_tcp server and applies cfg.
func DialRTLTCP(addr string, cfg Config) (*RTLTCP, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial rtl_tcp %s: %w", addr, err)
	}

	// The server opens with a 12-byte greeting: magic, tuner, gain count.
	//
	// It comes straight away or not at all, so the read is bounded. This
	// matters because rtl_tcp serves one client at a time: a second one is
	// accepted by the kernel into the listen backlog and then never spoken
	// to, so an unbounded read here waits for as long as the other client
	// stays connected — which, for a long-lived receiver, is forever.
	if err := conn.SetReadDeadline(time.Now().Add(greetingTimeout)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("rtl_tcp read deadline: %w", err)
	}
	var hdr [12]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		conn.Close()
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, fmt.Errorf("no greeting from rtl_tcp at %s within %s: another client is probably still connected", addr, greetingTimeout)
		}
		return nil, fmt.Errorf("read rtl_tcp greeting: %w", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("clear rtl_tcp read deadline: %w", err)
	}
	if string(hdr[:4]) != "RTL0" {
		conn.Close()
		return nil, fmt.Errorf("not an rtl_tcp server at %s", addr)
	}

	r := &RTLTCP{
		conn:      conn,
		br:        bufio.NewReaderSize(conn, 1<<20),
		Tuner:     TunerType(binary.BigEndian.Uint32(hdr[4:8])),
		GainCount: binary.BigEndian.Uint32(hdr[8:12]),
	}
	if err := r.apply(cfg); err != nil {
		conn.Close()
		return nil, err
	}
	return r, nil
}

func (r *RTLTCP) command(cmd byte, param uint32) error {
	var b [5]byte
	b[0] = cmd
	binary.BigEndian.PutUint32(b[1:], param)
	if _, err := r.conn.Write(b[:]); err != nil {
		return fmt.Errorf("rtl_tcp command %#02x: %w", cmd, err)
	}
	return nil
}

func (r *RTLTCP) apply(cfg Config) error {
	cmds := []struct {
		op    byte
		param uint32
		when  bool
	}{
		// Direct sampling has to be set before the frequency: the tuning
		// range it allows is completely different.
		{cmdSetDirectSamp, uint32(cfg.DirectSampling), cfg.DirectSampling != 0},
		{cmdSetSampleRate, cfg.SampleRate, cfg.SampleRate != 0},
		{cmdSetFreq, cfg.CenterFreq, cfg.CenterFreq != 0},
		{cmdSetGainMode, 0, cfg.Gain < 0},
		{cmdSetGainMode, 1, cfg.Gain >= 0},
		{cmdSetGain, uint32(cfg.Gain), cfg.Gain >= 0},
		{cmdSetFreqCorr, uint32(cfg.FreqCorrection), cfg.FreqCorrection != 0},
		{cmdSetBiasTee, 1, cfg.BiasTee},
	}
	for _, c := range cmds {
		if !c.when {
			continue
		}
		if err := r.command(c.op, c.param); err != nil {
			return err
		}
	}
	return nil
}

func (r *RTLTCP) Read(p []byte) (int, error) { return r.br.Read(p) }

// Drain throws away everything already buffered, returning once the
// connection has gone quiet for the given interval or maxBytes have been
// discarded. It returns how much was dropped.
//
// This exists because retuning does not affect samples already in
// flight. Those were captured at the old frequency, and a sweep that
// folds them into the new segment shows ghosts of strong carriers
// displaced by exactly one tuning step. Draining until the socket runs
// dry adapts to however much the server happens to be holding, which a
// fixed delay cannot do without being wastefully long.
func (r *RTLTCP) Drain(quiet time.Duration, maxBytes int) (int, error) {
	buf := make([]byte, 1<<16)
	total := 0
	for total < maxBytes {
		if err := r.conn.SetReadDeadline(time.Now().Add(quiet)); err != nil {
			return total, err
		}
		n, err := r.br.Read(buf)
		total += n
		if err != nil {
			// A timeout is the expected exit: nothing more is waiting.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				break
			}
			_ = r.conn.SetReadDeadline(time.Time{})
			return total, err
		}
	}
	return total, r.conn.SetReadDeadline(time.Time{})
}

// Apply sends a whole tuner configuration to a connection that is
// already running.
//
// This is what makes handing the radio from one receiver to another a
// reconfiguration rather than a restart: centre frequency, sample rate,
// gain and the rest are all settable over the protocol on a live session.
// Measured on an R820T, a sample rate commanded this way is honoured to
// within a fraction of a percent — see the hardware tests.
func (r *RTLTCP) Apply(cfg Config) error { return r.apply(cfg) }

// SetReadDeadline bounds, or interrupts, a read on the connection.
//
// This is how a receiver parked in a blocking Read is let go of the radio
// without tearing the session down. Closing the connection would also
// interrupt the read, but rtl_tcp serves one client at a time and takes a
// moment to come back round its accept loop, so a receiver that is only
// being set aside keeps its socket and is simply stopped from reading on
// it. A deadline in the past interrupts a read already in progress.
func (r *RTLTCP) SetReadDeadline(t time.Time) error { return r.conn.SetReadDeadline(t) }

// ResetBuffer discards everything buffered and forgets any error the last
// read ended with.
//
// Both halves matter after the tuner has been reconfigured. The buffered
// bytes were captured under the old settings, and bufio remembers a read
// error and hands it back once more on the next call — so a Drain that
// followed an interrupted read would see that stale timeout, conclude the
// socket had gone quiet, and return having dropped nothing at all.
func (r *RTLTCP) ResetBuffer() { r.br.Reset(r.conn) }

// DrainFor discards everything buffered until the stream goes quiet or
// the budget elapses, returning how much was dropped.
//
// Drain bounds itself by bytes, which suits a sweep: the backlog after a
// retune is one settle's worth and its size is known. It does not suit
// handing the radio between receivers, where the socket may have gone
// unread for as long as nobody was listening and the backlog is however
// large that made it. A budget in time bounds the delay directly, which
// is the thing that actually matters when someone is waiting for a tab to
// come up.
func (r *RTLTCP) DrainFor(quiet, budget time.Duration) (int, error) {
	buf := make([]byte, 1<<16)
	deadline := time.Now().Add(budget)
	total := 0
	for {
		now := time.Now()
		if !now.Before(deadline) {
			break
		}
		wait := quiet
		if left := deadline.Sub(now); left < wait {
			wait = left
		}
		if err := r.conn.SetReadDeadline(now.Add(wait)); err != nil {
			return total, err
		}
		n, err := r.br.Read(buf)
		total += n
		if err != nil {
			// A timeout means either the stream went quiet or the budget
			// ran out. Both are reasons to stop.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				break
			}
			_ = r.conn.SetReadDeadline(time.Time{})
			return total, err
		}
	}
	return total, r.conn.SetReadDeadline(time.Time{})
}

// Tune moves the centre frequency without interrupting the stream.
func (r *RTLTCP) Tune(hz uint32) error { return r.command(cmdSetFreq, hz) }

func (r *RTLTCP) Close() error { return r.conn.Close() }
