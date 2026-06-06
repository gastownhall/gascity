// Package nudgepoller defines the shared nudge poller process contract.
package nudgepoller

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/internal/pidutil"
)

const (
	cityFlag    = "--city"
	sessionFlag = "--session"
)

// CommandArgs returns the argv tail for a nudge poller.
func CommandArgs(cityPath, sessionName, agentName string) []string {
	return []string{"nudge", "poll", cityFlag, cityPath, sessionFlag, sessionName, agentName}
}

// CmdlineMatcher returns a predicate that recognizes the nudge poller command
// for the supplied city, session, and target key.
func CmdlineMatcher(cityPath, sessionName, agentName string) func([]string) bool {
	expectedCity := pathutil.NormalizePathForCompare(cityPath)
	expectedAgent := strings.TrimSpace(agentName)
	return func(argv []string) bool {
		if expectedCity == "" || sessionName == "" || expectedAgent == "" {
			return false
		}
		if !pidutil.ArgvContainsSequence(argv, "nudge", "poll") {
			return false
		}
		if !argvHasPathFlagValue(argv, cityFlag, expectedCity) {
			return false
		}
		if !pidutil.ArgvHasFlagValue(argv, sessionFlag, sessionName) {
			return false
		}
		return argvHasPollTarget(argv, expectedAgent)
	}
}

func argvHasPollTarget(argv []string, expected string) bool {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] != "nudge" || argv[i+1] != "poll" {
			continue
		}
		for j := i + 2; j < len(argv); j++ {
			arg := argv[j]
			switch {
			case arg == cityFlag || arg == sessionFlag || arg == "--interval" || arg == "--quiescence":
				j++
			case strings.HasPrefix(arg, cityFlag+"=") ||
				strings.HasPrefix(arg, sessionFlag+"=") ||
				strings.HasPrefix(arg, "--interval=") ||
				strings.HasPrefix(arg, "--quiescence="):
			case strings.HasPrefix(arg, "-"):
				if !strings.Contains(arg, "=") && j+1 < len(argv) && !strings.HasPrefix(argv[j+1], "-") {
					j++
				}
			default:
				return arg == expected
			}
		}
		return false
	}
	return false
}

// PollerFileStem returns the filesystem-safe stem for poller PID and log
// files owned by a concrete session/target tuple.
func PollerFileStem(sessionName, agentName string) string {
	sessionName = strings.TrimSpace(sessionName)
	agentName = strings.TrimSpace(agentName)
	digest := sha256.Sum256([]byte(sessionName + "\x00" + agentName))
	prefix := safeFileStemPart(sessionName)
	if prefix == "" {
		prefix = "session"
	}
	return prefix + "-" + hex.EncodeToString(digest[:8])
}

func safeFileStemPart(value string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(value) {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.Trim(b.String(), ".-")
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
