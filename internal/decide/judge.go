package decide

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// JudgeVersion is part of every cached judgment's and pick check's key:
// changing the judge or the check prompt (verify.go) means bumping it.
const JudgeVersion = "j8" // j7: each pick is checked on its own (Verify); j8: a refused pick is ruled out and judged again

const judgePrompt = `You check whether a solo developer's decision, made in a chat with an AI coding agent, was written down or carried out afterwards. Each item has the decision ("decision": a one-line summary, "words": what the developer typed) and numbered candidate records made after it: git commits, edits of notes or status files, and coordination-bus lines. Pick the candidate that records or carries out THIS decision: the same specific choice, rule or piece of work, not just the same topic or project. A commit that implements the decided work counts; a status note or bus line counts only when it states the decided choice itself (what will or must be done, or what was dropped). Text that only describes the current setup, lists the topic, or recommends something else does not count. Candidates of kind "acted" are what the agent did right after the decision (commands, file edits): pick one when it carries the decision out (does the decided work, applies the setting, files the ticket), not when it only reads, searches or asks. If no candidate does, answer -1.
Also say whether a candidate REPLACES or WITHDRAWS this decision ("replaced"): candidates of kind "decision" are later decisions by the same developer; one replaces this decision only when it settles the same specific question differently (a new number, a different option, the item dropped or declared out of scope). A note, commit or bus line replaces it only when it says this choice is withdrawn, cancelled, reverted or superseded. A later decision that agrees with, extends or merely mentions this one does not replace it. If none does, answer -1.
Reply with ONLY a JSON array, one object per item, in input order, no backslashes in "why": [{"id":"...","record":<candidate number or -1>,"replaced":<candidate number or -1>,"why":"<max 12 words>"}]`

// JudgeItem is one decision with its candidate records.
type JudgeItem struct {
	ID         string   `json:"id"`
	Decision   string   `json:"decision"`
	Words      string   `json:"words"`
	Candidates []string `json:"candidates"` // "0: commit kettle 1a2b3c4 … | text"
}

type JudgeAnswer struct {
	ID       string `json:"id"`
	Record   int    `json:"record"`
	Replaced int    `json:"replaced"` // candidate that replaces or withdraws the decision, or -1
	Why      string `json:"why"`
}

// Judge says which candidate, if any, records each decision.
type Judge interface {
	Judge(ctx context.Context, items []JudgeItem) ([]JudgeAnswer, error)
}

func (c ClaudeCLI) Judge(ctx context.Context, items []JudgeItem) ([]JudgeAnswer, error) {
	in, _ := json.Marshal(items)
	result, err := c.run(ctx, judgePrompt, in)
	if err != nil {
		return nil, err
	}
	return parseJudgments(result, items)
}

// parseJudgments reads the reply and checks every item got an answer whose
// record number is -1 or one of its candidates.
func parseJudgments(raw string, items []JudgeItem) ([]JudgeAnswer, error) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "["); i > 0 {
		raw = raw[i:]
	}
	if j := strings.LastIndex(raw, "]"); j >= 0 && j < len(raw)-1 {
		raw = raw[:j+1]
	}
	var raws []struct {
		ID       string `json:"id"`
		Record   *int   `json:"record"`
		Replaced *int   `json:"replaced"`
		Why      string `json:"why"`
	}
	if err := json.Unmarshal([]byte(raw), &raws); err != nil {
		return nil, fmt.Errorf("judge reply is not a JSON array: %w", err)
	}
	byID := map[string]JudgeAnswer{}
	for _, a := range raws {
		if a.Record != nil { // no "record": not an answer, never candidate 0
			r := -1 // no "replaced": nothing replaces it, the row stays as found
			if a.Replaced != nil {
				r = *a.Replaced
			}
			byID[a.ID] = JudgeAnswer{ID: a.ID, Record: *a.Record, Replaced: r, Why: a.Why}
		}
	}
	out := make([]JudgeAnswer, 0, len(items))
	var bad []string
	for _, it := range items {
		a, ok := byID[it.ID]
		if !ok || a.Record < -1 || a.Record >= len(it.Candidates) || a.Replaced < -1 || a.Replaced >= len(it.Candidates) {
			bad = append(bad, it.ID)
			continue
		}
		out = append(out, a)
	}
	if len(bad) > 0 {
		return out, fmt.Errorf("judge skipped or misnumbered %d of %d items", len(bad), len(items))
	}
	return out, nil
}
