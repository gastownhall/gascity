package tmux

import (
	"testing"
)

// callHasTokens reports whether call contains every token as an exact arg
// (order-independent, not substring) so assertions cannot be fooled by a flag
// appearing inside another argument.
func callHasTokens(call []string, tokens ...string) bool {
	for _, tok := range tokens {
		found := false
		for _, a := range call {
			if a == tok {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// callIndexWithTokens returns the index of the first recorded tmux call that
// contains all the given tokens, or -1 if none match.
func callIndexWithTokens(calls [][]string, tokens ...string) int {
	for i, c := range calls {
		if callHasTokens(c, tokens...) {
			return i
		}
	}
	return -1
}

// TestSendKeysCancelsCopyModeBeforeDelivery pins the ga-c4w major #2 fix: the
// WheelUpPane->copy-mode binding can park an interactive pane in copy-mode, and
// tmux then swallows controller keystrokes. SendKeysDebounced must probe
// #{pane_in_mode} and cancel copy-mode BEFORE the literal -l send when parked,
// while leaving the happy path (not parked) untouched.
func TestSendKeysCancelsCopyModeBeforeDelivery(t *testing.T) {
	t.Run("parked pane cancels copy-mode before the literal send", func(t *testing.T) {
		// First tmux call is the #{pane_in_mode} probe -> report parked.
		fe := &fakeExecutor{outs: []string{"1"}}
		tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

		if err := tm.SendKeysDebounced("sess", "hello", 0); err != nil {
			t.Fatalf("SendKeysDebounced: %v", err)
		}

		probe := callIndexWithTokens(fe.calls, "display-message", "#{pane_in_mode}")
		cancel := callIndexWithTokens(fe.calls, "send-keys", "-X", "cancel")
		literal := callIndexWithTokens(fe.calls, "send-keys", "-l", "hello")

		if probe < 0 {
			t.Fatalf("expected a #{pane_in_mode} probe before delivery; calls=%v", fe.calls)
		}
		if cancel < 0 {
			t.Fatalf("parked pane: expected a copy-mode `-X cancel` before delivery; calls=%v", fe.calls)
		}
		if literal < 0 {
			t.Fatalf("literal keys were never delivered; calls=%v", fe.calls)
		}
		if cancel >= literal {
			t.Fatalf("copy-mode cancel (idx %d) must precede the literal send (idx %d); calls=%v", cancel, literal, fe.calls)
		}
	})

	t.Run("unparked pane issues no cancel and delivers normally", func(t *testing.T) {
		// Probe reports not-in-mode -> no cancel, happy path unchanged.
		fe := &fakeExecutor{outs: []string{"0"}}
		tm := &Tmux{cfg: Config{SocketName: "x"}, exec: fe}

		if err := tm.SendKeysDebounced("sess", "hello", 0); err != nil {
			t.Fatalf("SendKeysDebounced: %v", err)
		}

		if cancel := callIndexWithTokens(fe.calls, "send-keys", "-X", "cancel"); cancel >= 0 {
			t.Fatalf("unparked pane: spurious copy-mode cancel at idx %d; calls=%v", cancel, fe.calls)
		}
		if literal := callIndexWithTokens(fe.calls, "send-keys", "-l", "hello"); literal < 0 {
			t.Fatalf("happy path must still deliver the literal keys; calls=%v", fe.calls)
		}
	})
}
