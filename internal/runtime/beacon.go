package runtime

import (
	"time"

	"github.com/gastownhall/gascity/internal/progname"
)

// FormatBeacon returns a startup identification string that appears in
// the agent's initial prompt. When an agent crashes and restarts in a
// new session, this beacon makes the predecessor session discoverable
// in tools like Claude Code's /resume picker.
//
// Format: [city-name] agent-name • timestamp
//
// If includePrimeInstruction is true, the beacon also tells the agent
// to run "gc prime" manually. This is needed for non-hook agents that
// won't auto-run gc prime on session restart.
func FormatBeacon(cityName, agentName string, includePrimeInstruction bool) string {
	return FormatBeaconAt(cityName, agentName, includePrimeInstruction, time.Now())
}

// FormatBeaconAt is like FormatBeacon but accepts an explicit time
// for testability.
func FormatBeaconAt(cityName, agentName string, includePrimeInstruction bool, t time.Time) string {
	beacon := "[" + cityName + "] " + agentName + " \u2022 " + t.Format("2006-01-02T15:04:05")
	if includePrimeInstruction {
		beacon += "\n\nRun `" + progname.Get() + " prime` to initialize your context."
	}
	return beacon
}
