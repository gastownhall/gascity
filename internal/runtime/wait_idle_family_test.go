package runtime

import "testing"

// WaitIdleLiveFamily is the one gate every wait-idle delivery path asks: the
// families whose idle boundary this build can wait on, and only those. The
// tmux leaf pins its busy-trust policy against this list.
func TestWaitIdleLiveFamily(t *testing.T) {
	for _, family := range WaitIdleLiveFamilies {
		if !WaitIdleLiveFamily(family) {
			t.Errorf("WaitIdleLiveFamily(%q) = false, want true", family)
		}
	}
	// Controls: families a wait-idle nudge still cannot wait on. Codex and
	// Gemini run turns the scan can read, but their wait-idle delivery is not
	// wired: the gates refused them before this list existed and still do.
	for _, family := range []string{"codex", "gemini", "kimi", "opencode", "", "some-unregistered-family"} {
		if WaitIdleLiveFamily(family) {
			t.Errorf("WaitIdleLiveFamily(%q) = true, want false", family)
		}
	}
}
