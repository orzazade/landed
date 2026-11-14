package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadDropsRepeatedTurns(t *testing.T) {
	root := t.TempDir()
	line := `{"type":"user","uuid":"same","sessionId":"%s","cwd":"/p","entrypoint":"cli","timestamp":"2026-11-01T09:00:00Z","origin":{"kind":"human"},"message":{"role":"user","content":"go with plan b"}}` + "\n"
	for _, id := range []string{"a", "b"} {
		dir := filepath.Join(root, "proj")
		os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(fmt.Sprintf(line, id)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := load([]string{root}, time.Time{})
	if len(l.Errors) != 0 {
		t.Fatal(l.Errors)
	}
	n := 0
	for _, s := range l.Sessions {
		n += len(s.Turns)
	}
	if n != 1 || l.Repeated != 1 {
		t.Errorf("turns = %d, repeated = %d; want 1 and 1", n, l.Repeated)
	}
}

// A read cut short by the deadline says so: it never looks like a full read.
func TestLoadStopsWhenCancelled(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "p"), 0o755)
	os.WriteFile(filepath.Join(root, "p", "a.jsonl"), []byte("{}\n"), 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	l := loadCtx(ctx, []string{root}, time.Time{})
	if len(l.Errors) == 0 || !errors.Is(l.Errors[0], context.Canceled) {
		t.Errorf("errors = %v; want the cancel", l.Errors)
	}
}

// check reads only transcripts changed since the session's first owner turn.
func TestSessionCut(t *testing.T) {
	o := hookFixture(t)
	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	got, ok, err := sessionCut(o.roots, "kettle-1", time.Now().Add(-30*24*time.Hour))
	if err != nil || !ok || !got.Equal(start) {
		t.Errorf("cut = %v, err %v; want %v", got, err, start)
	}
	if _, _, err := sessionCut(o.roots, "kettle-9", time.Time{}); err == nil || err.Error() != "no transcript for session kettle-9" {
		t.Errorf("unknown session: %v", err)
	}
}
