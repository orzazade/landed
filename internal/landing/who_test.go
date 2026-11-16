package landing

import (
	"testing"
	"time"

	"github.com/orzazade/landed/internal/transcript"
)

// Who names the session whose action made a landing, so `landed proof` can
// tell whether the session that saw a warning is the one that landed it.
func TestWho(t *testing.T) {
	at := func(m int) time.Time { return time.Date(2026, 11, 2, 12, m, 0, 0, time.UTC) }
	acts := []transcript.Action{
		{Session: "s-edit", Time: at(5), Tool: "Edit", Path: "/p/kettle/AGENTS.md", Text: "Settings live in TOML."},
		{Session: "s-other", Time: at(6), Tool: "Bash", Command: "git status"},
		{Session: "s-commit", Time: at(10), Tool: "Bash", Command: "git commit -m \"$(cat <<'EOF'\nMove kettle settings to TOML\n\nbody\nEOF\n)\""},
		{Session: "s-bus", Time: at(20), Tool: "Bash", Command: `bus-post ~/bus/kettle.log DECISION kettle:lead kettle:dev "settings stay in TOML from now on"`},
		{Session: "s-note", Time: at(30), Tool: "Write", Path: "/notes/kettle.md", Text: "TOML"},
		{Session: "s-late", Time: at(50), Tool: "Bash", Command: "git commit -m 'Move kettle settings to TOML'"},
	}
	for _, c := range []struct {
		name string
		e    Evidence
		want string
	}{
		{"edit by time and path", Evidence{Kind: "edit", Ref: "/p/kettle/AGENTS.md", When: at(5)}, "s-edit"},
		{"acted", Evidence{Kind: "acted", Ref: "Edit /p/kettle/AGENTS.md", When: at(5)}, "s-edit"},
		{"commit by subject, closest before", Evidence{Kind: "commit", Ref: "kettle 1a2b3c4 Move kettle settings to TOML", When: at(10).Add(20 * time.Second)}, "s-commit"},
		{"commit nobody ran", Evidence{Kind: "commit", Ref: "kettle 9f9f9f9 Something else", When: at(10)}, ""},
		{"bus line by its text", Evidence{Kind: "bus", Ref: "kettle.log 2026-11-02T12:20 DECISION kettle:lead -> kettle:dev — settings stay in TOML from now on", When: at(20)}, "s-bus"},
		{"new note by its writer", Evidence{Kind: "note", Ref: "/notes/kettle.md (new)", When: at(30).Add(2 * time.Second)}, "s-note"},
		{"changed note: its time may be an unrelated edit", Evidence{Kind: "note", Ref: "/notes/kettle.md (changed)", When: at(30).Add(2 * time.Second)}, ""},
		{"a later decision is no landing", Evidence{Kind: "decision", Ref: "session s-edit x", When: at(5)}, ""},
	} {
		if got := Who(acts, c.e); got != c.want {
			t.Errorf("%s: Who = %q, want %q", c.name, got, c.want)
		}
	}
}
