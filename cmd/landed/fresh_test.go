package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orzazade/landed/internal/landing"
)

// The default repos folder is a guess: a user without ~/Projects gets no
// error. A folder the user named that is not there still fails loudly.
func TestDefaultReposFolderMayBeMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var src landing.Sources
	widen(&src, defaultReposUnder, true)
	if src.ReposErr != nil {
		t.Errorf("missing default ~/Projects: %v; want no error", src.ReposErr)
	}
	var named landing.Sources
	widen(&named, filepath.Join(t.TempDir(), "typo"), true)
	if named.ReposErr == nil {
		t.Error("a named folder that is not there gave no error")
	}
}

// A failed model call says why, so a user can fix it (for example a login).
func TestRunErrorsSayWhy(t *testing.T) {
	var c checked
	c.Stats.Errors = []error{errors.New("claude -p: Not logged in · Please run /login")}
	c.JudgeErrors = []string{"judge: timed out"}
	err := runErrors(c, nil)
	if err == nil || !strings.Contains(err.Error(), "Not logged in") || !strings.Contains(err.Error(), "judge: timed out") {
		t.Errorf("got %v", err)
	}
}
