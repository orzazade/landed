package main

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"time"
)

// cmdTurns prints owner turns as JSON lines, for local inspection and labelling.
// Its output holds private text: never redirect it into the repo.
func cmdTurns(args []string) error {
	fs := flag.NewFlagSet("turns", flag.ExitOnError)
	since := fs.String("since", "14d", "only turns newer than this age")
	project := fs.String("project", "", "only sessions whose cwd contains this text")
	injected := fs.Bool("injected", false, "print the turns filtered out as tool-typed instead")
	var roots rootList
	fs.Var(&roots, "root", "transcript root (repeatable)")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	age, err := parseAge(*since)
	if err != nil {
		return err
	}
	cut := time.Now().Add(-age)
	l := load(roots, cut)
	enc := json.NewEncoder(os.Stdout)
	for _, s := range l.Sessions {
		if s.Headless || (*project != "" && !strings.Contains(s.Cwd, *project)) {
			continue
		}
		list := s.Turns
		if *injected {
			list = s.Injected
		}
		for _, t := range list {
			if t.Time.Before(cut) {
				continue
			}
			if err := enc.Encode(map[string]any{
				"id": t.UUID, "session": s.ID, "cwd": s.Cwd, "time": t.Time.Format(time.RFC3339),
				"queue": t.QueueID, "text": t.Text, "prev": t.PrevAssistant,
			}); err != nil {
				return err
			}
		}
	}
	return l.report()
}
