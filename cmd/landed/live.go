package main

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/orzazade/landed/internal/landing"
	"github.com/orzazade/landed/internal/proof"
	"github.com/orzazade/landed/internal/transcript"
)

// proofPath is the live log of warnings and landings, next to the database.
func proofPath(o checkOpts) string { return proof.Path(filepath.Dir(o.db)) }

// logWarns writes one warning line per shown row. session is the session
// the rows were shown in ("" outside one); shown[i] is the text printed for
// rows[i].
func logWarns(path, where, session string, rows []landing.Check, shown []string, now time.Time) error {
	evs := make([]proof.Event, len(rows))
	for i, ch := range rows {
		evs[i] = proof.Event{At: now, Event: proof.Warn, Where: where, Session: session,
			Decision: ch.Decision.ID, DecisionSession: ch.Decision.Session, Shown: shown[i]}
	}
	return proof.Append(path, evs)
}

// sayNotRecorded is the one line that tells a failed log read or write.
func sayNotRecorded(w io.Writer, err error) {
	if err != nil {
		fmt.Fprintf(w, "landed could not record to its proof log: %s\n", quote(err.Error(), 300))
	}
}

// recordLandings writes a landed line for each warned decision this run
// finds landed for the first time: the landing, the session that made it,
// and whether that session had seen a warning for it before then.
func recordLandings(path string, checks []landing.Check, acts []transcript.Action, now time.Time) error {
	evs, _, err := proof.Read(path) // bad lines: landed proof counts them
	if err != nil || len(evs) == 0 {
		return err
	}
	warns := map[string][]proof.Event{}
	done := map[string]bool{}
	for _, e := range evs {
		switch e.Event {
		case proof.Warn:
			warns[e.Decision] = append(warns[e.Decision], e)
		case proof.Landed:
			done[e.Decision] = true
		}
	}
	var add []proof.Event
	for _, ch := range checks {
		id := ch.Decision.ID
		if ch.State != landing.Landed || ch.Evidence == nil || done[id] || len(warns[id]) == 0 {
			continue
		}
		ev := *ch.Evidence
		who := landing.Who(acts, ev)
		verdict, _ := proof.Judge(warns[id], who, ev.When)
		add = append(add, proof.Event{At: now, Event: proof.Landed, Session: who, Decision: id, DecisionSession: ch.Decision.Session,
			Kind: ev.Kind, Ref: ev.Ref, When: ev.When, WarnedBefore: verdict == proof.Caused})
		done[id] = true
	}
	return proof.Append(path, add)
}
