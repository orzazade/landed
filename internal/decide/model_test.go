package decide

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
