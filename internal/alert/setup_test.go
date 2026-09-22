package alert

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAppendAlertLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nested", "alerts.jsonl")
	for _, a := range []Alert{
		{At: time.Now(), Rule: "US military", Hex: "ae1234", Callsign: "RCH271"},
		{At: time.Now(), Rule: "emergency squawk", Hex: "a00001", Squawk: "7700", Urgent: true},
	} {
		if err := appendAlert(p, a); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2 — the log must append, not overwrite", len(lines))
	}
	var got Alert
	if err := json.Unmarshal([]byte(lines[1]), &got); err != nil {
		t.Fatalf("line 2 is not JSON: %v", err)
	}
	if got.Squawk != "7700" || !got.Urgent {
		t.Errorf("read back %+v", got)
	}
}

// TestAlertCommandGetsTheAlert checks the hook a background receiver
// uses to reach a desktop notification, including that the detail
// arrives in the environment rather than on the command line.
func TestAlertCommandGetsTheAlert(t *testing.T) {
	out := filepath.Join(t.TempDir(), "fired")
	a := Alert{
		At: time.Now(), Rule: "emergency squawk", Hex: "a00001",
		Callsign: "N123; rm -rf /", Squawk: "7700", Urgent: true, Altitude: 31000,
	}
	runAlertCommand(context.Background(),
		`printf '%s|%s|%s|%s' "$ALERT_RULE" "$ALERT_CALLSIGN" "$ALERT_SQUAWK" "$ALERT_URGENT" > `+out, a)

	deadline := time.Now().Add(5 * time.Second)
	var b []byte
	var err error
	for time.Now().Before(deadline) {
		if b, err = os.ReadFile(out); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the command never ran: %v", err)
	}
	want := "emergency squawk|N123; rm -rf /|7700|true"
	if string(b) != want {
		t.Errorf("command saw %q, want %q", b, want)
	}
}

// TestAlertCommandCannotHangTheReceiver: a command that never returns
// must not take the radio down with it.
func TestAlertCommandIsDetached(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAlertCommand(context.Background(), "sleep 30", Alert{Rule: "x", Hex: "a1"})
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runAlertCommand blocked on the command")
	}
}

func TestWriteExample(t *testing.T) {
	p := filepath.Join(t.TempDir(), "alerts.json")
	if err := WriteExample(p); err != nil {
		t.Fatalf("write: %v", err)
	}
	rules, cooldown, found, err := LoadOrBuiltin(p)
	if err != nil || !found {
		t.Fatalf("the file it writes does not load: found=%v err=%v", found, err)
	}
	if len(rules) <= len(Builtin()) || cooldown != 30*time.Minute {
		t.Errorf("loaded %d rules, cooldown %v", len(rules), cooldown)
	}
	// Writing over someone's rules would be the one unrecoverable thing
	// this command could do.
	if err := WriteExample(p); err == nil {
		t.Error("it overwrote an existing rules file")
	}
}
