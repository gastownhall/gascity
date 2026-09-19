package tmux

import (
	"os"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This pane was captured from an isolated Claude 2.1.270 session before any
// response was sent. Its third choice changes permission mode; denial is fourth.
func claude270ApprovalPane(t *testing.T) string {
	t.Helper()
	pane, err := os.ReadFile("testdata/claude-2.1.270-bash-approval.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(pane)
}

func TestPendingClaude270UsesModalCommandAndEveryChoice(t *testing.T) {
	provider := &Provider{tm: &Tmux{exec: &fakeExecutor{out: claude270ApprovalPane(t)}}}
	pending, err := provider.Pending("fixture")
	if err != nil || pending == nil {
		t.Fatalf("Pending = %+v, %v; want captured Claude permission request", pending, err)
	}
	if pending.Metadata["tool_name"] != "Bash" || !strings.Contains(pending.Prompt, "python3 -c") ||
		!strings.Contains(pending.Prompt, `.open("x").write("approved once\n")`) ||
		!strings.Contains(pending.Prompt, "Create approval-proof.txt with exclusive open") {
		t.Fatalf("Pending lost the modal command or title: %+v", pending)
	}
	if len(pending.Options) != 4 || pending.Options[0] != "Yes" || pending.Options[3] != "No" ||
		!strings.Contains(pending.Options[2], "switch to auto mode") {
		t.Fatalf("Pending options = %q, want the four actual menu choices", pending.Options)
	}
}

func TestRespondClaude270DeniesWithoutChangingPermissionMode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fe := &fakeExecutor{outs: []string{claude270ApprovalPane(t), "0", "", "assistant ready"}}
		provider := &Provider{tm: &Tmux{exec: fe}}
		if err := provider.Respond("fixture", runtime.InteractionResponse{Action: "deny"}); err != nil {
			t.Fatal(err)
		}
		var sent []string
		for _, call := range fe.calls {
			if strings.Contains(strings.Join(call, " "), "send-keys") {
				sent = append(sent, call[len(call)-1])
			}
		}
		if len(sent) != 1 || sent[0] != "4" {
			t.Fatalf("denial keys = %q, want exactly 4 (No), never 3 (switch to auto mode)", sent)
		}
	})
}
