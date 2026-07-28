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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/systemdscope"
)

const (
	// managedDoltSliceEnv overrides the systemd user slice that managed dolt
	// sql-servers are placed in. Unset selects managedDoltDefaultSlice. An
	// explicit empty value is rejected by the production placement boundary.
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

	// The reference host observed a 5.7 GiB managed-Dolt peak during the
	// incident window. Eight GiB leaves bounded headroom for that working set,
	// the watchdog, and short compaction bursts without allowing the shared
	// store to consume the whole user slice. MemoryLow reserves two GiB under
	// systemd-oomd pressure; it is protection, not a guarantee, when an
	// ancestor does not enable memory protection.
	managedDoltMemoryMaxBytes            = "8589934592"
	managedDoltMemoryLowBytes            = "2147483648"
	managedDoltManagedOOMPreference      = "avoid"
	managedDoltPlacementOperationTimeout = 5 * time.Second

	// managedDoltOOMScoreAdj is the best-effort badness target. Zero, not
	// negative: a manager-imposed unprivileged floor can still reject lowering
	// to zero, while negative values additionally require CAP_SYS_RESOURCE.
	managedDoltOOMScoreAdj = 0

	// procSelfOOMScoreAdj is the calling process's badness knob. Writing it
	// before spawning the server is how the value reaches the server: it is
	// inherited across fork, and Go's os/exec exposes no pre-exec hook.
	procSelfOOMScoreAdj = "/proc/self/oom_score_adj"
)

// managedDoltSliceWrapper carries the once-only systemd-run availability probe
// for managed dolt spawns. Managed-Dolt callers use WrapRequired and fail
// closed; the shared wrapper's graceful form remains available to other
// process-placement users.
var (
	managedDoltSliceWrapper            = &systemdscope.Wrapper{Label: managedDoltSliceEnv}
	managedDoltSliceManager            = systemdscope.Manager{}
	managedDoltEnsureSlice             = managedDoltSliceManager.EnsureSlice
	managedDoltStartScopeWithProcesses = managedDoltSliceManager.StartScopeWithProcesses
)

// managedDoltSlice resolves the slice for managed dolt spawns.
func managedDoltSlice() string {
	value, ok := os.LookupEnv(managedDoltSliceEnv)
	return managedDoltSliceFor(managedDoltTestModeEnabled(), value, ok)
}

// managedDoltSliceFor is the pure decision behind managedDoltSlice, split out
// for tests (the test binary is always in managed-dolt test mode, so the
// production default is otherwise unreachable in-process).
//
// An explicit setting always wins at resolution time. The production boundary
// rejects an empty result; keeping that case in this pure resolver lets tests
// pin the configuration decision separately from enforcement. Absent a
// setting, test mode declines the implicit default: managed-dolt tests spawn
// fake servers in tight loops and must not depend on a systemd user manager.
func managedDoltSliceFor(testMode bool, envValue string, envSet bool) string {
	if envSet {
		return strings.TrimSpace(envValue)
	}
	if testMode {
		return ""
	}
	return managedDoltDefaultSlice
}

// prepareManagedDoltPlacement applies and verifies the bounded resource policy
// before a managed-Dolt spawn or adoption can be considered safe.
func prepareManagedDoltPlacement(ctx context.Context, slice string) error {
	if err := validateManagedDoltSliceName(slice); err != nil {
		return err
	}
	policy := systemdscope.SlicePolicy{
		MemoryMax:            managedDoltMemoryMaxBytes,
		MemoryLow:            managedDoltMemoryLowBytes,
		ManagedOOMPreference: managedDoltManagedOOMPreference,
	}
	if err := managedDoltEnsureSlice(ctx, slice, policy); err != nil {
		return fmt.Errorf("prepare managed dolt slice %s: %w", slice, err)
	}
	return nil
}

func validateManagedDoltSliceName(slice string) error {
	slice = strings.TrimSpace(slice)
	if !strings.HasSuffix(slice, ".slice") || filepath.Base(slice) != slice {
		return fmt.Errorf("managed dolt slice %q is not a systemd slice unit name", slice)
	}
	base := strings.TrimSuffix(slice, ".slice")
	if base == "" || strings.Contains(base, "-") {
		return fmt.Errorf("managed dolt slice %q is not top-level; systemd hyphens encode nested slices", slice)
	}
	return nil
}

// wrapManagedDoltArgv places a managed dolt spawn in its verified bounded
// slice. Production placement is fail-closed: no unwrapped fallback is
// returned when the slice policy or transient-scope probe fails.
//
// Wrapping is safe for the callers' PID bookkeeping: `systemd-run --scope`
// execs in place, so the PID observed from Cmd.Start is the spawned process's
// own PID, and the start-identity snapshot, termination guards and reaping all
// continue to address the right process.
func wrapManagedDoltArgv(argv []string) ([]string, error) {
	slice := managedDoltSlice()
	_, explicitlyConfigured := os.LookupEnv(managedDoltSliceEnv)
	return wrapManagedDoltArgvFor(argv, slice, explicitlyConfigured)
}

func wrapManagedDoltArgvFor(argv []string, slice string, explicitlyConfigured bool) ([]string, error) {
	if slice == "" {
		// The test suite deliberately declines the production default unless a
		// test opts into real systemd placement. An explicit empty setting is
		// not an opt-out: in production that would recreate the incident path.
		if managedDoltTestModeEnabled() && !explicitlyConfigured {
			return argv, nil
		}
		return nil, fmt.Errorf("managed dolt placement is required; %s is empty", managedDoltSliceEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), managedDoltPlacementOperationTimeout)
	defer cancel()
	if err := prepareManagedDoltPlacement(ctx, slice); err != nil {
		return nil, err
	}
	wrapped, err := managedDoltSliceWrapper.WrapRequired(slice, argv)
	if err != nil {
		return nil, fmt.Errorf("prepare managed dolt spawn: %w", err)
	}
	return wrapped, nil
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

// applyManagedDoltOOMScoreAdj attempts to clear the inherited badness bonus
// from the calling process so a dolt server forked from it inherits the best
// value available to this unprivileged process. It reports the previous value
// — so a caller that must not keep the new one can hand it to
// [restoreManagedDoltOOMScoreAdj] — and whether anything changed.
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
