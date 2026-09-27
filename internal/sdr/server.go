package sdr

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// Server is an rtl_tcp child process, watched.
//
// rtl_tcp that cannot open the dongle says why on stderr and exits, and
// all a client sees is a refused connection. Left unwatched, that turned
// "No supported devices found" into "connection refused" — true, and no
// help at all — and a server that had died was still counted as running,
// so nothing ever started another. Keeping the tail of stderr and
// noticing the exit fixes both.
type Server struct {
	cmd  *exec.Cmd
	done chan struct{}
	tail *tailBuffer
	err  error // how it exited; read only after done is closed
}

// StartRTLTCP starts rtl_tcp listening on addr.
func StartRTLTCP(ctx context.Context, addr string, cfg Config) (*Server, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("bad rtl_tcp address %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	// Direct sampling is set over the protocol once connected; rtl_tcp
	// has no command-line flag for it.
	args := []string{"-a", host, "-p", port, "-d", strconv.Itoa(cfg.DeviceIndex)}
	if cfg.CenterFreq != 0 {
		args = append(args, "-f", strconv.FormatUint(uint64(cfg.CenterFreq), 10))
	}
	if cfg.SampleRate != 0 {
		args = append(args, "-s", strconv.FormatUint(uint64(cfg.SampleRate), 10))
	}
	s, err := startServer(ctx, "rtl_tcp", args...)
	if err != nil {
		return nil, fmt.Errorf("start rtl_tcp (is rtl-sdr installed?): %w", err)
	}
	return s, nil
}

func startServer(ctx context.Context, name string, args ...string) (*Server, error) {
	s := &Server{done: make(chan struct{}), tail: &tailBuffer{}}
	s.cmd = exec.CommandContext(ctx, name, args...)
	// Still shown as it happens, as before; the tail is kept as well.
	s.cmd.Stderr = io.MultiWriter(os.Stderr, s.tail)
	if err := s.cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		s.err = s.cmd.Wait()
		close(s.done)
	}()
	return s, nil
}

// Done is closed when the process has exited, for whatever reason.
func (s *Server) Done() <-chan struct{} { return s.done }

// Exited reports whether the process has gone.
func (s *Server) Exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Err explains an exit, in rtl_tcp's own words where it left any. It is
// nil while the process is running.
func (s *Server) Err() error {
	if !s.Exited() {
		return nil
	}
	why := "exited"
	if s.err != nil {
		why = "exited: " + s.err.Error()
	}
	if said := s.tail.lastLines(3); said != "" {
		return fmt.Errorf("rtl_tcp %s — %s", why, said)
	}
	return fmt.Errorf("rtl_tcp %s", why)
}

// Stop kills the process and waits for it.
func (s *Server) Stop() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	<-s.done
}

// tailBuffer keeps the last few kilobytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

const tailKeep = 4096

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailKeep {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-tailKeep:]...)
	}
	return len(p), nil
}

// lastLines returns up to n of the last non-blank lines, joined.
func (t *tailBuffer) lastLines(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var kept []string
	lines := strings.Split(strings.ReplaceAll(string(t.buf), "\r", ""), "\n")
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			kept = append([]string{l}, kept...)
		}
	}
	return strings.Join(kept, "; ")
}
