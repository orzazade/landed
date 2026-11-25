package decide

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/orzazade/landed/internal/store"
	"github.com/orzazade/landed/internal/transcript"
)

// Result is the call for one turn. Known=false means "could not check", with
// the reason in Err: it is never read as "not a decision".
type Result struct {
	Turn    transcript.Turn
	Verdict store.Verdict
	Known   bool
	Err     string
}

// Stats counts where each call came from.
type Stats struct {
	Turns, Cached, ByRule, ByModel, Unknown int
	Errors                                  []error
}

const maxText, maxPrev = 1500, 400

// modelWorkers is how many model calls run at once.
const modelWorkers = 8

// PerCall is how many of n items go in one call: spread over every worker,
// so a cold run waits about one short call, not one long call; at least
// least and at most most a call.
func PerCall(n, least, most, workers int) int {
	return min(most, max(least, (n+workers-1)/workers))
}

// minKeywords is how many keywords a decision needs for its landing check.
const minKeywords = 2

// Find calls every turn: the cache first, then the rules, then the model for
// what is left (in batches). Every new call is cached by turn id. With m nil,
// turns the rules cannot settle come back unknown.
func Find(ctx context.Context, turns []transcript.Turn, st *store.Store, m Model, batch int) ([]Result, Stats) {
	if batch <= 0 {
		batch = 25
	}
	var stats Stats
	stats.Turns = len(turns)
	res := make([]Result, len(turns))
	var ask []int // indexes that need the model
	for i, t := range turns {
		res[i].Turn = t
		if v, ok, err := st.Verdict(t.UUID, PromptVersion); err != nil {
			stats.Errors = append(stats.Errors, err)
		} else if ok {
			res[i].Verdict, res[i].Known = v, true
			stats.Cached++
			continue
		}
		call, why := Rule(t)
		res[i].Verdict = store.Verdict{TurnID: t.UUID, Version: PromptVersion, Source: "rule:" + why}
		switch call {
		case Not:
			res[i].Known = true
			stats.ByRule++
			if err := st.PutVerdict(res[i].Verdict); err != nil {
				stats.Errors = append(stats.Errors, err)
			}
		case Decision:
			res[i].Verdict.Decision = true
			ask = append(ask, i) // decided; the model writes summary and keywords
		default:
			ask = append(ask, i)
		}
	}
	// The model calls run first, side by side (the model proves the login
	// once before them). The answers are then read in batch order.
	type reply struct {
		byID map[string]Answer
		err  error
	}
	batch = PerCall(len(ask), 2, batch, modelWorkers) // 2: each call also sends the prompt
	var batches [][]int
	for start := 0; start < len(ask); start += batch {
		batches = append(batches, ask[start:min(start+batch, len(ask))])
	}
	replies := make([]reply, len(batches))
	var mu sync.Mutex
	var stopped error // an API-key refusal: no more calls this run
	call := func(items []Item) ([]Answer, error) {
		mu.Lock()
		s := stopped
		mu.Unlock()
		if s != nil {
			return nil, s // never call again after an API-key refusal
		}
		a, err := m.Classify(ctx, items)
		if errors.Is(err, ErrAPIKey) {
			mu.Lock()
			if stopped == nil {
				stopped = err
			}
			mu.Unlock()
		}
		return a, err
	}
	ask1 := func(k int) {
		idx := batches[k]
		items := make([]Item, len(idx))
		for j, i := range idx {
			t := turns[i]
			items[j] = Item{ID: t.UUID, Text: cut(t.Text, maxText), Prev: tailCut(t.PrevAssistant, maxPrev)}
		}
		answers, err := call(items)
		byID := map[string]Answer{}
		for _, a := range answers {
			byID[a.ID] = a
		}
		// The model sometimes skips an item in a long batch: ask once more for those.
		var retry []Item
		for _, it := range items {
			if _, ok := byID[it.ID]; !ok {
				retry = append(retry, it)
			}
		}
		if len(retry) > 0 && !errors.Is(err, ErrAPIKey) {
			again, err2 := call(retry)
			for _, a := range again {
				byID[a.ID] = a
			}
			err = err2 // the retry's result decides whether the batch failed
		}
		replies[k] = reply{byID, err}
	}
	if m != nil && len(batches) > 0 {
		var wg sync.WaitGroup
		sem := make(chan struct{}, modelWorkers)
		for k := range batches {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				ask1(k)
			}()
		}
		wg.Wait()
	}
	for k, idx := range batches {
		start := k * batch
		end := start + len(idx)
		if m == nil {
			for _, i := range idx {
				if res[i].Verdict.Decision {
					res[i].Known = true // rule decision, no summary
					stats.ByRule++
					continue
				}
				res[i].Err = "rules could not decide and the model is off"
				stats.Unknown++
			}
			continue
		}
		byID, err := replies[k].byID, replies[k].err
		if err != nil {
			stats.Errors = append(stats.Errors, fmt.Errorf("model batch %d-%d: %w", start, end, err))
		}
		for _, i := range idx {
			a, ok := byID[turns[i].UUID]
			if !ok && res[i].Verdict.Decision {
				// A rule decision stays one without the model; it just has no
				// keywords, so its landing check will say why it cannot match.
				res[i].Known = true
				stats.ByRule++
				continue
			}
			if !ok {
				res[i].Err = "model gave no answer"
				if err != nil {
					res[i].Err = "model failed: " + err.Error()
				}
				stats.Unknown++
				continue
			}
			v := &res[i].Verdict
			if v.Source == "rule:" { // unclear: the model decides
				v.Source = "haiku"
				v.Decision = a.Decision
				stats.ByModel++
			} else {
				stats.ByRule++
			}
			v.Summary, v.Keywords = a.Summary, a.Keywords
			if v.Decision {
				v.Kind = a.Kind // a rule decision takes the model's kind; the model's "not" has none
				if v.Kind == "" {
					v.Kind = "rule" // the model called a rule-decided turn "not": treat it as lasting, the careful side
				}
			}
			res[i].Known = true
			if v.Decision && len(v.Keywords) < minKeywords {
				continue // not cached: the next run asks again for keywords to match on
			}
			if err := st.PutVerdict(*v); err != nil {
				stats.Errors = append(stats.Errors, err)
			}
		}
	}
	return res, stats
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + " …"
}

func tailCut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "… " + string(r[len(r)-n:])
}
