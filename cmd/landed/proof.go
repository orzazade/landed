package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/orzazade/landed/internal/decide"
	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/proof"
	"github.com/orzazade/landed/internal/store"
	"github.com/orzazade/landed/internal/transcript"
)

// proofIn is what `landed proof` reads: the live log, the transcripts (for
// warnings shown before the log existed and the decisions' words), the
// store's earlier missing rows, and this run's check of each warned decision.
type proofIn struct {
	Live     []proof.Event
	Sessions []*transcript.Session
	Seen     []store.Seen
	Checks   map[string]landing.Check // by decision id; absent: not a lasting decision in the window read
	Actions  []transcript.Action      // every session's actions, sorted by time
}

// proofRow is one warned decision and what became of it.
type proofRow struct {
	ID, Text, Session, Project string
	Warns                      []proof.Event // every warning, live and rebuilt, oldest first
	Rebuilt                    bool          // no live warning: the chain comes from transcripts and the store
	State                      string        // landed, missing, ?, superseded, or why it was not checked
	Landing                    *proof.Event
	Verdict, Why               string
}

type proofOut struct {
	Rows      []proofRow
	Unmatched int       // hook lines that matched no owner turn of their session
	LiveFrom  time.Time // the first live event (zero: none)
}

// rebuilt is the same warning seen in the log: same decision and session,
// within a minute.
func alreadyLive(live []proof.Event, e proof.Event) bool {
	for _, l := range live {
		if l.Event == proof.Warn && l.Decision == e.Decision && l.Session == e.Session {
			if d := l.At.Sub(e.At); d > -time.Minute && d < time.Minute {
				return true
			}
		}
	}
	return false
}

// notOwnerTurn is the state of a warned decision whose turn is not among
// the owner turns read.
const notOwnerTurn = "not an owner turn now (a relayed agent message, or older than --since)"

func buildProof(in proofIn) proofOut {
	var out proofOut
	turns := map[string]transcript.Turn{}
	project := map[string]string{}
	warns := map[string][]proof.Event{}
	live := map[string]bool{}
	landedEv := map[string]proof.Event{}
	for _, s := range in.Sessions {
		project[s.ID] = s.Project
		for _, t := range s.Turns {
			turns[t.UUID] = t
		}
	}
	for _, e := range in.Live {
		if out.LiveFrom.IsZero() || e.At.Before(out.LiveFrom) {
			out.LiveFrom = e.At
		}
		switch e.Event {
		case proof.Warn:
			warns[e.Decision] = append(warns[e.Decision], e)
			live[e.Decision] = true
		case proof.Landed:
			landedEv[e.Decision] = e
		}
	}
	// Hook messages kept in the transcripts: each listed line is matched to
	// the session's owner turn the hook quoted.
	for _, s := range in.Sessions {
		for _, w := range s.Warnings {
			for _, line := range w.Lines {
				id := ""
				for _, t := range s.Turns {
					if quote(t.Text, 160) == line {
						id = t.UUID
						break
					}
				}
				if id == "" {
					out.Unmatched++
					continue
				}
				e := proof.Event{At: w.Time, Event: proof.Warn, Where: "hook", Session: s.ID, Decision: id, DecisionSession: s.ID, Shown: "- " + strconv.Quote(line)}
				if !alreadyLive(in.Live, e) {
					warns[id] = append(warns[id], e)
				}
			}
		}
	}
	// Rows shown missing by report or missing runs before the live log.
	for _, r := range in.Seen {
		if live[r.ID] {
			continue
		}
		warns[r.ID] = append(warns[r.ID], proof.Event{At: r.MissingAt, Event: proof.Warn, Where: "report or missing (store)", Decision: r.ID})
	}
	for id, ws := range warns {
		sort.Slice(ws, func(i, j int) bool { return ws[i].At.Before(ws[j].At) })
		t := turns[id]
		r := proofRow{ID: id, Text: t.Text, Session: t.SessionID, Project: project[t.SessionID], Warns: ws, Rebuilt: !live[id]}
		if r.Session == "" {
			r.Session = ws[0].DecisionSession
		}
		ch, ok := in.Checks[id]
		switch {
		case !ok && t.UUID == "":
			// Not among the owner turns read: dropped since as a relayed agent
			// message, or before --since. The loader does not say which.
			r.State = notOwnerTurn
		case !ok:
			r.State = "no longer a lasting decision"
		case ch.State == landing.Landed && ch.Evidence != nil:
			r.State = "landed"
			l, logged := landedEv[id]
			if !logged {
				l = proof.Event{Event: proof.Landed, Decision: id, Session: landing.Who(in.Actions, *ch.Evidence),
					Kind: ch.Evidence.Kind, Ref: ch.Evidence.Ref, When: ch.Evidence.When}
			}
			r.Landing = &l
			r.Verdict, r.Why = proof.Judge(ws, l.Session, l.When)
		default:
			r.State = ch.State.String()
		}
		out.Rows = append(out.Rows, r)
	}
	sort.Slice(out.Rows, func(i, j int) bool {
		if a, b := out.Rows[i].Warns[0].At, out.Rows[j].Warns[0].At; !a.Equal(b) {
			return a.Before(b)
		}
		return out.Rows[i].ID < out.Rows[j].ID
	})
	return out
}

func writeProof(w io.Writer, p proofOut) {
	var rescues, open []proofRow
	caused := 0
	for _, r := range p.Rows {
		if r.State == "landed" {
			rescues = append(rescues, r)
			if r.Verdict == proof.Caused {
				caused++
			}
		} else {
			open = append(open, r)
		}
	}
	live := "no live log yet"
	if !p.LiveFrom.IsZero() {
		live = "live log from " + p.LiveFrom.Local().Format("2006-01-02 15:04")
	}
	fmt.Fprintf(w, "landed proof (%s; rows marked rebuilt come from transcripts and the store)\n\n", live)
	fmt.Fprintf(w, "rescues (warned, then landed): %d: %d %s, %d %s\n", len(rescues), caused, proof.Caused, len(rescues)-caused, proof.Anyway)
	for _, r := range rescues {
		tag := ""
		if r.Rebuilt {
			tag = " [rebuilt]"
		}
		fmt.Fprintf(w, "\n%s%s  %q\n", r.Verdict, tag, quote(r.Text, 100))
		fmt.Fprintf(w, "  decision %s in session %s (%s)\n", r.ID, r.Session, r.Project)
		for _, e := range r.Warns {
			fmt.Fprintf(w, "  flagged  %s  %s\n", stampOf(e.At), where(e))
		}
		l := r.Landing
		by := "session not known"
		if l.Session != "" {
			by = "by session " + l.Session
		}
		fmt.Fprintf(w, "  landed   %s  %s %s, %s\n", stampOf(l.When), l.Kind, quote(l.Ref, 100), by)
		fmt.Fprintf(w, "  why      %s\n", r.Why)
	}
	fmt.Fprintf(w, "\nwarned, no landing yet: %d\n", len(open))
	for _, r := range open {
		tag := ""
		if r.Rebuilt {
			tag = " [rebuilt]"
		}
		fmt.Fprintf(w, "  %-10s %q%s\n", r.State, quote(r.Text, 90), tag)
		fmt.Fprintf(w, "             decision %s in session %s; first flagged %s %s\n", r.ID, r.Session, stampOf(r.Warns[0].At), where(r.Warns[0]))
	}
	fmt.Fprintf(w, "\nhook lines that matched no owner turn: %d\n", p.Unmatched)
}

func stampOf(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

func where(e proof.Event) string {
	if e.Session == "" {
		return e.Where + " (no session)"
	}
	return e.Where + " in session " + e.Session
}

// cmdProof shows what landed's warnings led to: each warned decision that
// landed, who landed it and whether the warning came first, and how often
// the owner had to say a decision again, per week, before and after install.
func cmdProof(args []string) error {
	fs := flag.NewFlagSet("proof", flag.ExitOnError)
	var o checkOpts
	since, _, window := o.flags(fs)
	fs.Lookup("since").DefValue, *since = "30d", "30d"
	installed := fs.String("installed", "", "when landed was installed (2006-01-02 or RFC 3339); default: its first warning")
	noRepeats := fs.Bool("no-repeats", false, "skip the repeat count (it classifies every owner turn read)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := o.parse(*since, "0", *window); err != nil {
		return err
	}
	ctx := context.Background()
	now := time.Now()
	cut := now.Add(-o.since)
	var errs []error
	live, bad, err := proof.Read(proofPath(o))
	if err != nil {
		return err
	}
	if bad > 0 {
		errs = append(errs, fmt.Errorf("%d lines of the proof log are not events (cut short?); they were skipped", bad))
	}
	st, err := store.Open(o.db)
	if err != nil {
		return err
	}
	seen, err := st.SeenRows()
	if err != nil {
		st.Close()
		return err
	}
	// The repeat count looks back repeatSpan before the window's first turn.
	l := loadCtx(ctx, o.roots, cut.Add(-repeatSpan))
	var sessions []*transcript.Session
	var turns []transcript.Turn
	for _, s := range l.Sessions {
		if s.Headless || (o.project != "" && !strings.Contains(s.Cwd, o.project)) {
			continue
		}
		sessions = append(sessions, s)
		turns = append(turns, s.Turns...)
	}
	in := proofIn{Live: live, Sessions: sessions, Seen: seen, Actions: allActions(l), Checks: map[string]landing.Check{}}
	// Which decisions were warned: a first pass with no checks.
	only := map[string]bool{}
	for _, r := range buildProof(in).Rows {
		only[r.ID] = true
	}
	var rs repeats
	var decs []repeatDec
	if !*noRepeats && !o.noModel {
		res, stats := decide.Find(ctx, turns, st, decide.ClaudeCLI{}, 25)
		errs = append(errs, stats.Errors...)
		for _, r := range res {
			if r.Known && decide.Lasting(r.Verdict) {
				decs = append(decs, repeatDec{ID: r.Turn.UUID, Session: r.Turn.SessionID, Text: r.Turn.Text, Time: r.Turn.Time,
					Summary: r.Verdict.Summary, Keywords: r.Verdict.Keywords})
			}
		}
		sort.Slice(decs, func(i, j int) bool { return decs[i].Time.Before(decs[j].Time) })
		rs = findRepeats(ctx, decs, cut, st, decide.ClaudeCLI{Model: o.judgeModel}, o.judgeModel)
		if stats.Unknown > 0 {
			errs = append(errs, fmt.Errorf("%d turns could not be classified (the repeat count leaves them out)", stats.Unknown))
		}
	}
	st.Close()
	o.only, o.pre, o.background = only, &l, true
	c, err := runChecks(ctx, o, now)
	if err != nil {
		return err
	}
	for _, ch := range c.Checks {
		in.Checks[ch.Decision.ID] = ch
	}
	p := buildProof(in)
	writeProof(os.Stdout, p)
	sayNotRecorded(os.Stdout, c.ProofErr)
	fmt.Println()
	switch {
	case *noRepeats:
		fmt.Println("repeats: not counted (--no-repeats)")
	case o.noModel:
		fmt.Println("repeats: not counted (they need the model; --no-model given)")
	default:
		inst := p.firstWarning()
		if *installed != "" {
			if inst, err = parseWhen(*installed); err != nil {
				return err
			}
		} else if inst.IsZero() {
			inst = now // no warning yet: every week is before
			fmt.Println("repeats: no warning found, so no install date; every week counts as before (--installed sets it)")
		}
		var oldest time.Time
		if len(decs) > 0 {
			oldest = decs[0].Time
		}
		writeRepeats(os.Stdout, weekly(decs, cut, rs, inst, time.Local), rs, inst, oldest)
		for _, e := range rs.Errs {
			errs = append(errs, errors.New("repeat: "+e))
		}
	}
	if err := runErrors(c, c.Loaded.report()); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// firstWarning is the earliest warning, live or rebuilt (zero: none).
func (p proofOut) firstWarning() time.Time {
	var first time.Time
	for _, r := range p.Rows {
		if t := r.Warns[0].At; first.IsZero() || t.Before(first) {
			first = t
		}
	}
	return first
}

func parseWhen(s string) (time.Time, error) {
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return t, fmt.Errorf("bad --installed %q (use 2006-01-02 or RFC 3339)", s)
	}
	return t, nil
}
