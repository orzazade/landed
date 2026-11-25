package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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
	v             decide.Verifier // checks each pick on its own; nil: picks stand
	model         string          // part of the cache key
	batch         int             // decisions per call; 0: spread over the workers, at most 8
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

// checkKey names a check of one pick: everything the check reads.
func checkKey(it decide.VerifyItem, model string) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\x00%s", decide.JudgeVersion, model, it.ID, it.Decision, it.Words, it.Record)
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

// judgeItem is what the judge reads for one decision. Candidates an
// earlier round's check refused are shown as ruled out, keeping their numbers.
func judgeItem(p pending, ruled map[int]string) decide.JudgeItem {
	it := decide.JudgeItem{ID: p.c.Decision.ID, Decision: p.c.Decision.Summary, Words: quote(p.c.Decision.Text, 600)}
	for i, e := range p.cands {
		if why, out := ruled[i]; out {
			it.Candidates = append(it.Candidates, fmt.Sprintf("%d: (ruled out: %s)", i, why))
			continue
		}
		it.Candidates = append(it.Candidates, fmt.Sprintf("%d: %s %s | %s", i, e.Kind, e.Ref, e.Text))
	}
	return it
}

// judgeRounds is how often a decision is judged when the check refuses
// the pick: the first pick, then twice more with refused picks ruled out.
const judgeRounds = 3

func (jg *judging) finish(ctx context.Context, pend []pending) []landing.Check {
	out := make([]landing.Check, len(pend))
	var asks []pick
	for i, p := range pend {
		switch {
		case !p.c.Gathered():
			out[i] = p.c
		case len(p.cands) == 0:
			out[i] = landing.Finish(p.c, nil, -1, -1, "")
		default:
			key := judgeKey(p, jg.model)
			if j, ok, err := jg.st.Judgment(key); err != nil {
				jg.fail(&out[i], p, "judgment cache: "+err.Error())
			} else if ok {
				jg.cached++
				out[i] = landing.Finish(p.c, p.cands, j.Record, j.Replaced, "judge: "+j.Why)
			} else {
				asks = append(asks, pick{i: i, key: key, ruled: map[int]string{}})
			}
		}
	}
	// Each group of decisions (one call's worth) runs its own rounds, groups
	// side by side (the judge proves the login once before): a refused pick
	// is judged again at once, never after a slower group's calls.
	n := jg.perCall(len(asks))
	var wg sync.WaitGroup
	sem := make(chan struct{}, judgeWorkers)
	for len(asks) > 0 {
		g := asks[:min(n, len(asks))]
		asks = asks[len(g):]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			jg.rounds(ctx, pend, g, out)
		}()
	}
	wg.Wait()
	return out
}

// rounds judges one group of decisions: a call, the check of its picks,
// and again with refused picks ruled out, up to judgeRounds times. It writes
// only out[i] of its own decisions.
func (jg *judging) rounds(ctx context.Context, pend []pending, asks []pick, out []landing.Check) {
	for round := 1; len(asks) > 0; round++ {
		picks := jg.round(ctx, pend, asks, out)
		jg.verify(ctx, pend, picks)
		asks = nil
		for _, pk := range picks {
			p, r := pend[pk.i], pk.ans
			if pk.failed != "" {
				jg.fail(&out[pk.i], p, pk.failed)
				continue
			}
			if _, again := pk.ruled[r.Record]; again {
				r.Record, r.Why = -1, "judge picked a ruled-out candidate again" // its own reason would say "recorded"
			}
			if pk.refused >= 0 {
				pk.ruled[pk.refused] = pk.ans.Why
				if round < judgeRounds {
					pk.refused = -1
					asks = append(asks, pk)
					continue
				}
			}
			if e := jg.st.PutJudgment(store.Judgment{Key: pk.key, Record: r.Record, Replaced: r.Replaced, Why: r.Why}); e != nil {
				jg.addErr("judgment cache: " + e.Error())
			}
			out[pk.i] = landing.Finish(p.c, p.cands, r.Record, r.Replaced, "judge: "+r.Why)
		}
	}
}

// round asks the judge about every ask in one call; a decision the judge
// does not answer becomes ? in out.
func (jg *judging) round(ctx context.Context, pend []pending, asks []pick, out []landing.Check) []pick {
	items := make([]decide.JudgeItem, len(asks))
	for i, a := range asks {
		items[i] = judgeItem(pend[a.i], a.ruled)
	}
	ans, err := jg.call(ctx, items)
	byID := map[string]decide.JudgeAnswer{}
	for _, a := range ans {
		byID[a.ID] = a
	}
	var picks []pick
	for _, a := range asks {
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
		a.ans, a.refused = r, -1
		picks = append(picks, a)
	}
	return picks
}

// pick is the judge's answer for pend[i], before it is stored.
type pick struct {
	i       int
	key     string
	ans     decide.JudgeAnswer
	ruled   map[int]string // candidates refused in earlier rounds, with why
	refused int            // this round's pick, refused by the check (-1: not refused)
	failed  string         // the check of the pick failed: the row is ?
}

// verify checks every pick of a record, in one call apart from the judge's.
// A pick the check refuses, or whose quote is not in the record, becomes
// "no record" and is marked refused, so the next round rules it out.
// With no verifier (tests that do not check picks) picks stand as given.
func (jg *judging) verify(ctx context.Context, pend []pending, picks []pick) {
	if jg.v == nil {
		return
	}
	var idx []int // picks to check
	var items []decide.VerifyItem
	for k, pk := range picks {
		p, r := pend[pk.i], pk.ans.Record
		if _, again := pk.ruled[r]; again || r < 0 || r >= len(p.cands) || p.cands[r].Kind == "decision" {
			continue
		}
		e := p.cands[r]
		idx = append(idx, k)
		items = append(items, decide.VerifyItem{ID: p.c.Decision.ID, Decision: p.c.Decision.Summary,
			Words: quote(p.c.Decision.Text, 600), Record: e.Kind + " " + e.Ref + " | " + e.Text})
	}
	// A check reads only the decision and the picked record: one asked
	// before (in this run or an earlier one) is answered from the cache.
	ans := map[string]decide.VerifyAnswer{}
	var ask []decide.VerifyItem
	for _, it := range items {
		c, ok, err := jg.st.PickCheck(checkKey(it, jg.model))
		if err != nil {
			jg.addErr("check cache: " + err.Error()) // loud, then asked as usual
		}
		if ok {
			ans[it.ID] = decide.VerifyAnswer{ID: it.ID, Records: c.Records, Quote: c.Quote, Why: c.Why}
			continue
		}
		ask = append(ask, it)
	}
	var err error
	if len(ask) > 0 {
		jg.mu.Lock()
		stopped := jg.stopped
		jg.mu.Unlock()
		var got []decide.VerifyAnswer
		if stopped != nil {
			err = stopped
		} else {
			got, err = jg.v.Verify(ctx, ask)
			jg.stop(err)
		}
		for _, a := range got {
			ans[a.ID] = a
		}
		for _, it := range ask {
			if a, ok := ans[it.ID]; ok {
				if e := jg.st.PutPickCheck(store.PickCheck{Key: checkKey(it, jg.model), Records: a.Records, Quote: a.Quote, Why: a.Why}); e != nil {
					jg.addErr("check cache: " + e.Error())
				}
			}
		}
	}
	for j, k := range idx {
		pk := &picks[k]
		a, ok := ans[items[j].ID]
		switch {
		case !ok:
			pk.failed = "check of the judge's pick gave no answer"
			if err != nil {
				pk.failed = "check of the judge's pick failed: " + err.Error()
			}
		case !a.Records:
			pk.refused = pk.ans.Record
			pk.ans.Record, pk.ans.Why = -1, "pick refused: "+a.Why
		case !quoted(a.Quote, items[j].Record):
			pk.refused = pk.ans.Record
			pk.ans.Record, pk.ans.Why = -1, "pick refused: its quote is not in the record"
		}
	}
}

// quoted reports whether quote stands in text: as written (case and spaces
// aside), or with at least 80% of its words (3+ letters) found in text.
func quoted(quote, text string) bool {
	norm := func(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }
	q := strings.Trim(norm(quote), " .,;:!?\"'“”‘’…")
	if q == "" {
		return false
	}
	t := norm(text)
	if strings.Contains(t, q) {
		return true
	}
	have := map[string]bool{}
	for _, w := range wordRe.FindAllString(t, -1) {
		have[w] = true
	}
	n, found := 0, 0
	for _, w := range wordRe.FindAllString(q, -1) {
		if len([]rune(w)) < 3 {
			continue
		}
		n++
		if have[w] {
			found++
		}
	}
	return n > 0 && found*5 >= n*4
}

var wordRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

// judgeWorkers is how many judge calls run at once.
const judgeWorkers = 8

// perCall is how many of n decisions go in one judge or check call: the set
// batch, else spread over the workers, at most 8 a call. A decision carries
// dozens of candidates, so up to judgeWorkers decisions each get a call of
// their own.
func (jg *judging) perCall(n int) int {
	if jg.batch > 0 {
		return jg.batch
	}
	return decide.PerCall(n, 1, 8, judgeWorkers)
}

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
	if jg.stopped != nil { // another call found an API key meanwhile
		jg.mu.Unlock()
		return ans, jg.stopped
	}
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

// addErr records a failure; groups of decisions run side by side.
func (jg *judging) addErr(msg string) {
	jg.mu.Lock()
	defer jg.mu.Unlock()
	jg.errs = append(jg.errs, msg)
}

// fail leaves the row as ?: the candidates were never judged.
func (jg *judging) fail(c *landing.Check, p pending, msg string) {
	jg.addErr(p.c.Decision.ID + ": " + msg)
	*c = p.c
	c.State = landing.Unknown
	c.Why = msg
}

// newJudging starts a judge for one run. An API-key refusal among the
// classifier's errors (classifyErrs) stops it before its first call.
func newJudging(st *store.Store, j decide.Judge, model string, classifyErrs []error) *judging {
	jg := &judging{st: st, j: j, model: model}
	if v, ok := j.(decide.Verifier); ok {
		jg.v = v
	}
	for _, err := range classifyErrs {
		if errors.Is(err, decide.ErrAPIKey) {
			jg.stopped = err
			break
		}
	}
	return jg
}
