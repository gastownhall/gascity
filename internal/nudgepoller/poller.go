// Package nudgepoller defines the shared nudge poller process contract.
package nudgepoller

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"os"
	"path/filepath"
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

// IsGoTestExecutable reports whether path names a Go test binary, and must gate
// every spawn that re-executes this program as a poller.
//
// A Go test binary ignores unrecognized positional arguments, so re-executing
// one with CommandArgs does not start a poller: it starts the whole test suite
// again, with no -test.timeout to bound it. Each of those runs reaches the
// spawn path again, so the fan-out is exponential rather than a single stray
// process.
//
// The two answers are not equally costly, and the predicate is deliberately
// biased. Missing a test binary ends in that fan-out; refusing a production
// binary only means a poller does not start. So an uncertain case is refused.
func IsGoTestExecutable(path string) bool {
	// The authoritative answer, available whenever path names this very
	// program — which is what both spawn paths pass, since they re-execute
	// themselves. A Go test binary registers the testing package's flags no
	// matter what the file is called, and no naming rule can match that:
	// `go test -o api-tests.exe` produces a test binary with no ".test" in its
	// name at all, and bazel produces one named <target>_test.
	if runningGoTest() && isThisExecutable(path) {
		return true
	}

	// Past here the path names some other file, which can only be judged by
	// how `go test` names its output.
	base := filepath.Base(path)
	// A trailing ".exe" means Windows naming, where the filesystem is
	// case-insensitive and "Api.Test.EXE" and "api.test.exe" are the same
	// file. The ".test" suffix has to be compared the same way, or the
	// guard misses the spellings this trimming exists to normalize. Unix
	// filenames are case-sensitive, so an exact match is correct there.
	if ext := filepath.Ext(base); strings.EqualFold(ext, ".exe") {
		base = strings.TrimSuffix(base, ext)
		if hasSuffixFold(base, ".test") {
			return true
		}
	} else if strings.HasSuffix(base, ".test") {
		return true
	}
	// A bazel test binary drops the .test suffix but keeps the <target>_test
	// name and exports TEST_SRCDIR. Both markers are required so a probe that
	// execs a renamed spy binary is not mistaken for the test binary itself.
	return os.Getenv("TEST_SRCDIR") != "" && isThisExecutable(path) && strings.HasSuffix(base, "_test")
}

// runningGoTest reports whether this process is a Go test binary.
//
// The testing package registers its -test.* flags while the binary starts, so
// their presence identifies it whatever the file on disk is called. Reading the
// flag rather than importing testing keeps the test framework out of the
// production binary.
//
// It is a variable so a test can describe the case it cannot otherwise reach:
// every test runs inside a test binary, so the false branch is unobservable.
var runningGoTest = func() bool { return flag.Lookup("test.v") != nil }

// isThisExecutable reports whether path names the running program's own file.
//
// The comparison is by file identity rather than by string because the same
// file arrives spelled differently: os.Args[0] carries whatever the caller
// typed, os.Executable reports a resolved absolute path, and on Windows the
// two can also differ in case or by one being a short name. Comparing the
// spellings answered "no" for a file that was plainly the same one.
func isThisExecutable(path string) bool {
	self, err := os.Executable()
	if err != nil {
		return false
	}
	if path == self {
		return true
	}
	selfInfo, err := os.Stat(self)
	if err != nil {
		return false
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(selfInfo, pathInfo)
}

// hasSuffixFold reports whether s ends in suffix under case-insensitive
// comparison.
func hasSuffixFold(s, suffix string) bool {
	return len(s) >= len(suffix) && strings.EqualFold(s[len(s)-len(suffix):], suffix)
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
