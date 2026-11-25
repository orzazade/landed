package landing

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/orzazade/landed/internal/transcript"
)

func git(t *testing.T, dir string, env []string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// commitAt makes a commit with a fixed date so the time window is testable.
func commitAt(t *testing.T, dir string, when time.Time, file, body, msg string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	d := when.Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_DATE=" + d, "GIT_COMMITTER_DATE=" + d}
	git(t, dir, env, "add", file)
	git(t, dir, env, "commit", "-q", "-m", msg)
}

// A made-up kettle project: the owner made three decisions on 11-01.
// The boil-timer one landed in a commit, the beep rule landed on the bus,
// and the lid-sensor rule never landed anywhere. It must show as missing.
func TestReplayMissingDecision(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, nil, "init", "-q")
	git(t, repo, nil, "config", "user.email", "dev@example.invalid")
	git(t, repo, nil, "config", "user.name", "dev")
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	commitAt(t, repo, t0.Add(-24*time.Hour), "README.md", "kettle\n", "initial kettle app")
	// Before the decisions: a commit about the lid must not count.
	commitAt(t, repo, t0.Add(-time.Hour), "lid.go", "package kettle\n", "lid sensor stub")
	commitAt(t, repo, t0.Add(30*time.Minute), "timer.go", "package kettle\n", "add boil timer with countdown")
	commitAt(t, repo, t0.Add(2*time.Hour), "colour.go", "package kettle\n", "paint the handle red")

	bus := t.TempDir()
	busLine := "2026-11-01T10:15 DECISION crius:lead -> kettle:lead — beep when the water boils, silent mode later\n"
	os.WriteFile(filepath.Join(bus, "kettle.log"), []byte("2026-10-31T08:00 INFO crius:lead -> kettle:lead — lid sensor research\n"+busLine), 0o644)

	src := &Sources{BusDir: bus, Window: 7 * 24 * time.Hour, Now: t0.Add(72 * time.Hour), Loc: time.UTC}
	ds := []Decision{
		{ID: "d-timer", Cwd: repo, Time: t0, Summary: "add a boil timer", Keywords: []string{"boil", "timer", "countdown"}},
		{ID: "d-beep", Cwd: repo, Time: t0.Add(10 * time.Minute), Summary: "beep when boiling", Keywords: []string{"beep", "boils"}},
		{ID: "d-lid", Cwd: repo, Time: t0.Add(20 * time.Minute), Summary: "never ship without the lid sensor", Keywords: []string{"lid", "sensor", "ship"}},
	}
	got := map[string]Check{}
	for _, d := range ds {
		got[d.ID] = src.Check(d)
	}
	if c := got["d-timer"]; c.State != Landed || c.Evidence.Kind != "commit" || !strings.Contains(c.Evidence.Ref, "add boil timer") {
		t.Errorf("timer: %+v", c)
	}
	if c := got["d-beep"]; c.State != Landed || c.Evidence.Kind != "bus" {
		t.Errorf("beep: %+v", c)
	}
	if c := got["d-lid"]; c.State != Missing {
		t.Errorf("lid sensor decision never landed but got %v: %+v", c.State, c.Evidence)
	}
}

func TestLaterEditInSessionIsALanding(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	src := &Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	d := Decision{ID: "d", Cwd: t.TempDir(), Time: t0, Keywords: []string{"spout", "ceramic"},
		Later: []transcript.Action{
			{Time: t0.Add(-time.Minute), Tool: "Edit", Path: "/x/STATUS.md", Text: "spout is ceramic"}, // before: no
			{Time: t0.Add(time.Minute), Tool: "Edit", Path: "/x/main.go", Text: "ceramic spout"},       // not a record file
			{Time: t0.Add(2 * time.Minute), Tool: "Edit", Path: "/x/STATUS.md", Text: "- D4 the spout is ceramic from now on"},
		}}
	c := src.Check(d)
	if c.State != Landed || c.Evidence.Kind != "edit" || !strings.HasSuffix(c.Evidence.Ref, "STATUS.md") {
		t.Errorf("got %+v", c)
	}
}

func TestCouldNotCheckIsUnknownNotMissing(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	src := &Sources{BusDir: filepath.Join(t.TempDir(), "no-bus"), Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	// No keywords: nothing to match on.
	if c := src.Check(Decision{ID: "a", Cwd: t.TempDir(), Time: t0}); c.State != Unknown || c.Why == "" {
		t.Errorf("no keywords: %+v", c)
	}
	// Bus folder missing and the cwd is no repo: every source failed.
	if c := src.Check(Decision{ID: "b", Cwd: filepath.Join(t.TempDir(), "gone"), Time: t0, Keywords: []string{"lid", "sensor"}}); c.State != Unknown || c.Why == "" {
		t.Errorf("all sources failed: %+v", c)
	}
}

func TestMatchNeedsTwoDistinctKeywords(t *testing.T) {
	m := matcher([]string{"boil", "timer", "kettle"}, "kettle")
	if got := m.match("kettle app readme"); len(got) != 0 {
		t.Errorf("project name alone matched: %v", got)
	}
	if got := m.match("Added boiling timers"); len(got) != 2 {
		t.Errorf("stems: %v", got)
	}
	if got := m.match("the timer"); len(got) != 1 {
		t.Errorf("one word: %v", got)
	}
}

// Git fails (folder gone) but the bus was read and holds nothing: the landing
// may be in the repo we could not read, so this is ?, not missing.
func TestOneSourceFailedIsUnknown(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	bus := t.TempDir()
	os.WriteFile(filepath.Join(bus, "kettle.log"), []byte("2026-11-01T10:00 INFO crius:lead -> kettle:lead — handle colour chosen\n"), 0o644)
	src := &Sources{BusDir: bus, Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour), Loc: time.UTC}
	c := src.Check(Decision{ID: "a", Cwd: filepath.Join(t.TempDir(), "gone"), Time: t0, Keywords: []string{"lid", "sensor"}})
	if c.State != Unknown || !strings.Contains(c.Why, "could not read git") {
		t.Errorf("git failed, bus empty: got %v %q", c.State, c.Why)
	}
	// A folder that is simply not a repo has no commits: missing, and Why says so.
	c = src.Check(Decision{ID: "b", Cwd: t.TempDir(), Time: t0, Keywords: []string{"lid", "sensor"}})
	if c.State != Missing || !strings.Contains(c.Why, "not in a git repo") {
		t.Errorf("no repo: got %v %q", c.State, c.Why)
	}
}

func TestOneKeywordIsUnknown(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	src := &Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	c := src.Check(Decision{ID: "a", Cwd: t.TempDir(), Time: t0, Keywords: []string{"spout", "the"},
		Later: []transcript.Action{{Time: t0.Add(time.Minute), Tool: "Edit", Path: "/x/STATUS.md", Text: "spout"}}})
	if c.State != Unknown {
		t.Errorf("one usable keyword must not land on one word: %v %+v", c.State, c.Evidence)
	}
}

// A record line on another project's bus log, from that project's lead, is not
// a landing for this project. Lines from the coordinator count anywhere, and a
// line in the same minute as the decision counts.
func TestBusScopeAndSameMinute(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 30, 0, time.UTC)
	repo := t.TempDir()
	git(t, repo, nil, "init", "-q")
	os.Rename(repo, filepath.Join(filepath.Dir(repo), "kettle"))
	repo = filepath.Join(filepath.Dir(repo), "kettle")
	bus := t.TempDir()
	os.WriteFile(filepath.Join(bus, "toaster.log"), []byte("2026-11-02T08:00 DECISION toaster:lead -> crius:lead — crumb tray removable\n"), 0o644)
	src := &Sources{BusDir: bus, Window: 7 * 24 * time.Hour, Now: t0.Add(72 * time.Hour), Loc: time.UTC}
	d := Decision{ID: "a", Cwd: repo, Time: t0, Keywords: []string{"crumb", "tray"}}
	if c := src.Check(d); c.State != Missing {
		t.Errorf("other project's lead line counted: %v %+v", c.State, c.Evidence)
	}
	os.WriteFile(filepath.Join(bus, "toaster.log"), []byte("2026-11-01T09:00 DECISION crius:lead -> kettle:lead — crumb tray removable\n"), 0o644)
	src = &Sources{BusDir: bus, Window: 7 * 24 * time.Hour, Now: t0.Add(72 * time.Hour), Loc: time.UTC}
	if c := src.Check(d); c.State != Landed || c.Evidence.Kind != "bus" {
		t.Errorf("coordinator line in the same minute: %v %q", c.State, c.Why)
	}
}

// A commit that touches many files does not match on its file names.
func TestBigCommitFileNamesIgnored(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, nil, "init", "-q")
	git(t, repo, nil, "config", "user.email", "dev@example.invalid")
	git(t, repo, nil, "config", "user.name", "dev")
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		os.WriteFile(filepath.Join(repo, fmt.Sprintf("%c-%s", rune('a'+i), []string{"steam-valve.go", "whistle-tone.go"}[i%2])), []byte("x"), 0o644)
	}
	d := t0.Add(time.Hour).Format(time.RFC3339)
	env := []string{"GIT_AUTHOR_DATE=" + d, "GIT_COMMITTER_DATE=" + d}
	git(t, repo, env, "add", ".")
	git(t, repo, env, "commit", "-q", "-m", "vendor sync")
	src := &Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	c := src.Check(Decision{ID: "a", Cwd: repo, Time: t0, Keywords: []string{"steam valve", "whistle tone"}})
	if c.State != Missing {
		t.Errorf("file names of a 12-file commit matched: %v %+v", c.State, c.Evidence)
	}
}

// The session works in one repo but commits the decision in another repo it
// edited files in afterwards (kettle-ui next to kettle).
func TestCommitInAnotherRepoTheSessionTouched(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	mk := func() string {
		r := t.TempDir()
		git(t, r, nil, "init", "-q")
		git(t, r, nil, "config", "user.email", "dev@example.invalid")
		git(t, r, nil, "config", "user.name", "dev")
		return r
	}
	main, ui := mk(), mk()
	commitAt(t, ui, t0.Add(time.Hour), "dial.go", "package ui\n", "temperature dial shows celsius by default")
	src := &Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	d := Decision{ID: "a", Cwd: main, Time: t0, Keywords: []string{"temperature dial", "celsius"}}
	if c := src.Check(d); c.State != Missing {
		t.Fatalf("without the edit the ui repo is unknown to the check: %v %+v", c.State, c.Evidence)
	}
	d.Later = []transcript.Action{{Time: t0.Add(30 * time.Minute), Tool: "Edit", Path: filepath.Join(ui, "dial.go"), Text: "x"}}
	if c := src.Check(d); c.State != Landed || c.Evidence.Kind != "commit" || !strings.Contains(c.Evidence.Ref, "celsius") {
		t.Errorf("commit in a touched repo: %v %q %+v", c.State, c.Why, c.Evidence)
	}
}

// knownBirth makes creation times known on every system (Linux does not
// report them): the notes in these tests are written once, so a file's
// change time is its creation time.
func knownBirth(t *testing.T) {
	t.Helper()
	b := birthOf
	t.Cleanup(func() { birthOf = b })
	birthOf = func(fi os.FileInfo) (time.Time, bool) { return fi.ModTime(), true }
}

// A note created after the decision in a shared notes folder is a landing; a
// note created before it is not, even if it was changed later.
func TestNewNoteIsALandingOldNoteIsNot(t *testing.T) {
	knownBirth(t)
	notes := t.TempDir()
	os.WriteFile(filepath.Join(notes, "old.md"), []byte("glass lid with steam vent\n"), 0o600)
	time.Sleep(20 * time.Millisecond)
	t0 := time.Now() // the decision: after old.md was created, before new.md
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(filepath.Join(notes, "new.md"), []byte("decided: kettle lid is glass\n"), 0o600)
	src := &Sources{Window: 7 * 24 * time.Hour, Now: time.Now().Add(time.Hour), Since: t0.Add(-time.Hour), NoteDirs: []string{notes, filepath.Join(notes, "absent")}}
	c := src.Check(Decision{ID: "a", Cwd: t.TempDir(), Time: t0, Keywords: []string{"glass", "lid"}})
	if c.State != Landed || c.Evidence.Kind != "note" || !strings.Contains(c.Evidence.Ref, "new.md (new)") {
		t.Errorf("got %v %q %+v", c.State, c.Why, c.Evidence)
	}
	c = src.Check(Decision{ID: "b", Cwd: t.TempDir(), Time: t0, Keywords: []string{"steam", "vent"}})
	if c.State != Missing {
		t.Errorf("old note counted: %v %+v", c.State, c.Evidence)
	}
}

// Another session (a team lead) writes the decision into its STATUS.md and
// commits elsewhere: with Actions set, that counts.
func TestEditByAnotherSessionCounts(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	acts := []transcript.Action{
		{Time: t0.Add(-time.Hour), Tool: "Edit", Path: "/state/kettle/STATUS.md", Text: "descale reminder monthly"}, // before
		{Time: t0.Add(time.Hour), Tool: "Bash", Command: `git -C /src/kettle commit -m "descale reminder every month"`},
	}
	src := &Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour), Actions: acts}
	c := src.Check(Decision{ID: "a", Cwd: t.TempDir(), Time: t0, Keywords: []string{"descale", "reminder"}})
	if c.State != Landed || !strings.Contains(c.Evidence.Ref, "git -C /src/kettle commit") {
		t.Errorf("got %v %q %+v", c.State, c.Why, c.Evidence)
	}
}

// git that fails for a reason other than "not a git repository" (here: unsafe
// ownership) is a failure to look: the row is ?, never missing.
func TestGitFailureIsNotNoRepo(t *testing.T) {
	bin := t.TempDir()
	fake := filepath.Join(bin, "git")
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'fatal: detected dubious ownership in repository' >&2\nexit 128\n"), 0o700)
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	src := &Sources{Git: fake, Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	c := src.Check(Decision{ID: "a", Cwd: t.TempDir(), Time: t0, Keywords: []string{"lid", "sensor"}})
	if c.State != Unknown || !strings.Contains(c.Why, "dubious ownership") {
		t.Errorf("got %v %q", c.State, c.Why)
	}
	// A repo the session touched later that git cannot read also makes it ?.
	real := &Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	d := Decision{ID: "b", Cwd: t.TempDir(), Time: t0, Keywords: []string{"lid", "sensor"},
		Later: []transcript.Action{{Time: t0.Add(time.Minute), Tool: "Edit", Path: filepath.Join(t.TempDir(), "x.go")}}}
	if c := real.Check(d); c.State != Missing {
		t.Fatalf("control: %v %q", c.State, c.Why)
	}
	src = &Sources{Git: fake, Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	if c := src.Check(d); c.State != Unknown {
		t.Errorf("touched repo unreadable: %v %q", c.State, c.Why)
	}
}

// With ChangedNotes, a note created before the decision but changed after it
// is a candidate, and the judge reads only its matching lines. Without it, it
// is not (keywords alone would match the old text).
func TestChangedNoteShowsMatchingLines(t *testing.T) {
	notes := t.TempDir()
	p := filepath.Join(notes, "STATUS.md")
	os.WriteFile(p, []byte("# kettle\nold line about colours\n"), 0o600)
	time.Sleep(20 * time.Millisecond)
	t0 := time.Now()
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(p, []byte("# kettle\nold line about colours\n- D7 the descale reminder runs monthly\n"), 0o600)
	d := Decision{ID: "a", Cwd: t.TempDir(), Time: t0, Keywords: []string{"descale", "reminder"}}
	narrow := &Sources{Window: 7 * 24 * time.Hour, Now: time.Now().Add(time.Hour), Since: t0.Add(-time.Hour), NoteDirs: []string{notes}}
	if c := narrow.Check(d); c.State != Missing {
		t.Errorf("narrow: changed note counted: %v %+v", c.State, c.Evidence)
	}
	wide := &Sources{Window: 7 * 24 * time.Hour, Now: time.Now().Add(time.Hour), Since: t0.Add(-time.Hour), NoteDirs: []string{notes}, ChangedNotes: true}
	_, cands := wide.Gather(d)
	if len(cands) != 1 || !strings.Contains(cands[0].Ref, "(changed)") || cands[0].Text != "- D7 the descale reminder runs monthly" {
		t.Errorf("wide candidates: %+v", cands)
	}
}

// Repos lists repos searched for every decision, even ones the session
// never touched.
func TestExtraReposAreSearched(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	other := t.TempDir()
	git(t, other, nil, "init", "-q")
	git(t, other, nil, "config", "user.email", "dev@example.invalid")
	git(t, other, nil, "config", "user.name", "dev")
	commitAt(t, other, t0.Add(time.Hour), "filter.go", "package f\n", "limescale filter in the spout")
	src := &Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour), Repos: []string{other}}
	c := src.Check(Decision{ID: "a", Cwd: t.TempDir(), Time: t0, Keywords: []string{"limescale", "filter"}})
	if c.State != Landed || c.Evidence.Kind != "commit" {
		t.Errorf("got %v %q", c.State, c.Why)
	}
}

func TestFindRepos(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a/.git", "b/c/.git", "b/c/node_modules/x/.git", "deep/1/2/3/4/5/6/.git"} {
		os.MkdirAll(filepath.Join(root, d), 0o700)
	}
	got, err := FindRepos(root, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{filepath.Join(root, "a"): true, filepath.Join(root, "b/c"): true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, r := range got {
		if !want[r] {
			t.Errorf("unexpected %s", r)
		}
	}
}

// A worktree shows the same history as its main repo: each commit is one
// candidate, not one per checkout.
func TestWorktreeCommitsCountOnce(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	main := t.TempDir()
	git(t, main, nil, "init", "-q")
	git(t, main, nil, "config", "user.email", "dev@example.invalid")
	git(t, main, nil, "config", "user.name", "dev")
	commitAt(t, main, t0.Add(time.Hour), "spout.go", "package s\n", "limescale filter in the spout")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, main, nil, "worktree", "add", "-q", wt)
	src := &Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour), Repos: []string{main, wt}}
	_, cands := src.Gather(Decision{ID: "a", Cwd: t.TempDir(), Time: t0, Keywords: []string{"limescale", "filter"}})
	n := 0
	for _, e := range cands {
		if e.Kind == "commit" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d commit candidates, want 1: %+v", n, cands)
	}
}

// A note over the read limit is read up to the limit, and the row says so.
func TestBigNoteIsCutNotSkipped(t *testing.T) {
	knownBirth(t)
	notes := t.TempDir()
	t0 := time.Now()
	time.Sleep(20 * time.Millisecond)
	big := "decided: kettle lid is glass\n" + strings.Repeat("filler line\n", maxNoteBytes/12+10)
	os.WriteFile(filepath.Join(notes, "big.md"), []byte(big), 0o600)
	src := &Sources{Window: 7 * 24 * time.Hour, Now: time.Now().Add(time.Hour), Since: t0.Add(-time.Hour), NoteDirs: []string{notes}}
	c := src.Check(Decision{ID: "a", Cwd: t.TempDir(), Time: t0, Keywords: []string{"glass", "lid"}})
	if c.State != Landed || c.Evidence.Kind != "note" {
		t.Errorf("big note skipped: %v %q", c.State, c.Why)
	}
	c = src.Check(Decision{ID: "b", Cwd: t.TempDir(), Time: t0, Keywords: []string{"steam", "vent"}})
	if c.State != Missing || !strings.Contains(c.Why, "1 cut at 1 MB") {
		t.Errorf("cut not reported: %v %q", c.State, c.Why)
	}
}

// Only Markdown notes count: tool output (.txt, .yaml) is not a record and
// may hold secrets that must not reach the judge.
func TestOnlyMarkdownNotes(t *testing.T) {
	notes := t.TempDir()
	t0 := time.Now()
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(filepath.Join(notes, "dump.txt"), []byte("kettle lid is glass\n"), 0o600)
	os.WriteFile(filepath.Join(notes, "conf.yaml"), []byte("lid: glass\n"), 0o600)
	src := &Sources{Window: 7 * 24 * time.Hour, Now: time.Now().Add(time.Hour), Since: t0.Add(-time.Hour), NoteDirs: []string{notes}}
	c := src.Check(Decision{ID: "a", Cwd: t.TempDir(), Time: t0, Keywords: []string{"glass", "lid"}})
	if c.State != Missing {
		t.Errorf("non-Markdown note counted: %v %+v", c.State, c.Evidence)
	}
}

// A worktree whose git folder was deleted is not a repo (git log would fail
// on it and turn every row into ?); a live one is.
func TestFindReposSkipsStaleWorktree(t *testing.T) {
	root := t.TempDir()
	gitdir := filepath.Join(root, "main", ".git", "worktrees", "live")
	os.MkdirAll(gitdir, 0o700)
	os.MkdirAll(filepath.Join(root, "live"), 0o700)
	os.WriteFile(filepath.Join(root, "live", ".git"), []byte("gitdir: "+gitdir+"\n"), 0o600)
	os.MkdirAll(filepath.Join(root, "stale"), 0o700)
	os.WriteFile(filepath.Join(root, "stale", ".git"), []byte("gitdir: "+filepath.Join(root, "gone")+"\n"), 0o600)
	os.MkdirAll(filepath.Join(root, "sub"), 0o700) // a submodule: relative gitdir
	os.MkdirAll(filepath.Join(root, "main", ".git", "modules", "sub"), 0o700)
	os.WriteFile(filepath.Join(root, "sub", ".git"), []byte("gitdir: ../main/.git/modules/sub\n"), 0o600)
	got, err := FindRepos(root, 5)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(root, "live"), filepath.Join(root, "main"), filepath.Join(root, "sub")}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// A command that only reads or shows (git show, cat, grep) never carries a
// decision out, so the judge is never offered it; a command that writes is.
func TestReadOnlyCommandsAreNotActed(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	src := &Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	at := func(m int, cmd string) transcript.Action {
		return transcript.Action{Time: t0.Add(time.Duration(m) * time.Minute), Tool: "Bash", Command: cmd}
	}
	d := Decision{ID: "d", Cwd: t.TempDir(), Time: t0, Keywords: []string{"spout", "ceramic"},
		Later: []transcript.Action{
			at(1, "git show abc123 -- kettle.go"),
			at(2, "cat STATUS.md | grep spout"),
			at(3, "cd /x && git log --oneline -5"),
			at(4, "sed -n 1,20p kettle.go"),
			at(5, "sed -i '' s/steel/ceramic/ kettle.go"),
			at(6, "git commit -m 'ceramic spout'"),
			at(7, "find . -name '*.tmp' -delete"),
			at(8, "echo ceramic > STATUS.md"),
		}}
	_, cands := src.Gather(d)
	var acted []string
	for _, e := range cands {
		if e.Kind == "acted" {
			acted = append(acted, e.Ref)
		}
	}
	want := []string{"Bash shell: sed -i '' s/steel/ceramic/ kettle.go", "Bash shell: git commit -m 'ceramic spout'",
		"Bash shell: find . -name '*.tmp' -delete", "Bash shell: echo ceramic > STATUS.md"}
	if !slices.Equal(acted, want) {
		t.Errorf("acted = %q, want %q", acted, want)
	}
}

func TestReadOnly(t *testing.T) {
	reads := []string{
		"git show abc123 -- kettle.go", "cat STATUS.md | grep spout", "sed -n 1,20p kettle.go",
		"git log --oneline -S'spout' -- kettle.go 2>/dev/null | head -5",
		"git log --oneline -S'spout' -- kettle.go | cat; /usr/bin/grep -n -i 'lid\\|spout' kettle_test.go | head -30",
		"grep -n 'a > b; c' kettle.go", "/usr/bin/grep -n spout kettle.go 2>/dev/null",
	}
	writes := []string{
		"sed -i '' s/steel/ceramic/ kettle.go", "git commit -m 'ceramic spout'", "find . -name '*.tmp' -delete",
		"echo ceramic > STATUS.md", "sed -n --in-place 1p kettle.go", "sort -o kettle.txt kettle.txt",
		"cat <(rm kettle.go)", "find . -name x -fprint out.txt", "echo 'x' > 'out file'", `echo \' > out`,
		"ls # don't\nrm -rf build", "sed -n 1p -i kettle.go", "sed -n 1p -e 'w out.txt' kettle.go",
		"git grep -O'rm -rf x' spout", "git diff --ext-diff", "rg --pre ./script.sh spout", "/tmp/x/cat kettle.go",
		"cat kettle.go >/dev/nullx", "less --log-file=x a", "FOO=1 cat a", "cat a | tee b", "cat a | xargs rm",
		strings.Repeat("a", transcript.MaxCommand),
	}
	for _, c := range reads {
		if !readOnly(c) {
			t.Errorf("read counted as a write: %q", c)
		}
	}
	for _, c := range writes {
		if readOnly(c) {
			t.Errorf("write counted as a read: %q", c)
		}
	}
}

// Where file creation times cannot be read (Linux), notes still work as
// changed notes: no error, no ? row.
func TestNotesWithoutCreationTime(t *testing.T) {
	defer func(b func(os.FileInfo) (time.Time, bool)) { birthOf = b }(birthOf)
	birthOf = func(os.FileInfo) (time.Time, bool) { return time.Time{}, false }
	notes := t.TempDir()
	t0 := time.Now()
	time.Sleep(20 * time.Millisecond)
	os.WriteFile(filepath.Join(notes, "STATUS.md"), []byte("- D7 the descale reminder runs monthly\n"), 0o600)
	d := Decision{ID: "a", Cwd: t.TempDir(), Time: t0, Keywords: []string{"descale", "reminder"}}
	wide := &Sources{Window: 7 * 24 * time.Hour, Now: time.Now().Add(time.Hour), Since: t0.Add(-time.Hour), NoteDirs: []string{notes}, ChangedNotes: true}
	c, cands := wide.Gather(d)
	if len(c.failed) > 0 || len(cands) != 1 || !strings.Contains(cands[0].Ref, "(changed)") {
		t.Errorf("failed %v, candidates %+v", c.failed, cands)
	}
}

// A later owner decision on the same subject is a candidate, so a judge can
// say it replaces this one. Earlier decisions, this decision itself and
// decisions after the window are not candidates.
func TestLaterDecisionIsACandidate(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	src := &Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(30 * 24 * time.Hour), Decisions: []Decision{
		{ID: "old", Session: "s0", Time: t0.Add(-time.Hour), Summary: "spout is plastic", Text: "the spout stays plastic"},
		{ID: "d", Session: "s1", Time: t0, Summary: "spout is ceramic", Text: "make the spout ceramic"},
		{ID: "new", Session: "s2", Time: t0.Add(time.Hour), Summary: "spout is steel, not ceramic", Text: "forget ceramic, the spout is steel"},
		{ID: "late", Session: "s3", Time: t0.Add(10 * 24 * time.Hour), Summary: "spout is glass", Text: "ceramic spout out, glass spout in"},
	}}
	d := Decision{ID: "d", Cwd: t.TempDir(), Time: t0, Summary: "spout is ceramic", Keywords: []string{"spout", "ceramic"}}
	c, cands := src.Gather(d)
	var refs []string
	for _, e := range cands {
		if e.Kind == "decision" {
			refs = append(refs, e.Ref)
		}
	}
	if len(refs) != 1 || !strings.Contains(refs[0], "s2") {
		t.Fatalf("decision candidates = %v, want only the later one in s2", refs)
	}
	i := slices.IndexFunc(cands, func(e Evidence) bool { return e.Kind == "decision" })
	got := Finish(c, cands, -1, i, "judge: steel replaces ceramic")
	if got.State != Superseded || got.Evidence == nil || got.Evidence.Kind != "decision" {
		t.Errorf("replaced by a later decision: %v %+v", got.State, got.Evidence)
	}
	if got.State.String() != "superseded" {
		t.Errorf("state name %q", got.State.String())
	}
	// A record found wins over a replacement: the decision did land.
	withNote := append(slices.Clone(cands), Evidence{Kind: "note", Ref: "STATUS.md", Text: "spout is ceramic"})
	if got := Finish(c, withNote, len(withNote)-1, i, ""); got.State != Landed {
		t.Errorf("record and replaced: %v", got.State)
	}
	// A later decision is never the record of this one.
	if got := Finish(c, cands, i, -1, ""); got.State == Landed {
		t.Errorf("a later decision was taken as the record")
	}
}

// A decision carried out late in its session (after the first actions) by
// writing a script, not a note, is still a candidate when it matches.
func TestLateSessionActionIsACandidate(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	var later []transcript.Action
	for i := range 15 {
		later = append(later, transcript.Action{Time: t0.Add(time.Duration(i+1) * time.Minute), Tool: "Bash", Command: fmt.Sprintf("make build-%d", i)})
	}
	// Weaker matches first (two words each), as in a busy session.
	for i := range 12 {
		later = append(later, transcript.Action{Time: t0.Add(time.Hour + time.Duration(i)*time.Minute), Tool: "Bash", Command: fmt.Sprintf("echo docker up > job-%d.log", i)})
	}
	// The one that carries it out: the matching line sits deep in a long script.
	later = append(later, transcript.Action{Time: t0.Add(3 * time.Hour), Tool: "Write", Path: "/x/jobs/g1/step.sh",
		Text: strings.Repeat("echo setting up the kettle test database\n", 20) + "docker volume rm kettle-db-g1\n"})
	src := &Sources{Window: 7 * 24 * time.Hour, Now: t0.Add(48 * time.Hour)}
	_, cands := src.Gather(Decision{ID: "d", Cwd: t.TempDir(), Time: t0, Keywords: []string{"docker volume rm", "job scripts"}, Later: later})
	i := slices.IndexFunc(cands, func(e Evidence) bool { return strings.Contains(e.Ref, "step.sh") })
	if i < 0 {
		t.Fatalf("late script write is not a candidate; got %d candidates", len(cands))
	}
	if !strings.Contains(cands[i].Text, "docker volume rm") {
		t.Errorf("the judge does not see the matching line: %q", cands[i].Text)
	}
}

// Commits, notes and bus lines read ahead (repos several at once) are the
// same candidates Gather finds by reading them itself, and Gather reads
// none of them again.
func TestPrefetchedSourcesAreTheSame(t *testing.T) {
	knownBirth(t)
	t0 := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	var repos []string
	for i := range 12 {
		r := t.TempDir()
		git(t, r, nil, "init", "-q")
		git(t, r, nil, "config", "user.email", "dev@example.invalid")
		git(t, r, nil, "config", "user.name", "dev")
		commitAt(t, r, t0.Add(time.Duration(i+1)*time.Minute), "dial.go", "package ui\n", fmt.Sprintf("temperature dial %d shows celsius", i))
		repos = append(repos, r)
	}
	notes, bus := t.TempDir(), t.TempDir()
	note, busLog := filepath.Join(notes, "dial.md"), filepath.Join(bus, "kettle.log")
	if err := os.WriteFile(note, []byte("decided: the temperature dial shows celsius\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(busLog, []byte(t0.Add(time.Hour).Format("2006-01-02T15:04")+" DECISION crius:lead -> kettle:lead — temperature dial shows celsius\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Hour) // the note is new: written just now
	d := Decision{ID: "a", Time: t0, Keywords: []string{"temperature dial", "celsius"}}
	mk := func() *Sources {
		return &Sources{Window: 365 * 24 * time.Hour, Now: now, Since: t0.Add(-time.Hour), Repos: repos,
			NoteDirs: []string{notes}, ChangedNotes: true, BusDir: bus, AllBus: true, Loc: time.UTC}
	}
	_, want := mk().Gather(d)
	kinds := map[string]int{}
	for _, e := range want {
		kinds[e.Kind]++
	}
	if kinds["commit"] != len(repos) || kinds["note"] != 1 || kinds["bus"] != 1 {
		t.Fatalf("control: candidates by kind %v, want a commit per repo (%d), one note, one bus line", kinds, len(repos))
	}
	src := mk()
	src.Prefetch()
	src.Git = filepath.Join(t.TempDir(), "no-git") // any git log now would fail
	os.Remove(note)                                // and so would reading these
	os.Remove(busLog)
	_, got := src.Gather(d)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("prefetched candidates differ:\n got %+v\nwant %+v", got, want)
	}
}
