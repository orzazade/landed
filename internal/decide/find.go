package decide

import (
	"context"
	"errors"
	"fmt"

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
	var stopped error // an API-key refusal: no more calls this run
	for start := 0; start < len(ask); start += batch {
		end := min(start+batch, len(ask))
		idx := ask[start:end]
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
		items := make([]Item, len(idx))
		for k, i := range idx {
			t := turns[i]
			items[k] = Item{ID: t.UUID, Text: cut(t.Text, maxText), Prev: tailCut(t.PrevAssistant, maxPrev)}
		}
		var answers []Answer
		var err error
		if stopped != nil {
			err = stopped // never call again after an API-key refusal
		} else if answers, err = m.Classify(ctx, items); errors.Is(err, ErrAPIKey) {
			stopped = err
		}
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
		if len(retry) > 0 && stopped == nil {
			again, err2 := m.Classify(ctx, retry)
			if errors.Is(err2, ErrAPIKey) {
				stopped = err2
			}
			for _, a := range again {
				byID[a.ID] = a
			}
			err = err2 // the retry's result decides whether the batch failed
		}
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
