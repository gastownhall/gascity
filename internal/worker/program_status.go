package worker

import "github.com/gastownhall/gascity/internal/runtime"

// ProgramStatusOf reads the judged OSC 7501 program status of runtime session
// sessionName from the backend serving it. A reading the runtime cannot take
// (no such capability, a missing session, an unobservable runtime) is the
// Unread reading: it carries no state, so a consumer reads the session's
// program status as unknown, never as idle. tmux serves the reading from its
// shared state cache, so this forks nothing per call.
func ProgramStatusOf(sp runtime.Provider, sessionName string) runtime.ProgramStatus {
	ps, err := runtime.ReadProgramStatus(sp, sessionName)
	if err != nil {
		return runtime.ProgramStatus{}
	}
	return ps
}

// ProgramPhase is the worker phase a program-status reading decides, and
// whether it decides one (WC-PSTAT-002). Only a reading that Decides (valid,
// taken under the city switch's on mode) does: working is Busy, blocked is
// Blocked, and the input boundaries idle, done and error are Ready (error is
// a failed turn with the program alive, never Failed). A shadow reading, or
// one that is not valid, decides nothing and the phase stays today's.
func ProgramPhase(ps runtime.ProgramStatus) (Phase, bool) {
	if !ps.Decides() {
		return "", false
	}
	switch {
	case ps.Running():
		return PhaseBusy, true
	case ps.Blocked():
		return PhaseBlocked, true
	case ps.InputBoundary():
		return PhaseReady, true
	default:
		return "", false
	}
}
