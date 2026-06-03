// Package nudgepoller defines the shared per-session nudge poller process
// contract.
package nudgepoller

import (
	"strings"

	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/pidutil"
)

const (
	cityFlag    = "--city"
	sessionFlag = "--session"
)

// CommandArgs returns the argv tail for the shared per-session nudge poller.
func CommandArgs(cityPath, sessionName, agentName string) []string {
	return []string{"nudge", "poll", cityFlag, cityPath, sessionFlag, sessionName, agentName}
}

// CmdlineMatcher returns a predicate that recognizes the shared per-session
// nudge poller command for the supplied city and session.
func CmdlineMatcher(cityPath, sessionName string) func([]string) bool {
	expectedCity := pathutil.NormalizePathForCompare(cityPath)
	return func(argv []string) bool {
		if expectedCity == "" || sessionName == "" {
			return false
		}
		if !pidutil.ArgvContainsSequence(argv, "nudge", "poll") {
			return false
		}
		if !argvHasPathFlagValue(argv, cityFlag, expectedCity) {
			return false
		}
		return pidutil.ArgvHasFlagValue(argv, sessionFlag, sessionName)
	}
}

func argvHasPathFlagValue(argv []string, flag, expected string) bool {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) {
			if pathutil.NormalizePathForCompare(argv[i+1]) == expected {
				return true
			}
		}
		if strings.HasPrefix(arg, flag+"=") {
			if pathutil.NormalizePathForCompare(strings.TrimPrefix(arg, flag+"=")) == expected {
				return true
			}
		}
	}
	return false
}
