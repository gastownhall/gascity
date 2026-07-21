package main

// Systemd slice placement and OOM hardening for managed dolt sql-servers.
//
// gc auto-starts the managed server from whichever process first needs it —
// the supervisor, a control-dispatcher pane, an interactive command. Nothing
// placed that process tree in a cgroup of its own, so it inherited the
// starter's, and the shared bead store for every rig in the city ended up
// wherever the race happened to land it. Observed on one host: the canonical
// server sat in the agent slice, was OOM-killed four times in a single morning
// once that slice reached its MemoryMax, and respawned on a different port
// each time, opening the city-wide dolt circuit breaker.
//
// Two independent inheritances caused that, and both are corrected here.
//
// Cgroup: the server is placed in its own top-level slice, so a memory
// blow-out among agents can no longer select it — the kernel's memcg OOM
// killer only considers tasks inside the cgroup that hit its limit. The slice
// must NOT be a descendant of the agent slice for this to hold; see
// managedDoltDefaultSlice.
//
// oom_score_adj: systemd's user manager applies DefaultOOMScoreAdjust=200 to
// the units under it, and that value is inherited by every descendant process.
// On a 62 GiB host it adds a ~12.5 GiB-equivalent bonus to the kernel's
// badness score, so a ~1 GiB dolt was ranked as though it were ~13 GiB and
// picked ahead of genuinely large processes. Resetting it to 0 is legal for an
// unprivileged process; negative values need CAP_SYS_RESOURCE.

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/runtime/systemdscope"
)

const (
	// managedDoltSliceEnv overrides the systemd user slice that managed dolt
	// sql-servers are placed in. Unset selects managedDoltDefaultSlice; set to
	// the empty string disables cgroup placement.
	//
	// It disables placement ONLY. The oom_score_adj hardening is independent
	// and stays on, because it is gated by GC_DOLT_SCOPE_WATCHDOG instead —
	// so restoring the full pre-fix spawn takes GC_DOLT_SLICE="" *and*
	// GC_DOLT_SCOPE_WATCHDOG=0, not this knob alone.
	managedDoltSliceEnv = "GC_DOLT_SLICE"

	// managedDoltDefaultSlice is the slice managed dolt servers are placed in
	// when the operator expresses no preference.
	//
	// The name is deliberately flat and deliberately NOT prefixed with the
	// name of the agent slice's parent. systemd derives slice nesting from the
	// unit name — "a-b.slice" is a child of "a.slice" — so naming this
	// "gascity-dolt.slice" would nest it under gascity.slice and leave the
	// server inside the very memcg whose MemoryMax breach kills it. A sibling
	// slice is the whole point; TestManagedDoltDefaultSliceEscapesTheAgentMemcg
	// guards the property.
	managedDoltDefaultSlice = "gcdolt.slice"

	// managedDoltOOMScoreAdj is the badness adjustment managed dolt servers
	// are pinned to. Zero, not negative: lowering toward 0 is permitted for
	// unprivileged processes, while any negative value requires
	// CAP_SYS_RESOURCE and would fail on the deployments this runs in.
	managedDoltOOMScoreAdj = 0

	// procSelfOOMScoreAdj is the calling process's badness knob. Writing it
	// before spawning the server is how the value reaches the server: it is
	// inherited across fork, and Go's os/exec exposes no pre-exec hook.
	procSelfOOMScoreAdj = "/proc/self/oom_score_adj"
)

// managedDoltSliceWrapper carries the once-only systemd-run availability probe
// for managed dolt spawns. A host without systemd-run or without a reachable
// user bus warns once and spawns unwrapped — placement is hardening, and must
// never be the reason the bead store fails to start.
var managedDoltSliceWrapper = &systemdscope.Wrapper{Label: managedDoltSliceEnv}

// managedDoltSlice resolves the slice for managed dolt spawns.
func managedDoltSlice() string {
	value, ok := os.LookupEnv(managedDoltSliceEnv)
	return managedDoltSliceFor(managedDoltTestModeEnabled(), value, ok)
}

// managedDoltSliceFor is the pure decision behind managedDoltSlice, split out
// for tests (the test binary is always in managed-dolt test mode, so the
// production default is otherwise unreachable in-process).
//
// An explicit setting always wins, including an explicit empty value, so a
// test can opt into real placement and an operator can opt out. Absent one,
// test mode declines the implicit default: managed-dolt tests spawn fake
// servers in tight loops and must not depend on a systemd user manager, nor
// pay the probe timeout on hosts without one.
func managedDoltSliceFor(testMode bool, envValue string, envSet bool) string {
	if envSet {
		return strings.TrimSpace(envValue)
	}
	if testMode {
		return ""
	}
	return managedDoltDefaultSlice
}

// wrapManagedDoltArgv places a managed dolt spawn in its slice, returning argv
// unchanged when placement is disabled or unavailable.
//
// Wrapping is safe for the callers' PID bookkeeping: `systemd-run --scope`
// execs in place, so the PID observed from Cmd.Start is the spawned process's
// own PID, and the start-identity snapshot, termination guards and reaping all
// continue to address the right process.
func wrapManagedDoltArgv(argv []string) []string {
	return managedDoltSliceWrapper.Wrap(managedDoltSlice(), argv)
}

// managedDoltOOMScoreAdjNeedsLowering reports whether the current badness
// adjustment is above the target. Lowering is the only direction ever applied,
// so a value already at or below the target is left alone and an operator who
// deliberately protected the server further is not overridden.
func managedDoltOOMScoreAdjNeedsLowering(current int) bool {
	return current > managedDoltOOMScoreAdj
}

// managedDoltOOMScoreAdjMu serializes the lower/spawn/restore sequence on the
// watchdog-free path. oom_score_adj is per-process, and one supervisor process
// drives several cities, so two concurrent spawns could otherwise interleave
// such that the second server inherits the value the first restored.
var managedDoltOOMScoreAdjMu sync.Mutex

// applyManagedDoltOOMScoreAdj clears the inherited badness bonus from the
// calling process so a dolt server forked from it inherits a neutral
// oom_score_adj. It reports the previous value — so a caller that must not keep
// the new one can hand it to [restoreManagedDoltOOMScoreAdj] — and whether anything
// changed.
//
// The knob only exists on the calling process and is inherited across fork, and
// Go's os/exec offers no pre-exec hook, so writing it here is the only way to
// reach the child. Errors are returned rather than swallowed: callers log them,
// but none treats a failure as fatal, because failing to harden the server is
// never a reason to refuse to start it. Unavailable on non-Linux hosts and in
// restricted sandboxes.
func applyManagedDoltOOMScoreAdj() (previous int, changed bool, err error) {
	raw, err := os.ReadFile(procSelfOOMScoreAdj)
	if err != nil {
		return 0, false, fmt.Errorf("read %s: %w", procSelfOOMScoreAdj, err)
	}
	current, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false, fmt.Errorf("parse %s: %w", procSelfOOMScoreAdj, err)
	}
	if !managedDoltOOMScoreAdjNeedsLowering(current) {
		return current, false, nil
	}
	if err := os.WriteFile(procSelfOOMScoreAdj, []byte(strconv.Itoa(managedDoltOOMScoreAdj)), 0o644); err != nil {
		return current, false, fmt.Errorf("write %s: %w", procSelfOOMScoreAdj, err)
	}
	return current, true, nil
}

// restoreManagedDoltOOMScoreAdj puts back a value captured by applyManagedDoltOOMScoreAdj.
// It cannot fail for want of privilege: the captured value is by construction
// higher than the target, and raising oom_score_adj is always permitted.
func restoreManagedDoltOOMScoreAdj(previous int) error {
	if err := os.WriteFile(procSelfOOMScoreAdj, []byte(strconv.Itoa(previous)), 0o644); err != nil {
		return fmt.Errorf("restore %s to %d: %w", procSelfOOMScoreAdj, previous, err)
	}
	return nil
}
