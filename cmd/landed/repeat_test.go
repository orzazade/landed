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

// One message sent to two tabs is one message. Shaped on a real case (made-up
// words): the owner sent an order to two tabs 3 seconds apart, the second copy
// with a note added and one agent name changed, and both copies restate an
// older rule. That is one repeat out of one decision, not two of two.
func TestBroadcastCountsOnce(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	at := time.Date(2026, 11, 14, 17, 31, 20, 0, time.UTC)
	decs := []repeatDec{
		{ID: "old", Session: "s0", Time: time.Date(2026, 10, 29, 1, 53, 0, 0, time.UTC),
			Text: "Kettle order: every boil test runs through the shared kettle slot, one at a time", Summary: "one boil test at a time", Keywords: []string{"boil", "kettle", "slot"}},
		{ID: "tab1", Session: "s1", Time: at,
			Text:    "Kettle limit: 15-min heat 30, 5-min 50 (steam vent ~5 holes; boil tests run on top). Until I lift it: one boil test at a time across kettles, 2 burners. Agree the order with spout and lid. I lift it when the 15-min heat is under 28.",
			Summary: "one boil test at a time", Keywords: []string{"boil", "kettle", "slot"}},
		{ID: "tab2", Session: "s2", Time: at.Add(3 * time.Second),
			Text:    "Kettle limit: 15-min heat 30, 5-min 50. Until I lift it: one boil test at a time across kettles, 2 burners. Agree the order with handle and lid. I lift it when the 15-min heat is under 28.",
			Summary: "one boil test at a time", Keywords: []string{"boil", "kettle", "slot"}},
	}
	fr := &fakeRestater{pick: map[string]int{"tab1": 0, "tab2": 0}}
	rs := findRepeats(context.Background(), decs, time.Time{}, st, fr, "haiku")
	installed := time.Date(2026, 11, 14, 15, 8, 0, 0, time.UTC)
	var after weekRow
	for _, r := range weekly(decs, time.Time{}, rs, installed, time.UTC) {
		if r.After {
			after = r
		}
	}
	if after.Decisions != 1 || after.Repeats != 1 {
		t.Errorf("after install: %d repeats of %d decisions, want 1 of 1 (two tabs, one message)", after.Repeats, after.Decisions)
	}
	var out bytes.Buffer
	writeRepeats(&out, weekly(decs, time.Time{}, rs, installed, time.UTC), rs, installed, decs[0].Time)
	if !strings.Contains(out.String(), "1 copy of a message sent to several tabs left out") {
		t.Errorf("output does not say a copy was left out:\n%s", out.String())
	}
}

// Only a copy to another tab within copySpan, with nearly the same words, is
// left out. A message pasted again into the same tab stays: the owner said it
// twice there.
func TestCopiesOf(t *testing.T) {
	at := time.Date(2026, 11, 14, 17, 31, 0, 0, time.UTC)
	msg := "Kettle limit: one boil test at a time across kettles, two burners, agree the order with lid"
	cases := []struct {
		name string
		b    repeatDec
		copy bool
	}{
		{"other tab, same minute", repeatDec{ID: "b", Session: "s2", Time: at.Add(3 * time.Second), Text: msg}, true},
		{"same tab, pasted again", repeatDec{ID: "b", Session: "s1", Time: at.Add(3 * time.Second), Text: msg}, false},
		{"other tab, 11 minutes later", repeatDec{ID: "b", Session: "s2", Time: at.Add(11 * time.Minute), Text: msg}, false},
		{"other tab, other words", repeatDec{ID: "b", Session: "s2", Time: at.Add(time.Minute), Text: "the spout stays ceramic, never steel, for every kettle model"}, false},
		{"too few words", repeatDec{ID: "b", Session: "s2", Time: at.Add(time.Minute), Text: "yes, go"}, false},
	}
	for _, c := range cases {
		a := repeatDec{ID: "a", Session: "s1", Time: at, Text: msg}
		if c.name == "too few words" {
			a.Text = "yes, go"
		}
		got := copiesOf([]repeatDec{a, c.b}, time.Time{})
		if got["b"] != c.copy || got["a"] {
			t.Errorf("%s: copies %v, want b copy=%v", c.name, got, c.copy)
		}
	}
}

// Review cases: a short message is not a copy of a long, different one;
// a re-paste into a tab that got a copy still counts; a copy whose first
// copy is before the window counts (the first one is never counted).
func TestCopiesOfEdges(t *testing.T) {
	at := time.Date(2026, 11, 14, 17, 31, 0, 0, time.UTC)
	msg := "Kettle limit: one boil test at a time across kettles, two burners, agree the order with lid"
	short := repeatDec{ID: "a", Session: "s1", Time: at, Text: "Run the boil tests before every kettle ships, always."}
	long := repeatDec{ID: "b", Session: "s2", Time: at.Add(5 * time.Minute), Text: "Ship through the spout line only, never by hand; run the boil tests before every kettle ships, always; pin the lid sizes and log each burner."}
	if got := copiesOf([]repeatDec{short, long}, time.Time{}); got["b"] {
		t.Error("a long, different message was taken for a copy of a short one")
	}
	a := repeatDec{ID: "a", Session: "s1", Time: at, Text: msg}
	b := repeatDec{ID: "b", Session: "s2", Time: at.Add(3 * time.Second), Text: msg}
	d := repeatDec{ID: "d", Session: "s2", Time: at.Add(5 * time.Minute), Text: msg}
	if got := copiesOf([]repeatDec{a, b, d}, time.Time{}); !got["b"] || got["d"] {
		t.Errorf("copies %v; want b a copy, d (pasted again into s2) counted", got)
	}
	if got := copiesOf([]repeatDec{a, b}, at.Add(time.Second)); got["b"] {
		t.Error("b's first copy is before the window: b must count")
	}
}

// A copy is still an earlier decision a later one can restate: when the
// later copy holds the extra words, the restatement is found through it.
func TestCopyIsStillAnEarlierCandidate(t *testing.T) {
	at := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	decs := []repeatDec{
		{ID: "a", Session: "s1", Time: at, Text: "Kettle limit: one boil test at a time across kettles, two burners, agree the order with lid", Keywords: []string{"boil", "kettle"}},
		{ID: "b", Session: "s2", Time: at.Add(3 * time.Second), Text: "Kettle limit: one boil test at a time across kettles, two burners, agree the order with lid (descale weekly)", Keywords: []string{"boil", "kettle"}},
	}
	later := repeatDec{ID: "c", Session: "s3", Time: at.Add(48 * time.Hour), Text: "again: descale weekly", Keywords: []string{"descale", "weekly"}}
	got := earlierOf(append(decs, later))
	if len(got[2]) != 1 || got[2][0] != 1 {
		t.Errorf("earlier candidates of c: %v, want [1] (the copy with the extra words)", got[2])
	}
}
