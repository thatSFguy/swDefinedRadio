package sdr

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"time"
)

// EnsureRTLTCP connects to an rtl_tcp server, starting one if nothing is
// listening. The scanner needs a source it can retune without restarting,
// which is what rtl_tcp provides and a plain rtl_sdr pipe does not.
//
// The returned stop function shuts down a server this call started, and
// does nothing for one that was already running.
func EnsureRTLTCP(ctx context.Context, addr string, cfg Config) (*RTLTCP, func(), error) {
	if c, err := DialRTLTCP(addr, cfg); err == nil {
		return c, func() {}, nil
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, nil, fmt.Errorf("bad rtl_tcp address %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}

	// Direct sampling is set over the protocol once connected; rtl_tcp
	// has no command-line flag for it.
	cmd := exec.CommandContext(ctx, "rtl_tcp",
		"-a", host, "-p", port,
		"-d", strconv.Itoa(cfg.DeviceIndex),
		"-f", strconv.FormatUint(uint64(cfg.CenterFreq), 10),
		"-s", strconv.FormatUint(uint64(cfg.SampleRate), 10),
	)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("start rtl_tcp (is rtl-sdr installed?): %w", err)
	}
	stop := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}

	// rtl_tcp takes a moment to claim the device and open its socket.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			stop()
			return nil, nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if c, err := DialRTLTCP(addr, cfg); err == nil {
			return c, stop, nil
		}
	}
	stop()
	return nil, nil, fmt.Errorf("rtl_tcp did not come up on %s", addr)
}
