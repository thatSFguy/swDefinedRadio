package sdr

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fakeRTLTCP puts a program called rtl_tcp first on the PATH, which runs
// script. It stands in for rtl_tcp failing the way it does without a
// dongle it can open.
func fakeRTLTCPProgram(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake rtl_tcp is a shell script")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rtl_tcp"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A port nothing is listening on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

// rtl_tcp that cannot open the dongle says so and exits. That is the
// reason worth reporting — not the refused connection it leaves behind,
// and not after waiting out the whole start-up allowance for a server
// that is already gone.
func TestEnsureRTLTCPReportsWhyTheServerDied(t *testing.T) {
	fakeRTLTCPProgram(t, `echo "No supported devices found." >&2; exit 1`)

	start := time.Now()
	_, _, err := EnsureRTLTCP(context.Background(), closedPort(t), Config{})
	if err == nil {
		t.Fatal("no error from a server that exited")
	}
	if !strings.Contains(err.Error(), "No supported devices found.") {
		t.Errorf("error does not say why: %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v to notice the server had gone", took)
	}
}

func TestServerErrIsNilWhileRunning(t *testing.T) {
	fakeRTLTCPProgram(t, `sleep 30`)
	s, err := StartRTLTCP(context.Background(), closedPort(t), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Exited() || s.Err() != nil {
		t.Errorf("a running server reported exited=%v err=%v", s.Exited(), s.Err())
	}
	s.Stop()
	if !s.Exited() {
		t.Error("a stopped server is not reported as exited")
	}
}

func TestTailKeepsTheLastLines(t *testing.T) {
	var tb tailBuffer
	tb.Write([]byte(strings.Repeat("noise\n", 2000)))
	tb.Write([]byte("Found 1 device(s):\r\n\nusb_claim_interface error -3\r\n"))
	got := tb.lastLines(2)
	if want := "Found 1 device(s):; usb_claim_interface error -3"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(tb.buf) > tailKeep {
		t.Errorf("kept %d bytes, limit %d", len(tb.buf), tailKeep)
	}
}
