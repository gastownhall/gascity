package herdr

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// A Claude session must never receive multi-line text as a paste: Claude Code
// marks it <pasted_content> and the model declines instructions inside it. The
// startup prime moves to --append-system-prompt-file behind a one-line kickoff
// that keeps the beacon; a multi-line nudge moves to a file behind one line
// naming it; a one-line nudge is delivered as it is.
func TestClaudeSessionsNeverReceiveMultilinePastes(t *testing.T) {
	p, state := newFakeHerdrProvider(t)
	listenHerdrSocket(t, p)

	const name = "gastown__worker"
	beacon := "[gastown] gastown/worker-1 • 2026-09-28T10:00:00"
	prime := beacon + "\n\n# GC Role Worker\n\nRun the claim block first."
	cfg := runtime.Config{
		Command:      "claude --effort medium",
		PromptSuffix: shellquote.Quote(prime),
		Nudge:        "Run gc hook --claim --json now.",
	}
	if err := p.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	calls := fakeCalls(t, state)
	flag := regexp.MustCompile(`agent start \S+ --kind claude .* -- --effort medium --append-system-prompt-file (\S+)`).FindStringSubmatch(calls)
	if flag == nil {
		t.Fatalf("claude was not launched with the startup text as a system-prompt file:\n%s", calls)
	}
	if b, err := os.ReadFile(flag[1]); err != nil || string(b) != prime+"\n\nRun gc hook --claim --json now." {
		t.Fatalf("system-prompt file = %q, %v; want the prime then the nudge", b, err)
	}
	kickoff := beacon + " Your Gas City instructions for this session are appended to your system prompt."
	if !strings.Contains(calls, "agent prompt %5 "+kickoff) {
		t.Fatalf("first turn is not the one-line kickoff carrying the beacon:\n%s", calls)
	}

	if err := p.Nudge(name, runtime.TextContent("Blocker closed.\n\nContinue your bead.")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if err := p.Nudge(name, runtime.TextContent("You have mail.")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	calls = fakeCalls(t, state)
	pointer := regexp.MustCompile(`agent prompt %5 Gas City sent you a message for this session in (\S+)\. Read that file and act on it now\.`).FindStringSubmatch(calls)
	if pointer == nil {
		t.Fatalf("multi-line nudge was not replaced by a one-line pointer:\n%s", calls)
	}
	if b, err := os.ReadFile(pointer[1]); err != nil || string(b) != "Blocker closed.\n\nContinue your bead." {
		t.Fatalf("nudge file = %q, %v", b, err)
	}
	if !strings.Contains(calls, "agent prompt %5 You have mail.") {
		t.Fatalf("one-line nudge was not delivered as it is:\n%s", calls)
	}
}
