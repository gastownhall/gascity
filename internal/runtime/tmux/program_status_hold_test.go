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
			r, ok := tm.holdRead("s", fakeAgentPane("s"))
			if ok != c.ok {
				t.Fatalf("holdRead ok = %v, want %v (reading %+v)", ok, c.ok, r)
			}
			if got := ex.holdReads(); got != 1 || tmuxSubcommand(ex.reads[0]) != "display-message" {
				t.Fatalf("hold reads %q, want one display-message", ex.reads)
			}
			if !c.ok {
				return
			}
			if !r.Status.Running() || r.Status.Mode != runtime.ProgramStatusOn || !r.Activity.Equal(time.Unix(activity, 0)) {
				t.Fatalf("reading %+v, want a valid working record under on with the pane's activity", r)
			}
		})
	}
	tm, ex := holdTmux(runtime.ProgramStatusOn, &fakeExecutor{}, func(int) string { return holdPaneRow(working, "%1", activity) })
	if _, ok := tm.holdRead("s", ""); !ok || tmuxSubcommand(ex.reads[0]) != "list-panes" {
		t.Fatalf("a session-name hold read: ok=%v reads %q, want one list-panes of the session", ok, ex.reads)
	}
}

// Kills a hold read taken under a switch that is not on.
func TestHoldReadReadsNothingUnlessOn(t *testing.T) {
	for _, mode := range []runtime.ProgramStatusMode{runtime.ProgramStatusOff, runtime.ProgramStatusShadow} {
		inner := &fakeExecutor{}
		tm, ex := holdTmux(mode, inner, func(int) string { return "" })
		if _, ok := tm.holdRead("s", fakeAgentPane("s")); ok {
			t.Errorf("switch %s: holdRead ok", mode)
		}
		if err := tm.refuseHeldInput("s", ""); err != nil {
			t.Errorf("switch %s: refuseHeldInput = %v", mode, err)
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

// Kills a typing path that types into a held pane, refuses with an error
// that spends an attempt, holds on an idle record or one past its bound, or
// trusts anything but its own fresh read.
func TestNudgeSessionHeldByAFreshRecord(t *testing.T) {
	now := time.Now()
	old := func(s *runtime.ProgramStatus) { s.Since = now.Add(-runtime.ProgramStatusHoldLimit - time.Minute) }
	for _, c := range []struct {
		name               string
		record             string
		activity           int64
		busy, pendingRefus bool
	}{
		{"working", psRecord(t, holdNonce, runtime.ProgramWorking, liveSince), 0, true, false},
		{"blocked", psRecord(t, holdNonce, runtime.ProgramBlocked, liveSince), 0, false, true},
		{"working past its bound", psRecord(t, holdNonce, runtime.ProgramWorking, old), 0, false, false},
		{"working past its bound, drawing", psRecord(t, holdNonce, runtime.ProgramWorking, old), now.Add(-time.Minute).Unix(), true, false},
		{"idle", psRecord(t, holdNonce, runtime.ProgramIdle, liveSince), 0, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls, reads, err := heldNudge(t, runtime.ProgramStatusOn, func(int) string { return holdPaneRow(c.record, "%1", c.activity) })
			if reads != 1 {
				t.Fatalf("%d hold reads, want one", reads)
			}
			held := c.busy || c.pendingRefus
			if errors.Is(err, runtime.ErrSessionBusy) != c.busy || errors.Is(err, runtime.ErrPendingInteraction) != c.pendingRefus {
				t.Fatalf("NudgeSession = %v, want busy=%v pending=%v", err, c.busy, c.pendingRefus)
			}
			if typed(calls) == held {
				t.Fatalf("held=%v but typed=%v: %q", held, typed(calls), calls)
			}
		})
	}
}

// Kills a hold that changes anything under off or shadow, or for a pane
// that does not declare it: the pane sees exactly the commands the switch
// off sends, and only on reads the pane once more.
func TestNudgeSessionUnheldSendsWhatOffSends(t *testing.T) {
	working := psRecord(t, holdNonce, runtime.ProgramWorking, liveSince)
	offCalls, offReads, offErr := heldNudge(t, runtime.ProgramStatusOff, func(int) string { return holdPaneRow(working, "%1", 0) })
	if offReads != 0 || !typed(offCalls) {
		t.Fatalf("off: %d hold reads, typed=%v; want none and typed", offReads, typed(offCalls))
	}
	for _, c := range []struct {
		name  string
		mode  runtime.ProgramStatusMode
		hold  string
		reads int
	}{
		{"shadow, declared", runtime.ProgramStatusShadow, "%1", 0},
		{"on, undeclared", runtime.ProgramStatusOn, "", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			calls, reads, err := heldNudge(t, c.mode, func(int) string { return holdPaneRow(working, c.hold, 0) })
			if reads != c.reads {
				t.Fatalf("%d hold reads, want %d", reads, c.reads)
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
			r, held := runtime.ProgramRecordHold(p, "s", time.Now())
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
