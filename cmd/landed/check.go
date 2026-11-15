package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/orzazade/landed/internal/landing"
)

// hookBudget is how long the hook may take; Claude Code's own timeout for it
// is set a little higher in the hook config.
var hookBudget = 8 * time.Second

// slow: the check was not done within the budget. Mostly the first run of
// a session, which reads its history and asks the model; the PreCompact
// warm-up started that read in the background.
func slow() error { return fmt.Errorf("still reading history (not done within %v)", hookBudget) }

// warmBudget bounds a background warm-up, so a hung read cannot pile up.
const warmBudget = 10 * time.Minute

// checkRun runs the checks; a test swaps it for a slow one.
var checkRun = runChecks

// maxListed is how many decisions the hook lists; the rest are counted.
const maxListed = 10

// cmdCheck lists one session's lasting decisions that have no landing yet.
// With --hook it reads the session from a Claude Code hook's stdin JSON,
// prints nothing when every decision landed, and never fails.
func checkFlags() (fs *flag.FlagSet, o *checkOpts, since, grace, window *string, hook, warm *bool) {
	fs = flag.NewFlagSet("check", flag.ExitOnError)
	o = &checkOpts{}
	since, grace, window = o.flags(fs)
	fs.Lookup("since").DefValue, *since = "30d", "30d" // the whole session
	fs.Lookup("grace").DefValue, *grace = "0", "0"     // a decision made a minute ago counts too
	fs.StringVar(&o.session, "session", "", "session id (the transcript file's name)")
	hook = fs.Bool("hook", false, "Claude Code hook mode: session from stdin JSON, silent when nothing is missing, never fails")
	warm = fs.Bool("warm", false, "with --hook: only start a background check of the session (for PreCompact)")
	fs.BoolVar(&o.background, "background", false, "internal: the warm-up's own run (one per session, output dropped)")
	return
}

func cmdCheck(args []string) error {
	fs, op, since, grace, window, hook, warm := checkFlags()
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	o := *op
	if err := o.parse(*since, *grace, *window); err != nil {
		return err
	}
	if *hook && *warm {
		// PreCompact stdout steers the summary, so errors go to stderr.
		return runWarm(os.Stdin, os.Stderr, startCheck)
	}
	if *hook {
		return runHook(os.Stdin, os.Stdout, o)
	}
	if o.session == "" {
		return errors.New("--session is required (or --hook)")
	}
	ctx := context.Background()
	if o.background {
		unlock, ok := lockSession(filepath.Dir(o.db), o.session, time.Now())
		if !ok {
			return nil // a warm-up for this session is already running
		}
		defer unlock()
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, warmBudget)
		defer cancel()
	}
	c, err := runChecks(ctx, o, time.Now())
	if err != nil {
		return err
	}
	open := unlanded(c.Checks)
	for _, ch := range open {
		fmt.Printf("%-7s %q\n        why: %s\n", ch.State, quote(ch.Decision.Text, 100), quote(ch.Why, 200))
	}
	sup := 0
	for _, ch := range c.Checks {
		if ch.State == landing.Superseded {
			sup++
		}
	}
	fmt.Printf("\n%d lasting decisions in session %s: %d landed, %d superseded, %d with no landing yet\n",
		len(c.Checks), o.session, len(c.Checks)-len(open)-sup, sup, len(open))
	return runErrors(c, c.Loaded.report())
}

// runHook is the SessionStart hook: it reads {"session_id": …} from stdin
// and writes the session's unlanded lasting decisions for the agent to read.
// Every failure becomes one "landed could not check" line; it returns nil.
func runHook(stdin io.Reader, stdout io.Writer, o checkOpts) error {
	fail := func(why string) error {
		fmt.Fprintf(stdout, "landed could not check: %s\n", quote(why, 200))
		return nil
	}
	id, err := readHookInput(stdin)
	if err != nil {
		return fail(err.Error())
	}
	o.session = id
	// The check runs on its own; the hook answers at its budget whatever
	// stage is slow, so Claude Code never has to kill it (that would be silent).
	ctx, cancel := context.WithTimeout(context.Background(), hookBudget)
	defer cancel()
	type result struct {
		c   checked
		err error
	}
	done := make(chan result, 1)
	run := checkRun
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- result{err: fmt.Errorf("panic: %v", r)}
			}
		}()
		c, err := run(ctx, o, time.Now())
		done <- result{c, err}
	}()
	var c checked
	select {
	case r := <-done:
		c, err = r.c, r.err
		if err == nil && ctx.Err() != nil {
			err = slow()
		}
	case <-ctx.Done():
		err = slow()
	}
	if err == nil {
		err = hookErrors(c)
	}
	if err != nil {
		return fail(err.Error())
	}
	open := unlanded(c.Checks)
	if len(open) == 0 {
		return nil
	}
	fmt.Fprintln(stdout, "Owner decisions in this session with no landing yet:")
	for i, ch := range open {
		if i == maxListed {
			fmt.Fprintf(stdout, "- … and %d more (landed check --session %s)\n", len(open)-maxListed, o.session)
			break
		}
		fmt.Fprintf(stdout, "- %q\n", quote(ch.Decision.Text, 160))
	}
	fmt.Fprintln(stdout, "Land each before going on.")
	return nil
}

// hookErrors is runErrors without printing: the hook's stderr is not read.
func hookErrors(c checked) error {
	if n := len(c.Loaded.Errors); n > 0 {
		return fmt.Errorf("%d errors while reading transcripts (first: %v)", n, c.Loaded.Errors[0])
	}
	return runErrors(c, nil)
}

// unlanded are the checks with no landing: missing, or ? (could not check).
// Superseded ones are not: a later decision or line replaced them.
func unlanded(checks []landing.Check) []landing.Check {
	var out []landing.Check
	for _, ch := range checks {
		if ch.State == landing.Missing || ch.State == landing.Unknown {
			out = append(out, ch)
		}
	}
	return out
}

// sessionIDRe is what a Claude Code session id looks like; anything else is
// refused before it reaches a file name or a command line.
var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// readHookInput reads the session id from a hook's stdin JSON.
func readHookInput(stdin io.Reader) (string, error) {
	var in struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&in); err != nil {
		return "", fmt.Errorf("hook input is not JSON: %w", err)
	}
	if !sessionIDRe.MatchString(in.SessionID) {
		return "", fmt.Errorf("hook input has no usable session_id (%q)", quote(in.SessionID, 40))
	}
	return in.SessionID, nil
}

// runWarm is the PreCompact hook: it starts a background check of the
// session and returns at once, so the SessionStart hook after the summary
// finds the model's answers cached. Like the other hook it never fails.
func runWarm(stdin io.Reader, stdout io.Writer, start func(id string) error) error {
	id, err := readHookInput(stdin)
	if err == nil {
		err = start(id)
	}
	if err != nil {
		fmt.Fprintf(stdout, "landed could not check: %s\n", quote(err.Error(), 200))
	}
	return nil
}

// startCheck runs `landed check --session id` in the background, detached
// from the hook, its output dropped (it fills the cache).
func startCheck(id string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, "check", "--background", "--session", id)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// staleLock: a lock older than this was left by a run that died (a live
// one stops at warmBudget).
const staleLock = warmBudget + 5*time.Minute

// lockSession takes the warm-up lock of session id in dir. It refuses while
// another run holds it, and takes over a stale one.
func lockSession(dir, id string, now time.Time) (unlock func(), ok bool) {
	p := filepath.Join(dir, "warm-"+id+".lock")
	if os.MkdirAll(dir, 0o700) != nil {
		return nil, false
	}
	for range 2 {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			os.Chtimes(p, now, now)
			return func() { os.Remove(p) }, true
		}
		fi, serr := os.Stat(p)
		if serr != nil || now.Sub(fi.ModTime()) < staleLock {
			return nil, false
		}
		os.Remove(p) // stale: its run died
	}
	return nil, false
}
