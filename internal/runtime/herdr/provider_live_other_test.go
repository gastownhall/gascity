//go:build !windows

package herdr

// TestProviderLive's command, the output it puts on screen, and the process
// names it runs under, for this platform's pane shell.
const (
	liveTickCommand = `for i in $(seq 1 60); do echo "tick $i"; sleep 1; done`
	liveTickOutput  = "tick"
)

var liveProcessNames = []string{"sleep", "sh", "bash"}

// liveSwapCommand is TestProviderLiveOccupantSwapKeepsLiveness's command: the
// launch shell execs into another process, the occupant swap it is named for.
const liveSwapCommand = `exec sleep 120`
