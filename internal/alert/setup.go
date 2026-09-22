package alert

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// Setup builds the watcher and the places an alert goes: the log,
// a file that outlives the session, and optionally a command.
func Setup(ctx context.Context, rulesPath, logPath, command string) (*Watcher, error) {
	rules, cooldown, found, err := LoadOrBuiltin(rulesPath)
	if err != nil {
		// Carry on with the built-in rules rather than refusing to
		// receive, but say so clearly: a watcher that silently watches
		// for the wrong things is worse than one that is not running.
		log.Printf("alerts: %v", err)
		log.Print("alerts: using the built-in rules instead")
	}
	switch {
	case found:
		log.Printf("alerts: %d rules (%s plus the built-in ones)", len(rules), rulesPath)
	case rulesPath != "":
		log.Printf("alerts: %d built-in rules; add your own in %s (./sdr alerts-example writes a starter)",
			len(rules), rulesPath)
	}

	sink := func(a Alert) {
		mark := "ALERT"
		if a.Urgent {
			mark = "ALERT!"
		}
		log.Printf("%s %s", mark, a.Line())
		if logPath != "" {
			if err := appendAlert(logPath, a); err != nil {
				log.Printf("alerts: cannot write %s: %v", logPath, err)
			}
		}
		if command != "" {
			runAlertCommand(ctx, command, a)
		}
	}
	return New(rules, cooldown, sink), nil
}

// appendAlert adds one line of JSON to the alert log, so what flew over
// while nobody was watching can be read back later.
func appendAlert(path string, a Alert) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(a)
}

// runAlertCommand runs the -alert-cmd program with the alert in its
// environment, which is how this reaches a desktop notification:
//
//	./sdr start adsb -alert-cmd 'notify-send "$ALERT_RULE" "$ALERT_TEXT"'
//
// It is run through the shell, detached from the receive loop, and
// given a deadline: a command that hangs must not take the radio with
// it. Nothing from the air is ever put on the command line — it goes in
// the environment, where a callsign cannot turn into another command.
func runAlertCommand(ctx context.Context, command string, a Alert) {
	go func() {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		name, args := shellCommand(command)
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = append(os.Environ(),
			"ALERT_RULE="+a.Rule,
			"ALERT_NOTE="+a.Note,
			"ALERT_TEXT="+a.Line(),
			"ALERT_HEX="+a.Hex,
			"ALERT_CALLSIGN="+a.Callsign,
			"ALERT_SQUAWK="+a.Squawk,
			"ALERT_EMERGENCY="+a.Emergency,
			"ALERT_ALTITUDE="+strconv.Itoa(a.Altitude),
			"ALERT_DISTANCE_NM="+fmt.Sprintf("%.1f", a.DistanceNM),
			"ALERT_LAT="+fmt.Sprintf("%.5f", a.Lat),
			"ALERT_LON="+fmt.Sprintf("%.5f", a.Lon),
			"ALERT_URGENT="+strconv.FormatBool(a.Urgent),
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("alerts: -alert-cmd failed: %v: %s", err, out)
		}
	}()
}

// WriteExample writes a starter rules file, refusing to clobber one
// that already has someone's rules in it.
func WriteExample(path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists — delete it first if you want a fresh one", path)
	}
	if err := os.WriteFile(path, []byte(Example), 0o644); err != nil {
		return err
	}
	log.Printf("wrote %s — edit it, then restart the receiver", path)
	return nil
}
