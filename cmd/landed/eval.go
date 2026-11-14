package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/orzazade/landed/internal/decide"
	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/store"
	"github.com/orzazade/landed/internal/transcript"
)

// label is one hand label. The file lives outside the repo (private text).
type label struct {
	ID    string `json:"id"`
	Label string `json:"label"` // decision | not | unsure
	Kind  string `json:"kind"`  // for decisions, when labelled: action | rule
}

func home0() string {
	h, _ := os.UserHomeDir()
	return h
}

func defaultLabels() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "landed", "labels.jsonl")
}

func readLabels(path string) ([]label, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []label
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for n := 1; sc.Scan(); n++ {
		var l label
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		switch l.Label {
		case "decision", "not", "unsure":
		default:
			return nil, fmt.Errorf("%s line %d: label %q is not decision/not/unsure", path, n, l.Label)
		}
		if l.Kind != "" && (l.Label != "decision" || (l.Kind != "action" && l.Kind != "rule")) {
			return nil, fmt.Errorf("%s line %d: kind %q needs label decision and action/rule", path, n, l.Kind)
		}
		out = append(out, l)
	}
	return out, sc.Err()
}

// score compares the finder with the hand labels.
type score struct {
	Decisions, Nots, Unsure  int // labels
	NotExtracted             int // labelled turns the reader no longer keeps as owner turns
	Found, FalseHit, Unknown int // found = labelled decision found; falseHit = labelled not, called decision
	KindLabelled, KindRight  int // found decisions with a hand kind, and those the classifier got right
	// Lasting decisions, the ones landed counts: truth is a hand "decision"
	// with kind rule. False = called lasting on a hand "not" or one-off.
	LastingWant, LastingFound, LastingFalse int
	MissedIDs, FalseIDs                     []string
}

func (s score) recall() float64 { return pct(s.Found, s.Decisions) }
func (s score) precision() float64 {
	return pct(s.Found, s.Found+s.FalseHit)
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}

func scoreFinder(labels []label, results map[string]decide.Result) score {
	var s score
	for _, l := range labels {
		switch l.Label {
		case "unsure":
			s.Unsure++
			continue
		case "decision":
			s.Decisions++
		case "not":
			s.Nots++
		}
		r, ok := results[l.ID]
		if !ok {
			s.NotExtracted++
			if l.Label == "decision" {
				s.MissedIDs = append(s.MissedIDs, l.ID+" (not extracted)")
			}
			continue
		}
		if !r.Known {
			s.Unknown++
			if l.Label == "decision" {
				s.MissedIDs = append(s.MissedIDs, l.ID+" (unknown: "+r.Err+")")
			}
			continue
		}
		lasting := decide.Lasting(r.Verdict)
		switch {
		case l.Label == "decision" && l.Kind == "rule":
			s.LastingWant++
			if lasting {
				s.LastingFound++
			}
		case lasting && (l.Label == "not" || l.Kind == "action"):
			s.LastingFalse++
		}
		switch {
		case l.Label == "decision" && r.Verdict.Decision:
			s.Found++
			if l.Kind != "" {
				s.KindLabelled++
				if l.Kind == r.Verdict.Kind {
					s.KindRight++
				}
			}
		case l.Label == "decision":
			s.MissedIDs = append(s.MissedIDs, l.ID+" ("+r.Verdict.Source+")")
		case r.Verdict.Decision:
			s.FalseHit++
			s.FalseIDs = append(s.FalseIDs, l.ID+" ("+r.Verdict.Source+")")
		}
	}
	return s
}

func cmdEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	labelsPath := fs.String("labels", defaultLabels(), "hand labels (JSON lines, outside the repo)")
	dbPath := fs.String("db", store.DefaultPath(), "SQLite file")
	noModel := fs.Bool("no-model", false, "rules only; unclear turns count as unknown")
	reposUnder := fs.String("repos-under", defaultReposUnder, "search commits in every git repo under this folder (\"\" for none; the default is skipped when missing)")
	judgeModel := fs.String("judge-model", "haiku", "model that picks which candidate records a decision")
	since := fs.String("since", "30d", "how far back to read transcripts for the labelled turns")
	verbose := fs.Bool("v", false, "list the ids of misses and false hits")
	landLabels := fs.String("land-labels", defaultLandLabels(), "hand landing checks: file or glob (JSON lines, outside the repo)")
	todo := fs.String("todo", "", "write the decisions to hand-check for landing to this file")
	bus := fs.String("bus", "", "bus folder (*.log); default ~/.crius/bus when it exists")
	var roots rootList
	fs.Var(&roots, "root", "transcript root (repeatable)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	labels, err := readLabels(*labelsPath)
	if err != nil {
		return err
	}
	if len(labels) == 0 {
		return fmt.Errorf("no labels in %s", *labelsPath)
	}
	want := map[string]bool{}
	for _, l := range labels {
		want[l.ID] = true
	}
	age, err := parseAge(*since)
	if err != nil {
		return err
	}
	l := load(roots, time.Now().Add(-age))
	var turns []transcript.Turn
	sessOf := map[string]*transcript.Session{}
	for _, s := range l.Sessions {
		if s.Headless {
			continue
		}
		for _, t := range s.Turns {
			if want[t.UUID] {
				turns = append(turns, t)
				sessOf[t.UUID] = s
			}
		}
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	var m decide.Model = decide.ClaudeCLI{}
	if *noModel {
		m = nil
	}
	res, stats := decide.Find(context.Background(), turns, st, m, 25)
	byID := map[string]decide.Result{}
	for _, r := range res {
		byID[r.Turn.UUID] = r
	}
	s := scoreFinder(labels, byID)
	mode := "rules + haiku"
	if *noModel {
		mode = "rules only"
	}
	fmt.Printf("landed eval (%s, classifier %s) on %d hand labels: %d decision, %d not, %d unsure\n",
		mode, decide.PromptVersion, len(labels), s.Decisions, s.Nots, s.Unsure)
	fmt.Printf("  recall (decisions found):        %5.1f%%  (%d of %d; target >= 80%%)\n", s.recall(), s.Found, s.Decisions)
	fmt.Printf("  precision (calls that are right): %5.1f%%  (%d of %d)\n", s.precision(), s.Found, s.Found+s.FalseHit)
	fmt.Printf("  kind (one-off action or lasting rule) right: %d of %d hand-labelled\n", s.KindRight, s.KindLabelled)
	fmt.Printf("  lasting decisions (what landed counts): recall %5.1f%% (%d of %d hand rules), precision %5.1f%% (%d of %d)\n",
		pct(s.LastingFound, s.LastingWant), s.LastingFound, s.LastingWant, pct(s.LastingFound, s.LastingFound+s.LastingFalse), s.LastingFound, s.LastingFound+s.LastingFalse)
	fmt.Printf("  unknown (could not check):       %d; labelled turns no longer extracted: %d\n", s.Unknown, s.NotExtracted)
	fmt.Printf("  calls: cached %d, rules %d, haiku %d, unknown %d\n", stats.Cached, stats.ByRule, stats.ByModel, stats.Unknown)
	now := time.Now()
	src := &landing.Sources{BusDir: busDir(*bus, home0()), Window: 7 * 24 * time.Hour, Now: now, Since: now.Add(-age), NoteDirs: defaultNoteDirs(), Actions: allActions(l)}
	widen(src, *reposUnder, !*noModel)
	var jg *judging
	if !*noModel {
		jg = newJudging(st, decide.ClaudeCLI{Model: *judgeModel}, *judgeModel, stats.Errors)
	}
	checks, _, _ := checkResults(context.Background(), res, sessOf, src, now, 0, jg)
	if jg != nil {
		for _, e := range jg.errs {
			fmt.Fprintln(os.Stderr, "judge error:", e)
		}
		fmt.Printf("  landing judge: %s on %s, %d landed outcomes reused, %d cached, %d asked, %d failed\n", decide.JudgeVersion, *judgeModel, jg.reused, jg.cached, jg.asked, len(jg.errs))
	}
	labelBy := map[string]label{}
	for _, lb := range labels {
		labelBy[lb.ID] = lb
	}
	truth, err := readLandTruth(*landLabels)
	if err != nil {
		return err
	}
	scoreLanding(checks, labelBy, truth).print(*verbose)
	if *todo != "" {
		n, err := writeLandTodo(*todo, checks, labelBy, truth)
		if err != nil {
			return err
		}
		fmt.Printf("  wrote %d decisions to hand-check to %s\n", n, *todo)
	}
	if *verbose {
		sort.Strings(s.MissedIDs)
		sort.Strings(s.FalseIDs)
		for _, id := range s.MissedIDs {
			fmt.Println("  missed:", id)
		}
		for _, id := range s.FalseIDs {
			fmt.Println("  false hit:", id)
		}
	}
	for _, e := range stats.Errors {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
	if err := l.report(); err != nil {
		return err
	}
	if len(stats.Errors) > 0 {
		return fmt.Errorf("%d classifier errors", len(stats.Errors))
	}
	if jg != nil && len(jg.errs) > 0 {
		return fmt.Errorf("%d judge errors", len(jg.errs))
	}
	return nil
}
