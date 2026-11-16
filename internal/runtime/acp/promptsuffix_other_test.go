//go:build !windows

package acp

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// TestStartPassesThePromptSuffixAsLiteralArguments pins that a prompt suffix,
// which carries a session-create API message, reaches the agent as literal
// arguments under sh and nothing in it runs. Each payload, run as code, would
// create a file named pwned in the session's working directory.
func TestStartPassesThePromptSuffixAsLiteralArguments(t *testing.T) {
	want := []string{
		"it\u2019s fine; New-Item -ItemType File -Path pwned | Out-Null; \u2019",
		"x'; touch pwned; '$(touch pwned)$(New-Item -ItemType File -Path pwned)` `\n\u2018\u201A\u201B \"dq\"",
	}
	argsFile := filepath.Join(t.TempDir(), "args")
	workDir := t.TempDir()
	p := newTestProvider(t)
	name := testName()
	// The fixture is not an ACP agent, so Start fails its handshake once the
	// fixture exits; only the arguments it captured matter here.
	_ = p.Start(context.Background(), name, runtime.Config{
		Command:      fixtureCaptureArgsCommand,
		PromptSuffix: shellquote.Join(want),
		WorkDir:      workDir,
		Env:          map[string]string{"GC_ARGS_FILE": argsFile},
	})
	_ = p.Stop(name)

	out, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("the agent never received its arguments: %v", err)
	}
	got, err := decodeCapturedArgs(string(out))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("agent arguments = %q,\nwant %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(workDir, "pwned")); err == nil {
		t.Error("a payload in the prompt suffix ran: pwned exists in the session's working directory")
	}
}
