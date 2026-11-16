// Command landed finds the owner's chat decisions that never landed in a
// commit, a status file or a bus line.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const usage = `landed: did the decisions you made in chat land anywhere?

usage:
  landed scan [--since 14d] [--root DIR]...   count owner turns per project (no text printed)
  landed turns [--since 14d] [--project X] [--injected]   owner turns as JSON lines (private text; local only)
  landed missing [--since 48h] [--project X] [--grace 4h] [--all]   decisions with no landing: quote, session, project, age
  landed check --session ID | --hook   one session's lasting decisions with no landing yet
  landed report [--since 48h]   count lines for a daily report, with the rescued count (no rows)
  landed proof [--since 30d] [--installed DATE]   what the warnings led to, and how often you repeat a decision, per week
  landed eval [--no-model] [-v]   recall and precision against the hand labels (~/.local/share/landed/labels.jsonl)
`

type rootList []string

func (r *rootList) String() string     { return strings.Join(*r, ",") }
func (r *rootList) Set(v string) error { *r = append(*r, v); return nil }

func main() {
	syscall.Umask(0o077) // the store and its -wal file hold summaries of private text
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "scan":
		err = cmdScan(os.Args[2:])
	case "turns":
		err = cmdTurns(os.Args[2:])
	case "eval":
		err = cmdEval(os.Args[2:])
	case "missing":
		err = cmdMissing(os.Args[2:])
	case "report":
		err = cmdReport(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	case "proof":
		err = cmdProof(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "landed:", err)
		os.Exit(1)
	}
}

// defaultRoots are the transcript folders of every persona on this machine.
// (~/.claude*/projects, links resolved and counted once). Missing ones are skipped.
func defaultRoots() []string {
	home, _ := os.UserHomeDir()
	dirs, _ := filepath.Glob(filepath.Join(home, ".claude*", "projects"))
	var out []string
	seen := map[string]bool{}
	for _, p := range dirs {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		if st, err := os.Stat(real); err != nil || !st.IsDir() || seen[real] {
			continue
		}
		seen[real] = true
		out = append(out, real)
	}
	return out
}

// defaultNoteDirs: the coordinator's state folder and every memory folder
// under the transcript roots.
func defaultNoteDirs() []string {
	home, _ := os.UserHomeDir()
	out := []string{filepath.Join(home, ".crius", "state")}
	for _, r := range defaultRoots() {
		mem, _ := filepath.Glob(filepath.Join(r, "*", "memory"))
		out = append(out, mem...)
	}
	return out
}

// parseAge reads "48h", "14d", "90m".
func parseAge(s string) (time.Duration, error) {
	var d time.Duration
	if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); strings.HasSuffix(s, "d") && err == nil {
		d = time.Duration(n) * 24 * time.Hour
	} else if d, err = time.ParseDuration(s); err != nil {
		return 0, fmt.Errorf("bad age %q (use 48h, 14d, 90m)", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("age must be positive: %q", s)
	}
	return d, nil
}

func cmdScan(args []string) error {
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	since := fs.String("since", "14d", "only sessions changed within this age")
	var roots rootList
	fs.Var(&roots, "root", "transcript root (repeatable; default: every persona's projects dir)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	age, err := parseAge(*since)
	if err != nil {
		return err
	}
	cut := time.Now().Add(-age)
	l := load(roots, cut)
	type agg struct{ sessions, headless, turns, queue, injected, bad int }
	per := map[string]*agg{}
	for _, s := range l.Sessions {
		a := per[s.Cwd]
		if a == nil {
			a = &agg{}
			per[s.Cwd] = a
		}
		a.sessions++
		a.bad += s.BadLines
		a.injected += len(s.Injected)
		if s.Headless {
			a.headless++
			continue
		}
		for _, t := range s.Turns {
			if t.Time.Before(cut) {
				continue
			}
			a.turns++
			if t.Queue {
				a.queue++
			}
		}
	}
	names := make([]string, 0, len(per))
	for k := range per {
		names = append(names, k)
	}
	sort.Slice(names, func(i, j int) bool { return per[names[i]].turns > per[names[j]].turns })
	var tot agg
	fmt.Printf("%-60s %8s %8s %8s %6s %8s %6s\n", "project", "sessions", "headless", "turns", "queue", "injected", "bad")
	for _, n := range names {
		a := per[n]
		tot.sessions += a.sessions
		tot.headless += a.headless
		tot.turns += a.turns
		tot.queue += a.queue
		tot.bad += a.bad
		tot.injected += a.injected
		if a.turns == 0 {
			continue
		}
		fmt.Printf("%-60s %8d %8d %8d %6d %8d %6d\n", n, a.sessions, a.headless, a.turns, a.queue, a.injected, a.bad)
	}
	fmt.Printf("%-60s %8d %8d %8d %6d %8d %6d\n", "TOTAL", tot.sessions, tot.headless, tot.turns, tot.queue, tot.injected, tot.bad)
	fmt.Printf("roots: %s\n", strings.Join(l.Roots, ", "))
	return l.report()
}

// parseFlags parses a subcommand's flags and rejects stray arguments, so
// "landed scan 7d" fails instead of quietly using the default window.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q (flags go like --since 7d)", fs.Arg(0))
	}
	return nil
}

// busDir is the bus folder to read: the --bus flag when given, else the
// coordinator bus (~/.crius/bus) when it exists, else none.
func busDir(given, home string) string {
	if given != "" {
		return given
	}
	d := filepath.Join(home, ".crius", "bus")
	if fi, err := os.Stat(d); err == nil && fi.IsDir() {
		return d
	}
	return ""
}
