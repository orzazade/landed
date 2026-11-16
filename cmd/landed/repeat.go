package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/orzazade/landed/internal/decide"
	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/store"
)

// repeatDec is one lasting owner decision for the repeat count.
type repeatDec struct {
	ID, Session, Text, Summary string
	Time                       time.Time
	Keywords                   []string
}

// repeat: Later states Earlier again.
type repeat struct {
	Later, Earlier repeatDec
	Why            string
}

type repeats struct {
	Found         map[string]repeat // by later decision id
	Failed        map[string]bool   // later decisions whose answer failed
	Copies        map[string]bool   // later copies of one message sent to several tabs: never counted
	Asked, Cached int
	Errs          []string
}

const (
	repeatGap  = time.Hour           // closer than this is the same exchange, not a restatement
	repeatSpan = 30 * 24 * time.Hour // how far back an earlier decision is looked for
	copySpan   = 10 * time.Minute    // copies of one message to several tabs are sent within this
	maxEarlier = 5
)

// copiesOf finds messages sent to several tabs: a decision in another
// session within copySpan of an earlier kept one at or after from, with
// nearly the same words. The first copy is kept; the rest are returned. A
// decision that nearly matches one already in its own session is a message
// pasted again there, and always counts. Decisions must be sorted by time.
func copiesOf(decs []repeatDec, from time.Time) map[string]bool {
	out := map[string]bool{}
	words := make([]map[string]bool, len(decs))
	for i, d := range decs {
		words[i] = wordSet(d.Text)
	}
	for i, d := range decs {
		if d.Time.Before(from) {
			continue
		}
		copyOf, again := false, false
		for j := i - 1; j >= 0 && d.Time.Sub(decs[j].Time) <= copySpan; j-- {
			e := decs[j]
			if !nearlySame(words[i], words[j]) {
				continue
			}
			if e.Session == d.Session {
				again = true
				break
			}
			if !out[e.ID] && !e.Time.Before(from) {
				copyOf = true
			}
		}
		if copyOf && !again {
			out[d.ID] = true
		}
	}
	return out
}

var copyWordRe = regexp.MustCompile(`[\p{L}\p{N}]{3,}`)

func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range copyWordRe.FindAllString(strings.ToLower(s), -1) {
		out[w] = true
	}
	return out
}

// nearlySame: at least 4 words each, close in length (the shorter has 60%
// of the longer one's words), and 80% of the shorter one's words are in the
// other.
func nearlySame(a, b map[string]bool) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	if len(a) < 4 || len(a)*5 < len(b)*3 {
		return false
	}
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return n*5 >= len(a)*4
}

// earlierOf offers, for each decision, the earlier decisions that share at
// least two keyword words with it, at least repeatGap older: most shared
// words first, then the newest. Copies to other tabs are offered too: a copy
// may hold words its first copy lacks.
func earlierOf(decs []repeatDec) map[int][]int {
	out := map[int][]int{}
	for i, d := range decs {
		type cand struct{ j, n int }
		var cs []cand
		for j, e := range decs {
			gap := d.Time.Sub(e.Time)
			if gap < repeatGap || gap > repeatSpan {
				continue
			}
			sh, usable := landing.Shared(d.Keywords, e.Summary+" | "+e.Text)
			if usable >= 2 && len(sh) >= 2 {
				cs = append(cs, cand{j, len(sh)})
			}
		}
		sort.Slice(cs, func(a, b int) bool {
			if cs[a].n != cs[b].n {
				return cs[a].n > cs[b].n
			}
			return decs[cs[a].j].Time.After(decs[cs[b].j].Time)
		})
		for k := 0; k < len(cs) && k < maxEarlier; k++ {
			out[i] = append(out[i], cs[k].j)
		}
	}
	return out
}

func restateKey(model string, d repeatDec, earlier []repeatDec) string {
	h := sha256.New()
	fmt.Fprintf(h, "restate\x00%s\x00%s\x00%s\x00%s\x00%s\x00", decide.RestateVersion, model, d.ID, d.Summary, d.Text)
	for _, e := range earlier {
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00", e.ID, e.Summary, e.Text)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// findRepeats asks the model, for each decision from `from` on with earlier
// candidates, whether it states one of them again. Decisions before `from`
// are only looked back at. Answers are cached in the store. decs must be
// sorted by time.
func findRepeats(ctx context.Context, decs []repeatDec, from time.Time, st *store.Store, r decide.Restater, model string) repeats {
	rs := repeats{Found: map[string]repeat{}, Failed: map[string]bool{}, Copies: copiesOf(decs, from)}
	cands := earlierOf(decs)
	type ask struct {
		i   int
		key string
		it  decide.RestateItem
	}
	var asks []ask
	for i, d := range decs {
		js := cands[i]
		if len(js) == 0 || d.Time.Before(from) || rs.Copies[d.ID] {
			continue
		}
		var earlier []repeatDec
		it := decide.RestateItem{ID: d.ID, Later: d.Summary + " | " + quote(d.Text, 600)}
		for k, j := range js {
			e := decs[j]
			earlier = append(earlier, e)
			it.Earlier = append(it.Earlier, fmt.Sprintf("%d: %s | %s | %s", k, e.Time.Format("2006-01-02"), e.Summary, quote(e.Text, 300)))
		}
		key := restateKey(model, d, earlier)
		if j, ok, err := st.Judgment(key); err != nil {
			rs.Errs = append(rs.Errs, "restate cache: "+err.Error())
		} else if ok {
			rs.Cached++
			if j.Record >= 0 && j.Record < len(js) {
				rs.Found[d.ID] = repeat{Later: d, Earlier: decs[js[j.Record]], Why: j.Why}
			}
			continue
		}
		asks = append(asks, ask{i, key, it})
	}
	var batches [][]ask
	for len(asks) > 0 {
		b := asks[:min(8, len(asks))]
		asks = asks[len(b):]
		batches = append(batches, b)
	}
	var mu sync.Mutex
	var stopped error
	run := func(b []ask) {
		items := make([]decide.RestateItem, len(b))
		for k, a := range b {
			items[k] = a.it
		}
		mu.Lock()
		s := stopped
		rs.Asked += len(b)
		mu.Unlock()
		var ans []decide.RestateAnswer
		err := s
		if s == nil {
			ans, err = r.Restate(ctx, items)
		}
		byID := map[string]decide.RestateAnswer{}
		for _, a := range ans {
			byID[a.ID] = a
		}
		mu.Lock()
		defer mu.Unlock()
		if errors.Is(err, decide.ErrAPIKey) && stopped == nil {
			stopped = err
		}
		for _, a := range b {
			d := decs[a.i]
			x, ok := byID[d.ID]
			if !ok {
				rs.Failed[d.ID] = true
				if err != nil && len(rs.Errs) < 20 {
					rs.Errs = append(rs.Errs, d.ID+": "+err.Error())
				}
				continue
			}
			if e := st.PutJudgment(store.Judgment{Key: a.key, Record: x.Restates, Replaced: -1, Why: x.Why}); e != nil {
				rs.Errs = append(rs.Errs, "restate cache: "+e.Error())
			}
			if x.Restates >= 0 {
				rs.Found[d.ID] = repeat{Later: d, Earlier: decs[cands[a.i][x.Restates]], Why: x.Why}
			}
		}
	}
	if len(batches) > 0 {
		run(batches[0]) // proves the login before the rest start
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, judgeWorkers)
	for _, b := range batches[min(1, len(batches)):] {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			run(b)
		}()
	}
	wg.Wait()
	return rs
}

// weekRow counts one week's lasting decisions and repeats; a week that
// holds the install time is split in two rows.
type weekRow struct {
	Start              time.Time
	After              bool // after landed was installed
	Decisions, Repeats int
	Failed             int
	Copies             int // left out: later copies of a message sent to several tabs
	Examples           []repeat
}

const examplesPerWeek = 3

func weekStart(t time.Time, loc *time.Location) time.Time {
	t = t.In(loc)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7)) // back to Monday
}

// weekly counts the decisions from `from` on; earlier ones were read only
// to look back at.
func weekly(decs []repeatDec, from time.Time, rs repeats, installed time.Time, loc *time.Location) []weekRow {
	type key struct {
		start time.Time
		after bool
	}
	rows := map[key]*weekRow{}
	for _, d := range decs {
		if d.Time.Before(from) {
			continue
		}
		k := key{weekStart(d.Time, loc), !d.Time.Before(installed)}
		r := rows[k]
		if r == nil {
			r = &weekRow{Start: k.start, After: k.after}
			rows[k] = r
		}
		if rs.Copies[d.ID] {
			r.Copies++
			continue
		}
		r.Decisions++
		if rs.Failed[d.ID] {
			r.Failed++
		}
		if x, ok := rs.Found[d.ID]; ok {
			r.Repeats++
			if len(r.Examples) < examplesPerWeek {
				r.Examples = append(r.Examples, x)
			}
		}
	}
	var out []weekRow
	for _, r := range rows {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return !out[i].After
	})
	return out
}

// writeRepeats prints the weekly table. oldest is the first lasting
// decision read: a week that starts less than repeatSpan after it has less
// history to look back at, so its count is low; such weeks are marked.
// The rate leaves out decisions whose answer failed.
func writeRepeats(w io.Writer, rows []weekRow, rs repeats, installed, oldest time.Time) {
	fmt.Fprintf(w, "repeats: owner turns that state an earlier decision of theirs again (lasting decisions only; landed installed %s)\n", stampOf(installed))
	full := oldest.Add(repeatSpan)
	fmt.Fprintf(w, "  earlier decisions are looked for up to %d days back; the oldest one read is from %s, so weeks marked * have less to look back at\n",
		int(repeatSpan.Hours()/24), stampOf(oldest))
	fmt.Fprintf(w, "  %-11s %-7s %9s %7s %6s %7s\n", "week of", "install", "decisions", "repeats", "rate", "failed")
	var tot [2][3]int // before/after: decisions, repeats, failed
	for _, r := range rows {
		side := "before"
		i := 0
		if r.After {
			side, i = "after", 1
		}
		tot[i][0] += r.Decisions
		tot[i][1] += r.Repeats
		tot[i][2] += r.Failed
		mark := " "
		if r.Start.Before(full) {
			mark = "*"
		}
		fmt.Fprintf(w, "  %-10s%s %-7s %9d %7d %5.1f%% %7d\n", r.Start.Format("2006-01-02"), mark, side, r.Decisions, r.Repeats, pct(r.Repeats, r.Decisions-r.Failed), r.Failed)
	}
	for i, side := range []string{"before", "after"} {
		fmt.Fprintf(w, "  %s install: %d of %d checked lasting decisions were repeats (%.1f%%); %d more could not be checked\n", side, tot[i][1], tot[i][0]-tot[i][2], pct(tot[i][1], tot[i][0]-tot[i][2]), tot[i][2])
	}
	copies := 0
	for _, r := range rows {
		copies += r.Copies
	}
	fmt.Fprintf(w, "  %d %s of a message sent to several tabs left out (same words within %v, other session; the first copy counts)\n",
		copies, map[bool]string{true: "copy", false: "copies"}[copies == 1], copySpan)
	fmt.Fprintf(w, "  model answers: %d cached, %d asked\n", rs.Cached, rs.Asked)
	for _, r := range rows {
		if len(r.Examples) == 0 {
			continue
		}
		side := "before"
		if r.After {
			side = "after"
		}
		fmt.Fprintf(w, "\n  examples, week of %s (%s install):\n", r.Start.Format("2006-01-02"), side)
		for _, x := range r.Examples {
			fmt.Fprintf(w, "  - %s session %s: %q\n", stampOf(x.Later.Time), x.Later.Session, quote(x.Later.Text, 120))
			fmt.Fprintf(w, "    restates %s session %s: %q\n", stampOf(x.Earlier.Time), x.Earlier.Session, quote(x.Earlier.Text, 120))
			fmt.Fprintf(w, "    why: %s\n", x.Why)
		}
	}
}
