package tpms

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

// SourceConfig controls how rtl_433 is invoked.
type SourceConfig struct {
	// Freqs are the frequencies to listen on, in rtl_433 notation. TPMS
	// is at 315 MHz in North America and 433.92 MHz in Europe; plenty of
	// cars sold in the US use either, so both are worth covering.
	Freqs []string

	// HopSeconds is how long to dwell on each frequency. It only applies
	// when more than one is given — the dongle's 2.4 MHz of bandwidth
	// cannot span 315 and 434 MHz at once.
	HopSeconds int

	Gain string
	PPM  int

	// Device is what rtl_433 should open: a USB device index such as "0",
	// or "rtl_tcp:host:port" to reach the radio through an rtl_tcp server
	// that something else is holding. The latter is what lets this
	// receiver share a dongle with the others rather than taking it.
	Device    string
	ExtraArgs []string
}

// Args builds the rtl_433 command line.
func (c SourceConfig) Args() []string {
	dev := c.Device
	if dev == "" {
		dev = "0"
	}
	args := []string{"-d", dev}
	for _, f := range c.Freqs {
		args = append(args, "-f", f)
	}
	if len(c.Freqs) > 1 && c.HopSeconds > 0 {
		args = append(args, "-H", strconv.Itoa(c.HopSeconds))
	}
	if c.Gain != "" {
		args = append(args, "-g", c.Gain)
	}
	if c.PPM != 0 {
		args = append(args, "-p", strconv.Itoa(c.PPM))
	}
	args = append(args, c.ExtraArgs...)
	// ISO timestamps and JSON on stdout, so the output is parseable
	// regardless of the user's locale.
	return append(args, "-M", "time:iso", "-F", "json")
}

// Source runs rtl_433 and yields decoded readings.
type Source struct {
	cmd *exec.Cmd
	out io.ReadCloser
}

// Start launches rtl_433. Cancelling ctx stops it.
func Start(ctx context.Context, cfg SourceConfig) (*Source, error) {
	cmd := exec.CommandContext(ctx, "rtl_433", cfg.Args()...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("rtl_433 stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start rtl_433 (is it installed?): %w", err)
	}
	return &Source{cmd: cmd, out: out}, nil
}

// Run reads until rtl_433 exits, calling onTPMS for every tire sensor
// report and onOther for anything else it decodes.
func (s *Source) Run(onTPMS func(Reading), onOther func([]byte)) error {
	sc := bufio.NewScanner(s.out)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue // rtl_433 writes banner text to stdout too
		}
		if r, ok := ParseRTL433(line); ok {
			onTPMS(r)
			continue
		}
		if onOther != nil {
			cp := make([]byte, len(line))
			copy(cp, line)
			onOther(cp)
		}
	}
	return sc.Err()
}

// Close stops rtl_433 and reaps it.
func (s *Source) Close() error {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.out.Close()
	_ = s.cmd.Wait()
	return nil
}
