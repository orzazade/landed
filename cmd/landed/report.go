package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/orzazade/landed/internal/landing"
)

// cmdReport prints a short block for a daily report: counts
// only, never rows. `missing` is not precise enough (50% when last measured)
// for its rows to go into a report; `landed missing` lists them.
func cmdReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	var o checkOpts
	since, grace, window := o.flags(fs)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := o.parse(*since, *grace, *window); err != nil {
		return err
	}
	now := time.Now()
	c, err := runChecks(context.Background(), o, now)
	if err != nil {
		fmt.Println("landed: could not run (the error is on stderr)") // the report still says something; no paths in it
		return err
	}
	readErr := c.Loaded.report()
	var buf bytes.Buffer
	writeReport(&buf, c, *since, *grace, readErr)
	// The report shows counts, not rows: each row counted as unlanded is
	// logged with the count line as what was shown.
	first, _, _ := strings.Cut(buf.String(), "\n")
	open := unlanded(c.Checks)
	shown := make([]string, len(open))
	for i := range shown {
		shown[i] = first
	}
	sayNotRecorded(&buf, errors.Join(logWarns(proofPath(o), "report", "", open, shown, now), c.ProofErr))
	os.Stdout.Write(buf.Bytes())
	return runErrors(c, readErr)
}

func writeReport(w io.Writer, c checked, since, grace string, readErr error) {
	counts := map[landing.State]int{}
	reasons := map[string]int{}
	for _, ch := range c.Checks {
		counts[ch.State]++
		if ch.State == landing.Unknown {
			reasons[reasonOf(ch.Why)]++
		}
	}
	// The same counts as `landed missing`: decisions are the checked ones plus
	// the too-new ones; turns whose decision call failed are counted apart.
	fmt.Fprintf(w, "landed · last %s: %d decisions found: %d landed, %d missing, %d superseded, %d ?, %d too new (< %s)\n",
		since, len(c.Checks)+c.TooNew, counts[landing.Landed], counts[landing.Missing], counts[landing.Superseded], counts[landing.Unknown], c.TooNew, grace)
	if readErr != nil {
		fmt.Fprintln(w, "  ! transcripts not fully read")
	}
	if len(c.Unknown) > 0 {
		fmt.Fprintf(w, "  ! %d turns could not be classified\n", len(c.Unknown))
	}
	if len(c.Stats.Errors) > 0 {
		fmt.Fprintf(w, "  ! %d errors while finding decisions\n", len(c.Stats.Errors))
	}
	if len(c.JudgeErrors) > 0 {
		fmt.Fprintf(w, "  ! %d errors while judging landings\n", len(c.JudgeErrors))
	}
	if counts[landing.Unknown] > 0 {
		var parts []string
		for r, k := range reasons {
			parts = append(parts, fmt.Sprintf("%d %s", k, r))
		}
		sort.Strings(parts)
		fmt.Fprintf(w, "  ? %d could not be checked: %s\n", counts[landing.Unknown], strings.Join(parts, "; "))
	}
	fmt.Fprintf(w, "  rescued (shown missing, landed later): %d in the last 7 days, %d in all\n", c.RescuedWeek, c.RescuedAll)
	fmt.Fprintf(w, "  details: landed missing --since %s\n", since)
}

// reasonOf maps a ? row's reason to a fixed phrase. The free text names
// repos, paths and keywords, which do not belong in a report.
func reasonOf(why string) string {
	switch {
	case strings.HasPrefix(why, "fewer than"):
		return "too few keywords"
	case strings.HasPrefix(why, "could not read"):
		return "a source could not be read"
	case strings.HasPrefix(why, "judge"), strings.HasPrefix(why, "judgment cache"):
		return "landing judge failed"
	default:
		return "other reason"
	}
}

// runErrors is every failure of a run, joined; nil when there was none.
// Rows that are ? for too few keywords are a limit, not a failure.
func runErrors(c checked, readErr error) error {
	var errs []string
	if readErr != nil {
		errs = append(errs, readErr.Error())
	}
	if n := len(c.Unknown); n > 0 {
		errs = append(errs, fmt.Sprintf("%d turns could not be classified", n))
	}
	if n := len(c.Stats.Errors); n > 0 {
		errs = append(errs, fmt.Sprintf("%d classifier errors (first: %s)", n, quote(c.Stats.Errors[0].Error(), 120)))
	}
	if n := len(c.JudgeErrors); n > 0 {
		errs = append(errs, fmt.Sprintf("%d landing judgments failed (first: %s)", n, quote(c.JudgeErrors[0], 120)))
	}
	unread := 0
	for _, ch := range c.Checks {
		if ch.State == landing.Unknown && reasonOf(ch.Why) == "a source could not be read" {
			unread++
		}
	}
	if unread > 0 {
		errs = append(errs, fmt.Sprintf("%d rows: a source could not be read", unread))
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("%s (those rows are ?; landed missing shows them)", strings.Join(errs, "; "))
}
