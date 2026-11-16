// Package proof keeps the live record of landed's warnings and of the
// landings that followed them, so `landed proof` can show whether a warning
// got a decision landed or it would have landed anyway.
package proof

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	Warn   = "warn"   // landed showed a decision as having no landing yet
	Landed = "landed" // a later run found a warned decision landed

	Caused = "caused by landed"         // the warned session landed it after the warning
	Anyway = "would have landed anyway" // landed first, or in a session that never saw a warning
)

// Event is one line of the log.
type Event struct {
	At    time.Time `json:"at"`
	Event string    `json:"event"`
	Where string    `json:"where,omitempty"` // warn: hook, check, report or missing
	// Session: for a warning, the session it was shown in ("" when shown
	// outside a session: a report or a terminal); for a landing, the session
	// that made it ("" when no transcript shows who).
	Session         string `json:"session,omitempty"`
	Decision        string `json:"decision"` // the owner turn's id
	DecisionSession string `json:"decision_session,omitempty"`
	Shown           string `json:"shown,omitempty"` // warn: the exact text shown
	// A landing: its kind (commit, edit, bus, note, acted), where it is, when
	// it was made, and whether its session had seen a warning before then.
	Kind         string    `json:"kind,omitempty"`
	Ref          string    `json:"ref,omitempty"`
	When         time.Time `json:"when,omitzero"`
	WarnedBefore bool      `json:"warned_before,omitempty"`
}

// Path is the log next to the database.
func Path(dir string) string { return filepath.Join(dir, "proof.jsonl") }

// Append writes evs to the log in one write. The file is owner-only (it
// quotes the owner's words). Anything but a regular file at the path is an
// error, and opening never waits (a FIFO with no reader would block a hook).
func Append(path string, evs []Event) error {
	if len(evs) == 0 {
		return nil
	}
	var buf []byte
	for _, e := range evs {
		b, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf = append(append(buf, b...), '\n')
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND|os.O_CREATE|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("proof log: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return fmt.Errorf("proof log: %w", err)
	} else if !fi.Mode().IsRegular() {
		return fmt.Errorf("proof log %s is not a regular file", path)
	}
	if fi, _ := f.Stat(); fi.Size() > 0 {
		// A line cut short by an earlier failed write must not swallow this one.
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, fi.Size()-1); err == nil && last[0] != '\n' {
			buf = append([]byte{'\n'}, buf...)
		}
	}
	if _, err := f.Write(buf); err != nil {
		return fmt.Errorf("proof log: %w", err)
	}
	return f.Close()
}

// Read returns every event and the number of lines that are not one (a
// half-written line after a full disk). No log yet is no events. Like
// Append it refuses anything but a regular file and never waits on open.
func Read(path string) ([]Event, int, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, 0, err
	} else if !fi.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("proof log %s is not a regular file", path)
	}
	var out []Event
	bad := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Event == "" {
			bad++
			continue
		}
		out = append(out, e)
	}
	return out, bad, sc.Err()
}

// Judge gives a landing's verdict from the decision's warnings: caused when
// a session that saw a warning landed it after that warning.
func Judge(warns []Event, landSession string, landAt time.Time) (verdict, why string) {
	first := time.Time{}
	for _, w := range warns {
		if first.IsZero() || w.At.Before(first) {
			first = w.At
		}
	}
	if !first.IsZero() && landAt.Before(first) {
		return Anyway, "landed " + landAt.Format(stamp) + ", before the first warning"
	}
	if landSession == "" {
		return Anyway, "the landing session is not known"
	}
	var own []Event
	for _, w := range warns {
		if w.Session == landSession {
			own = append(own, w)
		}
	}
	if len(own) == 0 {
		return Anyway, "landed in session " + short(landSession) + ", which never saw a warning"
	}
	for _, w := range own {
		if w.At.Before(landAt) {
			return Caused, fmt.Sprintf("session %s landed it %s after the warning (%s)", short(landSession), landAt.Sub(w.At).Round(time.Minute), w.Where)
		}
	}
	return Anyway, "session " + short(landSession) + " landed it before its warning"
}

const stamp = "01-02 15:04"

func short(id string) string { return id[:min(8, len(id))] }
