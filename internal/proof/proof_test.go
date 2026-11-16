package proof

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func at(h, m int) time.Time { return time.Date(2026, 11, 16, h, m, 0, 0, time.UTC) }

func TestAppendRead(t *testing.T) {
	p := filepath.Join(t.TempDir(), "proof.jsonl")
	if evs, _, err := Read(p); err != nil || len(evs) != 0 {
		t.Fatalf("no log yet: %v %v, want none and no error", evs, err)
	}
	w := Event{At: at(10, 0), Event: Warn, Where: "hook", Session: "s1", Decision: "d1", DecisionSession: "s1", Shown: `- "settings live in TOML"`}
	l := Event{At: at(11, 0), Event: Landed, Session: "s1", Decision: "d1", Kind: "commit", Ref: "kettle 1a2b3c4 TOML", When: at(10, 30), WarnedBefore: true}
	if err := Append(p, []Event{w}); err != nil {
		t.Fatal(err)
	}
	if err := Append(p, []Event{l}); err != nil {
		t.Fatal(err)
	}
	evs, _, err := Read(p)
	if err != nil || len(evs) != 2 || evs[0].Shown != w.Shown || !evs[1].When.Equal(l.When) || !evs[1].WarnedBefore {
		t.Fatalf("read back %+v, %v", evs, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v, want 0600 (it quotes private text)", fi.Mode().Perm())
	}
}

// A log that cannot be written fails loudly, and a FIFO in its place never
// blocks the hook.
func TestAppendFails(t *testing.T) {
	dir := t.TempDir()
	if err := Append(dir, []Event{{Event: Warn}}); err == nil {
		t.Error("a folder at the log path: no error")
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { done <- Append(fifo, []Event{{Event: Warn}}) }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO at the log path: no error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Append blocked on a FIFO")
	}
}

// A half-written line is counted and skipped, never the end of the log,
// and the next write starts on a line of its own.
func TestReadBadLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "proof.jsonl")
	os.WriteFile(p, []byte("{\"event\":\"warn\",\"decision\":\"d1\"}\n{\"event\":\"wa"), 0o600)
	if err := Append(p, []Event{{Event: Warn, Decision: "d2"}}); err != nil {
		t.Fatal(err)
	}
	evs, bad, err := Read(p)
	if err != nil || bad != 1 || len(evs) != 2 || evs[1].Decision != "d2" {
		t.Errorf("events %+v, bad %d, err %v; want d1 and d2, 1 bad", evs, bad, err)
	}
}

// Reading never hangs on a FIFO at the log path either.
func TestReadRefusesFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "proof.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal("mkfifo:", err)
	}
	done := make(chan error, 1)
	go func() { _, _, err := Read(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO at the log path: no error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Read blocked on a FIFO")
	}
}

// The verdict: caused only when a session that saw the warning landed the
// decision after it.
func TestJudge(t *testing.T) {
	warns := []Event{
		{At: at(10, 0), Event: Warn, Where: "report", Decision: "d1"},
		{At: at(12, 0), Event: Warn, Where: "hook", Session: "s1", Decision: "d1"},
	}
	for _, c := range []struct {
		name    string
		session string
		when    time.Time
		want    string
		why     string
	}{
		{"warned session landed it after", "s1", at(12, 30), Caused, "after the warning"},
		{"landed before any warning", "s1", at(9, 0), Anyway, "before the first warning"},
		{"warned session landed it before its own warning", "s1", at(11, 0), Anyway, "before its warning"},
		{"another session", "s2", at(13, 0), Anyway, "never saw a warning"},
		{"landing session not known", "", at(13, 0), Anyway, "not known"},
	} {
		got, why := Judge(warns, c.session, c.when)
		if got != c.want || !strings.Contains(why, c.why) {
			t.Errorf("%s: %q (%s), want %q with %q", c.name, got, why, c.want, c.why)
		}
	}
}
