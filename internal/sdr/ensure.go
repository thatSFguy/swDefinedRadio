package sdr

import (
	"context"
	"fmt"
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

	srv, err := StartRTLTCP(ctx, addr, cfg)
	if err != nil {
		return nil, nil, err
	}

	// rtl_tcp takes a moment to claim the device and open its socket.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			srv.Stop()
			return nil, nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		if c, err := DialRTLTCP(addr, cfg); err == nil {
			return c, srv.Stop, nil
		}
		if srv.Exited() {
			return nil, nil, srv.Err()
		}
	}
	srv.Stop()
	return nil, nil, fmt.Errorf("rtl_tcp did not come up on %s", addr)
}
