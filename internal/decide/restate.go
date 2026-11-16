package decide

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// RestateVersion is part of every cached restatement answer's key.
const RestateVersion = "r1"

const restatePrompt = `You read decisions a solo developer made in chats with AI coding agents. Each item has a later message of the developer ("later") and numbered earlier decisions of the same developer ("earlier"). Pick the earlier decision that the later message states AGAIN: the same specific rule or choice, said a second time (for example "again: never X", "I told you, use Y", or the same rule simply restated). Not a restatement: a new or changed choice on the same topic, the next step of the same work, another detail of the same project, a question, or a mere mention. If none is restated, answer -1.
Reply with ONLY a JSON array, one object per item, in input order, no backslashes in "why": [{"id":"...","restates":<earlier number or -1>,"why":"<max 12 words>"}]`

// RestateItem is one later decision with the earlier ones it may restate.
type RestateItem struct {
	ID      string   `json:"id"`
	Later   string   `json:"later"`
	Earlier []string `json:"earlier"` // "0: 2026-11-02 | summary | words"
}

type RestateAnswer struct {
	ID       string `json:"id"`
	Restates int    `json:"restates"` // index into Earlier, or -1
	Why      string `json:"why"`
}

// Restater says which earlier decision, if any, a later message restates.
type Restater interface {
	Restate(ctx context.Context, items []RestateItem) ([]RestateAnswer, error)
}

var _ Restater = ClaudeCLI{}

func (c ClaudeCLI) Restate(ctx context.Context, items []RestateItem) ([]RestateAnswer, error) {
	in, _ := json.Marshal(items)
	result, err := c.run(ctx, restatePrompt, in)
	if err != nil {
		return nil, err
	}
	return parseRestate(result, items)
}

func parseRestate(raw string, items []RestateItem) ([]RestateAnswer, error) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "["); i > 0 {
		raw = raw[i:]
	}
	if j := strings.LastIndex(raw, "]"); j >= 0 && j < len(raw)-1 {
		raw = raw[:j+1]
	}
	var raws []struct {
		ID       string `json:"id"`
		Restates *int   `json:"restates"`
		Why      string `json:"why"`
	}
	if err := json.Unmarshal([]byte(raw), &raws); err != nil {
		return nil, fmt.Errorf("restate reply is not a JSON array: %w", err)
	}
	byID := map[string]RestateAnswer{}
	for _, a := range raws {
		if a.Restates != nil {
			byID[a.ID] = RestateAnswer{ID: a.ID, Restates: *a.Restates, Why: a.Why}
		}
	}
	out := make([]RestateAnswer, 0, len(items))
	bad := 0
	for _, it := range items {
		a, ok := byID[it.ID]
		if !ok || a.Restates < -1 || a.Restates >= len(it.Earlier) {
			bad++
			continue
		}
		out = append(out, a)
	}
	if bad > 0 {
		return out, fmt.Errorf("restate skipped or misnumbered %d of %d items", bad, len(items))
	}
	return out, nil
}
