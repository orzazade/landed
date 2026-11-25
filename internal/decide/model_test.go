package decide

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// A claude CLI that is not logged in exits 1 and says why only in its
// stream-json result message; that reason must reach the user.
func TestFailedCallSaysWhy(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\n" +
		`echo '{"type":"system","subtype":"init","apiKeySource":"none"}'` + "\n" +
		`echo '{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login"}'` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := ClaudeCLI{Bin: bin}.run(context.Background(), "p", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "Not logged in") {
		t.Errorf("got %v; want the CLI's reason", err)
	}
}

// fakeCLI writes a claude stand-in that logs each start to a file, reports
// apiKeySource src and exits with code exit. Calls whose system prompt is the
// login probe say "probe" in the log.
func fakeCLI(t *testing.T, src string, exit int) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "claude"), filepath.Join(dir, "starts")
	script := "#!/bin/sh\n" +
		`case "$*" in *"` + loginPrompt + `"*) echo probe >> ` + log + ` ;; *) echo call >> ` + log + ` ;; esac` + "\n" +
		`cat > /dev/null` + "\n" +
		`echo '{"type":"system","subtype":"init","apiKeySource":"` + src + `"}'` + "\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"[]"}'` + "\n" +
		fmt.Sprintf("exit %d\n", exit)
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func starts(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// Calls side by side are safe only because one tiny call proves the login
// first: with an API key, that probe is the only start; nothing else runs.
func TestAPIKeyFoundByOneProbeBeforeParallelCalls(t *testing.T) {
	for _, exit := range []int{0, 1} { // 1: the call failed (rate limit, overload) after its init line
		bin, log := fakeCLI(t, "ANTHROPIC_API_KEY", exit)
		apiKeyProbe(t, ClaudeCLI{Bin: bin}, log)
	}
}

func apiKeyProbe(t *testing.T, c ClaudeCLI, log string) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.Classify(context.Background(), []Item{{ID: "a"}})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if !errors.Is(err, ErrAPIKey) {
			t.Errorf("call %d: %v; want the API-key refusal", i, err)
		}
	}
	if got := starts(t, log); len(got) != 1 || got[0] != "probe" {
		t.Errorf("claude started %v; want only the probe", got)
	}
}

// With the login fine, the probe runs once per run, before the calls.
func TestLoginProbedOncePerRun(t *testing.T) {
	bin, log := fakeCLI(t, "none", 0)
	c := ClaudeCLI{Bin: bin}
	for range 3 {
		if _, err := c.Classify(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(starts(t, log), " "); got != "probe call call call" {
		t.Errorf("claude started %q; want one probe, then the three calls", got)
	}
}

// claude reads the folder it starts in; the shared temp folder can hold tens
// of thousands of files (10 s of CPU per start, measured 11-26). Each call
// starts in an empty folder of landed's own.
func TestClaudeStartsInEmptyFolder(t *testing.T) {
	dir := t.TempDir()
	bin, log := filepath.Join(dir, "claude"), filepath.Join(dir, "where")
	script := "#!/bin/sh\n" +
		`echo "$PWD $(ls -A | wc -l | tr -d ' ')" >> ` + log + "\n" +
		`cat > /dev/null` + "\n" +
		`echo '{"type":"system","subtype":"init","apiKeySource":"none"}'` + "\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"[]"}'` + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := (ClaudeCLI{Bin: bin}).Classify(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	tmp, _ := filepath.EvalSymlinks(os.TempDir())
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		where, n, _ := strings.Cut(line, " ")
		w, _ := filepath.EvalSymlinks(where)
		if w == tmp || n != "0" {
			t.Errorf("claude started in %s with %s entries; want an empty folder of its own, not the shared temp folder", where, n)
		}
	}
}

// A link planted at the start folder's name (another user on a shared /tmp)
// is refused, never followed.
func TestStartFolderMustBeOwnFolder(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	if err := os.Symlink(t.TempDir(), filepath.Join(tmp, fmt.Sprintf("landed-claude-%d", os.Getuid()))); err != nil {
		t.Fatal(err)
	}
	if d, err := workDir(); err == nil || !strings.Contains(err.Error(), "not a folder of this user's own") {
		t.Errorf("workDir() = %q, %v; want the planted link refused", d, err)
	}
}
