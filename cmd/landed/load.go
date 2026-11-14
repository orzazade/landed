package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/orzazade/landed/internal/transcript"
)

// loaded is every session changed after the cut, plus what could not be read.
type loaded struct {
	Roots    []string
	Files    int
	Sessions []*transcript.Session
	Errors   []error // walk and read errors; callers print them, never drop them
	Repeated int     // turns already seen in another file (a resumed or forked session), dropped
	BadLines int     // transcript lines that were not valid JSON
	BadTimes int     // owner turns without a readable timestamp
}

func load(roots []string, cut time.Time) loaded { return loadCtx(context.Background(), roots, cut) }

// loadCtx reads every transcript changed after cut, several at once. When ctx
// ends first, the files not read are an error, never a quiet partial read.
func loadCtx(ctx context.Context, roots []string, cut time.Time) loaded {
	if len(roots) == 0 {
		roots = defaultRoots()
	}
	out := loaded{Roots: roots}
	files, err := transcript.Walk(roots)
	out.Files = len(files)
	if err != nil {
		out.Errors = append(out.Errors, err)
	}
	type result struct {
		s    *transcript.Session
		err  error
		skip bool
	}
	res := make([]result, len(files))
	next := make(chan int)
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				f := files[i]
				st, err := os.Stat(f)
				if err != nil {
					res[i].err = err
					continue
				}
				if st.ModTime().Before(cut) {
					res[i].skip = true
					continue
				}
				s, err := transcript.ParseFile(f)
				if err != nil {
					err = fmt.Errorf("read %s: %w", f, err)
				}
				res[i] = result{s: s, err: err}
			}
		}()
	}
	sent := 0
	for i := range files {
		if ctx.Err() != nil {
			break
		}
		next <- i
		sent++
	}
	close(next)
	wg.Wait()
	if sent < len(files) || (len(files) == 0 && ctx.Err() != nil) {
		out.Errors = append(out.Errors, fmt.Errorf("%d of %d transcripts not read: %w", len(files)-sent, len(files), ctx.Err()))
	}
	// Turns are kept in file order, so the same turn in two files (a resumed
	// or forked session) is dropped the same way every run.
	seen := map[string]bool{}
	dedupe := func(list []transcript.Turn) []transcript.Turn {
		kept := list[:0]
		for _, t := range list {
			if t.UUID != "" {
				if seen[t.UUID] {
					out.Repeated++
					continue
				}
				seen[t.UUID] = true
			}
			kept = append(kept, t)
		}
		return kept
	}
	for _, r := range res[:sent] {
		if r.err != nil {
			out.Errors = append(out.Errors, r.err)
		}
		if r.s == nil {
			continue
		}
		s := r.s
		s.Turns = dedupe(s.Turns)
		s.Injected = dedupe(s.Injected)
		out.BadLines += s.BadLines
		out.BadTimes += s.BadTimes
		out.Sessions = append(out.Sessions, s)
	}
	return out
}

// sessionCut is when session id's first owner turn after cut was made: older
// transcripts cannot hold a landing of its decisions, so check skips them.
// It fails when no transcript is named after the session; ok is false when
// the session has no owner turn after cut (nothing to check).
func sessionCut(roots []string, id string, cut time.Time) (_ time.Time, ok bool, _ error) {
	if len(roots) == 0 {
		roots = defaultRoots()
	}
	files, err := transcript.Walk(roots)
	if err != nil {
		return cut, false, err
	}
	found := false
	first := time.Time{}
	for _, f := range files {
		if filepath.Base(f) != id+".jsonl" {
			continue
		}
		found = true
		s, err := transcript.ParseFile(f)
		if err != nil {
			return cut, false, fmt.Errorf("read %s: %w", f, err)
		}
		for _, t := range s.Turns {
			if !t.Time.Before(cut) && (first.IsZero() || t.Time.Before(first)) {
				first = t.Time
			}
		}
	}
	if !found {
		return cut, false, fmt.Errorf("no transcript for session %s", id)
	}
	if first.IsZero() {
		return cut, false, nil
	}
	return first, true, nil
}

// report prints the read summary to stderr and turns read errors into the
// command's error, so a partial read never looks like a clean one.
func (l loaded) report() error {
	fmt.Fprintf(os.Stderr, "read %d sessions from %d files; bad lines %d; turns without time %d; repeated turns %d; errors %d\n",
		len(l.Sessions), l.Files, l.BadLines, l.BadTimes, l.Repeated, len(l.Errors))
	for _, e := range l.Errors {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
	if len(l.Errors) > 0 {
		return fmt.Errorf("%d errors while reading transcripts", len(l.Errors))
	}
	return nil
}
