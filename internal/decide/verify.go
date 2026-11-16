package decide

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const verifyPrompt = `You check one claim per item: that a record made after a developer's decision (a git commit, a note or status file edit, a coordination-bus line, or an action the agent took) states or carries out THAT decision. Each item has the decision ("decision": a one-line summary, "words": what the developer typed) and the one record ("record"). Answer "records": true only when the record states the same specific choice or does the decided work. Answer false when the record states a different or opposite choice on the same topic, only describes the topic or the current setup, recommends something else, is research or a draft for later, or belongs to another project or task. When true, copy into "quote" the exact words of the record (max 25 words) that state the choice or do the work; when false, "quote" is "".
Reply with ONLY a JSON array, one object per item, in input order, no backslashes: [{"id":"...","records":true|false,"quote":"...","why":"<max 12 words>"}]`

// VerifyItem is one judge pick to check on its own.
type VerifyItem struct {
	ID       string `json:"id"`
	Decision string `json:"decision"`
	Words    string `json:"words"`
	Record   string `json:"record"`
}

type VerifyAnswer struct {
	ID      string `json:"id"`
	Records bool   `json:"records"`
	Quote   string `json:"quote"`
	Why     string `json:"why"`
}

// Verifier checks a judge's pick in a call of its own, so the pick is not
// read in the light of the other candidates.
type Verifier interface {
	Verify(ctx context.Context, items []VerifyItem) ([]VerifyAnswer, error)
}

var _ Verifier = ClaudeCLI{}

func (c ClaudeCLI) Verify(ctx context.Context, items []VerifyItem) ([]VerifyAnswer, error) {
	in, _ := json.Marshal(items)
	result, err := c.run(ctx, verifyPrompt, in)
	if err != nil {
		return nil, err
	}
	return parseVerify(result, items)
}

func parseVerify(raw string, items []VerifyItem) ([]VerifyAnswer, error) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "["); i > 0 {
		raw = raw[i:]
	}
	if j := strings.LastIndex(raw, "]"); j >= 0 && j < len(raw)-1 {
		raw = raw[:j+1]
	}
	var raws []struct {
		ID      string `json:"id"`
		Records *bool  `json:"records"`
		Quote   string `json:"quote"`
		Why     string `json:"why"`
	}
	if err := json.Unmarshal([]byte(raw), &raws); err != nil {
		return nil, fmt.Errorf("verify reply is not a JSON array: %w", err)
	}
	byID := map[string]VerifyAnswer{}
	for _, a := range raws {
		if a.Records != nil { // no "records": not an answer
			byID[a.ID] = VerifyAnswer{ID: a.ID, Records: *a.Records, Quote: a.Quote, Why: a.Why}
		}
	}
	out := make([]VerifyAnswer, 0, len(items))
	skipped := 0
	for _, it := range items {
		if a, ok := byID[it.ID]; ok {
			out = append(out, a)
		} else {
			skipped++
		}
	}
	if skipped > 0 {
		return out, fmt.Errorf("verify skipped %d of %d items", skipped, len(items))
	}
	return out, nil
}
