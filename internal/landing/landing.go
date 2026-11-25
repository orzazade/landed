// Package landing looks for where a decision landed after it was made: a git
// commit, a change to a record file (STATUS.md, TASKS.md, AGENTS.md, …), a bus
// line, or a later edit of such a file in the same session.
package landing

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orzazade/landed/internal/transcript"
)

// State of one decision. The zero value is Unknown, so nothing is called
// missing by accident.
type State int

const (
	Unknown    State = iota // could not check; Why says what failed
	Missing                 // checked, no landing found
	Landed                  // Evidence says where
	Superseded              // a later decision or line replaces or withdraws it; Evidence says which
)

func (s State) String() string { return [...]string{"?", "missing", "landed", "superseded"}[s] }

// Decision is one owner decision to check.
type Decision struct {
	ID       string
	Session  string
	Cwd      string
	Time     time.Time
	Text     string
	Summary  string
	Keywords []string
	Later    []transcript.Action // the assistant's actions in the same session after the decision
	Kind     string              // "rule" (lasting) for the rows checked; one-off actions are dropped before the check
}

// Evidence is the best landing found.
type Evidence struct {
	Kind    string // commit, bus, edit, note
	Ref     string // "abc1234 add boil timer", "STATUS.md", "kettle.log 2026-11-01T10:15 DECISION …"
	When    time.Time
	Matched []string // the keyword words that matched
	Text    string   // what a judge reads: commit message, edited text or bus line (cut)
}

type Check struct {
	Decision Decision
	State    State
	Evidence *Evidence
	Why      string // for missing: what was searched; for unknown: what failed

	searched, failed, absent []string // kept so a judge's answer can finish the check
	kws                      []string
}

// Sources holds where to look and caches what was read.
type Sources struct {
	BusDir string         // ~/.crius/bus
	Window time.Duration  // how long after a decision a landing still counts
	Now    time.Time      // zero: time.Now()
	Since  time.Time      // earliest decision time; git history is read from here once per repo
	Loc    *time.Location // time zone of bus timestamps; nil: local
	// NoteDirs hold Markdown notes any session may write (~/.crius/state,
	// memory folders). A note counts only when it was created after the
	// decision, so old text never counts.
	NoteDirs []string
	// Actions are the assistant actions of every session, sorted by time.
	// When set, record edits, commits and bus posts by any session count, not
	// only those of the decision's own session.
	Actions []transcript.Action
	Git     string // git binary; "" is "git" (tests use a fake)
	// Repos are searched for commits for every decision, besides the
	// session's own repos (e.g. every repo under ~/Projects).
	Repos    []string
	ReposErr error // finding Repos failed somewhere: those commits were not searched
	// Wide: a judge reads the candidates, so changed notes (old text in them)
	// and bus lines from any log count. Keywords alone stay narrow.
	ChangedNotes, AllBus bool
	// Decisions are the owner's lasting decisions of every session read,
	// sorted by time. A later one on the same subject is a candidate, so a
	// judge can say it replaces an earlier one.
	Decisions []Decision

	tops    map[string]topResult
	commits map[string]commitResult
	bus     *busResult
	notes   *notesResult
}

type note struct {
	path          string
	born, changed time.Time
	text          string
	cut           bool // only the first maxNoteBytes were read
}

type notesResult struct {
	list []note
	err  error
}

type topResult struct {
	top string
	err error
}

type commit struct {
	hash    string
	when    time.Time
	subject string
	text    string // subject, body and changed file names
}

type commitResult struct {
	list []commit
	err  error
}

type busLine struct {
	file   string
	when   time.Time
	kind   string
	sender string // "crius:lead" in "… DECISION crius:lead -> x:lead — …"
	raw    string
}

type busResult struct {
	lines []busLine
	err   error
}

// isRecordFile: any Markdown file (STATUS.md, AGENTS.md, notes, docs, state
// folders) is where agents write things down.
func isRecordFile(p string) bool { return strings.HasSuffix(strings.ToLower(p), ".md") }

// Bus line kinds that ask rather than record.
var busAsk = map[string]bool{"NEED": true, "ALERT": true}

var busLineRe = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2})\s+([A-Z-]+)\s+(\S*)(.*)$`)

// maxCommitFiles: above this many changed files, file names are not matched.
const maxCommitFiles = 10

func (s *Sources) now() time.Time {
	if s.Now.IsZero() {
		return time.Now()
	}
	return s.Now
}

// Check looks for a landing of one decision by keywords alone: the best
// candidate sharing at least minMatch keyword words.
func (s *Sources) Check(d Decision) Check {
	c, cands := s.Gather(d)
	if !c.Gathered() {
		return c // could not even start
	}
	best := -1
	for i, e := range cands {
		if e.Kind != "acted" && e.Kind != "decision" && len(e.Matched) >= minMatch {
			best = i // cands are ranked: the first good one is the best
			break
		}
	}
	return Finish(c, cands, best, -1, "")
}

// Gather collects the candidates for one decision: record-file edits later in
// the session, commits in the session's repos and bus lines, inside the time
// window, sharing at least one keyword word. They come ranked (most words
// matched, then earliest) and cut to maxCandidates. The returned Check is
// final when it is Unknown with nothing searched; otherwise pass it to Finish.
func (s *Sources) Gather(d Decision) (Check, []Evidence) {
	c := Check{Decision: d}
	project := ""
	top, topErr := s.toplevel(d.Cwd)
	if topErr == nil {
		project = filepath.Base(top)
	} else if d.Cwd != "" {
		project = filepath.Base(d.Cwd)
	}
	m := matcher(d.Keywords, project)
	if len(m.words) < minMatch {
		c.Why = fmt.Sprintf("fewer than %d usable keyword words to match on (%s)", minMatch, strings.Join(m.words, ", "))
		return c, nil
	}
	c.kws = m.kws
	from := d.Time
	to := d.Time.Add(s.Window)
	if n := s.now(); to.After(n) {
		to = n
	}
	in := func(t time.Time) bool { return !t.Before(from) && !t.After(to) }

	var cands []Evidence
	consider := func(e Evidence, text string) {
		e.Matched = m.match(text)
		if len(e.Matched) == 0 {
			return
		}
		if e.Text == "" {
			e.Text = firstRunes(strings.Join(strings.Fields(text), " "), maxCandText)
		}
		cands = append(cands, e)
	}

	// 1. Later record edits, commits and bus posts: by any session when
	// Actions is set, else by the decision's own session.
	edits := 0
	acts, scope := d.Later, "in the session"
	if len(s.Actions) > 0 {
		lo := sort.Search(len(s.Actions), func(i int) bool { return !s.Actions[i].Time.Before(from) })
		hi := sort.Search(len(s.Actions), func(i int) bool { return s.Actions[i].Time.After(to) })
		acts, scope = s.Actions[lo:hi], "by any session"
	}
	for _, a := range acts {
		if !in(a.Time) {
			continue
		}
		switch {
		case a.Path != "" && isRecordFile(a.Path):
			edits++
			consider(Evidence{Kind: "edit", Ref: a.Path, When: a.Time}, a.Text)
		case a.Tool == "Bash" && writesRecord(a.Command):
			edits++
			consider(Evidence{Kind: "edit", Ref: "shell: " + firstLine(a.Command, 80), When: a.Time}, a.Command)
		}
	}
	c.searched = append(c.searched, fmt.Sprintf("%d later record edits %s", edits, scope))

	// 1b. The owner's later decisions in any session that share minMatch
	// keyword words: a judge may find one that replaces this decision. They
	// never count as its record, and are ranked and capped apart, so they
	// never push a record out of the candidates.
	var later []Evidence
	if len(s.Decisions) > 0 {
		lo := sort.Search(len(s.Decisions), func(i int) bool { return s.Decisions[i].Time.After(from) })
		n := 0
		for _, o := range s.Decisions[lo:] {
			if o.Time.After(to) {
				break
			}
			if o.ID == d.ID {
				continue
			}
			n++
			text := o.Summary + " | " + o.Text
			if mt := m.match(text); len(mt) >= minMatch {
				later = append(later, Evidence{Kind: "decision", Ref: "session " + o.Session[:min(8, len(o.Session))] + " " + firstLine(o.Text, 80),
					When: o.Time, Matched: mt, Text: firstRunes(strings.Join(strings.Fields(text), " "), maxCandText)})
			}
		}
		rank(later)
		later = later[:min(len(later), maxActed)]
		c.searched = append(c.searched, fmt.Sprintf("%d later decisions", n))
	}

	// 2. Git commits (all branches) in the session's repo and in every other
	// repo the session edited files in after the decision. A folder that is not
	// a repo has no commits to search; any other git error is a failure.
	var repos []string
	switch {
	case errors.Is(topErr, errNotRepo):
		c.absent = append(c.absent, topErr.Error())
	case topErr != nil:
		c.failed = append(c.failed, "git: "+topErr.Error())
	default:
		repos = append(repos, top)
	}
	for _, a := range d.Later {
		if a.Path == "" || !in(a.Time) {
			continue
		}
		t, err := s.toplevel(filepath.Dir(a.Path))
		switch {
		case err == nil:
			if !slices.Contains(repos, t) {
				repos = append(repos, t)
			}
		case errors.Is(err, errNotRepo), errors.Is(err, errGone):
			// no repo there, or the folder is gone now: no commits to search
		default:
			if msg := "git: " + err.Error(); !slices.Contains(c.failed, msg) {
				c.failed = append(c.failed, msg)
			}
		}
	}
	if s.ReposErr != nil {
		c.failed = append(c.failed, "repo list: "+s.ReposErr.Error())
	}
	for _, r := range s.Repos {
		if !slices.Contains(repos, r) {
			repos = append(repos, r)
		}
	}
	nRepos, nCommits := 0, 0
	seenCommit := map[string]bool{} // worktrees of one repo share its history
	for _, r := range repos {
		list, err := s.repoCommits(r, d.Time)
		if err != nil {
			c.failed = append(c.failed, "git log in "+r+": "+err.Error())
			continue
		}
		n := 0
		for _, cm := range list {
			if !in(cm.when) || seenCommit[cm.hash] {
				continue
			}
			seenCommit[cm.hash] = true
			n++
			consider(Evidence{Kind: "commit", Ref: filepath.Base(r) + " " + cm.hash[:min(7, len(cm.hash))] + " " + cm.subject, When: cm.when}, cm.text)
		}
		nRepos++
		nCommits += n
	}
	if nRepos > 0 {
		c.searched = append(c.searched, fmt.Sprintf("%d commits in %d repos", nCommits, nRepos))
	}

	// 3. Bus lines that record (not ask), from this project's log or from the
	// coordinator. Bus times have minute precision.
	if s.BusDir == "" {
		c.absent = append(c.absent, "no bus folder given")
	} else if lines, err := s.busLines(); err != nil {
		c.failed = append(c.failed, "bus: "+err.Error())
	} else {
		n := 0
		fromMin := from.Truncate(time.Minute)
		for _, l := range lines {
			if l.when.Before(fromMin) || l.when.After(to) || busAsk[l.kind] {
				continue
			}
			if !s.AllBus && !(project != "" && l.file == project+".log") && l.sender != coordinator {
				continue
			}
			n++
			consider(Evidence{Kind: "bus", Ref: l.file + " " + firstLine(l.raw, 120), When: l.when}, l.raw)
		}
		c.searched = append(c.searched, fmt.Sprintf("%d bus lines", n))
	}

	// 4. Notes in the shared note folders: created after the decision (all
	// of it is new), or changed after it (only the matching lines are shown;
	// the judge decides whether they record this decision).
	if len(s.NoteDirs) > 0 {
		if list, err := s.notesList(); err != nil {
			c.failed = append(c.failed, "notes: "+err.Error())
		} else {
			n, cut := 0, 0
			for _, nt := range list {
				switch {
				case in(nt.born):
					consider(Evidence{Kind: "note", Ref: nt.path + " (new)", When: nt.born}, nt.text)
				case in(nt.changed) && s.ChangedNotes:
					consider(Evidence{Kind: "note", Ref: nt.path + " (changed)", When: nt.changed, Text: snippet(nt.text, m)}, nt.text)
				default:
					continue
				}
				n++
				if nt.cut {
					cut++
				}
			}
			if cut > 0 {
				c.searched = append(c.searched, fmt.Sprintf("%d notes (%d cut at 1 MB)", n, cut))
			} else {
				c.searched = append(c.searched, fmt.Sprintf("%d notes", n))
			}
		}
	}

	rank(cands)
	if len(cands) > maxCandidates {
		cands = cands[:maxCandidates]
	}

	// 5. What the session did after the decision: its first actions, keywords
	// or not (a decision carried out at once, like a config edit or a command,
	// has landed), and later ones that match the keywords (work done hours on,
	// in a script or code file, not a note). Only a judge can tell; keywords
	// alone never pick these.
	// The later ones are ranked by keyword rarity (not taken in time order)
	// and show their matching lines, which may sit deep in a long script.
	var acted, late []Evidence
	for _, a := range d.Later {
		if !in(a.Time) || (a.Tool == "Bash" && readOnly(a.Command)) {
			continue
		}
		ref, text := a.Path, a.Text
		if a.Tool == "Bash" {
			ref, text = "shell: "+firstLine(a.Command, 80), a.Command
		}
		e := Evidence{Kind: "acted", Ref: a.Tool + " " + ref, When: a.Time, Matched: m.match(text)}
		switch {
		case len(acted) < maxActed:
			e.Text = firstRunes(strings.Join(strings.Fields(text), " "), maxCandText)
			acted = append(acted, e)
		case len(e.Matched) >= minMatch:
			e.Text = snippet(text, m)
			late = append(late, e)
		}
	}
	rank(late)
	late = late[:min(len(late), maxActed)]
	acted = append(acted, late...)
	if len(acted) > 0 {
		c.searched = append(c.searched, fmt.Sprintf("%d actions after it (%d matched later)", len(acted), len(late)))
	}
	return c, append(append(cands, later...), acted...)
}

// Gathered reports whether Gather searched anything; when false the check is
// final (Unknown, Why says why).
func (c Check) Gathered() bool { return len(c.searched) > 0 }

// rank orders candidates by the rarity of the words they matched: a word
// found in most candidates says little, a word found in one says much
// (inverse document frequency over this decision's candidates). Ties go to the
// earliest.
func rank(cands []Evidence) {
	df := map[string]int{}
	for _, e := range cands {
		for _, w := range e.Matched {
			df[w]++
		}
	}
	n := float64(len(cands))
	sc := make([]float64, len(cands))
	for i, e := range cands {
		for _, w := range e.Matched {
			sc[i] += math.Log(1 + n/float64(df[w]))
		}
	}
	idx := make([]int, len(cands))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		if sc[idx[a]] != sc[idx[b]] {
			return sc[idx[a]] > sc[idx[b]]
		}
		return cands[idx[a]].When.Before(cands[idx[b]].When)
	})
	out := make([]Evidence, len(cands))
	for k, i := range idx {
		out[k] = cands[i]
	}
	copy(cands, out)
}

// Finish turns a gathered check into its final state. record is the index of
// the candidate that records the decision, or -1; replaced is the index of the
// candidate that replaces or withdraws it, or -1; why is the judge's reason
// ("" when keywords alone chose).
func Finish(c Check, cands []Evidence, record, replaced int, why string) Check {
	if record >= 0 && record < len(cands) && cands[record].Kind == "decision" {
		record = -1 // a later decision only states a choice again; it never records one
	}
	switch {
	case record >= 0 && record < len(cands):
		ev := cands[record]
		c.State, c.Evidence, c.Why = Landed, &ev, why
	case replaced >= 0 && replaced < len(cands):
		ev := cands[replaced]
		c.State, c.Evidence, c.Why = Superseded, &ev, why
	case len(c.failed) > 0:
		// A source we meant to read failed: the landing may be there.
		c.State = Unknown
		c.Why = "could not read " + strings.Join(c.failed, "; ") + "; searched " + strings.Join(c.searched, ", ")
	default:
		c.State = Missing
		c.Why = "searched " + strings.Join(c.searched, ", ") + " for " + strings.Join(c.kws, ", ")
		if why != "" {
			c.Why += fmt.Sprintf("; none of %d candidates records it (%s)", len(cands), why)
		}
		if len(c.absent) > 0 {
			c.Why += "; " + strings.Join(c.absent, "; ")
		}
	}
	return c
}

// birthOf reads a file's creation time; a test swaps it.
var birthOf = birth

// maxNoteBytes is how much of a note is read; the row says when a note was cut.
const maxNoteBytes = 1 << 20

// noteExt: notes are Markdown. Other text files in note folders are mostly
// tool output and may hold secrets the judge must not see.
var noteExt = map[string]bool{".md": true}

// notesList reads every note file under NoteDirs changed on or after Since.
// A folder that does not exist is skipped; any other read error fails the
// source.
func (s *Sources) notesList() ([]note, error) {
	if s.notes != nil {
		return s.notes.list, s.notes.err
	}
	s.notes = &notesResult{}
	since := s.Since
	for _, dir := range s.NoteDirs {
		err := filepath.WalkDir(dir, func(p string, de os.DirEntry, err error) error {
			if err != nil {
				if p == dir && errors.Is(err, os.ErrNotExist) {
					return filepath.SkipDir
				}
				return err
			}
			if de.IsDir() {
				if n := de.Name(); n == ".git" || n == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !noteExt[strings.ToLower(filepath.Ext(p))] {
				return nil
			}
			fi, err := de.Info()
			if err != nil {
				return err
			}
			if fi.ModTime().Before(since) {
				return nil
			}
			b, ok := birthOf(fi)
			if !ok {
				b = time.Time{} // unknown (Linux): the note can only count as changed
			}
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			text, err := io.ReadAll(io.LimitReader(f, maxNoteBytes))
			f.Close()
			if err != nil {
				return err
			}
			s.notes.list = append(s.notes.list, note{path: p, born: b, changed: fi.ModTime(), text: string(text), cut: fi.Size() > maxNoteBytes})
			return nil
		})
		if err != nil {
			s.notes.err = err
			s.notes.list = nil
			return nil, err
		}
	}
	return s.notes.list, nil
}

// snippet returns the lines of text that hold the most matched words, up to
// maxCandText: what a judge reads of a note that existed before the decision.
func snippet(text string, m kwMatcher) string {
	type ln struct {
		i, n int
	}
	lines := strings.Split(text, "\n")
	var hits []ln
	for i, l := range lines {
		if n := len(m.match(l)); n > 0 {
			hits = append(hits, ln{i, n})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool { return hits[a].n > hits[b].n })
	var out []string
	size := 0
	for _, h := range hits {
		l := strings.Join(strings.Fields(lines[h.i]), " ")
		if size+len(l) > maxCandText && len(out) > 0 {
			break
		}
		out = append(out, l)
		size += len(l) + 3
	}
	return firstRunes(strings.Join(out, " … "), maxCandText)
}

// maxCandidates is how many candidates a judge sees per decision.
const maxCandidates = 25

// readOnlyCmds only read or show; a command made only of them carries
// nothing out. git counts when its subcommand is in readOnlyGit.
var readOnlyCmds = map[string]bool{"cat": true, "head": true, "tail": true, "ls": true,
	"grep": true, "rg": true, "find": true, "wc": true, "cd": true, "pwd": true, "echo": true, "jq": true,
	"stat": true, "diff": true, "cut": true}
var readOnlyGit = map[string]bool{"show": true, "log": true, "diff": true, "status": true, "blame": true,
	"rev-parse": true, "ls-files": true, "grep": true, "shortlog": true}

// runsOrWrites are flag prefixes (any case) that write a file or run a
// program: -o/--output, git grep -O, git --ext-diff, rg --pre.
var runsOrWrites = []string{"-o", "--output", "--ext-diff", "--pre"}

// binDirs are where a full-path command is the same as its bare name.
var binDirs = map[string]bool{"/bin": true, "/usr/bin": true, "/usr/local/bin": true, "/opt/homebrew/bin": true}

// sedPrint is the one sed form counted as reading: sed -n 'N[,M]p' files.
var sedPrint = regexp.MustCompile(`^['"]?[0-9]+(,[0-9]+)?p['"]?$`)

// quiet are redirects that write nothing.
var quiet = regexp.MustCompile(`[0-9]?>/dev/null\b|2>&1`)

// readOnly reports whether a shell command only reads. Anything unsure counts
// as a write, so real work is never hidden from the judge: more than one line,
// a redirect into a file, a substitution, a flag that writes or runs
// something, a command cut short, a command not on the list.
func readOnly(cmd string) bool {
	if len([]rune(cmd)) >= transcript.MaxCommand || strings.TrimSpace(cmd) == "" || strings.ContainsAny(cmd, "\n\r") {
		return false // cut short, or several lines (a # comment can hide a quote)
	}
	cmd = maskQuoted(quiet.ReplaceAllString(cmd, " "))
	if strings.ContainsAny(cmd, "<>`$") {
		return false // a redirect or a substitution may write
	}
	for _, part := range strings.FieldsFunc(cmd, func(r rune) bool { return r == '|' || r == ';' || r == '&' }) {
		f := strings.Fields(part)
		if len(f) == 0 {
			continue
		}
		if dir := filepath.Dir(f[0]); binDirs[dir] {
			f[0] = filepath.Base(f[0]) // /usr/bin/grep is grep
		}
		if slices.ContainsFunc(f[1:], func(a string) bool {
			return slices.ContainsFunc(runsOrWrites, func(p string) bool { return strings.HasPrefix(strings.ToLower(a), p) })
		}) {
			return false
		}
		switch {
		case f[0] == "find" && slices.ContainsFunc(f, func(a string) bool {
			return a == "-delete" || strings.HasPrefix(a, "-exec") || strings.HasPrefix(a, "-ok") || strings.HasPrefix(a, "-f") && a != "-follow"
		}):
			return false
		case readOnlyCmds[f[0]]:
		case f[0] == "git" && len(f) > 1 && readOnlyGit[f[1]]:
		case f[0] == "sed" && len(f) >= 3 && f[1] == "-n" && sedPrint.MatchString(f[2]) &&
			!slices.ContainsFunc(f[3:], func(a string) bool { return strings.HasPrefix(a, "-") }):
		default:
			return false
		}
	}
	return true
}

// maskQuoted blanks | ; & < > and newlines inside quotes, where they are
// text, not shell syntax ('a\\|b' is one grep pattern). $ and ` stay: they
// still run inside double quotes.
func maskQuoted(cmd string) string {
	b := []rune(cmd)
	var q rune
	esc := false
	for i, r := range b {
		switch {
		case esc:
			esc = false
		case q != '\'' && r == '\\':
			esc = true // \' and \" outside quotes (and \" inside "...") open nothing
		case q == 0 && (r == '\'' || r == '"'):
			q = r
		case q != 0 && r == q:
			q = 0
		case q != 0 && strings.ContainsRune("|;&<>\n", r):
			b[i] = '_'
		}
	}
	return string(b)
}

// GatherVersion is part of every cached landed outcome's key: changing what
// Gather offers the judge (sources, filters, time bounds) means bumping it.
const GatherVersion = "g5"

// maxActed is how many of the session's next actions a judge sees.
const maxActed = 10

// maxCandText is how much of each candidate a judge reads.
const maxCandText = 400

func firstRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// minMatch is how many distinct keyword words a landing must share with the decision.
const minMatch = 2

// coordinator is the bus sender whose record lines count for every project.
const coordinator = "crius:lead"

var (
	errNotRepo = errors.New("not in a git repo")
	errGone    = errors.New("is gone")
)

var writesRecordRe = regexp.MustCompile(`(?:>>?|tee(?: -a)?)\s*\S*\.md\b|bus-post\b|\bgit\b[^|;&]*\bcommit\b`)

func writesRecord(cmd string) bool { return writesRecordRe.MatchString(cmd) }

func firstLine(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (s *Sources) git() string {
	if s.Git == "" {
		return "git"
	}
	return s.Git
}

func (s *Sources) toplevel(cwd string) (string, error) {
	if s.tops == nil {
		s.tops = map[string]topResult{}
	}
	if r, ok := s.tops[cwd]; ok {
		return r.top, r.err
	}
	var r topResult
	if cwd == "" {
		r.err = errors.New("session has no working folder")
	} else if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
		r.err = fmt.Errorf("folder %s: %w", cwd, errGone)
	} else {
		out, err := exec.Command(s.git(), "-C", cwd, "rev-parse", "--show-toplevel").Output()
		var ee *exec.ExitError
		switch {
		case errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "not a git repository"):
			r.err = fmt.Errorf("%s is %w", cwd, errNotRepo)
		case err != nil: // git missing, unsafe ownership, no permission: a failure to look
			msg := err.Error()
			if ee != nil {
				msg += ": " + strings.TrimSpace(string(ee.Stderr))
			}
			r.err = fmt.Errorf("git rev-parse in %s: %s", cwd, firstLine(msg, 200))
		default:
			r.top = strings.TrimSpace(string(out))
		}
	}
	s.tops[cwd] = r
	return r.top, r.err
}

func (s *Sources) repoCommits(top string, from time.Time) ([]commit, error) {
	since := s.Since
	if since.IsZero() || from.Before(since) {
		since = from
	}
	if s.commits == nil {
		s.commits = map[string]commitResult{}
	}
	key := commitKey(top, since)
	if r, ok := s.commits[key]; ok {
		return r.list, r.err
	}
	r := s.readCommits(top, since)
	s.commits[key] = r
	return r.list, r.err
}

func commitKey(top string, since time.Time) string { return top + "|" + since.Format(time.RFC3339) }

// Prefetch reads the notes, the bus lines and the commits since Since of
// every repo in Repos (several repos at once), so Gather finds them already
// read. None of them depend on the decisions.
func (s *Sources) Prefetch() {
	s.notesList()
	if s.BusDir != "" {
		s.busLines()
	}
	if s.Since.IsZero() || len(s.Repos) == 0 {
		return
	}
	res := make([]commitResult, len(s.Repos))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, r := range s.Repos {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			res[i] = s.readCommits(r, s.Since)
		}()
	}
	wg.Wait()
	if s.commits == nil {
		s.commits = map[string]commitResult{}
	}
	for i, r := range s.Repos {
		s.commits[commitKey(r, s.Since)] = res[i]
	}
}

// readCommits runs git log in top for the commits since since.
func (s *Sources) readCommits(top string, since time.Time) commitResult {
	out, err := exec.Command(s.git(), "-C", top, "log", "--all", "--since="+since.Add(-time.Minute).Format(time.RFC3339),
		"--format=%x1e%H%x1f%ct%x1f%s%x1f%b%x1f", "--name-only").Output()
	var r commitResult
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		r.err = err
	} else {
		for _, rec := range strings.Split(string(out), "\x1e") {
			f := strings.SplitN(rec, "\x1f", 5)
			if len(f) < 5 {
				continue
			}
			sec, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				r.err = fmt.Errorf("git log: bad commit time %q", f[1])
				r.list = nil
				break
			}
			text := f[2] + "\n" + f[3]
			// File names tell what a commit is about, unless it touches so many
			// files that every keyword would match one.
			if names := strings.Fields(f[4]); len(names) <= maxCommitFiles {
				text += "\n" + f[4]
			}
			r.list = append(r.list, commit{hash: f[0], when: time.Unix(sec, 0), subject: f[2], text: text})
		}
	}
	return r
}

func (s *Sources) busLines() ([]busLine, error) {
	if s.bus != nil {
		return s.bus.lines, s.bus.err
	}
	s.bus = &busResult{}
	loc := s.Loc
	if loc == nil {
		loc = time.Local
	}
	files, err := filepath.Glob(filepath.Join(s.BusDir, "*.log"))
	if err == nil && len(files) == 0 {
		err = fmt.Errorf("no *.log files in %s", s.BusDir)
	}
	if err != nil {
		s.bus.err = err
		return nil, err
	}
	sort.Strings(files)
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			s.bus.err = err
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			mm := busLineRe.FindStringSubmatch(sc.Text())
			if mm == nil {
				continue // continuation or other text
			}
			when, err := time.ParseInLocation("2006-01-02T15:04", mm[1], loc)
			if err != nil {
				continue
			}
			s.bus.lines = append(s.bus.lines, busLine{file: filepath.Base(f), when: when, kind: mm[2], sender: mm[3], raw: sc.Text()})
		}
		err = sc.Err()
		fh.Close()
		if err != nil {
			s.bus.err = fmt.Errorf("%s: %w", f, err)
			return nil, s.bus.err
		}
	}
	return s.bus.lines, nil
}

// Keywords too common to tell one decision from another.
var stopKeywords = map[string]bool{
	"owner": true, "lead": true, "crius": true, "team": true, "status": true, "update": true,
	"change": true, "add": true, "fix": true, "use": true, "new": true, "make": true, "work": true,
	"task": true, "plan": true, "the": true, "and": true, "for": true, "with": true,
	"option": true, "choice": true, "later": true, "yes": true, "approve": true, "approval": true,
}

type kwMatcher struct {
	kws   []string // usable keywords, as given (lowercase), for the report
	words []string // distinct words of all keywords, as written
	stems []string // the same words cut to a stem
}

var tokenRe = regexp.MustCompile(`[\p{L}\p{N}_]+`)

func stem(w string) string {
	if len([]rune(w)) > 6 {
		return string([]rune(w)[:6])
	}
	return w
}

// matcher prepares keywords: lowercase, drops stop words and the project
// name, and splits phrases into words. A landing must contain minMatch of
// these distinct words: a decision's summary and the commit that records it
// rarely share whole phrases.
func matcher(kws []string, project string) kwMatcher {
	var m kwMatcher
	seen := map[string]bool{}
	project = strings.ToLower(project)
	for _, k := range kws {
		k = strings.ToLower(strings.TrimSpace(k))
		if k == "" || stopKeywords[k] || k == project {
			continue
		}
		added := false
		for _, w := range tokenRe.FindAllString(k, -1) {
			st := stem(w)
			if len(w) < 3 || stopKeywords[w] || w == project || seen[st] {
				continue
			}
			seen[st] = true
			m.words = append(m.words, w)
			m.stems = append(m.stems, st)
			added = true
		}
		if added {
			m.kws = append(m.kws, k)
		}
	}
	return m
}

// match returns the keyword words that start a word of the text. Words of 3
// letters must match whole.
func (m kwMatcher) match(text string) []string {
	toks := tokenRe.FindAllString(strings.ToLower(text), -1)
	var got []string
	for i, w := range m.stems {
		for _, t := range toks {
			if t == w || (len(w) >= 4 && strings.HasPrefix(t, w)) {
				got = append(got, m.words[i])
				break
			}
		}
	}
	return got
}

// FindRepos returns every git repo (folder or worktree) under root, up to
// depth levels down. Unreadable folders are returned as an error alongside
// the repos that were found.
func FindRepos(root string, depth int) ([]string, error) {
	var repos []string
	var bad []string
	base := strings.Count(filepath.Clean(root), string(filepath.Separator))
	err := filepath.WalkDir(root, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			if p == root {
				return err
			}
			if !errors.Is(err, os.ErrNotExist) { // removed while we walk: gone, not unreadable
				bad = append(bad, p)
			}
			return filepath.SkipDir
		}
		if de.Name() == ".git" {
			if de.IsDir() {
				repos = append(repos, filepath.Dir(p))
				return filepath.SkipDir
			}
			// A worktree or submodule: .git is a file naming the real git
			// folder. When that folder is gone, git cannot read it: skip it.
			b, err := os.ReadFile(p)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					bad = append(bad, p)
				}
				return nil
			}
			gd, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
			if !ok {
				bad = append(bad, p)
				return nil
			}
			gd = strings.TrimSpace(gd)
			if !filepath.IsAbs(gd) {
				gd = filepath.Join(filepath.Dir(p), gd)
			}
			if _, err := os.Stat(gd); err == nil {
				repos = append(repos, filepath.Dir(p))
			} else if !errors.Is(err, os.ErrNotExist) {
				bad = append(bad, p)
			}
			return nil
		}
		if de.IsDir() {
			if n := de.Name(); n == "node_modules" || n == "vendor" || strings.Count(p, string(filepath.Separator))-base >= depth {
				return filepath.SkipDir
			}
		}
		return nil
	})
	if err == nil && len(bad) > 0 {
		err = fmt.Errorf("%d folders could not be read, e.g. %s", len(bad), bad[0])
	}
	return repos, err
}
