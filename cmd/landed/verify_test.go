package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orzazade/landed/internal/decide"
	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/store"
	"github.com/orzazade/landed/internal/transcript"
)

// verifyJudge picks candidate 0 for every item, then answers the check of
// that pick from verify (by decision id).
type verifyJudge struct {
	fakeJudge
	verify  map[string]decide.VerifyAnswer
	checked []string
}

func (v *verifyJudge) Verify(_ context.Context, items []decide.VerifyItem) ([]decide.VerifyAnswer, error) {
	v.mu.Lock() // the embedded fakeJudge's lock: checks run side by side
	defer v.mu.Unlock()
	var out []decide.VerifyAnswer
	for _, it := range items {
		v.checked = append(v.checked, it.ID)
		a := v.verify[it.ID]
		a.ID = it.ID
		out = append(out, a)
	}
	return out, nil
}

// The weak spot from the README: the judge took an unrelated note as the
// landing of a decision that never landed. Each pick is now checked on its
// own, and must quote the words in the record that state the choice; a pick
// whose check says no, or whose quote is not in the record, is no landing.
func TestJudgePickIsVerified(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mk := func(id, note string) pending {
		src := &landing.Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
		c, cands := src.Gather(landing.Decision{ID: id, Cwd: t.TempDir(), Time: t0, Summary: "spout stays ceramic", Keywords: []string{"spout", "ceramic"},
			Later: []transcript.Action{{Time: t0.Add(time.Minute), Tool: "Edit", Path: "/x/notes.md", Text: note}}})
		return pending{c, cands}
	}
	pend := []pending{
		mk("real", "Decided: the spout stays ceramic."),
		mk("unrelated", "Research: ceramic spout makers, prices."),
		mk("made-up-quote", "Spout supplier list, ceramic and steel."),
	}
	vj := &verifyJudge{verify: map[string]decide.VerifyAnswer{
		"real":          {Records: true, Quote: "the spout stays ceramic", Why: "states it"},
		"unrelated":     {Records: false, Why: "only research"},
		"made-up-quote": {Records: true, Quote: "we keep the spout ceramic forever", Why: "states it"},
	}}
	jg := newJudging(st, vj, "haiku", nil)
	got := jg.finish(context.Background(), pend)
	want := []landing.State{landing.Landed, landing.Missing, landing.Missing}
	for i, w := range want {
		if got[i].State != w {
			t.Errorf("%s: %v (%s), want %v", pend[i].c.Decision.ID, got[i].State, got[i].Why, w)
		}
	}
	// The fake judge keeps picking the refused candidate: the row must not
	// carry the judge's own "made-up reason" as if it recorded it.
	if !strings.Contains(got[1].Why, "ruled-out candidate again") {
		t.Errorf("unrelated: why %q", got[1].Why)
	}
	if len(vj.checked) != 3 {
		t.Errorf("checked %v, want every pick", vj.checked)
	}
	// The verified answer is what the cache keeps: a second run asks nothing.
	vj2 := &verifyJudge{verify: map[string]decide.VerifyAnswer{}}
	got2 := newJudging(st, vj2, "haiku", nil).finish(context.Background(), pend)
	if vj2.calls != 0 || len(vj2.checked) != 0 || got2[0].State != landing.Landed || got2[1].State != landing.Missing {
		t.Errorf("second run: judge calls %d, checks %v, states %v %v", vj2.calls, vj2.checked, got2[0].State, got2[1].State)
	}
}

func TestQuoted(t *testing.T) {
	text := "notes.md | Decided: the spout stays   ceramic. Handle is oak."
	for q, want := range map[string]bool{
		"the spout stays ceramic":     true,
		"The spout stays ceramic.":    true,
		"spout stays ceramic, handle": true, // most words there
		"we keep the spout ceramic":   false,
		"":                            false,
	} {
		if got := quoted(q, text); got != want {
			t.Errorf("quoted(%q) = %v, want %v", q, got, want)
		}
	}
}

// nextJudge picks the first candidate not ruled out, and checks picks by
// the record's text: only the one holding "stays ceramic" records it.
type nextJudge struct{ rounds int }

func (n *nextJudge) Judge(_ context.Context, items []decide.JudgeItem) ([]decide.JudgeAnswer, error) {
	n.rounds++
	var out []decide.JudgeAnswer
	for _, it := range items {
		pick := -1
		for i, c := range it.Candidates {
			if !strings.Contains(c, "(ruled out") {
				pick = i
				break
			}
		}
		out = append(out, decide.JudgeAnswer{ID: it.ID, Record: pick, Replaced: -1, Why: "first left"})
	}
	return out, nil
}

func (n *nextJudge) Verify(_ context.Context, items []decide.VerifyItem) ([]decide.VerifyAnswer, error) {
	var out []decide.VerifyAnswer
	for _, it := range items {
		ok := strings.Contains(it.Record, "stays ceramic")
		q := ""
		if ok {
			q = "the spout stays ceramic"
		}
		out = append(out, decide.VerifyAnswer{ID: it.ID, Records: ok, Quote: q, Why: "by text"})
	}
	return out, nil
}

// A refused pick goes back to the judge with that candidate ruled out, so a
// wrong first pick does not hide the real record behind it.
func TestRefusedPickIsJudgedAgain(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := &landing.Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	c, cands := src.Gather(landing.Decision{ID: "d", Cwd: t.TempDir(), Time: t0, Summary: "spout stays ceramic", Keywords: []string{"spout", "ceramic"},
		Later: []transcript.Action{
			{Time: t0.Add(time.Minute), Tool: "Edit", Path: "/x/spout-notes.md", Text: "ceramic spout notes, spout prices"},
			{Time: t0.Add(2 * time.Minute), Tool: "Edit", Path: "/x/AGENTS.md", Text: "Decided: the spout stays ceramic."},
		}})
	if len(cands) < 2 || !strings.Contains(cands[0].Ref, "spout-notes") {
		t.Fatalf("fixture: want the spout notes ranked first, got %+v", cands)
	}
	nj := &nextJudge{}
	got := newJudging(st, nj, "haiku", nil).finish(context.Background(), []pending{{c, cands}})
	if got[0].State != landing.Landed || got[0].Evidence == nil || !strings.Contains(got[0].Evidence.Ref, "AGENTS.md") || nj.rounds != 2 {
		t.Errorf("state %v evidence %+v after %d rounds; want AGENTS.md in round 2", got[0].State, got[0].Evidence, nj.rounds)
	}
}

// roundJudge is nextJudge whose check fails with err in round failRound.
type roundJudge struct {
	nextJudge
	failRound int
	err       error
	checks    int
}

func (r *roundJudge) Verify(ctx context.Context, items []decide.VerifyItem) ([]decide.VerifyAnswer, error) {
	r.checks++
	if r.rounds == r.failRound {
		return nil, r.err
	}
	return r.nextJudge.Verify(ctx, items)
}

// roundsFixture: a decision with four candidates, none of which records it.
func roundsFixture(t *testing.T) (*store.Store, []pending) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	src := &landing.Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	var later []transcript.Action
	for i, n := range []string{"survey", "prices", "makers", "steel"} {
		later = append(later, transcript.Action{Time: t0.Add(time.Duration(i+1) * time.Minute), Tool: "Edit", Path: "/x/" + n + ".md", Text: "ceramic spout " + n})
	}
	c, cands := src.Gather(landing.Decision{ID: "d", Cwd: t.TempDir(), Time: t0, Summary: "spout stays ceramic", Keywords: []string{"spout", "ceramic"}, Later: later})
	if len(cands) < 4 {
		t.Fatalf("fixture: %d candidates", len(cands))
	}
	return st, []pending{{c, cands}}
}

// Three refused picks end the rounds: the row is missing, and that final
// answer is cached, so the next run asks nothing.
func TestThreeRefusalsAreMissingAndCached(t *testing.T) {
	st, pend := roundsFixture(t)
	nj := &nextJudge{}
	got := newJudging(st, nj, "haiku", nil).finish(context.Background(), pend)
	if got[0].State != landing.Missing || nj.rounds != judgeRounds {
		t.Fatalf("state %v (%s) after %d rounds; want missing after %d", got[0].State, got[0].Why, nj.rounds, judgeRounds)
	}
	again := &nextJudge{}
	got = newJudging(st, again, "haiku", nil).finish(context.Background(), pend)
	if got[0].State != landing.Missing || again.rounds != 0 {
		t.Errorf("second run: state %v, %d rounds asked; want missing from the cache", got[0].State, again.rounds)
	}
}

// A check that fails in a later round leaves the row ? (could not check),
// never missing, and caches nothing.
func TestFailedCheckInLaterRoundIsUnknown(t *testing.T) {
	st, pend := roundsFixture(t)
	rj := &roundJudge{failRound: 2, err: errors.New("made-up outage")}
	got := newJudging(st, rj, "haiku", nil).finish(context.Background(), pend)
	if got[0].State != landing.Unknown || !strings.Contains(got[0].Why, "made-up outage") {
		t.Errorf("state %v (%s); want ? naming the failure", got[0].State, got[0].Why)
	}
	if _, ok, _ := st.Judgment(judgeKey(pend[0], "haiku")); ok {
		t.Error("a failed round was cached")
	}
}

// A check refused for an API key stops every later model call: no new round.
func TestAPIKeyInCheckStopsRounds(t *testing.T) {
	st, pend := roundsFixture(t)
	rj := &roundJudge{failRound: 1, err: decide.ErrAPIKey}
	got := newJudging(st, rj, "haiku", nil).finish(context.Background(), pend)
	if got[0].State != landing.Unknown || rj.rounds != 1 || rj.checks != 1 {
		t.Errorf("state %v (%s), %d judge rounds, %d checks; want ? after one of each", got[0].State, got[0].Why, rj.rounds, rj.checks)
	}
}

// A check reads only the decision and the one picked record, so its answer
// is kept: when a later run has new candidates (the judge is asked again) and
// the judge picks the same record, the check is not asked again, and the row
// comes out the same.
func TestCheckOfSamePickIsCached(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cwd := t.TempDir()
	mk := func(notes ...string) []pending {
		var later []transcript.Action
		for i, n := range notes {
			later = append(later, transcript.Action{Time: t0.Add(time.Duration(i+1) * time.Minute), Tool: "Edit", Path: fmt.Sprintf("/x/n%d.md", i), Text: n})
		}
		src := &landing.Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
		c, cands := src.Gather(landing.Decision{ID: "d", Cwd: cwd, Time: t0, Summary: "spout stays ceramic", Keywords: []string{"spout", "ceramic"}, Later: later})
		return []pending{{c, cands}}
	}
	answer := map[string]decide.VerifyAnswer{"d": {Records: true, Quote: "the spout stays ceramic", Why: "states it"}}
	first := &verifyJudge{verify: answer}
	got1 := newJudging(st, first, "haiku", nil).finish(context.Background(), mk("Decided: the spout stays ceramic."))
	if got1[0].State != landing.Landed || len(first.checked) != 1 {
		t.Fatalf("first run: %v (%s), checked %v", got1[0].State, got1[0].Why, first.checked)
	}
	again := &verifyJudge{verify: answer}
	got2 := newJudging(st, again, "haiku", nil).finish(context.Background(), mk("Decided: the spout stays ceramic.", "ceramic spout makers, later"))
	if again.calls != 1 {
		t.Fatalf("judge calls %d on new candidates; want 1 (the fixture must miss the judgment cache)", again.calls)
	}
	if len(again.checked) != 0 {
		t.Errorf("same pick checked again: %v", again.checked)
	}
	if got2[0].State != got1[0].State || got2[0].Why != got1[0].Why {
		t.Errorf("second run %v (%s), first %v (%s)", got2[0].State, got2[0].Why, got1[0].State, got1[0].Why)
	}
}

// slowFast: the judge call for "slow" returns only once "fast" is judged a
// second time (or after 2 s). Every check refuses.
type slowFast struct {
	mu       sync.Mutex
	fast     int
	second   chan struct{}
	timedOut bool
}

func (s *slowFast) Judge(_ context.Context, items []decide.JudgeItem) ([]decide.JudgeAnswer, error) {
	var out []decide.JudgeAnswer
	for _, it := range items {
		rec := -1
		switch it.ID {
		case "fast":
			s.mu.Lock()
			s.fast++
			if s.fast == 1 {
				rec = 0
			}
			if s.fast == 2 {
				close(s.second)
			}
			s.mu.Unlock()
		case "slow":
			select {
			case <-s.second:
			case <-time.After(2 * time.Second):
				s.mu.Lock()
				s.timedOut = true
				s.mu.Unlock()
			}
		}
		out = append(out, decide.JudgeAnswer{ID: it.ID, Record: rec, Replaced: -1, Why: "made-up reason"})
	}
	return out, nil
}

func (s *slowFast) Verify(_ context.Context, items []decide.VerifyItem) ([]decide.VerifyAnswer, error) {
	var out []decide.VerifyAnswer
	for _, it := range items {
		out = append(out, decide.VerifyAnswer{ID: it.ID, Records: false, Why: "made-up refusal"})
	}
	return out, nil
}

// Each decision runs its own rounds: a refused pick is judged again at once,
// not after the slowest other decision of the round is answered.
func TestDecisionDoesNotWaitForSlowerOne(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pend := []pending{gather(t, "slow", "lid", "hinge"), gather(t, "fast", "spout", "filter")}
	sf := &slowFast{second: make(chan struct{})}
	got := newJudging(st, sf, "haiku", nil).finish(context.Background(), pend)
	if sf.timedOut {
		t.Error("fast's second round waited for slow's first judge call")
	}
	for _, c := range got {
		if c.State != landing.Missing {
			t.Errorf("%s: %v (%s), want missing", c.Decision.ID, c.State, c.Why)
		}
	}
}
