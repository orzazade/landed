package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orzazade/landed/internal/decide"
	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/store"
)

func TestScoreLanding(t *testing.T) {
	ck := func(id string, s landing.State) landing.Check {
		return landing.Check{Decision: landing.Decision{ID: id}, State: s}
	}
	checks := []landing.Check{
		ck("real-miss", landing.Missing),    // decision, hand check: no -> true missing
		ck("did-land", landing.Missing),     // decision, hand check: yes -> wrong missing
		ck("not-decision", landing.Missing), // labelled not -> wrong missing
		ck("unchecked", landing.Missing),    // decision, no hand check
		ck("good-land", landing.Landed),     // hand check yes
		ck("bad-land", landing.Landed),      // hand check no -> disputed
		ck("cannot", landing.Unknown),
		ck("cannot-real-miss", landing.Unknown), // hand check no, shown as ?
		ck("undone-action", landing.Unknown),    // a one-off action never done: not a miss to write down
		ck("action-missing", landing.Missing),   // hand-labelled action shown as missing: a false alarm
	}
	labels := map[string]label{
		"real-miss": {Label: "decision"}, "did-land": {Label: "decision"}, "not-decision": {Label: "not"},
		"unchecked": {Label: "decision"}, "good-land": {Label: "decision"}, "bad-land": {Label: "decision"},
		"cannot": {Label: "decision"}, "cannot-real-miss": {Label: "decision"},
		"undone-action": {Label: "decision", Kind: "action"}, "action-missing": {Label: "decision", Kind: "action"},
	}
	truth := map[string]landTruth{
		"real-miss": {Landed: "no"}, "did-land": {Landed: "yes"}, "good-land": {Landed: "yes"}, "bad-land": {Landed: "no"},
		"cannot-real-miss": {Landed: "no"}, "undone-action": {Landed: "no"}, "action-missing": {Landed: "no"},
	}
	s := scoreLanding(checks, labels, truth)
	if s.CalledMissing != 5 || s.TrueMissing != 1 || s.FalseMissingLanded != 1 || s.FalseMissingNotDecision != 2 || s.UncheckedMissing != 1 {
		t.Errorf("missing: %+v", s)
	}
	if s.RealMisses != 3 { // real-miss, bad-land and cannot-real-miss
		t.Errorf("real misses: %d, want 3", s.RealMisses)
	}
	if s.CalledLanded != 2 || s.WrongLanded != 1 || s.States[landing.Unknown] != 3 {
		t.Errorf("landed: %+v", s)
	}
}

func TestReadLandTruthGlobAndDuplicates(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "landing-labels.jsonl"), []byte(`{"id":"a","landed":"yes","where":"commit"}`+"\n"), 0o600)
	os.WriteFile(filepath.Join(dir, "landing-labels2.jsonl"), []byte(`{"id":"b","landed":"no","where":"searched"}`+"\n"), 0o600)
	got, err := readLandTruth(filepath.Join(dir, "landing-labels*.jsonl"))
	if err != nil || len(got) != 2 || got["b"].Landed != "no" {
		t.Fatalf("got %v, %v", got, err)
	}
	os.WriteFile(filepath.Join(dir, "landing-labels3.jsonl"), []byte(`{"id":"a","landed":"no","where":"x"}`+"\n"), 0o600)
	if _, err := readLandTruth(filepath.Join(dir, "landing-labels*.jsonl")); err == nil || !strings.Contains(err.Error(), "also in another") {
		t.Errorf("duplicate id: %v", err)
	}
}

// Keywords alone stay narrow: the repos under ~/Projects and the other wide
// sources are searched only when a judge reads the candidates.
func TestWidenOnlyWhenJudged(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "a", ".git"), 0o700)
	var src landing.Sources
	widen(&src, root, false)
	if src.Repos != nil || src.ChangedNotes || src.AllBus {
		t.Errorf("keyword-only run widened: %+v", src)
	}
	widen(&src, root, true)
	if len(src.Repos) != 1 || !src.ChangedNotes || !src.AllBus {
		t.Errorf("judged run not widened: repos %v", src.Repos)
	}
}

func TestScoreFinderKind(t *testing.T) {
	labels := []label{{ID: "a", Label: "decision", Kind: "action"}, {ID: "b", Label: "decision", Kind: "rule"}, {ID: "c", Label: "decision"}}
	v := func(kind string) decide.Result {
		return decide.Result{Known: true, Verdict: store.Verdict{Decision: true, Kind: kind}}
	}
	s := scoreFinder(labels, map[string]decide.Result{"a": v("action"), "b": v("action"), "c": v("rule")})
	if s.Found != 3 || s.KindLabelled != 2 || s.KindRight != 1 {
		t.Errorf("%+v", s)
	}
	p := filepath.Join(t.TempDir(), "l.jsonl")
	os.WriteFile(p, []byte(`{"id":"x","label":"not","kind":"rule"}`+"\n"), 0o600)
	if _, err := readLabels(p); err == nil {
		t.Errorf("a kind on a non-decision must be refused")
	}
}

// The eval scores lasting decisions on their own: the truth is a hand label
// "decision" with kind rule; a lasting call on a hand "not" or a hand one-off
// is a false hit; a decision with no hand kind is not scored.
func TestScoreFinderLasting(t *testing.T) {
	labels := []label{{ID: "r1", Label: "decision", Kind: "rule"}, {ID: "r2", Label: "decision", Kind: "rule"},
		{ID: "a1", Label: "decision", Kind: "action"}, {ID: "n1", Label: "not"}, {ID: "u", Label: "decision"}}
	v := func(dec bool, kind string) decide.Result {
		return decide.Result{Known: true, Verdict: store.Verdict{Decision: dec, Kind: kind}}
	}
	s := scoreFinder(labels, map[string]decide.Result{"r1": v(true, "rule"), "r2": v(true, "action"),
		"a1": v(true, "rule"), "n1": v(true, "rule"), "u": v(true, "rule")})
	if s.LastingWant != 2 || s.LastingFound != 1 || s.LastingFalse != 2 {
		t.Errorf("lasting want/found/false = %d/%d/%d; want 2/1/2", s.LastingWant, s.LastingFound, s.LastingFalse)
	}
}
