// Package decide finds the owner turns that are decisions: cheap rules first,
// a small model only for the turns the rules cannot settle.
package decide

import (
	"regexp"
	"strings"

	"github.com/orzazade/landed/internal/store"
	"github.com/orzazade/landed/internal/transcript"
)

// Call is what the rules say about one turn.
type Call int

const (
	Unclear  Call = iota // ask the model
	Decision             // a decision for sure; the model only writes summary and keywords
	Not                  // not a decision for sure; the model is not asked
)

func (c Call) String() string {
	return [...]string{"unclear", "decision", "not"}[c]
}

var (
	// Words that make a rule, a choice or a go-ahead.
	markerRe = regexp.MustCompile(`(?i)\b(?:from now on|going forward|always|never|don'?t|do not|stop (?:doing|using)|go ahead|do it|go with|ship it|park (?:it|this|that)|drop (?:it|this|that)|instead of|make sure|approved?|let'?s go|yes,? (?:do|go|please))\b`)

	// Short answers that grant or choose, when the assistant just asked something.
	answerRe = regexp.MustCompile(`(?i)^(?:yes|yep|yeah|y|go|ok|okay|sure|do it|approved?|agreed|both|option \w+|\d+|[a-d]|first|second|the (?:first|second) one|proceed|continue with \S+)\b`)

	// A question with no marker: asking, not deciding.
	questionStartRe = regexp.MustCompile(`(?i)^(?:what|why|how|where|which|who|when|is|are|does|do you|did|can|could|would|will|any|have|has|was|were)\b`)

	// Acknowledgements and nudges that choose nothing.
	ackRe = regexp.MustCompile(`(?i)^(?:ok|okay|k|thanks|thank you|thx|nice|cool|great|good|hi|hello|ping|status\??|continue|go on|\?+)[.!]*$`)
)

// asked reports whether the assistant's last message ended on a question.
func asked(prev string) bool {
	if prev == "" {
		return false
	}
	end := prev
	if len(end) > 400 {
		end = end[len(end)-400:]
	}
	return strings.Contains(end, "?") || strings.Contains(end, "❓")
}

// Rule calls one turn without a model. reason names the rule that fired.
func Rule(t transcript.Turn) (Call, string) {
	text := strings.TrimSpace(t.Text)
	words := len(strings.Fields(text))
	isQuestion := strings.Contains(text, "?") && questionStartRe.MatchString(text)
	switch {
	case t.Queue && !strings.HasSuffix(text, "?"):
		return Decision, "queue-answer"
	case words <= 8 && asked(t.PrevAssistant) && answerRe.MatchString(text) && !isQuestion:
		return Decision, "short-answer"
	case ackRe.MatchString(text):
		return Not, "ack"
	case isQuestion && words <= 12 && !markerRe.MatchString(text):
		return Not, "question"
	}
	return Unclear, ""
}

// Lasting reports whether a verdict is a decision worth recording: a rule or
// lasting choice, or any answer from the owner queue. One-off actions ("check
// it", "give me the url") are not: their result is the work itself. A
// decision with no kind is read as lasting, the careful side.
func Lasting(v store.Verdict) bool {
	return v.Decision && (v.Kind != "action" || v.Source == "rule:queue-answer")
}
