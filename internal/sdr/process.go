package sdr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

// Process streams IQ from an rtl_sdr child process writing to stdout.
// It cannot retune once started, which is no loss for a fixed-frequency
// receiver and saves running a separate daemon.
type Process struct {
	cmd *exec.Cmd
	out io.ReadCloser
	br  *bufio.Reader
}

// ErrNoRetune is returned by Process.Tune; rtl_sdr takes its frequency
// only on the command line.
var ErrNoRetune = errors.New("sdr: this source cannot retune, use rtl_tcp instead")

// StartProcess launches rtl_sdr with cfg. Killing ctx stops the child.
func StartProcess(ctx context.Context, cfg Config) (*Process, error) {
	args := []string{
		"-f", strconv.FormatUint(uint64(cfg.CenterFreq), 10),
		"-s", strconv.FormatUint(uint64(cfg.SampleRate), 10),
		"-d", strconv.Itoa(cfg.DeviceIndex),
	}
	// rtl_sdr reads -g in whole dB and treats 0 as automatic gain.
	if cfg.Gain < 0 {
		args = append(args, "-g", "0")
	} else {
		args = append(args, "-g", strconv.FormatFloat(float64(cfg.Gain)/10, 'f', 1, 64))
	}
	if cfg.FreqCorrection != 0 {
		args = append(args, "-p", strconv.Itoa(cfg.FreqCorrection))
	}
	if cfg.BiasTee {
		args = append(args, "-T")
	}
	args = append(args, "-") // write raw IQ to stdout

	cmd := exec.CommandContext(ctx, "rtl_sdr", args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("rtl_sdr stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start rtl_sdr (is rtl-sdr installed?): %w", err)
	}
	return &Process{cmd: cmd, out: out, br: bufio.NewReaderSize(out, 1<<20)}, nil
}

func (p *Process) Read(b []byte) (int, error) { return p.br.Read(b) }

func (p *Process) Tune(uint32) error { return ErrNoRetune }

// Close stops rtl_sdr and waits for it to exit.
func (p *Process) Close() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	_ = p.out.Close()
	_ = p.cmd.Wait()
	return nil
}
