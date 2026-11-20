package tmux

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Every family runtime.WaitIdleLiveFamilies accepts is a promise about this
// leaf: its wait-idle delivery paths land here, so the leaf must trust the
// family's busy reading (busyReadingTrusted) and run its verified submit
// (submitVerifyEligibleFamily), or a wait-idle nudge would wait out a turn it
// cannot see and type into a working session. The trust is deliberately
// one-directional: codex's busy reading is trusted and its submits verified,
// yet codex is not wait-idle-live — waiting out a codex turn needs more than
// a readable busy indicator (AC-7).
func TestWaitIdleLiveFamiliesAreTrustedByTheTmuxLeaf(t *testing.T) {
	for _, family := range runtime.WaitIdleLiveFamilies {
		if !busyReadingTrusted(family) {
			t.Errorf("runtime.WaitIdleLiveFamilies lists %q, but the tmux leaf does not trust its busy reading: a wait-idle delivery would wait out a turn it cannot see", family)
		}
		if !submitVerifyEligibleFamily(family) {
			t.Errorf("runtime.WaitIdleLiveFamilies lists %q, but the leaf's verified submit refuses it: a wait-idle delivery could not confirm its Enter", family)
		}
	}
	// The one-directional arrangement: codex is busy-trusted (and
	// submit-verified) without being wait-idle-live.
	if !busyReadingTrusted("codex") || !submitVerifyEligibleFamily("codex") {
		t.Error("codex must stay busy-trusted and submit-verified: the one-directional check below depends on it")
	}
	if runtime.WaitIdleLiveFamily("codex") {
		t.Error("WaitIdleLiveFamily(codex) = true; busy-trust must not imply wait-idle liveness")
	}
}
