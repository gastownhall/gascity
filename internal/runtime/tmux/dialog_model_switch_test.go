package tmux

import (
	"testing"
	"time"
)

// TestDismissModelSwitchModal proves that when the pane shows the Codex/GPT
// model-switch modal, the dismisser selects "Keep current model" (Down off the
// default "Switch" option, then Enter) — keeping the model with no downgrade.
func TestDismissModelSwitchModal(t *testing.T) {
	modal := "Approaching rate limits\n" +
		"Switch to gpt-5.4-mini for lower credit usage?\n" +
		"  2. Keep current model\n" +
		"Press enter to confirm or esc to go back"
	var keys []string
	sendKeys := func(k ...string) error { keys = append(keys, k...); return nil }

	dismissed, err := dismissModelSwitchModal(modal, sendKeys, func(time.Duration) {})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !dismissed {
		t.Fatal("dismissed = false, want true for the model-switch modal")
	}
	if len(keys) != 2 || keys[0] != "Down" || keys[1] != "Enter" {
		t.Fatalf("keys = %v, want [Down Enter] (select Keep-current-model, then confirm)", keys)
	}
}

// TestDismissModelSwitchModalNoOpOnWorkingPane proves the dismisser never sends
// keystrokes into an ordinary working pane, even one whose output mentions rate
// limits — the safety property that lets it run mid-session.
func TestDismissModelSwitchModalNoOpOnWorkingPane(t *testing.T) {
	content := "Working (3s • esc to interrupt)\n" +
		"> I'm adding backoff because we're near the rate limit; keeping the current model."
	var keys []string
	sendKeys := func(k ...string) error { keys = append(keys, k...); return nil }

	dismissed, err := dismissModelSwitchModal(content, sendKeys, func(time.Duration) {})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if dismissed {
		t.Fatal("dismissed = true, want false on a non-modal working pane")
	}
	if len(keys) != 0 {
		t.Fatalf("keys = %v, want none (must not type into a working pane)", keys)
	}
}
