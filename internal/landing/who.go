package landing

import (
	"strings"
	"time"

	"github.com/orzazade/landed/internal/transcript"
)

// Who returns the session whose action made landing e, or "" when no action
// read shows it (a commit made outside Claude Code, a bus line posted by a
// script). acts are every session's actions, sorted by time.
//
// edit and acted rows come from an action, so time and path name it. A
// commit is matched by its subject in a `git commit` command run shortly
// before it, a bus line by its message in a bus-post command, a new note by
// the edit that created it. A changed note has no known session.
func Who(acts []transcript.Action, e Evidence) string {
	switch e.Kind {
	case "edit", "acted":
		for _, a := range acts {
			if a.Time.Equal(e.When) && strings.HasSuffix(e.Ref, refOf(a)) {
				return a.Session
			}
		}
	case "commit":
		parts := strings.SplitN(e.Ref, " ", 3) // repo, hash, subject
		if len(parts) < 3 {
			return ""
		}
		return closest(acts, e.When.Add(-15*time.Minute), e.When.Add(2*time.Minute), func(a transcript.Action) bool {
			return a.Tool == "Bash" && strings.Contains(a.Command, "commit") && containsText(a.Command, parts[2])
		})
	case "bus":
		msg := e.Ref
		if _, after, ok := strings.Cut(e.Ref, " — "); ok {
			msg = after
		}
		return closest(acts, e.When.Add(-2*time.Minute), e.When.Add(2*time.Minute), func(a transcript.Action) bool {
			return a.Tool == "Bash" && strings.Contains(a.Command, "bus-post") && containsText(a.Command, msg)
		})
	case "note":
		path, how, _ := strings.Cut(e.Ref, " (")
		if how != "new)" {
			// A changed note's time is its last change, which may be an
			// unrelated later edit: who wrote the decision into it is unknown.
			return ""
		}
		return closest(acts, e.When.Add(-15*time.Minute), e.When.Add(time.Minute), func(a transcript.Action) bool {
			return a.Path == path
		})
	}
	return ""
}

// refOf is how Gather names an action in Evidence.Ref.
func refOf(a transcript.Action) string {
	if a.Tool == "Bash" {
		return "shell: " + firstLine(a.Command, 80)
	}
	return a.Path
}

// closest is the session of the matching action nearest to the end of
// [from, to]; actions run before what they make.
func closest(acts []transcript.Action, from, to time.Time, ok func(transcript.Action) bool) string {
	who := ""
	for _, a := range acts {
		if a.Time.Before(from) || a.Time.After(to) || !ok(a) {
			continue
		}
		who = a.Session // sorted by time: the last match is the nearest
	}
	return who
}

// containsText: cmd holds the first 40 characters of text, spaces folded.
func containsText(cmd, text string) bool {
	t := strings.Join(strings.Fields(text), " ")
	t = strings.TrimSuffix(t, "…")
	if r := []rune(t); len(r) > 40 {
		t = string(r[:40])
	}
	return t != "" && strings.Contains(strings.Join(strings.Fields(cmd), " "), t)
}

// Shared returns the words of kws that text shares (as a landing check
// matches them), and how many usable words kws has.
func Shared(kws []string, text string) (shared []string, usable int) {
	m := matcher(kws, "")
	return m.match(text), len(m.words)
}
