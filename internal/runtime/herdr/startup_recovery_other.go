//go:build !windows

package herdr

// The stall recovery is one Enter and one confirming wait of the whole boot
// budget.
const (
	startupEnterRecoveryAttempts = 1
	startupEnterRecoveryWaitMS   = startupStallRecoveryTimeoutMS
)

// No state ends the recovery before its Enter: the first check proceeds only
// on an idle agent.
var startupFirstCheckConfirmStates []string
