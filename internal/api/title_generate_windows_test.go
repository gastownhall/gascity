//go:build windows

package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestGenerateTitleKeepsTheMessageFromABatchFileProvider pins that a session
// message, free text from the API, never reaches cmd.exe through a batch-file
// provider command such as an npm .cmd shim. cmd.exe would run its & | < >,
// expand its %NAME% and end the prompt at its first line break, which the
// title prompt itself holds, so such a provider never received the message;
// the title falls back to the truncated message instead.
func TestGenerateTitleKeepsTheMessageFromABatchFileProvider(t *testing.T) {
	workDir := t.TempDir()
	agent := filepath.Join(t.TempDir(), "agent.cmd")
	if err := os.WriteFile(agent, []byte("@echo A generated title\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := &config.ResolvedProvider{Command: agent, PrintArgs: []string{"-p"}}

	for _, msg := range []string{"plain message", `say "hi & echo>pwned & rem`} {
		if got := generateTitle(provider, msg, workDir); got != truncateTitle(msg) {
			t.Errorf("generateTitle(%q) = %q, want the truncated message", msg, got)
		}
	}
	if _, err := os.Stat(filepath.Join(workDir, "pwned")); err == nil {
		t.Error("cmd.exe ran the session message: pwned exists in the working directory")
	}
}
