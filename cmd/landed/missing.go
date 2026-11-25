package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/orzazade/landed/internal/decide"
	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/store"
	"github.com/orzazade/landed/internal/transcript"
)

type checkOpts struct {
	since, grace, window time.Duration
	project              string
	noModel              bool
	judgeModel           string
	db, bus              string
	roots, notes         rootList
	reposUnder           string
	session              string          // only this session's turns ("" for all)
	background           bool            // a warm-up whose rows nobody sees (proof runs as one too: it shows no row to act on)
	shownHere            bool            // check: the output is read inside the session it checks (/landed)
	only                 map[string]bool // only these turns (nil for all)
	pre                  *loaded         // transcripts already read since the same cut (nil: read them)
}

func (o *checkOpts) flags(fs *flag.FlagSet) (since, grace, window *string) {
	since = fs.String("since", "48h", "decisions made within this age")
	grace = fs.String("grace", "4h", "decisions younger than this are too new to call missing")
	window = fs.String("window", "7d", "how long after a decision a landing still counts")
	fs.StringVar(&o.project, "project", "", "only sessions whose folder contains this text")
	fs.BoolVar(&o.noModel, "no-model", false, "rules only (no claude -p)")
	fs.StringVar(&o.judgeModel, "judge-model", "haiku", "model that picks which candidate records a decision")
	fs.StringVar(&o.db, "db", store.DefaultPath(), "SQLite file")
	fs.StringVar(&o.bus, "bus", "", "bus folder (*.log); default ~/.crius/bus when it exists")
	fs.Var(&o.roots, "root", "transcript root (repeatable)")
	fs.StringVar(&o.reposUnder, "repos-under", defaultReposUnder, "with the judge, also search commits in every git repo under this folder (\"\" for none; the default is skipped when missing)")
	fs.Var(&o.notes, "notes", "folder of shared Markdown notes (repeatable; default ~/.crius/state and memory folders)")
	return
}

func (o *checkOpts) parse(since, grace, window string) error {
	var err error
	if o.since, err = parseAge(since); err != nil {
		return err
	}
	if grace != "0" { // 0: no grace, every decision is checked
		if o.grace, err = parseAge(grace); err != nil {
			return err
		}
	}
	o.window, err = parseAge(window)
	return err
}

// checked is the outcome of one run over the owner's history.
type checked struct {
	Checks      []landing.Check
	TooNew      int
	Unknown     []decide.Result // turns whose decision call failed
	JudgeErrors []string        // failed landing judgments (those rows are ?)
	Stats       decide.Stats
	Loaded      loaded
	Projects    map[string]string // session id -> project name
	// Rescued decisions: shown missing by a run, found landed by a later one.
	RescuedWeek, RescuedAll int
	ProofErr                error // writing the live log failed (said in one line, never fails the run)
}

func runChecks(ctx context.Context, o checkOpts, now time.Time) (checked, error) {
	var out checked
	cut := now.Add(-o.since)
	if o.session != "" {
		first, ok, err := sessionCut(o.roots, o.session, cut)
		if err != nil || !ok {
			return out, err // no owner turn in the session: nothing to check
		}
		cut = first
	}
	var l loaded
	if o.pre != nil {
		l = *o.pre
	} else {
		l = loadCtx(ctx, o.roots, cut)
	}
	out.Loaded = l
	out.Projects = map[string]string{}
	var turns []transcript.Turn
	sessOf := map[string]*transcript.Session{}
	for _, s := range l.Sessions {
		if s.Headless || (o.project != "" && !strings.Contains(s.Cwd, o.project)) {
			continue
		}
		for _, t := range s.Turns {
			if t.Time.Before(cut) || (o.session != "" && t.SessionID != o.session) || (o.only != nil && !o.only[t.UUID]) {
				continue
			}
			turns = append(turns, t)
			sessOf[t.UUID] = s
		}
	}
	st, err := store.Open(o.db)
	if err != nil {
		return out, err
	}
	defer st.Close()
	var m decide.Model = decide.ClaudeCLI{}
	if o.noModel {
		m = nil
	}
	notes := []string(o.notes)
	if len(notes) == 0 {
		notes = defaultNoteDirs()
	}
	src := &landing.Sources{BusDir: busDir(o.bus, home0()), Window: o.window, Now: now, Since: cut, NoteDirs: notes, Actions: allActions(l)}
	// The sources do not depend on the decisions: they are read while the
	// model sorts the turns.
	read := make(chan struct{})
	go func() {
		defer close(read)
		widen(src, o.reposUnder, !o.noModel)
	}()
	res, stats := decide.Find(ctx, turns, st, m, 25)
	out.Stats = stats
	<-read
	var jg *judging
	if !o.noModel {
		jg = newJudging(st, decide.ClaudeCLI{Model: o.judgeModel}, o.judgeModel, stats.Errors)
	}
	out.Checks, out.TooNew, out.Unknown = checkResults(ctx, res, sessOf, src, now, o.grace, jg)
	if jg != nil {
		out.JudgeErrors = jg.errs
	}
	for _, s := range sessOf {
		out.Projects[s.ID] = s.Project
	}
	out.Stats.Errors = append(out.Stats.Errors, remember(st, o, out.Checks, now)...)
	out.ProofErr = recordLandings(proofPath(o), out.Checks, src.Actions, now)
	var err2 error
	if out.RescuedWeek, out.RescuedAll, err2 = st.Rescued(now.Add(-7 * 24 * time.Hour)); err2 != nil {
		out.Stats.Errors = append(out.Stats.Errors, err2)
	}
	sort.Slice(out.Checks, func(i, j int) bool { return out.Checks[i].Decision.Time.Before(out.Checks[j].Decision.Time) })
	return out, nil
}

// remember notes what a run showed, so a row shown missing that lands later
// counts as rescued.
func remember(st *store.Store, o checkOpts, checks []landing.Check, now time.Time) []error {
	if o.background {
		return nil
	}
	for _, ch := range checks {
		var err error
		switch ch.State {
		case landing.Missing:
			err = st.SeenMissing(ch.Decision.ID, now)
		case landing.Landed:
			err = st.SeenLanded(ch.Decision.ID, now)
		}
		if err != nil {
			return []error{err}
		}
	}
	return nil
}

// defaultReposUnder is a guess at where the user keeps repos; widen skips
// it when it is not there.
const defaultReposUnder = "~/Projects"

// widen adds the wide sources (every repo under root, changed notes, every
// bus log) when a judge reads the candidates. Keywords alone stay narrow.
func widen(src *landing.Sources, root string, judged bool) {
	if !judged {
		return
	}
	if root == defaultReposUnder {
		root = filepath.Join(home0(), "Projects")
		if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
			root = ""
		}
	}
	if root != "" {
		src.Repos, src.ReposErr = landing.FindRepos(root, 5)
		if src.ReposErr != nil {
			fmt.Fprintln(os.Stderr, "repos:", src.ReposErr)
		}
	}
	src.ChangedNotes, src.AllBus = true, true
	src.Prefetch()
}

// allActions: every assistant action of every session read (headless ones
// too: workers write records), sorted by time.
func allActions(l loaded) []transcript.Action {
	var out []transcript.Action
	for _, s := range l.Sessions {
		out = append(out, s.Actions...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

func quote(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func age(now, t time.Time) string {
	d := now.Sub(t)
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

func short(id string) string { return id[:min(8, len(id))] }

func cmdMissing(args []string) error {
	fs := flag.NewFlagSet("missing", flag.ExitOnError)
	var o checkOpts
	since, grace, window := o.flags(fs)
	all := fs.Bool("all", false, "also list decisions that landed, with their evidence")
	ids := fs.Bool("ids", false, "print each row's turn id and kind (for hand labels)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := o.parse(*since, *grace, *window); err != nil {
		return err
	}
	now := time.Now()
	c, err := runChecks(context.Background(), o, now)
	if err != nil {
		return err
	}
	counts := map[landing.State]int{}
	var warned []landing.Check
	var shown []string
	for _, ch := range c.Checks {
		counts[ch.State]++
		if ch.State == landing.Landed && !*all {
			continue
		}
		d := ch.Decision
		line := fmt.Sprintf("%-10s %4s  %-14s %s  %q", ch.State, age(now, d.Time), quote(c.Projects[d.Session], 14), short(d.Session), quote(d.Text, 100))
		fmt.Println(line)
		if ch.State == landing.Missing || ch.State == landing.Unknown {
			warned, shown = append(warned, ch), append(shown, line)
		}
		if d.Summary != "" {
			fmt.Printf("        decision: %s\n", d.Summary)
		}
		if *ids {
			kind := d.Kind
			if kind == "" {
				kind = "rule" // an empty kind is read as lasting
			}
			fmt.Printf("        id: %s kind %s\n", d.ID, kind)
		}
		if ch.Evidence != nil {
			label := "landed"
			if ch.State == landing.Superseded {
				label = "replaced by"
			}
			fmt.Printf("        %s: %s %s (matched %s)\n", label, ch.Evidence.Kind, quote(ch.Evidence.Ref, 110), strings.Join(ch.Evidence.Matched, ", "))
			if ch.Why != "" {
				fmt.Printf("        %s\n", quote(ch.Why, 200))
			}
		} else {
			fmt.Printf("        why: %s\n", quote(ch.Why, 200))
		}
	}
	for _, r := range c.Unknown {
		fmt.Printf("%-10s %4s  %-14s %s  %q\n        why: decision call failed: %s\n", "?", age(now, r.Turn.Time), "", short(r.Turn.SessionID), quote(r.Turn.Text, 100), r.Err)
	}
	fmt.Printf("\n%d decisions in the last %s: %d landed, %d missing, %d superseded (a later decision or line replaced it), %d ? (could not check); %d too new (< %s); %d turns not classified\n",
		len(c.Checks)+c.TooNew, *since, counts[landing.Landed], counts[landing.Missing], counts[landing.Superseded], counts[landing.Unknown], c.TooNew, *grace, len(c.Unknown))
	for _, e := range c.Stats.Errors {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
	for _, e := range c.JudgeErrors {
		fmt.Fprintln(os.Stderr, "judge error:", e)
	}
	sayNotRecorded(os.Stderr, errors.Join(logWarns(proofPath(o), "missing", "", warned, shown, now), c.ProofErr))
	return runErrors(c, c.Loaded.report())
}

// checkResults looks for a landing of every known decision older than grace.
// With a judge, the model picks among the keyword candidates; without one,
// keywords alone decide.
func checkResults(ctx context.Context, res []decide.Result, sessOf map[string]*transcript.Session, src *landing.Sources,
	now time.Time, grace time.Duration, jg *judging) (checks []landing.Check, tooNew int, unknown []decide.Result) {
	// Every lasting decision, too new ones included: a later one may replace
	// an earlier one.
	src.Decisions = nil
	for _, r := range res {
		if r.Known && decide.Lasting(r.Verdict) {
			s := sessOf[r.Turn.UUID]
			src.Decisions = append(src.Decisions, landing.Decision{ID: r.Turn.UUID, Session: s.ID, Time: r.Turn.Time,
				Text: r.Turn.Text, Summary: r.Verdict.Summary})
		}
	}
	sort.Slice(src.Decisions, func(i, j int) bool { return src.Decisions[i].Time.Before(src.Decisions[j].Time) })
	var pend []pending
	for _, r := range res {
		if !r.Known {
			unknown = append(unknown, r)
			continue
		}
		if !decide.Lasting(r.Verdict) {
			continue // one-off actions are not counted at all
		}
		if now.Sub(r.Turn.Time) < grace {
			tooNew++
			continue
		}
		s := sessOf[r.Turn.UUID]
		var later []transcript.Action
		for _, a := range s.Actions {
			if a.Time.After(r.Turn.Time) {
				later = append(later, a)
			}
		}
		d := landing.Decision{
			ID: r.Turn.UUID, Session: s.ID, Cwd: s.Cwd, Time: r.Turn.Time, Text: r.Turn.Text,
			Summary: r.Verdict.Summary, Keywords: r.Verdict.Keywords, Later: later,
			Kind: r.Verdict.Kind,
		}
		if jg == nil {
			checks = append(checks, src.Check(d))
			continue
		}
		if c, ok := jg.cachedLanding(d, src.Window); ok {
			checks = append(checks, c)
			continue
		}
		c, cands := src.Gather(d)
		pend = append(pend, pending{c, cands})
	}
	if jg != nil {
		judged := jg.finish(ctx, pend)
		jg.keepLandings(judged, src.Window)
		checks = append(checks, judged...)
	}
	return checks, tooNew, unknown
}
