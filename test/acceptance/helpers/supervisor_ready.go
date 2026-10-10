package acceptancehelpers

import (
	"fmt"
	"strings"
)

// SupervisorStatusConfirmsPID reports whether `gc supervisor status` output
// shows the control socket answering with pid, the supervisor a harness
// started itself. The status command's service-manager and API fallbacks
// ("pid unavailable: ...") do not count: they report a supervisor that holds
// the single-instance lock but has not bound its control socket yet, which
// `gc init` and `gc start` — they probe only the socket — would then try, and
// fail, to start a second time.
func SupervisorStatusConfirmsPID(out string, pid int) bool {
	if pid <= 0 {
		return false
	}
	want := SupervisorStatusPIDLine(pid)
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// SupervisorStatusPIDLine is the line `gc supervisor status` prints when its
// control socket answers with pid, the line SupervisorStatusConfirmsPID
// looks for.
func SupervisorStatusPIDLine(pid int) string {
	return fmt.Sprintf("Supervisor is running (PID %d)", pid)
}
