package tmux

import (
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func currentClaudeApprovalPane(t *testing.T) string {
	t.Helper()
	pane, err := os.ReadFile("testdata/claude-2.1.263-bash-approval.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(pane)
}

func TestPendingCurrentClaudeBashApproval(t *testing.T) {
	provider := &Provider{tm: &Tmux{exec: &fakeExecutor{out: currentClaudeApprovalPane(t)}}}
	pending, err := provider.Pending("fixture")
	if err != nil || pending == nil {
		t.Fatalf("Pending = %+v, %v; want current Claude permission request", pending, err)
	}
	if pending.Metadata["tool_name"] != "Bash" || !strings.Contains(pending.Prompt, "printf 'APPROVED_fixture") {
		t.Fatalf("Pending lost the actual command: %+v", pending)
	}
	if len(pending.Options) != 4 || pending.Options[0] != "Yes" || pending.Options[3] != "No" {
		t.Fatalf("Pending options = %q, want the four actual menu choices", pending.Options)
	}
}

func TestRespondCurrentClaudeDenialDoesNotEnableAutoMode(t *testing.T) {
	fe := &fakeExecutor{outs: []string{currentClaudeApprovalPane(t), "0", "", "assistant ready"}}
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
}

func TestRespondApprovalUsesCurrentExactChoice(t *testing.T) {
	current := currentClaudeApprovalPane(t)
	for _, tc := range []struct{ name, pane, action, key string }{
		{"current-approve-once", current, "approve", "1"},
		{"legacy-deny", approvalPromptPane(), "deny", "3"},
		{"renumbered-deny", strings.Replace(current, "4. No", "7. No", 1), "deny", "7"},
		{"renumbered-approve", strings.Replace(current, "1. Yes", "5. Yes", 1), "approve", "5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := &fakeExecutor{outs: []string{tc.pane, "0", "", "assistant ready"}}
			provider := &Provider{tm: &Tmux{exec: fe}}
			if err := provider.Respond("fixture", runtime.InteractionResponse{Action: tc.action}); err != nil {
				t.Fatal(err)
			}
			var sent []string
			for _, call := range fe.calls {
				if strings.Contains(strings.Join(call, " "), "send-keys") {
					sent = append(sent, call[len(call)-1])
				}
			}
			if len(sent) != 1 || sent[0] != tc.key {
				t.Fatalf("sent %q, want exactly %q", sent, tc.key)
			}
		})
	}
}

func TestRespondApprovalNeverEscalatesPermissions(t *testing.T) {
	for _, action := range []string{"approve_always", "approve_accept_edits", "switch_auto", "2", "3"} {
		t.Run(action, func(t *testing.T) {
			fe := &fakeExecutor{out: currentClaudeApprovalPane(t)}
			provider := &Provider{tm: &Tmux{exec: fe}}
			if err := provider.Respond("fixture", runtime.InteractionResponse{Action: action}); err == nil {
				t.Fatal("unsafe or unsupported approval action succeeded")
			}
			if len(fe.calls) != 1 || !strings.Contains(strings.Join(fe.calls[0], " "), "capture-pane") {
				t.Fatalf("unsafe approval mutated the pane: %v", fe.calls)
			}
		})
	}
}

func TestRespondRejectsChangedApprovalMenu(t *testing.T) {
	before := currentClaudeApprovalPane(t)
	after := strings.Replace(before, "4. No", "5. No", 1)
	fe := &fakeExecutor{outs: []string{before, after}}
	provider := &Provider{tm: &Tmux{exec: fe}}
	pending, err := provider.Pending("fixture")
	if err != nil || pending == nil {
		t.Fatalf("Pending: %+v, %v", pending, err)
	}
	if err := provider.Respond("fixture", runtime.InteractionResponse{RequestID: pending.RequestID, Action: "deny"}); err == nil || !strings.Contains(err.Error(), "approval prompt changed") {
		t.Fatalf("Respond: %v, want stale menu rejection", err)
	}
	if len(fe.calls) != 2 {
		t.Fatalf("changed menu caused input: %v", fe.calls)
	}
}

func TestApprovalParsingRejectsUnrecognizedOrHistoricalMenus(t *testing.T) {
	current := currentClaudeApprovalPane(t)
	for name, pane := range map[string]string{
		"no-denial":            strings.Replace(current, "   4. No\n", "", 1),
		"no-once-approval":     strings.Replace(current, "1. Yes", "1. Yes, always", 1),
		"ambiguous-denial":     strings.Replace(current, "4. No", "4. No\n   5. No", 1),
		"duplicate-key":        strings.Replace(current, "4. No", "3. No", 1),
		"no-selected-option":   strings.Replace(current, "❯", " ", 1),
		"missing-modal-border": strings.ReplaceAll(current, "─", " "),
		"historical-menu":      current + "\nAssistant completed the action.\n❯ Next prompt\n",
	} {
		t.Run(name, func(t *testing.T) {
			fe := &fakeExecutor{out: pane}
			provider := &Provider{tm: &Tmux{exec: fe}}
			pending, err := provider.Pending("fixture")
			if err != nil || pending != nil {
				t.Fatalf("Pending = %+v, %v, want no safely recognized menu", pending, err)
			}
			if err := provider.Respond("fixture", runtime.InteractionResponse{Action: "deny"}); err == nil {
				t.Fatal("unrecognized approval menu should reject response")
			}
			if len(fe.calls) != 2 {
				t.Fatalf("unrecognized menu caused input: %v", fe.calls)
			}
		})
	}
}

func TestPendingDoesNotBindEarlierBashModalToCurrentEditApproval(t *testing.T) {
	current := currentClaudeApprovalPane(t)
	for name, earlier := range map[string]string{
		"complete-prior-dialog": current,
		"prior-menu-cleared":    strings.Split(current, " Do you want to proceed?")[0],
	} {
		t.Run(name, func(t *testing.T) {
			pane := earlier + "\n● Edit(file_path: /tmp/next.go)\n Approve edits?\n Do you want to proceed?\n ❯ 1. Yes\n   3. No\n"
			provider := &Provider{tm: &Tmux{exec: &fakeExecutor{out: pane}}}
			pending, err := provider.Pending("fixture")
			if err != nil || pending == nil || pending.Metadata["tool_name"] != "Edit" || !strings.Contains(pending.Prompt, "/tmp/next.go") {
				t.Fatalf("Pending associated the wrong tool: %+v, %v", pending, err)
			}
		})
	}
}
