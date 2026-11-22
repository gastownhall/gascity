package tmux

import (
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills a read that takes a settle option inherited from an outer scope
// (naming another pane, or none) as the pane's own declaration.
func TestSettleDeclaredOnlyByAnOptionBoundToThePane(t *testing.T) {
	for settle, want := range map[string]bool{"%5": true, "": false, "%9": false} {
		pane, ok := parseProgramPane([]string{"agent", "%5", "1", "r.%5", "", "", settle}, false)
		if !ok {
			t.Fatalf("settle %q: row did not parse", settle)
		}
		if got := pane.settleDeclared(); got != want {
			t.Errorf("settle %q: settleDeclared = %v, want %v", settle, got, want)
		}
		if pane.holdDeclared() {
			t.Errorf("settle %q: the settle option declared the hold", settle)
		}
	}
}

// Kills an arm that loses the settle declaration or leaves a stale one: a
// settling arm binds the option to the pane, a re-arm that does not settle
// unsets it, a create that does not settle adds nothing, and a disarm
// removes it, independent of the hold.
func TestArmCommandsCarryTheSettleDeclaration(t *testing.T) {
	plan := &capturePlan{rand: "r4nd", settle: true}
	set := []string{"set-option", "-p", "-F", "-t", "%5", programSettleOption, "#{pane_id}"}
	unset := []string{"set-option", "-pu", "-t", "%5", programSettleOption}
	holdSet := []string{"set-option", "-p", "-F", "-t", "%5", programHoldOption, "#{pane_id}"}
	contains := func(cmds [][]string, cmd []string) bool {
		return slices.ContainsFunc(cmds, func(c []string) bool { return slices.Equal(c, cmd) })
	}
	for _, clear := range []bool{false, true} {
		cmds := plan.armCommands("%5", clear)
		if !contains(cmds, set) || contains(cmds, unset) || contains(cmds, holdSet) {
			t.Errorf("settling arm (clear=%v) %q: want the settle option bound to the pane and no hold", clear, cmds)
		}
		if last := cmds[len(cmds)-1]; tmuxSubcommand(last) != "pipe-pane" {
			t.Errorf("settling arm (clear=%v) does not end with the tap: %q", clear, cmds)
		}
	}
	plan.settle = false
	if cmds := plan.armCommands("%5", true); contains(cmds, set) || !contains(cmds, unset) {
		t.Errorf("non-settling re-arm %q: want the option unset", cmds)
	}
	if cmds := plan.armCommands("%5", false); len(cmds) != 2 {
		t.Errorf("non-settling create arm %q: want only the nonce and the tap", cmds)
	}
	if cmds := disarmCommands("%5"); !contains(cmds, unset) {
		t.Errorf("disarm %q: want the option unset", cmds)
	}
}

// Kills a heal that leaves a live capture's settle declaration stale: a
// healthy capture armed before its provider opted into the settle has it
// backfilled, one whose provider withdrew it has it unset, each guarded by
// the pane's own nonce and leaving the hold as want has it, and a converged
// capture is left alone. A fresh arm declares it exactly when want does.
func TestHealConvergesTheSettleDeclarationOfALiveCapture(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	agent := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", pipe: true, tap: "l1vE.%5", hold: "%5"}
	server := &healServer{panes: []*healModelPane{agent}, taps: map[string]bool{"l1vE.%5": true}}
	p := healProvider(server, &now)
	holding := runtime.ProgramStatusArm{Capture: true, App: "codex", HoldMidTurn: true}
	settling := holding
	settling.DecideSettle = true

	mustHeal(t, p, "s", holding, runtime.ProgramCaptureUnchanged)
	mustHeal(t, p, "s", settling, runtime.ProgramCaptureDeclared)
	if agent.settle != "%5" || agent.hold != "%5" || agent.tap != "l1vE.%5" || !agent.pipe {
		t.Fatalf("after the backfill: %+v, want the settle bound to %%5, the hold kept and the capture untouched", agent)
	}
	muts := server.mutations()
	if len(muts) != 1 || muts[0][0] != "if-shell" || muts[0][4] != "#{==:#{"+programTapOption+"},l1vE.%5}" {
		t.Fatalf("mutations %q, want one if-shell guarded by the pane's nonce", muts)
	}
	mustHeal(t, p, "s", settling, runtime.ProgramCaptureUnchanged)
	mustHeal(t, p, "s", holding, runtime.ProgramCaptureDeclared)
	if agent.settle != "" || agent.hold != "%5" {
		t.Fatalf("after the withdrawal: %+v, want the settle unset and the hold kept", agent)
	}

	fresh := &healModelPane{session: "f", window: "@2", id: "%7", mark: "agent"}
	server.panes = append(server.panes, fresh)
	mustHeal(t, p, "f", runtime.ProgramStatusArm{Capture: true, DecideSettle: true}, runtime.ProgramCaptureArmed)
	if fresh.settle != "%7" || fresh.hold != "" {
		t.Fatalf("fresh arm: %+v, want the settle bound to %%7 and no hold", fresh)
	}
}

// Kills a disarm that leaves a declaration bound to the pane: heal's disarm
// (the session no longer declares capture) and the switch-off sweep unset
// the hold and settle options with the nonce, so a disarmed pane notes no
// interrupt for a settle and holds no delivery.
func TestDisarmUnsetsTheDeclarations(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	declaring := func(session, window, id string) *healModelPane {
		return &healModelPane{session: session, window: window, id: id, mark: "agent", pipe: true, tap: "l1vE." + id, hold: id, settle: id}
	}

	healed := declaring("s", "@1", "%5")
	server := &healServer{panes: []*healModelPane{healed}, taps: map[string]bool{healed.tap: true}}
	p := healProvider(server, &now)
	mustHeal(t, p, "s", runtime.ProgramStatusArm{}, runtime.ProgramCaptureDisarmed)
	if healed.tap != "" || healed.pipe || healed.hold != "" || healed.settle != "" {
		t.Fatalf("after heal's disarm: %+v, want the capture removed with both declarations", healed)
	}

	swept := declaring("a", "@2", "%7")
	server = &healServer{panes: []*healModelPane{swept}, taps: map[string]bool{swept.tap: true}}
	p = healProvider(server, &now)
	if n, deferred, err := p.DisarmProgramCaptures(); err != nil || n != 1 || deferred != 0 {
		t.Fatalf("DisarmProgramCaptures = %d, %d, %v, want 1 disarmed", n, deferred, err)
	}
	if swept.tap != "" || swept.pipe || swept.hold != "" || swept.settle != "" {
		t.Fatalf("after the switch-off sweep: %+v, want the capture removed with both declarations", swept)
	}
}
