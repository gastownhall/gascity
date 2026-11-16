//go:build windows

package herdr

// A Copilot CLI on Windows can be reported idle while it is still loading its
// instructions, plugins, hooks and skills, and keystrokes it receives then are
// lost. Measured live with Copilot CLI 1.0.91 and herdr 0.9.1: the first turn
// was typed during the load, herdr reported the stall 5.3 s later, and the
// recovery Enter that followed at once reached Copilot 3.3 s before it opened
// its session, so it was lost too. So on Windows the recovery Enter is
// retried after a short confirming wait, for as long as herdr still reports
// the agent idle. A wait of 6000 ms gives a submitted turn time to show as
// working.
const (
	startupEnterRecoveryAttempts = 10
	startupEnterRecoveryWaitMS   = 6000
)

// herdr reports a stalled first turn only after it observed no working or
// blocked state, with the agent idle ("current status is idle", measured with
// herdr 0.9.1). An agent herdr then reports working or done has taken the
// turn since, as a Copilot CLI does when it finishes loading after the stall,
// so the recovery ends there without a keystroke. Blocked is left out: an
// agent already parked on a dialog when its first turn arrived shows no state
// change either, and reads the same as one the turn put on a dialog, so the
// recovery withholds its Enter and reports the turn unconfirmed. After a
// recovery Enter, which the agent took from idle, every confirming state ends
// the recovery.
var startupFirstCheckConfirmStates = []string{"working", "done"}
