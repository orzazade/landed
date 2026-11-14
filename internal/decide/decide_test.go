package decide

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orzazade/landed/internal/store"
	"github.com/orzazade/landed/internal/transcript"
)

func TestRules(t *testing.T) {
	cases := []struct {
		text, prev string
		queue      bool
		want       Call
	}{
		{"from now on always log boil events", "", false, Unclear}, // markers are a hint; the model decides
		{"never ship without the lid sensor", "", false, Unclear},
		{"go with the blue lid", "", true, Decision},
		{"blue", "Blue or red lid?", true, Decision},
		{"yes", "Shall I merge the timer branch?", false, Decision},
		{"ok", "Want me to deploy now? ❓", false, Decision},
		{"option 2", "Option 1 keeps it, option 2 drops it. Which?", false, Decision},
		{"ok", "Deployed and checked.", false, Not},
		{"thanks", "", false, Not},
		{"what does the boil sensor report?", "", false, Not},
		{"why is the test red?", "", false, Not},
		{"what if we always keep the lid closed?", "", false, Unclear}, // a question with a marker: the model decides
		{"should we move the whole timer module into its own package before the release?", "", false, Unclear},
		{"the timer page loads slowly on the tablet", "", false, Unclear},
		{"add a cooldown timer to the kettle app", "", false, Unclear},
	}
	for _, c := range cases {
		got, why := Rule(transcript.Turn{Text: c.text, PrevAssistant: c.prev, Queue: c.queue})
		if got != c.want {
			t.Errorf("Rule(%q, prev %q) = %v (%s), want %v", c.text, c.prev, got, why, c.want)
		}
	}
}

type fakeModel struct {
	calls   int
	answers map[string]Answer
	err     error
}

func (f *fakeModel) Classify(_ context.Context, items []Item) ([]Answer, error) {
	f.calls++
	var out []Answer
	for _, it := range items {
		if a, ok := f.answers[it.ID]; ok {
			a.ID = it.ID
			out = append(out, a)
		}
	}
	return out, f.err
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "l.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestFindRulesModelAndCache(t *testing.T) {
	st := openStore(t)
	turns := []transcript.Turn{
		{UUID: "r", Text: "never ship without the lid sensor", Queue: true},
		{UUID: "q", Text: "why is the test red?"},
		{UUID: "u1", Text: "add a cooldown timer to the kettle app"},
		{UUID: "u2", Text: "the timer page loads slowly on the tablet"},
	}
	m := &fakeModel{answers: map[string]Answer{
		"r":  {Decision: false, Summary: "lid sensor required before shipping", Keywords: []string{"lid", "sensor"}},
		"u1": {Decision: true, Summary: "build a cooldown timer", Keywords: []string{"cooldown", "timer"}},
		"u2": {Decision: false},
	}}
	res, stats := Find(context.Background(), turns, st, m, 10)
	if len(stats.Errors) != 0 {
		t.Fatal(stats.Errors)
	}
	want := map[string]bool{"r": true, "q": false, "u1": true, "u2": false}
	for _, r := range res {
		if !r.Known || r.Verdict.Decision != want[r.Turn.UUID] {
			t.Errorf("%s: known=%v decision=%v source=%s", r.Turn.UUID, r.Known, r.Verdict.Decision, r.Verdict.Source)
		}
	}
	if res[0].Verdict.Summary != "lid sensor required before shipping" {
		t.Errorf("rule decision lost the model's summary: %+v", res[0].Verdict)
	}
	if m.calls != 1 || stats.ByModel != 2 || stats.ByRule != 2 {
		t.Errorf("calls=%d byModel=%d byRule=%d", m.calls, stats.ByModel, stats.ByRule)
	}
	// Second run: everything comes from the cache, the model is not called.
	_, stats2 := Find(context.Background(), turns, st, m, 10)
	if m.calls != 1 || stats2.Cached != 4 {
		t.Errorf("second run: calls=%d cached=%d", m.calls, stats2.Cached)
	}
}

func TestFindModelFailureIsUnknownNotCached(t *testing.T) {
	st := openStore(t)
	turns := []transcript.Turn{
		{UUID: "u1", Text: "add a cooldown timer to the kettle app"},
		{UUID: "u2", Text: "the timer page loads slowly on the tablet"},
	}
	// The model answers u1 only and reports an error for the batch.
	m := &fakeModel{answers: map[string]Answer{"u1": {Decision: true}}, err: errors.New("model skipped 1 of 2 items")}
	res, stats := Find(context.Background(), turns, st, m, 10)
	if len(stats.Errors) != 1 || stats.Unknown != 1 {
		t.Fatalf("errors=%v unknown=%d", stats.Errors, stats.Unknown)
	}
	if !res[0].Known || !res[0].Verdict.Decision {
		t.Errorf("u1 = %+v", res[0])
	}
	if res[1].Known || res[1].Err == "" {
		t.Errorf("u2 must be unknown with a reason: %+v", res[1])
	}
	if _, ok, _ := st.Verdict("u2", PromptVersion); ok {
		t.Errorf("a failed call must not be cached")
	}
}

func TestFindWithoutModel(t *testing.T) {
	st := openStore(t)
	res, stats := Find(context.Background(), []transcript.Turn{
		{UUID: "r", Text: "never ship without the lid sensor", Queue: true},
		{UUID: "u", Text: "the timer page loads slowly on the tablet"},
	}, st, nil, 10)
	if !res[0].Known || !res[0].Verdict.Decision || res[1].Known || stats.Unknown != 1 {
		t.Errorf("res=%+v stats=%+v", res, stats)
	}
}

func TestParseAnswers(t *testing.T) {
	items := []Item{{ID: "a"}, {ID: "b"}}
	got, err := parseAnswers("```json\n[{\"id\":\"a\",\"decision\":true,\"kind\":\"rule\"},{\"id\":\"b\",\"decision\":false}]\n```", items)
	if err != nil || len(got) != 2 || !got[0].Decision {
		t.Errorf("fenced: %v %v", got, err)
	}
	if _, err := parseAnswers(`[{"id":"a","decision":true}]`, items); err == nil {
		t.Errorf("a skipped item must be an error")
	}
	if _, err := parseAnswers("I think yes", items); err == nil {
		t.Errorf("prose must be an error")
	}
}

func TestCLIEnvDropsAPIKeys(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-made-up")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "made-up")
	t.Setenv("ANTHROPIC_BASE_URL", "https://example.invalid")
	t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
	for _, kv := range cliEnv() {
		if strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "CLAUDE_CODE_USE_") {
			t.Errorf("child env keeps %s", strings.SplitN(kv, "=", 2)[0])
		}
	}
}

// skipOnce answers everything except the given ids on the first call.
type skipOnce struct {
	fakeModel
	skip map[string]bool
}

func (f *skipOnce) Classify(ctx context.Context, items []Item) ([]Answer, error) {
	f.calls++
	var out []Answer
	for _, it := range items {
		if f.calls == 1 && f.skip[it.ID] {
			continue
		}
		out = append(out, Answer{ID: it.ID, Decision: true})
	}
	if len(out) < len(items) {
		return out, errors.New("model skipped items")
	}
	return out, nil
}

func TestFindRetriesSkippedItems(t *testing.T) {
	st := openStore(t)
	m := &skipOnce{skip: map[string]bool{"u2": true}}
	res, stats := Find(context.Background(), []transcript.Turn{
		{UUID: "u1", Text: "add a cooldown timer to the kettle app"},
		{UUID: "u2", Text: "the timer page loads slowly on the tablet"},
	}, st, m, 10)
	if m.calls != 2 || len(stats.Errors) != 0 || !res[1].Known {
		t.Errorf("calls=%d errors=%v u2=%+v", m.calls, stats.Errors, res[1])
	}
}

func TestReadStreamRefusesAPIKey(t *testing.T) {
	ok := `{"type":"system","subtype":"init","apiKeySource":"none"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"[]"}`
	if r, err := readStream([]byte(ok)); err != nil || r != "[]" {
		t.Errorf("max login: %q %v", r, err)
	}
	key := `{"type":"system","subtype":"init","apiKeySource":"ANTHROPIC_API_KEY"}` + "\n" + `{"type":"result","is_error":false,"result":"[]"}`
	if _, err := readStream([]byte(key)); err == nil || !strings.Contains(err.Error(), "API key") {
		t.Errorf("API key run not refused: %v", err)
	}
	if _, err := readStream([]byte(`{"type":"result","is_error":false,"result":"[]"}`)); !errors.Is(err, ErrAPIKey) {
		t.Errorf("missing init must stop the run like an API key: %v", err)
	}
}

func TestDecisionWithoutKeywordsIsNotCached(t *testing.T) {
	st := openStore(t)
	turns := []transcript.Turn{{UUID: "u1", Text: "add a cooldown timer to the kettle app"}}
	m := &fakeModel{answers: map[string]Answer{"u1": {Decision: true, Keywords: []string{"timer"}}}}
	res, _ := Find(context.Background(), turns, st, m, 10)
	if !res[0].Known || !res[0].Verdict.Decision {
		t.Fatalf("got %+v", res[0])
	}
	if _, ok, _ := st.Verdict("u1", PromptVersion); ok {
		t.Errorf("a decision with one keyword was cached; it would never get matchable keywords")
	}
}

func TestRuleDecisionSurvivesModelFailure(t *testing.T) {
	st := openStore(t)
	m := &fakeModel{err: errors.New("timeout")}
	res, _ := Find(context.Background(), []transcript.Turn{{UUID: "q", Text: "go with the blue lid", Queue: true}}, st, m, 10)
	if !res[0].Known || !res[0].Verdict.Decision {
		t.Errorf("queue answer lost when the model failed: %+v", res[0])
	}
}

func TestParseJudgmentsRefusesBadIndex(t *testing.T) {
	items := []JudgeItem{{ID: "a", Candidates: []string{"0: x"}}, {ID: "b", Candidates: []string{"0: y"}}}
	got, err := parseJudgments("```json\n[{\"id\":\"a\",\"record\":0,\"why\":\"w\"},{\"id\":\"b\",\"record\":3,\"why\":\"w\"}]\n```", items)
	if err == nil || len(got) != 1 || got[0].ID != "a" {
		t.Errorf("got %v, %v; want only a and an error", got, err)
	}
}

// After an API-key refusal no further batch is sent.
func TestFindStopsAtAPIKeyRefusal(t *testing.T) {
	st := openStore(t)
	var turns []transcript.Turn
	for _, id := range []string{"a", "b", "c", "d"} {
		turns = append(turns, transcript.Turn{UUID: id, Text: "maybe we paint the kettle in some shade we like"})
	}
	m := &fakeModel{err: fmt.Errorf("%w (apiKeySource %q)", ErrAPIKey, "ANTHROPIC_API_KEY")}
	res, stats := Find(context.Background(), turns, st, m, 1)
	if m.calls != 1 {
		t.Errorf("model called %d times after an API-key refusal; want 1", m.calls)
	}
	for _, r := range res {
		if r.Known {
			t.Errorf("%s known after refusal", r.Turn.UUID)
		}
	}
	if len(stats.Errors) != 4 {
		t.Errorf("errors %d, want one per batch (4)", len(stats.Errors))
	}
}

func TestParseAnswersNeedsDecisionField(t *testing.T) {
	items := []Item{{ID: "a"}, {ID: "b"}}
	got, err := parseAnswers(`[{"id":"a","decision":false,"summary":"","keywords":[]},{"id":"b","summary":"x"}]`, items)
	if err == nil || len(got) != 1 || got[0].ID != "a" {
		t.Errorf("got %v, %v; want only a and an error", got, err)
	}
}

func TestParseJudgmentsNeedsRecordField(t *testing.T) {
	items := []JudgeItem{{ID: "a", Candidates: []string{"0: x"}}}
	if got, err := parseJudgments(`[{"id":"a","why":"w"}]`, items); err == nil || len(got) != 0 {
		t.Errorf("missing record read as %v, %v", got, err)
	}
}

// A decision must say its kind: a one-off action or a lasting rule. One
// without a known kind is not an answer (it is asked again), never a guess.
func TestParseAnswersNeedsKindForDecisions(t *testing.T) {
	items := []Item{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}
	got, err := parseAnswers(`[{"id":"a","decision":true,"kind":"action"},{"id":"b","decision":true},{"id":"c","decision":true,"kind":"forever"},{"id":"d","decision":false}]`, items)
	if err == nil || len(got) != 2 || got[0].Kind != "action" || got[1].ID != "d" {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestFindKeepsKind(t *testing.T) {
	st := openStore(t)
	turns := []transcript.Turn{{UUID: "a", Text: "review the kettle ui now"}, {UUID: "b", Text: "from now on the lid sensor is required"}}
	m := &fakeModel{answers: map[string]Answer{
		"a": {Decision: true, Kind: "action", Summary: "review the ui", Keywords: []string{"kettle", "ui"}},
		"b": {Decision: true, Kind: "rule", Summary: "lid sensor required", Keywords: []string{"lid", "sensor"}},
	}}
	Find(context.Background(), turns, st, m, 10)
	res, stats := Find(context.Background(), turns, st, m, 10) // from the cache
	if stats.Cached != 2 || res[0].Verdict.Kind != "action" || res[1].Verdict.Kind != "rule" {
		t.Errorf("cached=%d kinds %q %q", stats.Cached, res[0].Verdict.Kind, res[1].Verdict.Kind)
	}
}
