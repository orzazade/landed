package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/store"
)

// A made-up project folder with two sessions. The owner answered a queue
// question with a lasting rule in session "kettle-1"; session "kettle-2"
// holds another decision that must not be listed.
func hookFixture(t *testing.T) checkOpts {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "-home-dev-kettle")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	line := func(sess, uuid, text string) string {
		return `{"type":"user","uuid":"` + uuid + `","sessionId":"` + sess + `","cwd":"/home/dev/kettle","entrypoint":"cli","timestamp":"` + ts +
			`","origin":{"kind":"human"},"message":{"role":"user","content":"` + text + `"}}` + "\n"
	}
	write := func(sess, body string) {
		if err := os.WriteFile(filepath.Join(dir, sess+".jsonl"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("kettle-1", line("kettle-1", "u1", "[queue q3] never ship without the lid sensor"))
	write("kettle-2", line("kettle-2", "u2", "[queue q4] the spout stays ceramic"))
	write("kettle-3", line("kettle-3", "u3", "thanks"))
	o := checkOpts{roots: rootList{root}, db: filepath.Join(t.TempDir(), "l.db"), noModel: true, reposUnder: ""}
	o.since, o.window = 30*24*time.Hour, 7*24*time.Hour
	return o
}

func TestHookListsThisSessionsUnlandedDecisions(t *testing.T) {
	o := hookFixture(t)
	var out bytes.Buffer
	stdin := `{"session_id":"kettle-1","transcript_path":"/x/kettle-1.jsonl","hook_event_name":"SessionStart","source":"compact"}`
	if err := runHook(strings.NewReader(stdin), &out, o); err != nil {
		t.Fatalf("the hook never fails: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "Owner decisions in this session with no landing yet:\n") ||
		!strings.Contains(got, "never ship without the lid sensor") ||
		!strings.HasSuffix(got, "Land each before going on.\n") {
		t.Errorf("hook output:\n%s", got)
	}
	if strings.Contains(got, "spout") {
		t.Errorf("another session's decision is listed:\n%s", got)
	}
}

func TestHookIsSilentOrOneLine(t *testing.T) {
	o := hookFixture(t)
	var out bytes.Buffer
	if err := runHook(strings.NewReader(`{"session_id":"kettle-3"}`), &out, o); err != nil || out.Len() != 0 {
		t.Errorf("a session with no decisions: err %v, output %q; want silence", err, out.String())
	}
	out.Reset()
	// No transcript for the session: saying nothing would look like "all landed".
	if err := runHook(strings.NewReader(`{"session_id":"kettle-9"}`), &out, o); err != nil || out.String() != "landed could not check: no transcript for session kettle-9\n" {
		t.Errorf("unknown session: err %v, output %q", err, out.String())
	}
	out.Reset()
	if err := runHook(strings.NewReader(`not json`), &out, o); err != nil || !strings.HasPrefix(out.String(), "landed could not check: ") || strings.Count(out.String(), "\n") != 1 {
		t.Errorf("bad stdin: err %v, output %q; want one line and no error", err, out.String())
	}
}

// check's own defaults must parse: grace 0 means a decision made a minute
// ago counts too.
func TestCheckDefaultsParse(t *testing.T) {
	fs, o, since, grace, window, _, _ := checkFlags()
	if err := parseFlags(fs, []string{"--session", "kettle-1"}); err != nil {
		t.Fatal(err)
	}
	if err := o.parse(*since, *grace, *window); err != nil || o.grace != 0 || o.session != "kettle-1" {
		t.Errorf("defaults: err %v, grace %v, session %q", err, o.grace, o.session)
	}
}

// The PreCompact hook only starts a background check of the session, so the
// cache is warm when the SessionStart hook asks after the summary.
func TestWarmStartsABackgroundCheck(t *testing.T) {
	var got []string
	start := func(id string) error { got = append(got, id); return nil }
	var out bytes.Buffer
	if err := runWarm(strings.NewReader(`{"session_id":"kettle-1","hook_event_name":"PreCompact"}`), &out, start); err != nil || out.Len() != 0 {
		t.Errorf("err %v, output %q; want silence", err, out.String())
	}
	if strings.Join(got, ",") != "kettle-1" {
		t.Errorf("started %v; want kettle-1", got)
	}
	out.Reset()
	if err := runWarm(strings.NewReader(`{"session_id":"../x"}`), &out, start); err != nil || !strings.HasPrefix(out.String(), "landed could not check: ") || len(got) != 1 {
		t.Errorf("a session id that is not a plain name: err %v, output %q, started %v", err, out.String(), got)
	}
}

// A session with no owner turns has nothing to check: it reads no other
// transcript (a cut that stays 30 days back would read them all).
func TestSessionWithNoOwnerTurnsReadsNothing(t *testing.T) {
	o := hookFixture(t)
	dir := filepath.Join(o.roots[0], "-home-dev-kettle")
	os.WriteFile(filepath.Join(dir, "kettle-empty.jsonl"), []byte(`{"type":"summary","summary":"x"}`+"\n"), 0o600)
	o.session = "kettle-empty"
	c, err := runChecks(context.Background(), o, time.Now())
	if err != nil || len(c.Loaded.Sessions) != 0 || len(c.Checks) != 0 {
		t.Errorf("err %v, sessions read %d, checks %d; want nil, 0, 0", err, len(c.Loaded.Sessions), len(c.Checks))
	}
}

// A first run that reads history for longer than the budget stops itself
// and says so, before Claude Code would kill it (which would be silent),
// whatever stage is slow, even one that ignores the deadline.
func TestHookStopsItselfWhenReadingIsSlow(t *testing.T) {
	o := hookFixture(t)
	defer func(b time.Duration, run func(context.Context, checkOpts, time.Time) (checked, error)) {
		hookBudget, checkRun = b, run
	}(hookBudget, checkRun)
	hookBudget = 50 * time.Millisecond
	checkRun = func(context.Context, checkOpts, time.Time) (checked, error) {
		time.Sleep(2 * time.Second) // a slow read that never looks at ctx
		return checked{}, nil
	}
	var out bytes.Buffer
	begin := time.Now()
	if err := runHook(strings.NewReader(`{"session_id":"kettle-1"}`), &out, o); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(begin); took > time.Second {
		t.Errorf("hook took %v; it must answer at its budget", took)
	}
	if out.String() != "landed could not check: still reading history (not done within 50ms)\n" {
		t.Errorf("output %q", out.String())
	}
}

// A background warm-up shows its rows to nobody, so it must not mark them
// as shown missing: a later landing would count as a rescue nobody saw.
func TestBackgroundRunMarksNothingShown(t *testing.T) {
	for _, bg := range []bool{true, false} {
		st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
		if err != nil {
			t.Fatal(err)
		}
		missing := []landing.Check{{Decision: landing.Decision{ID: "u1"}, State: landing.Missing}}
		if errs := remember(st, checkOpts{background: bg}, missing, time.Now()); len(errs) != 0 {
			t.Fatal(errs)
		}
		remember(st, checkOpts{}, []landing.Check{{Decision: landing.Decision{ID: "u1"}, State: landing.Landed}}, time.Now())
		_, all, _ := st.Rescued(time.Now().Add(-time.Hour))
		st.Close()
		if want := map[bool]int{true: 0, false: 1}[bg]; all != want {
			t.Errorf("background %v: rescued %d, want %d", bg, all, want)
		}
	}
}

// A panic inside the check still ends in the one line, never in silence.
func TestHookSurvivesAPanic(t *testing.T) {
	o := hookFixture(t)
	defer func(run func(context.Context, checkOpts, time.Time) (checked, error)) { checkRun = run }(checkRun)
	checkRun = func(context.Context, checkOpts, time.Time) (checked, error) { panic("boom") }
	var out bytes.Buffer
	if err := runHook(strings.NewReader(`{"session_id":"kettle-1"}`), &out, o); err != nil || out.String() != "landed could not check: panic: boom\n" {
		t.Errorf("err %v, output %q", err, out.String())
	}
}

// Only one background check per session runs at a time; a lock left by a
// run that died is taken over once it is stale.
func TestBackgroundLock(t *testing.T) {
	dir := t.TempDir()
	unlock, ok := lockSession(dir, "kettle-1", time.Now())
	if !ok {
		t.Fatal("first lock refused")
	}
	if _, ok := lockSession(dir, "kettle-1", time.Now()); ok {
		t.Error("second lock taken while the first is held")
	}
	if _, ok := lockSession(dir, "kettle-1", time.Now().Add(staleLock+time.Minute)); !ok {
		t.Error("stale lock not taken over")
	}
	unlock()
	if _, ok := lockSession(filepath.Join(dir, "not-made-yet"), "kettle-2", time.Now()); !ok {
		t.Error("lock refused in a data folder not made yet (a fresh install)")
	}
	if _, ok := lockSession(dir, "kettle-2", time.Now()); !ok {
		t.Error("another session's lock refused")
	}
}
