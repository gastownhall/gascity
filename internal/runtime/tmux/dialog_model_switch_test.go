package tmux

import (
	"strings"
	"testing"
)

// TestClearModelSwitchModalKeepsTheCurrentModel proves that when the pane shows
// the Codex/GPT model-switch modal, the dismissal selects "Keep current model"
// (Down off the default "Switch" option, then Enter) — keeping the model with no
// downgrade — and that both keys ride inside a single certified effect.
func TestClearModelSwitchModalKeepsTheCurrentModel(t *testing.T) {
	fe := &fencePaneExecutor{
		session:  "worker",
		attached: "0",
		inMode:   "0",
		panes:    []string{modelSwitchModalPane, "ready >"},
	}
	tm := NewTmux()
	tm.exec = fe

	if err := tm.clearModelSwitchModal("worker", ""); err != nil {
		t.Fatalf("clearModelSwitchModal: %v", err)
	}

	idx := -1
	for i, call := range fe.calls {
		if strings.Contains(strings.Join(call, " "), " Down ") {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("modal was never dismissed: %#v", fe.calls)
	}
	effect := strings.Join(fe.calls[idx], " ")
	if !strings.Contains(effect, "if-shell") {
		t.Fatalf("dismissal = %q, want a guarded effect", effect)
	}
	down := strings.Index(effect, " Down ")
	settle := strings.Index(effect, "run-shell 'sleep")
	enter := strings.Index(effect, " Enter ")
	if down < 0 || settle < down || enter < settle {
		t.Fatalf("dismissal = %q, want Down, a settle, then Enter", effect)
	}
	if strings.Count(effect, "#{==:#{pane_id},%9}") < 2 {
		t.Fatalf("dismissal = %q, want the pane re-proved after the settle", effect)
	}
}

// TestClearModelSwitchModalNoOpOnWorkingPane proves the dismisser never sends
// keystrokes into an ordinary working pane, even one whose output mentions rate
// limits — the safety property that lets it run before every nudge.
func TestClearModelSwitchModalNoOpOnWorkingPane(t *testing.T) {
	fe := &fencePaneExecutor{
		session:  "worker",
		attached: "0",
		inMode:   "0",
		panes: []string{"Working (3s • esc to interrupt)\n" +
			"> I'm adding backoff because we're near the rate limit; keeping the current model."},
	}
	tm := NewTmux()
	tm.exec = fe

	if err := tm.clearModelSwitchModal("worker", ""); err != nil {
		t.Fatalf("clearModelSwitchModal: %v", err)
	}
	for _, call := range fe.calls {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "send-keys") || strings.Contains(joined, "if-shell") {
			t.Fatalf("working pane received input: %#v", fe.calls)
		}
	}
}
