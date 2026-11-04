package tmux

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// holdNonce is the nonce armed on fakeAgentPaneRow's agent pane %1.
const holdNonce = "h0ldR4nd.%1"

// holdExecutor answers the leaf's hold reads (programHoldFormat) with row
// and hands every other command to inner: inner sees exactly the commands
// the nudge sends to the pane.
type holdExecutor struct {
	mu    sync.Mutex
	inner executor
	// row returns the nth hold read's output; nil answers nothing.
	row   func(n int) string
	reads [][]string
}

// isHoldRead reports whether args is a leaf's fresh hold read.
func isHoldRead(args []string) bool { return args[len(args)-1] == programHoldFormat }

func (e *holdExecutor) execute(args []string) (string, error) {
	e.mu.Lock()
	if isHoldRead(args) {
		defer e.mu.Unlock()
		n := len(e.reads)
		e.reads = append(e.reads, slices.Clone(args))
		if e.row == nil {
			return "", errors.New("unscripted hold read")
		}
		return e.row(n), nil
	}
	e.mu.Unlock()
	return e.inner.execute(args)
}

func (e *holdExecutor) executeCtx(_ context.Context, args []string) (string, error) {
	return e.execute(args)
}

// holdReads is how many hold reads the leaf took.
func (e *holdExecutor) holdReads() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.reads)
}

// holdPaneRow is a hold read's row for agent pane %1: armed under
// holdNonce, its pipe open, carrying record, its hold option as hold and its
// window's activity at activity (unix seconds; 0 for none).
func holdPaneRow(record, hold string, activity int64) string {
	return strings.Join([]string{"0", "agent", "%1", "1", holdNonce, record, hold, strconv.FormatInt(activity, 10), runtimeInventoryEnd}, "\t")
}

// holdTmux is a Tmux under mode over inner, its capture gate passed, its
// hold reads answered by row.
func holdTmux(mode runtime.ProgramStatusMode, inner executor, row func(int) string) (*Tmux, *holdExecutor) {
	ex := &holdExecutor{inner: inner, row: row}
	cfg := DefaultConfig()
	cfg.ProgramStatus = mode
	tm := &Tmux{cfg: cfg, exec: ex}
	tm.capture.probe = passGate
	return tm, ex
}

// Kills a hold read that holds for a pane whose hold option is unset or
// names another pane (a value inherited from an outer scope), takes more
// than one tmux read, or drops the pane's activity.
func TestHoldReadDeclaredOnlyByAnOptionBoundToThePane(t *testing.T) {
	working := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince)
	activity := time.Now().Add(-time.Minute).Unix()
	for _, c := range []struct {
		name, hold string
		ok         bool
	}{
		{"bound to the pane", "%1", true},
		{"unset", "", false},
		{"inherited, naming another pane", "%9", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			tm, ex := holdTmux(runtime.ProgramStatusOn, &fakeExecutor{}, func(int) string { return holdPaneRow(working, c.hold, activity) })
			r, ok := tm.holdRead("s")
			if ok != c.ok {
				t.Fatalf("holdRead ok = %v, want %v (reading %+v)", ok, c.ok, r)
			}
			if got := ex.holdReads(); got != 1 || tmuxSubcommand(ex.reads[0]) != "list-panes" {
				t.Fatalf("hold reads %q, want one list-panes of the session", ex.reads)
			}
			if !c.ok {
				return
			}
			if !r.Status.Running() || r.Status.Mode != runtime.ProgramStatusOn || !r.Activity.Equal(time.Unix(activity, 0)) {
				t.Fatalf("reading %+v, want a valid working record under on with the pane's activity", r)
			}
		})
	}
}

// Kills a hold read taken under a switch that is not on.
func TestHoldReadReadsNothingUnlessOn(t *testing.T) {
	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusOff, runtime.ProgramStatusShadow} {
		inner := &fakeExecutor{}
		tm, ex := holdTmux(mode, inner, func(int) string { return "" })
		if _, ok := tm.holdRead("s"); ok {
			t.Errorf("switch %s: holdRead ok", mode)
		}
		if ex.holdReads() != 0 || len(inner.calls) != 0 {
			t.Errorf("switch %s: %d hold reads, %d other commands, want none", mode, ex.holdReads(), len(inner.calls))
		}
	}
}

// heldNudge runs NudgeSession to session "held" over a claude pane fixture
// under mode, its hold reads answered by row, and returns the commands the
// pane saw, the hold reads taken and its error.
func heldNudge(t *testing.T, mode runtime.ProgramStatusMode, row func(int) string) ([][]string, int, error) {
	t.Helper()
	const session = "held"
	t.Cleanup(func() { sessionNudgeLocks.Delete(session) })
	inner := &agentPaneExecutor{
		session:  session,
		provider: "claude",
		attached: "0",
		pane:     []string{"● Done.", "────", "❯ ", "────", "  ⏵⏵ bypass permissions on"},
	}
	tm, ex := holdTmux(mode, inner, row)
	tm.cfg.NudgeReadyTimeout = 50 * time.Millisecond
	err := tm.NudgeSession(session, "reminder: please respond")
	sessionNudgeLocks.Delete(session)
	return inner.calls, ex.holdReads(), err
}

// typed reports whether calls typed text or keys into a pane.
func typed(calls [][]string) bool {
	return slices.ContainsFunc(calls, func(c []string) bool { return tmuxSubcommand(c) == "send-keys" })
}

// Kills a leaf that holds a nudge on the record: the slice vetoes only a
// starved queue's bypass, so a nudge to a pane whose record says working
// (Codex reports working while a background terminal runs, with no turn)
// types exactly what the switch off types, under shadow and on alike, and
// takes no hold read.
func TestNudgeSessionTypesAsOffWhateverTheRecord(t *testing.T) {
	working := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince)
	row := func(int) string { return holdPaneRow(working, "%1", 0) }
	offCalls, offReads, offErr := heldNudge(t, runtime.ProgramStatusOff, row)
	if offReads != 0 || !typed(offCalls) {
		t.Fatalf("off: %d hold reads, typed=%v; want none and typed", offReads, typed(offCalls))
	}
	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusShadow, runtime.ProgramStatusOn} {
		t.Run(string(mode), func(t *testing.T) {
			calls, reads, err := heldNudge(t, mode, row)
			if reads != 0 {
				t.Fatalf("%d hold reads, want none", reads)
			}
			if (err == nil) != (offErr == nil) || (err != nil && err.Error() != offErr.Error()) {
				t.Fatalf("NudgeSession = %v, off = %v", err, offErr)
			}
			if !slices.EqualFunc(calls, offCalls, slices.Equal) {
				t.Fatalf("the pane saw other commands than off:\n got %q\noff %q", calls, offCalls)
			}
		})
	}
}

// Kills a hold decision that trusts the state cache: a snapshot still
// showing a working record does not hold once the pane's own record says
// idle, and a snapshot showing idle does not release a pane whose record
// says working.
func TestProgramStatusHoldReadPastTheStateCache(t *testing.T) {
	idle := psRecord(t, readNonce, runtime.ProgramIdle, liveSince)
	working := psRecord(t, readNonce, runtime.ProgramWorking, liveSince)
	for _, c := range []struct {
		name          string
		cached, fresh string
		held          bool
	}{
		{"cache working, fresh idle", working, idle, false},
		{"cache idle, fresh working", idle, working, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			table := tapTable(readNonce)
			p, fe := readProvider(runtime.ProgramStatusOn, fetchRows(armedPane(c.cached)), &table)
			if cached, err := p.ProgramStatus("s"); err != nil || !cached.Valid() {
				t.Fatalf("fixture: cached reading %+v, %v", cached, err)
			}
			ex := &holdExecutor{inner: fe, row: func(int) string {
				return strings.Join([]string{"0", "agent", "%5", "1", readNonce, c.fresh, "%5", "0", runtimeInventoryEnd}, "\t")
			}}
			p.tm.exec = ex
			r, held := runtime.ProgramRecordTypingHold(p, "s", time.Now())
			if held != c.held || ex.holdReads() != 1 {
				t.Fatalf("held=%v after %d hold reads (reading %+v), want held=%v after one", held, ex.holdReads(), r.Status, c.held)
			}
		})
	}
}

// Kills an arm that loses the hold declaration or leaves a stale one: a
// holding arm binds the option to the pane, a re-arm that does not hold
// unsets it, a create that does not hold sends today's list, and a disarm
// removes it.
func TestArmCommandsCarryTheHoldDeclaration(t *testing.T) {
	plan := &capturePlan{rand: "r4nd", hold: true}
	set := []string{"set-option", "-p", "-F", "-t", "%5", programHoldOption, "#{pane_id}"}
	unset := []string{"set-option", "-pu", "-t", "%5", programHoldOption}
	contains := func(cmds [][]string, cmd []string) bool {
		return slices.ContainsFunc(cmds, func(c []string) bool { return slices.Equal(c, cmd) })
	}
	for _, clear := range []bool{false, true} {
		cmds := plan.armCommands("%5", clear)
		if !contains(cmds, set) || contains(cmds, unset) {
			t.Errorf("holding arm (clear=%v) %q: want the option bound to the pane", clear, cmds)
		}
		if last := cmds[len(cmds)-1]; tmuxSubcommand(last) != "pipe-pane" {
			t.Errorf("holding arm (clear=%v) does not end with the tap: %q", clear, cmds)
		}
	}
	plan.hold = false
	if cmds := plan.armCommands("%5", true); contains(cmds, set) || !contains(cmds, unset) {
		t.Errorf("non-holding re-arm %q: want the option unset", cmds)
	}
	if cmds := plan.armCommands("%5", false); len(cmds) != 2 {
		t.Errorf("non-holding create arm %q: want only the nonce and the tap", cmds)
	}
	if cmds := disarmCommands("%5"); !contains(cmds, unset) {
		t.Errorf("disarm %q: want the option unset", cmds)
	}
}

// Kills a heal that leaves a live capture's hold declaration stale: a
// healthy capture armed before its provider declared the hold has it set,
// one whose provider withdrew it has it unset, each guarded by the pane's
// own nonce, and a converged capture is left alone.
func TestHealConvergesTheHoldDeclarationOfALiveCapture(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	agent := &healModelPane{session: "s", window: "@1", id: "%5", mark: "agent", pipe: true, tap: "l1vE.%5"}
	server := &healServer{panes: []*healModelPane{agent}, taps: map[string]bool{"l1vE.%5": true}}
	p := healProvider(server, &now)
	holding := runtime.ProgramStatusArm{Capture: true, App: "codex", HoldMidTurn: true}

	mustHeal(t, p, "s", holding, runtime.ProgramCaptureDeclared)
	if agent.hold != "%5" || agent.tap != "l1vE.%5" || !agent.pipe {
		t.Fatalf("after the backfill: %+v, want the declaration bound to %%5 and the capture untouched", agent)
	}
	muts := server.mutations()
	if len(muts) != 1 || muts[0][0] != "if-shell" || muts[0][4] != "#{==:#{"+programTapOption+"},l1vE.%5}" {
		t.Fatalf("mutations %q, want one if-shell guarded by the pane's nonce", muts)
	}
	mustHeal(t, p, "s", holding, runtime.ProgramCaptureUnchanged)
	mustHeal(t, p, "s", healWant, runtime.ProgramCaptureDeclared)
	if agent.hold != "" {
		t.Fatalf("after the withdrawal: hold %q, want unset", agent.hold)
	}

	// A pane re-armed under another nonce between the read and the act
	// keeps the declaration its own arm gave it.
	agent.tap, server.taps = "n3wNonce.%5", map[string]bool{"n3wNonce.%5": true}
	if err := p.tm.declareHold(healPane{programPane: programPane{id: "%5", tap: "l1vE.%5"}, target: "=s:@1.%5"}, true); err != nil {
		t.Fatal(err)
	}
	if agent.hold != "" {
		t.Fatalf("a guarded declaration landed on a pane re-armed under another nonce: hold %q", agent.hold)
	}
}
