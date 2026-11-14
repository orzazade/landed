package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/orzazade/landed/internal/decide"
	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/store"
)

type pending struct {
	c     landing.Check
	cands []landing.Evidence
}

// judging asks the model which candidate records each decision, caching each
// answer under the decision, the judge version and the exact candidates.
type judging struct {
	st            *store.Store
	j             decide.Judge
	model         string // part of the cache key
	batch         int    // decisions per call; 0: 8
	errs          []string
	cached, asked int
	reused        int   // landed outcomes reused from earlier runs
	stopped       error // an API-key refusal: no more calls this run
	mu            sync.Mutex
}

func judgeKey(p pending, model string) string {
	h := sha256.New()
	d := p.c.Decision
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00", decide.JudgeVersion, model, d.ID, d.Summary, d.Text)
	for _, e := range p.cands { // everything the judge reads
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00", e.Kind, e.Ref, e.When.UTC(), e.Text)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// landingKey names a decision's landed outcome: the same decision, read the
// same way (classifier and judge versions, model, window).
func landingKey(d landing.Decision, model string, window time.Duration) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s", decide.PromptVersion, decide.JudgeVersion, landing.GatherVersion, model,
		window, d.ID, d.Kind, d.Summary, d.Text, strings.Join(d.Keywords, ","))
	return hex.EncodeToString(h.Sum(nil))
}

// cachedLanding returns the stored landed outcome of d, if there is one.
func (jg *judging) cachedLanding(d landing.Decision, window time.Duration) (landing.Check, bool) {
	l, ok, err := jg.st.Landing(landingKey(d, jg.model, window))
	if err != nil {
		jg.errs = append(jg.errs, "landing cache: "+err.Error()) // loud, then judged as usual
		return landing.Check{}, false
	}
	if !ok {
		return landing.Check{}, false
	}
	var ev landing.Evidence
	if err := json.Unmarshal([]byte(l.Evidence), &ev); err != nil {
		jg.errs = append(jg.errs, "landing cache: "+err.Error())
		return landing.Check{}, false
	}
	jg.reused++
	return landing.Check{Decision: d, State: landing.Landed, Evidence: &ev, Why: l.Why}, true
}

// keepLandings stores every judged landed outcome for the next runs.
func (jg *judging) keepLandings(checks []landing.Check, window time.Duration) {
	for _, c := range checks {
		if c.State != landing.Landed || c.Evidence == nil {
			continue
		}
		ev, _ := json.Marshal(c.Evidence)
		if err := jg.st.PutLanding(store.Landing{Key: landingKey(c.Decision, jg.model, window), Evidence: string(ev), Why: c.Why}); err != nil {
			jg.errs = append(jg.errs, "landing cache: "+err.Error())
		}
	}
}

func judgeItem(p pending) decide.JudgeItem {
	it := decide.JudgeItem{ID: p.c.Decision.ID, Decision: p.c.Decision.Summary, Words: quote(p.c.Decision.Text, 600)}
	for i, e := range p.cands {
		it.Candidates = append(it.Candidates, fmt.Sprintf("%d: %s %s | %s", i, e.Kind, e.Ref, e.Text))
	}
	return it
}

func (jg *judging) finish(ctx context.Context, pend []pending) []landing.Check {
	out := make([]landing.Check, len(pend))
	type ask struct {
		i   int
		key string
	}
	var asks []ask
	for i, p := range pend {
		switch {
		case !p.c.Gathered():
			out[i] = p.c
		case len(p.cands) == 0:
			out[i] = landing.Finish(p.c, nil, -1, "")
		default:
			key := judgeKey(p, jg.model)
			if j, ok, err := jg.st.Judgment(key); err != nil {
				jg.fail(&out[i], p, "judgment cache: "+err.Error())
			} else if ok {
				jg.cached++
				out[i] = landing.Finish(p.c, p.cands, j.Record, "judge: "+j.Why)
			} else {
				asks = append(asks, ask{i, key})
			}
		}
	}
	n := jg.batch
	if n <= 0 {
		n = 8
	}
	var batches [][]ask
	for len(asks) > 0 {
		b := asks[:min(n, len(asks))]
		asks = asks[len(b):]
		batches = append(batches, b)
	}
	type reply struct {
		ans []decide.JudgeAnswer
		err error
	}
	replies := make([]reply, len(batches))
	call := func(k int) {
		items := make([]decide.JudgeItem, len(batches[k]))
		for i, a := range batches[k] {
			items[i] = judgeItem(pend[a.i])
		}
		replies[k].ans, replies[k].err = jg.call(ctx, items)
	}
	// The first batch goes alone: it proves the login before any other call
	// starts. The rest run a few at a time.
	if len(batches) > 0 {
		call(0)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, judgeWorkers)
	for k := 1; k < len(batches); k++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			call(k)
		}()
	}
	wg.Wait()
	for k, b := range batches {
		ans, err := replies[k].ans, replies[k].err
		byID := map[string]decide.JudgeAnswer{}
		for _, a := range ans {
			byID[a.ID] = a
		}
		for _, a := range b {
			p := pend[a.i]
			r, ok := byID[p.c.Decision.ID]
			if !ok {
				msg := "judge gave no answer"
				if err != nil {
					msg = "judge failed: " + err.Error()
				}
				jg.fail(&out[a.i], p, msg)
				continue
			}
			if e := jg.st.PutJudgment(store.Judgment{Key: a.key, Record: r.Record, Why: r.Why}); e != nil {
				jg.errs = append(jg.errs, "judgment cache: "+e.Error())
			}
			out[a.i] = landing.Finish(p.c, p.cands, r.Record, "judge: "+r.Why)
		}
	}
	return out
}

// judgeWorkers is how many judge calls run at once after the first.
const judgeWorkers = 4

// call asks the judge about one batch, and once more for the items it skipped
// or could not parse. After an API-key refusal nothing is called again.
func (jg *judging) call(ctx context.Context, items []decide.JudgeItem) ([]decide.JudgeAnswer, error) {
	jg.mu.Lock()
	if jg.stopped != nil {
		jg.mu.Unlock()
		return nil, jg.stopped // never call again after an API-key refusal
	}
	jg.asked += len(items)
	jg.mu.Unlock()
	ans, err := jg.j.Judge(ctx, items)
	if jg.stop(err) || err == nil {
		return ans, err
	}
	got := map[string]bool{}
	for _, a := range ans {
		got[a.ID] = true
	}
	var again []decide.JudgeItem
	for _, it := range items {
		if !got[it.ID] {
			again = append(again, it)
		}
	}
	if len(again) == 0 {
		return ans, err // every item answered: no second call
	}
	jg.mu.Lock()
	jg.asked += len(again)
	jg.mu.Unlock()
	more, err2 := jg.j.Judge(ctx, again)
	jg.stop(err2)
	return append(ans, more...), err2
}

// stop records an API-key refusal; it reports whether the run is stopped.
func (jg *judging) stop(err error) bool {
	jg.mu.Lock()
	defer jg.mu.Unlock()
	if errors.Is(err, decide.ErrAPIKey) && jg.stopped == nil {
		jg.stopped = err
	}
	return jg.stopped != nil
}

// fail leaves the row as ?: the candidates were never judged.
func (jg *judging) fail(c *landing.Check, p pending, msg string) {
	jg.errs = append(jg.errs, p.c.Decision.ID+": "+msg)
	*c = p.c
	c.State = landing.Unknown
	c.Why = msg
}

// newJudging starts a judge for one run. An API-key refusal among the
// classifier's errors (classifyErrs) stops it before its first call.
func newJudging(st *store.Store, j decide.Judge, model string, classifyErrs []error) *judging {
	jg := &judging{st: st, j: j, model: model}
	for _, err := range classifyErrs {
		if errors.Is(err, decide.ErrAPIKey) {
			jg.stopped = err
			break
		}
	}
	return jg
}
