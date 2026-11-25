package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/orzazade/landed/internal/decide"
	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/store"
	"github.com/orzazade/landed/internal/transcript"
)

type fakeJudge struct {
	mu     sync.Mutex
	calls  int
	ids    []string // every item asked, in order
	record map[string]int
	err    error
}

func (f *fakeJudge) Judge(_ context.Context, items []decide.JudgeItem) ([]decide.JudgeAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var out []decide.JudgeAnswer
	for _, it := range items {
		f.ids = append(f.ids, it.ID)
		out = append(out, decide.JudgeAnswer{ID: it.ID, Record: f.record[it.ID], Replaced: -1, Why: "made-up reason"})
	}
	return out, nil
}

// Two decisions with keyword candidates in the session's notes: the judge
// says the first is recorded, the second is not. A second run uses the cache.
// A failing judge leaves rows as ?, never missing.
func TestJudgePicksCachesAndFailsToUnknown(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	dir := t.TempDir()
	mk := func(id, kw1, kw2, note string) pending {
		src := &landing.Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
		c, cands := src.Gather(landing.Decision{ID: id, Cwd: dir, Time: t0, Summary: id, Keywords: []string{kw1, kw2},
			Later: []transcript.Action{{Time: t0.Add(time.Minute), Tool: "Edit", Path: "/x/notes.md", Text: note}}})
		return pending{c, cands}
	}
	pend := []pending{
		mk("spout", "spout", "ceramic", "spout is ceramic now"),
		mk("handle", "handle", "colour", "handle colour research, nothing chosen"),
	}
	fj := &fakeJudge{record: map[string]int{"spout": 0, "handle": -1}}
	jg := &judging{st: st, j: fj}
	got := jg.finish(context.Background(), pend)
	if got[0].State != landing.Landed || got[1].State != landing.Missing || fj.calls != 2 { // a call per decision
		t.Fatalf("got %v %v, calls %d", got[0].State, got[1].State, fj.calls)
	}
	jg2 := &judging{st: st, j: fj}
	jg2.finish(context.Background(), pend)
	if fj.calls != 2 || jg2.cached != 2 {
		t.Errorf("second run asked again: calls %d cached %d", fj.calls, jg2.cached)
	}
	bad := &judging{st: st, j: &fakeJudge{err: errors.New("quota")}}
	pend[0].c.Decision.ID = "spout-2" // not cached
	got = bad.finish(context.Background(), pend[:1])
	if got[0].State != landing.Unknown || len(bad.errs) != 1 {
		t.Errorf("failed judge: %v %q errs %v", got[0].State, got[0].Why, bad.errs)
	}
}

// skipFirst answers every item but drops the last one on its first call.
type skipFirst struct{ calls int }

func (f *skipFirst) Judge(_ context.Context, items []decide.JudgeItem) ([]decide.JudgeAnswer, error) {
	f.calls++
	var out []decide.JudgeAnswer
	for _, it := range items {
		out = append(out, decide.JudgeAnswer{ID: it.ID, Record: 0, Replaced: -1})
	}
	if f.calls == 1 && len(out) > 1 {
		return out[:len(out)-1], errors.New("judge skipped or misnumbered 1 of 2 items")
	}
	return out, nil
}

func TestJudgeRetriesSkippedItems(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var pend []pending
	for _, id := range []string{"lid", "whistle"} {
		src := &landing.Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
		c, cands := src.Gather(landing.Decision{ID: id, Cwd: t.TempDir(), Time: t0, Keywords: []string{"lid", "whistle"},
			Later: []transcript.Action{{Time: t0.Add(time.Minute), Tool: "Edit", Path: "/x/notes.md", Text: "lid whistle"}}})
		pend = append(pend, pending{c, cands})
	}
	f := &skipFirst{}
	jg := &judging{st: st, j: f}
	got := jg.finish(context.Background(), pend)
	if f.calls != 2 || len(jg.errs) != 0 || got[0].State != landing.Landed || got[1].State != landing.Landed {
		t.Errorf("calls %d errs %v states %v %v", f.calls, jg.errs, got[0].State, got[1].State)
	}
}

// badOnce fails to parse on its first call and answers on the second.
type badOnce struct{ calls int }

func (f *badOnce) Judge(_ context.Context, items []decide.JudgeItem) ([]decide.JudgeAnswer, error) {
	f.calls++
	if f.calls == 1 {
		return nil, errors.New("judge reply is not a JSON array: invalid character '$' in string escape code")
	}
	var out []decide.JudgeAnswer
	for _, it := range items {
		out = append(out, decide.JudgeAnswer{ID: it.ID, Record: -1, Replaced: -1})
	}
	return out, nil
}

func TestJudgeRetriesUnparsedReply(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := &landing.Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	c, cands := src.Gather(landing.Decision{ID: "kettle-lid", Cwd: t.TempDir(), Time: t0, Keywords: []string{"lid", "hinge"},
		Later: []transcript.Action{{Time: t0.Add(time.Minute), Tool: "Edit", Path: "/x/notes.md", Text: "lid hinge"}}})
	f := &badOnce{}
	jg := &judging{st: st, j: f}
	got := jg.finish(context.Background(), []pending{{c, cands}})
	if f.calls != 2 || len(jg.errs) != 0 || got[0].State != landing.Missing {
		t.Errorf("calls %d errs %v state %v", f.calls, jg.errs, got[0].State)
	}
}

type keyRefusal struct {
	mu    sync.Mutex
	calls int
}

func (f *keyRefusal) Judge(context.Context, []decide.JudgeItem) ([]decide.JudgeAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return nil, fmt.Errorf("%w (apiKeySource %q)", decide.ErrAPIKey, "ANTHROPIC_API_KEY")
}

func gather(t *testing.T, id string, words ...string) pending {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	src := &landing.Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	c, cands := src.Gather(landing.Decision{ID: id, Cwd: t.TempDir(), Time: t0, Keywords: words,
		Later: []transcript.Action{{Time: t0.Add(time.Minute), Tool: "Edit", Path: "/x/notes.md", Text: strings.Join(words, " ")}}})
	return pending{c, cands}
}

func TestJudgeStopsAtAPIKeyRefusal(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// Calls run side by side (the real judge proves the login once before
	// any of them, see decide's probe tests): after a refusal, no call starts
	// beyond those already running, one per worker.
	var pend []pending
	for i := range 3 * judgeWorkers {
		pend = append(pend, gather(t, fmt.Sprint("d", i), "lid", fmt.Sprint("hinge", i)))
	}
	f := &keyRefusal{}
	jg := &judging{st: st, j: f, batch: 1}
	got := jg.finish(context.Background(), pend)
	if f.calls > judgeWorkers {
		t.Errorf("judge called %d times after an API-key refusal; want at most one per worker (%d)", f.calls, judgeWorkers)
	}
	for _, c := range got {
		if c.State != landing.Unknown {
			t.Errorf("%s: %v after refusal", c.Decision.ID, c.State)
		}
	}
}

// The cache key changes with the judge model and the candidate set.
func TestJudgeKeyCoversModelAndCandidates(t *testing.T) {
	p := gather(t, "a", "lid", "hinge")
	k := judgeKey(p, "haiku")
	if judgeKey(p, "sonnet") == k {
		t.Error("model not in key")
	}
	q := p
	q.cands = append(append([]landing.Evidence(nil), p.cands...), landing.Evidence{Kind: "commit", Ref: "kettle 1a2b3c4 lid hinge"})
	if judgeKey(q, "haiku") == k {
		t.Error("candidates not in key")
	}
}

type noJudge struct{ calls int }

func (n *noJudge) Judge(context.Context, []decide.JudgeItem) ([]decide.JudgeAnswer, error) {
	n.calls++
	return nil, nil
}

// Only lasting decisions are counted: a one-off action ("review the spout
// code") is dropped, also while it is too new; an owner-queue answer counts
// even when the model called it a one-off.
func TestCheckResultsCountsOnlyLasting(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	now := t0.Add(48 * time.Hour)
	sess := &transcript.Session{ID: "s", Cwd: t.TempDir()}
	mk := func(id string, at time.Time, kind, source string) decide.Result {
		turn := transcript.Turn{UUID: id, SessionID: "s", Time: at, Text: id}
		sess.Turns = append(sess.Turns, turn)
		return decide.Result{Turn: turn, Known: true, Verdict: store.Verdict{Decision: true, Kind: kind, Source: source, Summary: id, Keywords: []string{"spout", "lid"}}}
	}
	res := []decide.Result{
		mk("action", t0, "action", "haiku"),
		mk("young-action", now.Add(-time.Hour), "action", "haiku"),
		mk("queue", t0, "action", "rule:queue-answer"),
		mk("rule", t0, "rule", "haiku"),
		mk("no-kind", t0, "", "rule:short-answer"),
	}
	sessOf := map[string]*transcript.Session{}
	for _, r := range res {
		sessOf[r.Turn.UUID] = sess
	}
	src := &landing.Sources{Window: 7 * 24 * time.Hour, Now: now}
	checks, tooNew, _ := checkResults(context.Background(), res, sessOf, src, now, 4*time.Hour, nil)
	var ids []string
	for _, c := range checks {
		ids = append(ids, c.Decision.ID)
	}
	if strings.Join(ids, ",") != "queue,rule,no-kind" || tooNew != 0 {
		t.Errorf("checked %v, too new %d; want queue,rule,no-kind and 0 (one-off actions are not counted)", ids, tooNew)
	}
}

// A decision the judge found landed keeps that landing on later runs, even
// when new candidates appear: the judge is not asked again. A missing one is
// asked again when its candidates change (a landing may have appeared).
func TestLandedOutcomeIsCached(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	mk := func(id string, words []string, notes ...string) (decide.Result, *transcript.Session) {
		turn := transcript.Turn{UUID: id, SessionID: "s" + id, Time: t0, Text: strings.Join(words, " ")}
		s := &transcript.Session{ID: "s" + id, Cwd: t.TempDir(), Turns: []transcript.Turn{turn}}
		for i, n := range notes {
			s.Actions = append(s.Actions, transcript.Action{Time: t0.Add(time.Duration(i+1) * time.Minute), Tool: "Edit", Path: "/x/notes.md", Text: n})
		}
		return decide.Result{Turn: turn, Known: true, Verdict: store.Verdict{Decision: true, Kind: "rule", Summary: strings.Join(words, " "), Keywords: words}}, s
	}
	run := func(j *fakeJudge, extra string) []landing.Check {
		ra, sa := mk("a", []string{"lid", "hinge"}, "lid hinge is steel", extra)
		rb, sb := mk("b", []string{"spout", "filter"}, "spout filter later", extra)
		src := &landing.Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
		checks, _, _ := checkResults(context.Background(), []decide.Result{ra, rb},
			map[string]*transcript.Session{"a": sa, "b": sb}, src, t0.Add(48*time.Hour), 0, &judging{st: st, j: j, model: "m"})
		return checks
	}
	j := &fakeJudge{record: map[string]int{"a": 0, "b": -1}}
	first := run(j, "lid hinge spout filter v1")
	if first[0].State != landing.Landed || first[1].State != landing.Missing || j.calls != 2 { // a call per decision
		t.Fatalf("first run: %v %v, %d calls", first[0].State, first[1].State, j.calls)
	}
	j2 := &fakeJudge{record: map[string]int{"a": -1, "b": -1}} // would now say "a" is missing
	second := run(j2, "lid hinge spout filter v2")             // new candidate text
	if second[0].State != landing.Landed || second[0].Evidence == nil || second[0].Evidence.Ref != first[0].Evidence.Ref {
		t.Errorf("landed outcome not reused: %+v", second[0])
	}
	if strings.Join(j2.ids, ",") != "b" {
		t.Errorf("judge asked %v on the second run, want only b (its candidates changed)", j2.ids)
	}
}

type safeJudge struct {
	mu  sync.Mutex
	ids []string
}

func (f *safeJudge) Judge(_ context.Context, items []decide.JudgeItem) ([]decide.JudgeAnswer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []decide.JudgeAnswer
	for _, it := range items {
		f.ids = append(f.ids, it.ID)
		out = append(out, decide.JudgeAnswer{ID: it.ID, Record: 0, Replaced: -1, Why: "made-up reason"})
	}
	return out, nil
}

// Many batches run at once; every decision gets its own answer.
func TestJudgeBatchesInParallel(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var pend []pending
	for i := range 9 {
		pend = append(pend, gather(t, fmt.Sprint("d", i), "lid", fmt.Sprint("hinge", i)))
	}
	j := &safeJudge{}
	jg := &judging{st: st, j: j, batch: 1}
	got := jg.finish(context.Background(), pend)
	for i, c := range got {
		if c.State != landing.Landed || c.Decision.ID != fmt.Sprint("d", i) {
			t.Errorf("row %d: %s %v", i, c.Decision.ID, c.State)
		}
	}
	if len(j.ids) != 9 || jg.asked != 9 {
		t.Errorf("asked %v (%d)", j.ids, jg.asked)
	}
}

// An API-key refusal in the classifier stops the judge too: no call is made,
// and the rows stay ? (never missing).
func TestClassifierRefusalStopsJudge(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src := &landing.Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	c, cands := src.Gather(landing.Decision{ID: "spout", Cwd: t.TempDir(), Time: t0, Summary: "spout", Keywords: []string{"spout", "ceramic"},
		Later: []transcript.Action{{Time: t0.Add(time.Minute), Tool: "Edit", Path: "/x/notes.md", Text: "spout is ceramic"}}})
	fj := &fakeJudge{record: map[string]int{"spout": 0}}
	refused := fmt.Errorf("model batch 0-25: %w", decide.ErrAPIKey)
	jg := newJudging(st, fj, "haiku", []error{errors.New("other"), refused})
	got := jg.finish(context.Background(), []pending{{c, cands}})
	if fj.calls != 0 || got[0].State != landing.Unknown {
		t.Errorf("calls %d, state %v (%s)", fj.calls, got[0].State, got[0].Why)
	}
}

// The judge can say a later candidate replaces the decision: the row is
// superseded, not missing, the answer is cached, and the hook does not list it.
func TestJudgeReplacedIsSuperseded(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pend := []pending{gather(t, "spout", "spout", "ceramic")}
	fj := &replacer{}
	jg := &judging{st: st, j: fj}
	got := jg.finish(context.Background(), pend)
	if got[0].State != landing.Superseded || got[0].Evidence == nil {
		t.Fatalf("got %v %+v", got[0].State, got[0].Evidence)
	}
	if n := len(unlanded(got)); n != 0 {
		t.Errorf("superseded row listed by the hook (%d rows)", n)
	}
	jg2 := &judging{st: st, j: fj}
	if again := jg2.finish(context.Background(), pend); fj.calls != 1 || again[0].State != landing.Superseded {
		t.Errorf("cached run: calls %d state %v", fj.calls, again[0].State)
	}
}

type replacer struct{ calls int }

func (f *replacer) Judge(_ context.Context, items []decide.JudgeItem) ([]decide.JudgeAnswer, error) {
	f.calls++
	var out []decide.JudgeAnswer
	for _, it := range items {
		out = append(out, decide.JudgeAnswer{ID: it.ID, Record: -1, Replaced: 0, Why: "made-up: a later choice replaces it"})
	}
	return out, nil
}

// barrierJudge holds each judge or check call until want calls of that kind
// are in flight (or 2 s pass), and records the items per call. Every pick
// is a record and passes its check.
type barrierJudge struct {
	want int
	mu   sync.Mutex
	kind map[string]*barrier
}

type barrier struct {
	active, maxSeen int
	sizes           []int
	all             chan struct{}
}

func (b *barrierJudge) hold(kind string, n int) {
	b.mu.Lock()
	if b.kind == nil {
		b.kind = map[string]*barrier{}
	}
	k := b.kind[kind]
	if k == nil {
		k = &barrier{all: make(chan struct{})}
		b.kind[kind] = k
	}
	k.active++
	k.maxSeen = max(k.maxSeen, k.active)
	k.sizes = append(k.sizes, n)
	if k.active == b.want {
		close(k.all)
	}
	b.mu.Unlock()
	select {
	case <-k.all:
	case <-time.After(2 * time.Second):
	}
	b.mu.Lock()
	k.active--
	b.mu.Unlock()
}

func (b *barrierJudge) Judge(_ context.Context, items []decide.JudgeItem) ([]decide.JudgeAnswer, error) {
	b.hold("judge", len(items))
	var out []decide.JudgeAnswer
	for _, it := range items {
		out = append(out, decide.JudgeAnswer{ID: it.ID, Record: 0, Replaced: -1, Why: "made-up reason"})
	}
	return out, nil
}

func (b *barrierJudge) Verify(_ context.Context, items []decide.VerifyItem) ([]decide.VerifyAnswer, error) {
	b.hold("check", len(items))
	var out []decide.VerifyAnswer
	for _, it := range items {
		out = append(out, decide.VerifyAnswer{ID: it.ID, Records: true, Quote: it.Record, Why: "made-up reason"})
	}
	return out, nil
}

// A cold run's open decisions are spread over every judge worker at once,
// none alone: 7 decisions are 7 calls of one side by side, and so are the
// checks of their picks, not one call of 7 each.
func TestJudgeSpreadsDecisionsOverWorkers(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var pend []pending
	for i := range 7 {
		pend = append(pend, gather(t, fmt.Sprint("d", i), "lid", fmt.Sprint("hinge", i)))
	}
	bj := &barrierJudge{want: 7}
	got := newJudging(st, bj, "haiku", nil).finish(context.Background(), pend)
	for i, c := range got {
		if c.State != landing.Landed {
			t.Errorf("row %d: %v (%s)", i, c.State, c.Why)
		}
	}
	for _, kind := range []string{"judge", "check"} {
		k := bj.kind[kind]
		if k == nil {
			t.Errorf("no %s call", kind)
			continue
		}
		if len(k.sizes) != 7 || k.maxSeen != 7 || slices.Max(k.sizes) != 1 {
			t.Errorf("%s calls with %v items, at most %d at once; want 7 calls of one, all at once", kind, k.sizes, k.maxSeen)
		}
	}
}
