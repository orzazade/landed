package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/orzazade/landed/internal/decide"
	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/transcript"
)

func TestWriteReport(t *testing.T) {
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	d := func(id, sess, sum string, h int) landing.Decision {
		return landing.Decision{ID: id, Session: sess, Summary: sum, Time: now.Add(-time.Duration(h) * time.Hour)}
	}
	c := checked{
		Projects: map[string]string{"sess-kettle-1": "kettle", "sess-teapot-2": "teapot"},
		Checks: []landing.Check{
			{Decision: d("a", "sess-kettle-1", "add a boil timer", 50), State: landing.Landed},
			{Decision: d("b", "sess-kettle-1", "never ship without the lid sensor", 49), State: landing.Missing},
			{Decision: d("c", "sess-teapot-2", "ceramic spout", 10), State: landing.Unknown,
				Why: "could not read git log in /home/dev/teapot: exit status 128; searched 3 bus lines"},
			{Decision: d("f", "sess-teapot-2", "glaze colour", 9), State: landing.Unknown, Why: "judge failed: claude -p: timeout"},
		},
		TooNew:      2,
		Unknown:     []decide.Result{{Turn: transcript.Turn{UUID: "e"}, Err: "model failed: timeout"}},
		RescuedWeek: 2, RescuedAll: 5,
	}
	var b strings.Builder
	writeReport(&b, c, "48h", "4h", errors.New("1 errors while reading transcripts"))
	got := b.String()
	for _, want := range []string{
		"6 decisions found: 1 landed, 1 missing, 2 ?, 2 too new",
		"! transcripts not fully read",
		"! 1 turns could not be classified",
		"? 2 could not be checked: 1 a source could not be read; 1 landing judge failed",
		"details: landed missing --since 48h",
		"rescued (shown missing, landed later): 2 in the last 7 days, 5 in all",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
	for _, bad := range []string{"/home/dev", "teapot", "timeout", "ceramic"} {
		if strings.Contains(got, bad) {
			t.Errorf("report shows %q:\n%s", bad, got)
		}
	}
}

// Option B: missing-precision is 50%, so the report gives counts only and
// never lists a decision's words.
func TestReportListsNoRows(t *testing.T) {
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	var c checked
	c.Projects = map[string]string{"sess-kettle-1": "kettle"}
	for i := range 8 {
		c.Checks = append(c.Checks, landing.Check{State: landing.Missing, Decision: landing.Decision{
			ID: fmt.Sprint("m", i), Session: "sess-kettle-1", Time: now.Add(-10 * time.Hour),
			Summary: fmt.Sprint("summary of choice ", i), Text: fmt.Sprint("owner words ", i)}})
	}
	var b strings.Builder
	writeReport(&b, c, "48h", "4h", nil)
	got := b.String()
	for _, bad := range []string{"summary of choice", "owner words", "sess-ket", "kettle", "\"", "- missing"} {
		if strings.Contains(got, bad) {
			t.Errorf("report shows %q:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "8 decisions found: 0 landed, 8 missing, 0 ?") || !strings.Contains(got, "details: landed missing --since 48h") {
		t.Errorf("count line or details pointer missing:\n%s", got)
	}
}

// Every failure makes the run exit non-zero: unclassified turns and rows whose
// sources could not be read, not only model errors. Too few keywords is a
// limit of the decision, not an error.
func TestRunErrors(t *testing.T) {
	row := func(why string) landing.Check { return landing.Check{State: landing.Unknown, Why: why} }
	if err := runErrors(checked{Checks: []landing.Check{row("fewer than 2 usable keyword words to match on (lid)")}}, nil); err != nil {
		t.Errorf("too few keywords is not an error: %v", err)
	}
	if err := runErrors(checked{Checks: []landing.Check{row("could not read git: x; searched 2 bus lines")}}, nil); err == nil || !strings.Contains(err.Error(), "1 rows: a source could not be read") {
		t.Errorf("unread source: %v", err)
	}
	if err := runErrors(checked{Unknown: []decide.Result{{Err: "model gave no answer"}}}, nil); err == nil || !strings.Contains(err.Error(), "1 turns could not be classified") {
		t.Errorf("unclassified turn: %v", err)
	}
}
