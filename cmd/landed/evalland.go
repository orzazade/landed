package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/orzazade/landed/internal/landing"
)

// landTruth is a hand check of whether one decision landed.
type landTruth struct {
	ID     string `json:"id"`
	Landed string `json:"landed"` // yes | no | unsure
	Where  string `json:"where"`
}

func defaultLandLabels() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "landed", "landing-labels*.jsonl")
}

// readLandTruth reads hand landing checks from every file the pattern
// matches. No file means none yet; an id in two files is an error.
func readLandTruth(pattern string) (map[string]landTruth, error) {
	files, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	out := map[string]landTruth{}
	for _, f := range files {
		one, err := readLandFile(f)
		if err != nil {
			return nil, err
		}
		for id, t := range one {
			if _, dup := out[id]; dup {
				return nil, fmt.Errorf("%s: id %s is also in another landing-labels file", f, id)
			}
			out[id] = t
		}
	}
	return out, nil
}

func readLandFile(path string) (map[string]landTruth, error) {
	out := map[string]landTruth{}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for n := 1; sc.Scan(); n++ {
		var t landTruth
		if err := json.Unmarshal(sc.Bytes(), &t); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		switch t.Landed {
		case "yes", "no", "unsure":
		default:
			return nil, fmt.Errorf("%s line %d: landed %q is not yes/no/unsure", path, n, t.Landed)
		}
		out[t.ID] = t
	}
	return out, sc.Err()
}

type landScore struct {
	States                    map[landing.State]int
	CalledMissing             int // rows landed lists as missing (among labelled turns)
	TrueMissing               int // ... a labelled decision that the hand check says did not land
	FalseMissingNotDecision   int // ... not a decision at all
	FalseMissingLanded        int // ... the hand check found a landing
	UncheckedMissing          int // ... no hand landing check yet
	CalledLanded, WrongLanded int // landed rows with a hand check, and those the hand check says did not land
	RealMisses                int // checked decisions the hand check says did not land, in any state
	SupersededMisses          int // of those, called superseded: check by hand that a later choice really replaced them
	FalseMissingIDs, WrongIDs []string
}

func scoreLanding(checks []landing.Check, labels map[string]label, truth map[string]landTruth) landScore {
	s := landScore{States: map[landing.State]int{}}
	for _, c := range checks {
		s.States[c.State]++
		id := c.Decision.ID
		lab := labels[id].Label
		tr, checked := truth[id]
		if checked && tr.Landed == "no" && lab == "decision" && labels[id].Kind != "action" { // a one-off action is never a miss to write down
			s.RealMisses++
		}
		switch c.State {
		case landing.Superseded:
			if checked && tr.Landed == "no" && lab == "decision" && labels[id].Kind != "action" {
				s.SupersededMisses++
			}
		case landing.Missing:
			s.CalledMissing++
			switch {
			case lab == "not":
				s.FalseMissingNotDecision++
				s.FalseMissingIDs = append(s.FalseMissingIDs, id+" (not a decision)")
			case lab == "decision" && labels[id].Kind == "action": // a one-off action is never a miss to write down
				s.FalseMissingNotDecision++
				s.FalseMissingIDs = append(s.FalseMissingIDs, id+" (one-off action)")
			case !checked || tr.Landed == "unsure" || lab != "decision":
				s.UncheckedMissing++
			case tr.Landed == "no":
				s.TrueMissing++
			default:
				s.FalseMissingLanded++
				s.FalseMissingIDs = append(s.FalseMissingIDs, id+" (landed: "+tr.Where+")\n      cwd "+c.Decision.Cwd+"; kws "+strings.Join(c.Decision.Keywords, ", ")+"\n      we "+c.Why)
			}
		case landing.Landed:
			if checked && tr.Landed != "unsure" && lab == "decision" {
				s.CalledLanded++
				if tr.Landed == "no" {
					s.WrongLanded++
					what := "no evidence"
					if c.Evidence != nil {
						what = c.Evidence.Kind + " " + quote(c.Evidence.Ref, 120)
					}
					s.WrongIDs = append(s.WrongIDs, id+" (kind "+c.Decision.Kind+"; "+what+"; "+quote(c.Why, 160)+")")
				}
			}
		}
	}
	return s
}

func (s landScore) print(verbose bool) {
	judged := s.TrueMissing + s.FalseMissingNotDecision + s.FalseMissingLanded
	fmt.Printf("  landing over found decisions: %d landed, %d missing, %d superseded, %d ?\n",
		s.States[landing.Landed], s.States[landing.Missing], s.States[landing.Superseded], s.States[landing.Unknown])
	fmt.Printf("  missing-precision (missing rows that are real): %5.1f%%  (%d of %d hand-checked; target >= 80%%)\n",
		pct(s.TrueMissing, judged), s.TrueMissing, judged)
	fmt.Printf("    wrong missing rows: %d not a decision, %d did land; %d missing rows not hand-checked yet\n",
		s.FalseMissingNotDecision, s.FalseMissingLanded, s.UncheckedMissing)
	fmt.Printf("  real misses shown as missing:      %5.1f%%  (%d of %d lasting decisions the hand check says never landed)\n",
		pct(s.TrueMissing, s.RealMisses), s.TrueMissing, s.RealMisses)
	fmt.Printf("  landed rows the hand check disputes: %d of %d\n", s.WrongLanded, s.CalledLanded)
	fmt.Printf("  real misses called superseded:      %d (check by hand that a later choice replaced them)\n", s.SupersededMisses)
	if verbose {
		sort.Strings(s.FalseMissingIDs)
		for _, id := range s.FalseMissingIDs {
			fmt.Println("  wrong missing:", id)
		}
		for _, id := range s.WrongIDs {
			fmt.Println("  wrong landed:", id)
		}
	}
}

// writeLandTodo writes the decisions a person should check by hand: every
// missing row and as many landed rows, shuffled, without landed's verdict,
// so the hand check is not anchored on it.
func writeLandTodo(path string, checks []landing.Check, labels map[string]label, truth map[string]landTruth) (int, error) {
	var miss, land []landing.Check
	for _, c := range checks {
		if labels[c.Decision.ID].Label != "decision" {
			continue
		}
		if _, done := truth[c.Decision.ID]; done {
			continue
		}
		switch c.State {
		case landing.Missing, landing.Superseded: // a wrong "replaced" would hide a real miss
			miss = append(miss, c)
		case landing.Landed:
			land = append(land, c)
		}
	}
	r := rand.New(rand.NewSource(1114))
	r.Shuffle(len(land), func(i, j int) { land[i], land[j] = land[j], land[i] })
	todo := append(miss, land[:min(len(land), max(len(miss), 10))]...)
	r.Shuffle(len(todo), func(i, j int) { todo[i], todo[j] = todo[j], todo[i] })
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	if err := f.Chmod(0o600); err != nil { // an older file keeps its mode otherwise
		f.Close()
		return 0, err
	}
	enc := json.NewEncoder(f)
	for _, c := range todo {
		d := c.Decision
		if err := enc.Encode(map[string]any{"id": d.ID, "session": d.Session, "cwd": d.Cwd,
			"time": d.Time.Format(time.RFC3339), "text": quote(d.Text, 600), "summary": d.Summary}); err != nil {
			f.Close()
			return 0, err
		}
	}
	return len(todo), f.Close()
}
