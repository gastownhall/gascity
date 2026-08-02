package main

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// Liveness scan sources, recorded on liveWorktreeState so an operator can tell
// which mechanism produced the result rather than inferring it from the host.
const (
	liveScanSourceProc = "proc"
	liveScanSourceLsof = "lsof"
)

// liveScanFallbackTimeout bounds the fallback enumeration. The reaper runs on
// the controller tick, so a process-table query that hangs must not stall it. A
// timeout yields no records, which the caller treats as an indeterminate scan
// and fails closed on — the same posture as a missing /proc.
const liveScanFallbackTimeout = 20 * time.Second

// liveWorktreeCwdEnumerator lists the working directory of every process this
// user can see, in lsof field output: "p<pid>" per process, "f<fd>" per
// descriptor, and "n<path>" for the path. Indirected through a var so the parser
// and the fail-closed rules are unit-testable on any platform, without a process
// table and without lsof installed.
//
// The 0 in -F0 makes lsof NUL-terminate each field. Without it a path containing
// a newline would split across two lines and be silently truncated, and a
// truncated cwd no longer matches the worktree it is inside — the failure mode
// would be under-protection in a code path that deletes directories.
// It routes through lsofOutputWithTimeout so this call gets the same hardening
// as every other lsof invocation in the package — a WaitDelay and a
// process-group kill on cancel — without which the deadline below bounds only
// the wait, not the child, and a wedged lsof stalls the controller tick anyway.
var liveWorktreeCwdEnumerator = func() ([]byte, error) {
	// -a -d cwd restricts the listing to current-working-directory descriptors,
	// which is the only descriptor class this gate cares about and keeps the
	// output small enough to parse on every tick.
	return lsofOutputWithTimeout(liveScanFallbackTimeout, "-a", "-d", "cwd", "-F0pn")
}

// lsofErrAnnotation matches the per-process errors lsof reports inside the n
// field itself, as an absolute-looking string
// ("/proc/1/cwd (readlink: Permission denied)"). These pass the absolute-path
// filter and normalize non-empty, so counting them would make an unreadable scan
// look like a successful one and defeat the empty-listing rule below — on a host
// where lsof can read nothing, every record is one of these.
var lsofErrAnnotation = regexp.MustCompile(`\s\((?:readlink|stat|lstat|opendir|getcwd)[^)]*: [^)]*\)$`)

// collectLiveWorktreeStateFallback enumerates process working directories on a
// host that has no /proc, so the liveness gate has a real signal there instead
// of a permanent "indeterminate".
//
// It is strictly additive: every path that reaches it would otherwise have
// returned scanned=false, so it can only turn "protect everything, forever"
// into a usable scan. It can never authorize a removal the /proc path would
// have refused, and on a host with /proc it is not reached at all.
//
// Two rules, both pinned by tests:
//
//   - No records at all means the enumeration FAILED, not that the host is
//     idle. A running machine always has processes with a working directory, so
//     an empty listing yields scanned=false and the caller protects everything.
//     This also covers lsof being absent, which is why its absence degrades to
//     today's behavior rather than to a wrong answer.
//   - Records alongside an ordinary non-zero exit is a PARTIAL scan, and counts.
//     lsof cannot read other users' descriptors unprivileged; it warns and lists
//     the rest. The /proc path has the identical blind spot — os.Readlink on
//     another user's /proc/<pid>/cwd fails with EACCES, that pid is skipped, and
//     the scan still reports scanned=true — so a partial listing is treated the
//     same way on both platforms.
//
// A deadline is the one error that is consulted. Truncation at an arbitrary
// point is not the same bounded blind spot as EACCES on processes this user does
// not own: the records that never arrived are unrelated to permissions, so the
// listing carries no rule about what it omitted. That fails closed.
func collectLiveWorktreeStateFallback() liveWorktreeState {
	out, err := liveWorktreeCwdEnumerator()
	if errors.Is(err, context.DeadlineExceeded) {
		return liveWorktreeState{scanned: false}
	}

	seen := make(map[string]struct{})
	var cwds []string
	// Fields are NUL-terminated; records are newline-separated, so the first
	// field after a record boundary carries a leading newline to trim.
	for _, field := range strings.Split(string(out), "\x00") {
		// Only "n" fields carry a path; "p"/"f" identify the process and
		// descriptor.
		path, ok := strings.CutPrefix(strings.Trim(field, "\r\n"), "n")
		if !ok || !strings.HasPrefix(path, "/") {
			continue
		}
		if lsofErrAnnotation.MatchString(path) {
			continue
		}
		canon := pathutil.NormalizePathForCompare(path)
		if canon == "" {
			continue
		}
		if _, dup := seen[canon]; dup {
			continue
		}
		seen[canon] = struct{}{}
		cwds = append(cwds, canon)
	}

	if len(cwds) == 0 {
		return liveWorktreeState{scanned: false}
	}
	return liveWorktreeState{cwds: cwds, scanned: true, source: liveScanSourceLsof}
}
