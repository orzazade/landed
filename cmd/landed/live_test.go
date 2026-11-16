package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/proof"
	"github.com/orzazade/landed/internal/transcript"
)

// Every row the hook shows is written to the live log: when, which session
// saw it, which decision, and the exact line shown.
func TestHookRecordsWarning(t *testing.T) {
	o := hookFixture(t)
	var out bytes.Buffer
	if err := runHook(strings.NewReader(`{"session_id":"kettle-1"}`), &out, o); err != nil {
		t.Fatal(err)
	}
	evs, _, err := proof.Read(proofPath(o))
	if err != nil || len(evs) != 1 {
		t.Fatalf("log: %+v, %v; want one warning", evs, err)
	}
	e := evs[0]
	if e.Event != proof.Warn || e.Where != "hook" || e.Session != "kettle-1" || e.Decision != "u1" || e.DecisionSession != "kettle-1" {
		t.Errorf("warning = %+v", e)
	}
	if !strings.Contains(out.String(), e.Shown) || !strings.Contains(e.Shown, "never ship without the lid sensor") {
		t.Errorf("shown %q is not the line printed:\n%s", e.Shown, out.String())
	}
	if time.Since(e.At) > time.Minute {
		t.Errorf("warning time %v", e.At)
	}
}

// A log that cannot be written never blocks the session: the warning is
// still shown, and one line says it was not recorded.
func TestHookSaysWhenWarningNotRecorded(t *testing.T) {
	o := hookFixture(t)
	if err := os.MkdirAll(proofPath(o), 0o700); err != nil { // a folder where the log goes
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runHook(strings.NewReader(`{"session_id":"kettle-1"}`), &out, o); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "never ship without the lid sensor") || strings.Count(got, "landed could not record to its proof log") != 1 ||
		!strings.HasSuffix(got, "\n") || strings.Count(got[strings.Index(got, "landed could not record to its proof log"):], "\n") != 1 {
		t.Errorf("output:\n%s", got)
	}
}

// A warned decision found landed later gets one landed line naming the
// session that landed it and whether that session had seen the warning.
func TestRecordLandings(t *testing.T) {
	path := proof.Path(t.TempDir())
	t0 := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	proof.Append(path, []proof.Event{
		{At: t0, Event: proof.Warn, Where: "hook", Session: "s1", Decision: "d1", DecisionSession: "s1"},
		{At: t0, Event: proof.Warn, Where: "hook", Session: "s2", Decision: "d2", DecisionSession: "s2"},
	})
	acts := []transcript.Action{
		{Session: "s9", Time: t0.Add(-time.Hour), Tool: "Edit", Path: "/p/STATUS.md"},
		{Session: "s1", Time: t0.Add(10 * time.Minute), Tool: "Edit", Path: "/p/AGENTS.md"},
	}
	checks := []landing.Check{
		{Decision: landing.Decision{ID: "d1", Session: "s1"}, State: landing.Landed, Evidence: &landing.Evidence{Kind: "edit", Ref: "/p/AGENTS.md", When: t0.Add(10 * time.Minute)}},
		{Decision: landing.Decision{ID: "d2", Session: "s2"}, State: landing.Landed, Evidence: &landing.Evidence{Kind: "edit", Ref: "/p/STATUS.md", When: t0.Add(-time.Hour)}},
		{Decision: landing.Decision{ID: "d3", Session: "s3"}, State: landing.Landed, Evidence: &landing.Evidence{Kind: "edit", Ref: "/p/x.md", When: t0}}, // never warned
		{Decision: landing.Decision{ID: "d4", Session: "s1"}, State: landing.Missing},
	}
	now := t0.Add(2 * time.Hour)
	for range 2 { // a second run adds nothing
		if err := recordLandings(path, checks, acts, now); err != nil {
			t.Fatal(err)
		}
	}
	evs, _, _ := proof.Read(path)
	var landed []proof.Event
	for _, e := range evs {
		if e.Event == proof.Landed {
			landed = append(landed, e)
		}
	}
	if len(landed) != 2 {
		t.Fatalf("landed lines = %+v, want 2 (d1, d2)", landed)
	}
	byID := map[string]proof.Event{landed[0].Decision: landed[0], landed[1].Decision: landed[1]}
	if e := byID["d1"]; e.Session != "s1" || !e.WarnedBefore || e.Kind != "edit" || e.Ref != "/p/AGENTS.md" || !e.When.Equal(t0.Add(10*time.Minute)) || !e.At.Equal(now) {
		t.Errorf("d1 = %+v; want landed by s1 after its warning", e)
	}
	if e := byID["d2"]; e.Session != "s9" || e.WarnedBefore {
		t.Errorf("d2 = %+v; want landed by s9, not warned before", e)
	}
}

// report and missing show rows outside any session: their warnings are
// logged with no session, so a landing after them is never "caused".
func TestReportAndMissingRecordWarnings(t *testing.T) {
	for _, cmd := range []string{"report", "missing"} {
		o := hookFixture(t)
		args := []string{"--root", o.roots[0], "--db", o.db, "--no-model", "--repos-under", "", "--grace", "0", "--since", "2d", "--bus", t.TempDir()}
		run := map[string]func([]string) error{"report": cmdReport, "missing": cmdMissing}[cmd]
		run(args) // the rows are ? with no model; the run's error is not what this test checks
		evs, _, err := proof.Read(proofPath(o))
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, e := range evs {
			if e.Event != proof.Warn || e.Where != cmd || e.Session != "" || e.Shown == "" {
				t.Errorf("%s: event %+v", cmd, e)
			}
			got[e.Decision] = true
		}
		if !got["u1"] || !got["u2"] || len(got) != 2 {
			t.Errorf("%s: warned decisions %v, want u1 and u2", cmd, got)
		}
	}
}

// `landed check --session X` from a terminal shows the rows to no session;
// only /landed (--shown-here) runs inside X, so only then is X the session
// warned. Otherwise a later landing by X would count as caused.
func TestCheckNamesTheSessionOnlyWhenShownThere(t *testing.T) {
	for _, here := range []bool{false, true} {
		o := hookFixture(t)
		args := []string{"--root", o.roots[0], "--db", o.db, "--no-model", "--repos-under", "", "--bus", t.TempDir(), "--session", "kettle-1"}
		if here {
			args = append(args, "--shown-here")
		}
		cmdCheck(args) // the row is ? with no model; the run's error is not what this test checks
		evs, _, err := proof.Read(proofPath(o))
		if err != nil || len(evs) != 1 {
			t.Fatalf("here=%v: log %+v, %v; want one warning", here, evs, err)
		}
		want := map[bool]string{false: "", true: "kettle-1"}[here]
		if e := evs[0]; e.Where != "check" || e.Session != want || e.Decision != "u1" {
			t.Errorf("here=%v: warning %+v, want session %q", here, e, want)
		}
	}
}

// With nothing to warn the hook prints nothing, except a landing line it
// could not write: that is said in one line, never dropped.
func TestHookSaysFailedLandingWriteWhenAllLanded(t *testing.T) {
	o := hookFixture(t)
	old := checkRun
	checkRun = func(context.Context, checkOpts, time.Time) (checked, error) {
		return checked{ProofErr: errors.New("made-up full disk")}, nil
	}
	defer func() { checkRun = old }()
	var out bytes.Buffer
	runHook(strings.NewReader(`{"session_id":"kettle-1"}`), &out, o)
	if got := out.String(); got != "landed could not record to its proof log: made-up full disk\n" {
		t.Errorf("output %q", got)
	}
}
