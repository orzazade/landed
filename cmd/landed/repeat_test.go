package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orzazade/landed/internal/decide"
	"github.com/orzazade/landed/internal/store"
)

type fakeRestater struct {
	asked []string
	pick  map[string]int // later id -> earlier index
}

func (f *fakeRestater) Restate(_ context.Context, items []decide.RestateItem) ([]decide.RestateAnswer, error) {
	var out []decide.RestateAnswer
	for _, it := range items {
		f.asked = append(f.asked, it.ID)
		r, ok := f.pick[it.ID]
		if !ok {
			r = -1
		}
		out = append(out, decide.RestateAnswer{ID: it.ID, Restates: r, Why: "made-up reason"})
	}
	return out, nil
}

// Made-up decisions over two weeks (Mondays 11-02 and 11-09, landed
// installed 11-11 12:00):
//
//	a 11-03 rule; b two days later says it again (a repeat);
//	(b is offered d, then a: same words, newest first; it restates a)
//	c 10 min after b: b is too close to count, a and d are offered, judge says no;
//	d 30 min after a: nothing earlier is an hour old, never asked;
//	e 11-10 before install; f 11-12 after install says a again.
func repeatFixture() []repeatDec {
	t0 := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	return []repeatDec{
		{ID: "a", Session: "s1", Time: t0, Text: "from now on settings live in TOML", Summary: "settings in TOML", Keywords: []string{"settings", "toml"}},
		{ID: "d", Session: "s1", Time: t0.Add(30 * time.Minute), Text: "toml settings, yes", Summary: "settings in TOML", Keywords: []string{"settings", "toml"}},
		{ID: "b", Session: "s2", Time: t0.Add(48 * time.Hour), Text: "again: settings stay in TOML!", Summary: "settings in TOML", Keywords: []string{"settings", "toml"}},
		{ID: "c", Session: "s2", Time: t0.Add(48*time.Hour + 10*time.Minute), Text: "TOML settings, JSON for exports", Summary: "exports in JSON", Keywords: []string{"settings", "toml", "json"}},
		{ID: "e", Session: "s3", Time: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC), Text: "never ship without the lid sensor", Summary: "lid sensor required", Keywords: []string{"lid", "sensor"}},
		{ID: "f", Session: "s4", Time: time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC), Text: "I said it before: settings are TOML", Summary: "settings in TOML", Keywords: []string{"settings", "toml"}},
	}
}

func TestRepeats(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	decs := repeatFixture()
	fr := &fakeRestater{pick: map[string]int{"b": 1, "f": 0}}
	all := time.Time{}
	rs := findRepeats(context.Background(), decs, all, st, fr, "haiku")
	if strings.Join(fr.asked, ",") != "b,c,f" {
		t.Errorf("asked %v, want b,c,f (d has nothing an hour older; a and e nothing at all)", fr.asked)
	}
	if len(rs.Found) != 2 || rs.Found["b"].Earlier.ID != "a" || rs.Found["f"].Earlier.ID == "" {
		t.Errorf("found %+v, want b and f restating", rs.Found)
	}
	installed := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	rows := weekly(decs, all, rs, installed, time.UTC)
	want := []struct {
		start      string
		after      bool
		decs, reps int
	}{
		{"2026-11-02", false, 4, 1},
		{"2026-11-09", false, 1, 0},
		{"2026-11-09", true, 1, 1},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows %+v", rows)
	}
	for i, w := range want {
		r := rows[i]
		if r.Start.Format("2006-01-02") != w.start || r.After != w.after || r.Decisions != w.decs || r.Repeats != w.reps || len(r.Examples) != w.reps {
			t.Errorf("row %d = %s after=%v %d/%d (%d examples), want %+v", i, r.Start.Format("2006-01-02"), r.After, r.Repeats, r.Decisions, len(r.Examples), w)
		}
	}
	var out bytes.Buffer
	writeRepeats(&out, rows, rs, installed, decs[0].Time)
	s := out.String()
	// 11-02 starts less than 30 days after the oldest decision: marked.
	for _, x := range []string{"2026-11-02*", "oldest one read is from 2026-11-03", "before install: 1 of 5", "after install: 1 of 1", "again: settings stay in TOML!", "from now on settings live in TOML", "session s2", "session s1"} {
		if !strings.Contains(s, x) {
			t.Errorf("output lacks %q:\n%s", x, s)
		}
	}
	// A second run reads every answer from the cache.
	fr2 := &fakeRestater{}
	if rs2 := findRepeats(context.Background(), decs, all, st, fr2, "haiku"); len(fr2.asked) != 0 || len(rs2.Found) != 2 {
		t.Errorf("second run asked %v, found %d", fr2.asked, len(rs2.Found))
	}
}

// Decisions before the window are read only to look back at: they are
// never asked about and never counted, so the first week of the window
// still has earlier decisions to compare with.
func TestRepeatsLookBackBeforeWindow(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	decs := repeatFixture()
	from := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC) // a and d are before it
	fr := &fakeRestater{pick: map[string]int{"b": 1, "f": 0}}
	rs := findRepeats(context.Background(), decs, from, st, fr, "haiku")
	if strings.Join(fr.asked, ",") != "b,c,f" || rs.Found["b"].Earlier.ID != "a" {
		t.Errorf("asked %v, found %+v; want b restating a from before the window", fr.asked, rs.Found)
	}
	rows := weekly(decs, from, rs, time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC), time.UTC)
	if len(rows) == 0 || rows[0].Decisions != 2 || rows[0].Repeats != 1 {
		t.Errorf("first week %+v, want 2 decisions (b, c), 1 repeat", rows)
	}
}

// The rate leaves out decisions whose answer failed.
func TestRepeatRateLeavesOutFailed(t *testing.T) {
	rows := []weekRow{{Start: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC), Decisions: 4, Repeats: 1, Failed: 2}}
	var out bytes.Buffer
	writeRepeats(&out, rows, repeats{}, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if s := out.String(); !strings.Contains(s, "50.0%") || !strings.Contains(s, "before install: 1 of 2 checked") {
		t.Errorf("want 1 of 2 checked (50%%):\n%s", s)
	}
}
