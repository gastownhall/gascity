package runtime

import (
	"fmt"
	"strings"
)

var (
	_ ProgramStatusProvider = FullFake{}
	_ ProgramCaptureHealer  = FullFake{}

	_ ProgramStatusShadowProvider = FullFake{}
)

// SetProgramPane sets session name's agent-pane row ([Fake.ProgramPanes])
// under the fake's lock.
func (f *Fake) SetProgramPane(name string, row ProgramPaneRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ProgramPanes == nil {
		f.ProgramPanes = map[string]ProgramPaneRow{}
	}
	f.ProgramPanes[name] = row
}

// ProgramPane returns session name's agent-pane row ([Fake.ProgramPanes])
// under the fake's lock, and whether it has one.
func (f *Fake) ProgramPane(name string) (ProgramPaneRow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.ProgramPanes[name]
	return row, ok
}

// ProgramStatus implements [ProgramStatusProvider]: name's agent-pane row
// ([Fake.ProgramPanes]) judged by [JudgeProgramStatus], so a running session
// without a row reads Unread. A session the fake does not run is an error
// wrapping [ErrSessionNotFound], and a broken fake's read one wrapping
// [ErrRuntimeUnavailable].
func (f FullFake) ProgramStatus(name string) (ProgramStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "ProgramStatus", Name: name})
	if err := f.programReadableLocked(name); err != nil {
		return ProgramStatus{}, fmt.Errorf("program status of %q: %w", name, err)
	}
	if err := f.ProgramStatusErrors[name]; err != nil {
		return ProgramStatus{}, err
	}
	return JudgeProgramStatus(f.ProgramPanes[name]), nil
}

// ProgramStatusShadowSink implements [ProgramStatusShadowProvider]: the
// fake's [Fake.ProgramShadowSink], nil unless a test set one.
func (f FullFake) ProgramStatusShadowSink() ProgramStatusComparisonSink {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ProgramShadowSink
}

// ProgramStatusShadowRead implements [ProgramStatusShadowProvider]: name's
// agent-pane row judged as [FullFake.ProgramStatus] judges it, recorded as a
// call of its own so a test tells a fresh read from a ProgramStatus one. ok is
// false unless a test set a shadow sink and that read would succeed.
func (f FullFake) ProgramStatusShadowRead(name string) (ProgramStatus, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "ProgramStatusShadowRead", Name: name})
	if f.ProgramShadowSink == nil || f.programReadableLocked(name) != nil || f.ProgramStatusErrors[name] != nil {
		return ProgramStatus{}, false
	}
	return JudgeProgramStatus(f.ProgramPanes[name]), true
}

// HealProgramCapture implements [ProgramCaptureHealer] over name's agent-pane
// row, recording what it did in the call's Value:
//
//   - want declares no capture and the pane is not armed: unchanged;
//   - a session without a row, or whose pane is dead, is deferred;
//   - want declares no capture: the armed pane is disarmed, its tap and
//     record dropped, and its pipe closed only when the process table shows
//     its own tap alive, since any other pipe may be foreign; while its pipe
//     is open and its tap's liveness is unknown, it is deferred with its
//     nonce kept, since the pipe may be its live tap's;
//   - an armed pane whose pipe is open and tap not gone is unchanged;
//   - any other open pipe (a foreign reader, or a dead tap's pipe not yet
//     closed) is deferred: heal never takes a pipe over;
//   - otherwise the pane is armed under a new nonce naming it, with nothing
//     published yet.
func (f FullFake) HealProgramCapture(name string, want ProgramStatusArm) (ProgramCaptureAction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := Call{Method: "HealProgramCapture", Name: name, Value: "error"}
	defer func() { f.Calls = append(f.Calls, call) }()
	if err := f.programReadableLocked(name); err != nil {
		return "", fmt.Errorf("healing program capture of %q: %w", name, err)
	}
	if err := f.ProgramCaptureHealErrors[name]; err != nil {
		return "", err
	}
	row, ok := f.ProgramPanes[name]
	action := ProgramCaptureDeferred
	switch armed := ok && row.PaneID != "" && strings.HasSuffix(row.Tap, "."+row.PaneID); {
	case !want.Capture && !armed:
		action = ProgramCaptureUnchanged
	case !ok || row.PaneID == "" || row.PaneDead:
	case !want.Capture && row.PanePipe && row.TapLiveness == ProgramTapUnknown:
	case !want.Capture:
		if row.TapLiveness == ProgramTapAlive {
			row.PanePipe = false
		}
		row.Tap, row.TapLiveness = "", ProgramTapUnknown
		row.RecordForm, row.Record = ProgramRecordAbsent, ProgramStatus{}
		action = ProgramCaptureDisarmed
	case armed && row.PanePipe && row.TapLiveness != ProgramTapGone:
		action = ProgramCaptureUnchanged
	case row.PanePipe:
	default:
		f.programArms++
		row.Tap = fmt.Sprintf("heal%d.%s", f.programArms, row.PaneID)
		row.PanePipe, row.TapLiveness = true, ProgramTapAlive
		row.RecordForm, row.Record = ProgramRecordAbsent, ProgramStatus{}
		action = ProgramCaptureArmed
	}
	if action == ProgramCaptureArmed || action == ProgramCaptureDisarmed {
		f.ProgramPanes[name] = row
	}
	call.Value = string(action)
	return action, nil
}

// programReadableLocked reports why name's program status cannot be read:
// the fake is broken, or does not run name. The caller holds f.mu.
func (f FullFake) programReadableLocked(name string) error {
	if f.broken {
		return ErrRuntimeUnavailable
	}
	if _, running := f.sessions[name]; !running {
		return ErrSessionNotFound
	}
	return nil
}
