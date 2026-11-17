//go:build windows

package acp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/testutil"
)

// TestStartRefusesAPromptSuffix pins that on Windows no prompt text reaches an
// ACP agent's command line. For a batch-file agent, such as an npm .cmd shim,
// PowerShell hands that text on to cmd.exe, which runs its & as a command and
// expands its %NAME% references whatever the quoting. Start refuses before
// anything runs.
func TestStartRefusesAPromptSuffix(t *testing.T) {
	agentDir := t.TempDir()
	agent := filepath.Join(agentDir, "agent.cmd")
	if err := os.WriteFile(agent, []byte("@echo started>\"%~dp0started\"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	p := newTestProvider(t)
	p.cfg.HandshakeTimeout = testutil.ExecRaceTimeout
	name := testName()
	err := p.Start(context.Background(), name, runtime.Config{
		Command:      "& " + runtime.ShellQuote(agent),
		PromptSuffix: shellquote.Quote("a&echo>pwned"),
		WorkDir:      workDir,
	})
	_ = p.Stop(name)

	if err == nil || !strings.Contains(err.Error(), "command line") {
		t.Errorf("Start = %v, want a refusal of the prompt suffix", err)
	}
	if _, statErr := os.Stat(filepath.Join(agentDir, "started")); statErr == nil {
		t.Error("the agent ran although its prompt was refused")
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "pwned")); statErr == nil {
		t.Error("cmd.exe ran the prompt suffix: pwned exists in the session's working directory")
	}
}
