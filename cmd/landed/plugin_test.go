package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The plugin's hooks call bin/landed-hook. Without a landed binary it says
// how to install one and exits 0 (a hook never blocks); the PreCompact
// warm-up stays silent, since its stdout would steer the summary.
func TestPluginHookWrapper(t *testing.T) {
	script, _ := filepath.Abs("../../scripts/landed-hook")
	run := func(path string, args ...string) (string, error) {
		cmd := exec.Command(script, args...)
		cmd.Env = []string{"PATH=" + path, "HOME=" + t.TempDir()}
		cmd.Stdin = strings.NewReader(`{"session_id":"kettle-1"}`)
		out, err := cmd.Output()
		return string(out), err
	}
	out, err := run("/usr/bin:/bin", "check", "--hook")
	if err != nil || !strings.HasPrefix(out, "landed could not check: the landed binary is not installed") || strings.Count(out, "\n") != 1 {
		t.Errorf("no binary: err %v, output %q", err, out)
	}
	if out, err := run("/usr/bin:/bin", "check", "--hook", "--warm"); err != nil || out != "" {
		t.Errorf("no binary, warm-up: err %v, output %q; want silence", err, out)
	}
	bin := t.TempDir()
	fake := "#!/bin/sh\necho \"fake landed $*\"\ncat\n"
	if err := os.WriteFile(filepath.Join(bin, "landed"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := run(bin+":/usr/bin:/bin", "check", "--hook"); err != nil || out != "fake landed check --hook\n{\"session_id\":\"kettle-1\"}" {
		t.Errorf("binary on PATH: err %v, output %q", err, out)
	}
}

// The plugin files parse and point the hooks at the wrapper with timeouts
// under what the check needs (it answers by 8 s).
func TestPluginFiles(t *testing.T) {
	var hooks struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	b, err := os.ReadFile("../../hooks/hooks.json")
	if err == nil {
		err = json.Unmarshal(b, &hooks)
	}
	if err != nil {
		t.Fatal(err)
	}
	ss, pc := hooks.Hooks["SessionStart"], hooks.Hooks["PreCompact"]
	if len(ss) != 1 || ss[0].Matcher != "compact" || ss[0].Hooks[0].Command != `"${CLAUDE_PLUGIN_ROOT}/scripts/landed-hook" check --hook` || ss[0].Hooks[0].Timeout != 10 {
		t.Errorf("SessionStart: %+v", ss)
	}
	if len(pc) != 1 || pc[0].Hooks[0].Command != `"${CLAUDE_PLUGIN_ROOT}/scripts/landed-hook" check --hook --warm` || pc[0].Hooks[0].Timeout != 5 {
		t.Errorf("PreCompact: %+v", pc)
	}
	for _, f := range []string{"../../.claude-plugin/plugin.json", "../../.claude-plugin/marketplace.json"} {
		var v map[string]any
		b, err := os.ReadFile(f)
		if err == nil {
			err = json.Unmarshal(b, &v)
		}
		if err != nil || v["name"] != "landed" {
			t.Errorf("%s: %v, name %v", f, err, v["name"])
		}
	}
}
