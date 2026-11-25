package decide

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Item is one turn sent to the model.
type Item struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Prev string `json:"prev,omitempty"` // end of the assistant message before it
}

// Answer is the model's call for one item.
type Answer struct {
	ID       string   `json:"id"`
	Decision bool     `json:"decision"`
	Kind     string   `json:"kind"` // decisions only: "action" (one-off, done now) or "rule" (lasting)
	Summary  string   `json:"summary"`
	Keywords []string `json:"keywords"`
}

// Model classifies a batch of turns.
type Model interface {
	Classify(ctx context.Context, items []Item) ([]Answer, error)
}

// PromptVersion is part of every cached verdict's key: changing the prompt or
// the rules means bumping it, so old verdicts are not reused.
const PromptVersion = "v9"

const systemPrompt = `You label messages a solo developer typed to AI coding agents. For each message decide whether it contains a DECISION: the developer chooses or commits to something that should persist beyond the chat. Decisions: approval to go ahead with a proposed plan ("go", "yes" to a proposal), a choice between options, a rule or preference ("from now on", "never", "always", "don't"), a scope change (drop/park/add something), a priority, an assignment of work, or a request that starts real new work (build X, fix bug Y). NOT decisions: questions, requests for information or status, debugging chatter, complaints with no choice, greetings, acknowledgements that choose nothing, pasted logs or errors with no instruction, tiny one-off commands that need no record (run the tests, open the page), one-off permission for a single action (approve this sign-in, this command), restart or resume orders that only say to carry on with existing work (after an outage, a crash, a break), the developer reporting what they did or are doing themselves ("I posted it", "I approved it", "FYI I'm also researching X"), and asking to discuss or talk something through. Read "prev" (the agent's message just before) to understand short answers.
For each decision write "kind": "action" when it is a one-off request carried out now, whose result is the work itself: run, check, review or explain something, post or edit a comment, move an account, thread or tab, open or close sessions, stop or restart work, build or fix this now. A short answer that approves, picks or adjusts the step the agent just proposed ("go", "2", "yes do both", "use the browser then") is "action", unless that proposal is itself a standing rule. Saying who does the step the agent just handed back or asked about ("you do it", "you will handle all", "you did it for the other project, do it here", "I'll send it, we keep our focus") is "action", and so is correcting how to do the current step ("use the literal path, then rerun"). "rule" only when it must still hold after this task is done: a standing rule or preference (from now on, never, always), a lasting choice (design, tool, vendor, architecture; keep or drop a feature; a priority; who owns an area from now on, not who does this one step), or a plan someone must remember later. Then write "summary": a neutral one-line restatement of WHAT was decided (max 15 words; for a short answer, say what it chose using "prev"), and "keywords": 3-5 distinctive topic words or short names a commit message, status note or log line recording it would contain: the thing it is about (feature, file, tool, setting, machine), never the form of the answer ("option", "choice", "yes", "approve", "later") and never generic words ("owner", "update", "change", "remove", "add"). For non-decisions use "" and [].
Reply with ONLY a JSON array, one object per input message, in input order: [{"id":"...","decision":true|false,"kind":"action"|"rule"|"","summary":"...","keywords":["..."]}]`

// loginPrompt is the system prompt of the tiny call that proves the login.
const loginPrompt = "Reply with the single word ok."

// ClaudeCLI runs `claude -p` on the owner's own Max login. It never uses an
// API key: every ANTHROPIC_* and CLAUDE_CODE_USE_* variable is removed from the
// child's environment, no --bare (which needs an API key) is used, and a run
// whose init message reports any apiKeySource other than "none" is refused.
type ClaudeCLI struct {
	Bin   string // default "claude"
	Model string // default "haiku"
}

// cliEnv is the environment for the child, without API credentials.
func cliEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "CLAUDE_CODE_USE_") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func (c ClaudeCLI) Classify(ctx context.Context, items []Item) ([]Answer, error) {
	in, _ := json.Marshal(items)
	result, err := c.run(ctx, systemPrompt, in)
	if err != nil {
		return nil, err
	}
	return parseAnswers(result, items)
}

// logins holds one login probe per claude binary: a run probes once.
var logins sync.Map // bin -> *login

type login struct {
	once sync.Once
	err  error
}

// proveLogin makes one tiny call, once per run, before any real call starts.
// Calls then run side by side; an API key is found by that one probe, so no
// real call ever starts on one. Other probe failures are left to the calls,
// which report their own reason.
func (c ClaudeCLI) proveLogin(ctx context.Context) error {
	v, _ := logins.LoadOrStore(c.bin(), &login{})
	l := v.(*login)
	l.once.Do(func() { _, l.err = c.call(ctx, loginPrompt, []byte("ok")) })
	if errors.Is(l.err, ErrAPIKey) {
		return l.err
	}
	return nil
}

func (c ClaudeCLI) bin() string {
	if c.Bin == "" {
		return "claude"
	}
	return c.Bin
}

// run sends one prompt to `claude -p`, after the run's login probe, and
// returns the result text.
func (c ClaudeCLI) run(ctx context.Context, prompt string, in []byte) (string, error) {
	if err := c.proveLogin(ctx); err != nil {
		return "", err
	}
	return c.call(ctx, prompt, in)
}

// call sends one prompt to `claude -p` and returns the result text.
func (c ClaudeCLI) call(ctx context.Context, prompt string, in []byte) (string, error) {
	bin, model := c.bin(), c.Model
	if model == "" {
		model = "haiku"
	}
	cmd := exec.CommandContext(ctx, bin, "-p", "--model", model,
		"--tools", "", "--strict-mcp-config", "--disable-slash-commands",
		"--no-session-persistence", "--setting-sources", "",
		"--system-prompt", prompt, "--output-format", "stream-json", "--verbose")
	cmd.Env = cliEnv()
	cmd.Dir = os.TempDir()
	cmd.Stdin = bytes.NewReader(in)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		if ks := initKeySource(out.Bytes()); ks != nil && *ks != "none" {
			return "", fmt.Errorf("%w (apiKeySource %q)", ErrAPIKey, *ks) // a failed call on an API key is still one
		}
		why := strings.TrimSpace(tailStr(errb.String(), 300))
		if r := streamError(out.Bytes()); r != "" {
			why = r // stream-json puts the reason (e.g. "Not logged in") on stdout
		}
		return "", fmt.Errorf("claude -p: %w: %s", err, why)
	}
	return readStream(out.Bytes())
}

// parseAnswers reads the model's JSON array and checks it answers every item.
func parseAnswers(raw string, items []Item) ([]Answer, error) {
	raw = strings.TrimSpace(raw)
	if i := strings.Index(raw, "["); i > 0 {
		raw = raw[i:] // tolerate a ```json fence
	}
	if j := strings.LastIndex(raw, "]"); j >= 0 && j < len(raw)-1 {
		raw = raw[:j+1]
	}
	var raws []struct {
		ID       string   `json:"id"`
		Decision *bool    `json:"decision"`
		Kind     string   `json:"kind"`
		Summary  string   `json:"summary"`
		Keywords []string `json:"keywords"`
	}
	if err := json.Unmarshal([]byte(raw), &raws); err != nil {
		return nil, fmt.Errorf("model reply is not a JSON array: %w", err)
	}
	byID := map[string]Answer{}
	for _, a := range raws {
		if a.Decision == nil { // no "decision": not an answer, never "not a decision"
			continue
		}
		if *a.Decision && a.Kind != "action" && a.Kind != "rule" {
			continue // a decision with no known kind: not an answer, asked again
		}
		if !*a.Decision {
			a.Kind = ""
		}
		byID[a.ID] = Answer{ID: a.ID, Decision: *a.Decision, Kind: a.Kind, Summary: a.Summary, Keywords: a.Keywords}
	}
	out := make([]Answer, 0, len(items))
	var missing []string
	for _, it := range items {
		a, ok := byID[it.ID]
		if !ok {
			missing = append(missing, it.ID)
			continue
		}
		out = append(out, a)
	}
	if len(missing) > 0 {
		return out, fmt.Errorf("model skipped %d of %d items", len(missing), len(items))
	}
	return out, nil
}

func tailStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ErrAPIKey: the CLI ran on an API key, or its login could not be checked.
// The run stops calling the model at the first one (the call may be billed).
var ErrAPIKey = errors.New("claude -p ran on an API key or a login it could not check; landed runs only on a Claude subscription login")

// streamError is the text of an error result in stream-json output, or "".
func streamError(out []byte) string {
	why := ""
	for _, line := range bytes.Split(out, []byte("\n")) {
		var m struct {
			Type    string `json:"type"`
			Result  string `json:"result"`
			IsError bool   `json:"is_error"`
		}
		if json.Unmarshal(line, &m) == nil && m.Type == "result" && m.IsError {
			why = tailStr(m.Result, 300)
		}
	}
	return why
}

// initKeySource is the apiKeySource of the stream's init message (nil: none).
func initKeySource(out []byte) *string {
	ks, _, _ := scanStream(out)
	return ks
}

// readStream reads `claude -p --output-format stream-json` output. It refuses
// the answer unless the init message says no API key was used.
func readStream(out []byte) (string, error) {
	keySource, result, isError := scanStream(out)
	if keySource == nil {
		return "", fmt.Errorf("%w (no init message, so the login cannot be checked)", ErrAPIKey)
	}
	if *keySource != "none" {
		return "", fmt.Errorf("%w (apiKeySource %q)", ErrAPIKey, *keySource)
	}
	if result == nil {
		return "", fmt.Errorf("claude -p: no result message")
	}
	if isError {
		return "", fmt.Errorf("claude -p error: %s", tailStr(*result, 300))
	}
	return *result, nil
}

// scanStream reads the init message's apiKeySource and the result message.
func scanStream(out []byte) (keySource, result *string, isError bool) {
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var m struct {
			Type         string  `json:"type"`
			Subtype      string  `json:"subtype"`
			APIKeySource *string `json:"apiKeySource"`
			Result       *string `json:"result"`
			IsError      bool    `json:"is_error"`
		}
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch {
		case m.Type == "system" && m.Subtype == "init":
			keySource = m.APIKeySource
		case m.Type == "result":
			result, isError = m.Result, m.IsError
		}
	}
	return keySource, result, isError
}
