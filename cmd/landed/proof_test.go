package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/proof"
	"github.com/orzazade/landed/internal/store"
	"github.com/orzazade/landed/internal/transcript"
)

// A made-up history: four decisions landed warned, each a different chain.
//
//	d1: the hook warned session s1 (live), then s1 wrote AGENTS.md: caused.
//	d2: the hook warned s2 (rebuilt from its transcript), the landing came
//	    before the warning: anyway, marked rebuilt.
//	d3: a report counted it (no session), s3 then committed: anyway.
//	d4: warned live, still missing: listed as open, never a rescue.
func proofFixture() proofIn {
	t0 := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	m := func(n int) time.Time { return t0.Add(time.Duration(n) * time.Minute) }
	turn := func(id, sess, text string, at time.Time) transcript.Turn {
		return transcript.Turn{UUID: id, SessionID: sess, Text: text, Time: at}
	}
	s1 := &transcript.Session{ID: "s1", Project: "kettle", Turns: []transcript.Turn{turn("d1", "s1", "from now on settings live in TOML", m(0)), turn("d4", "s1", "never ship without the lid sensor", m(1))}}
	s2 := &transcript.Session{ID: "s2", Project: "kettle", Turns: []transcript.Turn{turn("d2", "s2", "the spout stays ceramic", m(0))},
		Warnings: []transcript.Warning{{Time: m(60), Lines: []string{"the spout stays ceramic", "a line that matches no turn"}}}}
	s3 := &transcript.Session{ID: "s3", Project: "kettle", Turns: []transcript.Turn{turn("d3", "s3", "lids are blue", m(0))}}
	acts := []transcript.Action{
		{Session: "s2", Time: m(30), Tool: "Edit", Path: "/k/STATUS.md"},
		{Session: "s1", Time: m(40), Tool: "Edit", Path: "/k/AGENTS.md"},
		{Session: "s3", Time: m(90), Tool: "Bash", Command: "git commit -m 'Make lids blue'"},
	}
	ev := func(kind, ref string, at time.Time) *landing.Evidence {
		return &landing.Evidence{Kind: kind, Ref: ref, When: at}
	}
	dec := func(id, sess string) landing.Decision { return landing.Decision{ID: id, Session: sess} }
	return proofIn{
		Live: []proof.Event{
			{At: m(20), Event: proof.Warn, Where: "hook", Session: "s1", Decision: "d1", DecisionSession: "s1", Shown: `- "from now on settings live in TOML"`},
			{At: m(20), Event: proof.Warn, Where: "hook", Session: "s1", Decision: "d4", DecisionSession: "s1", Shown: `- "never ship without the lid sensor"`},
			{At: m(70), Event: proof.Warn, Where: "report", Decision: "d3", DecisionSession: "s3", Shown: "landed · last 48h: …"},
		},
		Sessions: []*transcript.Session{s1, s2, s3},
		Checks: map[string]landing.Check{
			"d1": {Decision: dec("d1", "s1"), State: landing.Landed, Evidence: ev("edit", "/k/AGENTS.md", m(40))},
			"d2": {Decision: dec("d2", "s2"), State: landing.Landed, Evidence: ev("edit", "/k/STATUS.md", m(30))},
			"d3": {Decision: dec("d3", "s3"), State: landing.Landed, Evidence: ev("commit", "kettle 1a2b3c4 Make lids blue", m(91))},
			"d4": {Decision: dec("d4", "s1"), State: landing.Missing},
		},
		Actions: acts,
	}
}

func TestProofChains(t *testing.T) {
	p := buildProof(proofFixture())
	if p.Unmatched != 1 {
		t.Errorf("unmatched hook lines = %d, want 1", p.Unmatched)
	}
	got := map[string]proofRow{}
	for _, r := range p.Rows {
		got[r.ID] = r
	}
	for _, c := range []struct {
		id, state, verdict, who string
		rebuilt                 bool
	}{
		{"d1", "landed", proof.Caused, "s1", false},
		{"d2", "landed", proof.Anyway, "s2", true},
		{"d3", "landed", proof.Anyway, "s3", false},
		{"d4", "missing", "", "", false},
	} {
		r, ok := got[c.id]
		if !ok {
			t.Errorf("%s: no row", c.id)
			continue
		}
		who := ""
		if r.Landing != nil {
			who = r.Landing.Session
		}
		if r.State != c.state || r.Verdict != c.verdict || who != c.who || r.Rebuilt != c.rebuilt {
			t.Errorf("%s: state %q verdict %q by %q rebuilt %v; want %q %q %q %v", c.id, r.State, r.Verdict, who, r.Rebuilt, c.state, c.verdict, c.who, c.rebuilt)
		}
	}
	var out bytes.Buffer
	writeProof(&out, p)
	s := out.String()
	for _, want := range []string{
		"rescues (warned, then landed): 3: 1 caused by landed, 2 would have landed anyway",
		"caused by landed", "session s1", "/k/AGENTS.md", "1a2b3c4", "rebuilt",
		"warned, no landing yet: 1", "never ship without the lid sensor",
		"hook lines that matched no owner turn: 1",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q:\n%s", want, s)
		}
	}
}

// A live warning already in the log is not counted again from the transcript.
func TestRebuiltSkipsLiveWarnings(t *testing.T) {
	in := proofFixture()
	in.Live = append(in.Live, proof.Event{At: in.Sessions[1].Warnings[0].Time.Add(3 * time.Second), Event: proof.Warn, Where: "hook", Session: "s2", Decision: "d2"})
	for _, r := range buildProof(in).Rows {
		if r.ID == "d2" && (r.Rebuilt || len(r.Warns) != 1) {
			t.Errorf("d2: rebuilt %v, warnings %d; want live only", r.Rebuilt, len(r.Warns))
		}
	}
}

// Rows the store saw missing before the live log are rebuilt flags shown
// outside a session.
func TestProofUsesStoreSeenRows(t *testing.T) {
	in := proofFixture()
	in.Live = nil
	in.Seen = []store.Seen{{ID: "d3", MissingAt: in.Sessions[2].Turns[0].Time.Add(time.Hour)}}
	for _, r := range buildProof(in).Rows {
		if r.ID == "d3" && (!r.Rebuilt || r.Verdict != proof.Anyway || len(r.Warns) != 1 || r.Warns[0].Session != "") {
			t.Errorf("d3 = %+v", r)
		}
	}
}
