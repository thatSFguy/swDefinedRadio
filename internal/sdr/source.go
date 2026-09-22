// Package sdr provides IQ sample sources for RTL-SDR dongles.
//
// Two are available. Process spawns rtl_sdr and reads its stdout, which
// is the simplest thing that works for a fixed-frequency receiver like
// ADS-B. RTLTCP talks to an rtl_tcp server, which costs a daemon but
// allows retuning mid-stream — what a band scanner needs.
package sdr

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
)

// AutoGain selects the tuner's own AGC rather than a fixed gain.
const AutoGain = -1

// Config is the tuner state a Source is opened with.
type Config struct {
	CenterFreq     uint32 // Hz
	SampleRate     uint32 // Hz
	Gain           int    // tenths of a dB, or AutoGain
	FreqCorrection int    // ppm
	DeviceIndex    int
	BiasTee        bool

	// DirectSampling selects DirectOff, DirectI or DirectQ. See those
	// constants for what the mode costs and what hardware it needs.
	DirectSampling int
}

// Source streams interleaved unsigned 8-bit I/Q samples.
type Source interface {
	io.Reader

	// Tune moves the centre frequency of a running stream. Sources that
	// cannot retune report an error rather than silently doing nothing.
	Tune(hz uint32) error

	Close() error
}

// File replays a capture of raw IQ samples, which is how a receiver can
// be worked on when the band is quiet, when the antenna is somewhere
// else, or when a particular signal needs to be looked at more than
// once. Write one with:
//
//	rtl_sdr -f 978000000 -s 2083334 -g 0 capture.iq
type File struct{ f *os.File }

// OpenFile returns a Source that reads a raw capture.
func OpenFile(path string) (*File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return &File{f: f}, nil
}

func (f *File) Read(p []byte) (int, error) { return f.f.Read(p) }
func (f *File) Close() error               { return f.f.Close() }

// Tune is refused: a recording was made at one frequency and cannot be
// moved to another.
func (f *File) Tune(uint32) error {
	return errors.New("a capture file cannot be retuned")
}

// Open picks an IQ source from the ways a receiver can be pointed at one.
//
// A capture file wins if given, because replaying one is a deliberate act
// and should not be quietly overridden by hardware being present. Then an
// rtl_tcp address, for a receiver that needs to retune or that is sharing
// the radio with something else. Otherwise rtl_sdr is spawned directly,
// which is the simplest thing that works for a receiver sitting on one
// frequency and costs no daemon.
func Open(ctx context.Context, tcpAddr, replay string, cfg Config) (Source, error) {
	if replay != "" {
		log.Printf("replaying %s", replay)
		return OpenFile(replay)
	}
	if tcpAddr == "" {
		log.Print("starting rtl_sdr")
		return StartProcess(ctx, cfg)
	}
	r, err := DialRTLTCP(tcpAddr, cfg)
	if err != nil {
		return nil, err
	}
	log.Printf("connected to rtl_tcp at %s, tuner %s", tcpAddr, r.Tuner)
	return r, nil
}
