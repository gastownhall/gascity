//go:build windows

package proctable

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ScanBySessionID returns live agent root processes whose environment carries
// GC_SESSION_ID equal to id. Empty id returns all roots with any GC_SESSION_ID.
//
// A process this user could open but not read is reported as an error
// alongside the roots that were found: it might be the very session being
// looked for, so this unscoped scan cannot claim to have found everything.
// ScanBySessionIDForExecutables limits only unreadable-process uncertainty.
//
// Unlike Linux, where /proc/<pid>/environ freezes the environment a process was
// started with, the Windows environment block read here is the live one:
// SetEnvironmentVariableW writes through the very block the PEB points at, so a
// variable set after startup is visible and no separate merge of this process's
// own environment is needed.
func ScanBySessionID(id string) ([]runtime.LiveRuntime, error) {
	return ScanBySessionIDForExecutables(id, nil)
}

// ScanBySessionIDForExecutables preserves all readable roots, but limits
// unreadable-process errors to the supplied runtime executable names.
// An empty scope retains the strict host-wide completeness check.
func ScanBySessionIDForExecutables(id string, executables []string) ([]runtime.LiveRuntime, error) {
	if err := windowsScanGuard(); err != nil {
		return []runtime.LiveRuntime{}, err
	}
	table, err := readProcessTable()
	if err != nil {
		return []runtime.LiveRuntime{}, err
	}
	return scanTableBySessionID(table, id), table.incompleteForExecutables(executables)
}

// IsScanRoot reports whether pid should be treated as an agent root. A root
// carries a GC_SESSION_ID, is not itself infrastructure — a tmux server or
// client is never a root, whoever its parent is — and sits outside its
// parent's envelope: the parent is gone, carries a different GC_SESSION_ID,
// or is infrastructure.
//
// There is no Windows counterpart to the Linux "pid 1 is always a root" case.
// Nothing adopts orphans here: a process whose parent exits keeps naming the
// dead parent's PID and is reparented to no one, which the envelope test
// already reads as a root.
func IsScanRoot(pid int) bool {
	if err := windowsScanGuard(); err != nil {
		return false
	}
	if pid <= 0 || pid == os.Getpid() {
		return false
	}
	table, err := readProcessTable()
	if err != nil {
		return false
	}
	row, ok := table.byPID[pid]
	if !ok {
		return false
	}
	return isTableScanRoot(table, row)
}

// scanTableBySessionID is the pure half of ScanBySessionID, over an
// already-read process table.
func scanTableBySessionID(table processTable, id string) []runtime.LiveRuntime {
	out := []runtime.LiveRuntime{}
	for _, row := range table.rows {
		if row.pid <= 0 {
			continue
		}
		// Infrastructure is never an agent root, whoever its parent is: a tmux
		// server founded by a session inherits that session's GC_SESSION_ID and
		// outlives the founder, so reporting it as a root hands the orphan
		// sweep the one server every agent in the city shares
		// (gastownhall/gascity#5392).
		if isInfrastructureImage(row.image) {
			continue
		}
		sessionID := row.sessionID()
		if sessionID == "" {
			continue
		}
		if id != "" && sessionID != id {
			continue
		}
		parent, hasParent := table.parent(row)
		if hasParent && parent.sessionID() == sessionID && !isInfrastructureImage(parent.image) {
			continue
		}
		epoch, _ := strconv.Atoi(row.env["GC_RUNTIME_EPOCH"])
		city := row.env["GC_CITY_PATH"]
		if city == "" {
			city = row.env["GC_CITY"]
		}
		out = append(out, runtime.LiveRuntime{
			SessionID: sessionID,
			City:      city,
			Epoch:     epoch,
			PID:       row.pid,
			// The identity this scan saw, so termination can refuse a PID that
			// has since passed to another process.
			StartIdentity: row.startIdentity(),
			PPID:          reportedParentPID(table, row),
			// A parent this scan could not positively identify is not
			// infrastructure: the field reports only what was seen.
			ParentIsProviderInfrastructure: hasParent && isInfrastructureImage(parent.image),
			Name:                           row.image,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].PID < out[j].PID
	})
	return out
}

// isTableScanRoot is the pure half of IsScanRoot.
func isTableScanRoot(table processTable, row windowsProcess) bool {
	if isInfrastructureImage(row.image) {
		return false
	}
	sessionID := row.sessionID()
	if sessionID == "" {
		return false
	}
	parent, ok := table.parent(row)
	return !ok || parent.sessionID() != sessionID || isInfrastructureImage(parent.image)
}

// reportedParentPID is the PPID the scan is willing to stand behind.
//
// It is 0 whenever the parent link could not be confirmed, which on Windows
// also covers the reparenting case Unix reports as PPID 1: a parent that exited
// leaves its PID behind in the child's record, and that number may already
// belong to someone else. Reporting 0 rather than the raw field keeps a caller
// that fences on the parent from fencing on a stranger, and LiveRuntime already
// documents PPID <= 1 as "no longer owned by a provider".
func reportedParentPID(table processTable, row windowsProcess) int {
	if _, ok := table.parent(row); !ok {
		return 0
	}
	return row.ppid
}

// liveWindowsScanAllowedForTesting opts a test into scanning the host's real
// process table.
//
// SetScanRootForTesting cannot serve here. On Linux it CONFINES the scan to an
// injected procfs tree, and that confinement is what makes lifting the #2839
// refusal safe. Windows has no such tree — the only process table is the live
// one — so a test that genuinely needs the scanner must say so explicitly
// instead of borrowing a seam whose safety property it cannot get. It is
// unexported so only this package's own read-only tests can take it; the orphan
// sweep's tests elsewhere keep failing closed.
var liveWindowsScanAllowedForTesting bool

func allowLiveWindowsScanForTesting() (restore func()) {
	previous := liveWindowsScanAllowedForTesting
	liveWindowsScanAllowedForTesting = true
	return func() { liveWindowsScanAllowedForTesting = previous }
}

// hostScanGuard is the refusal every entry point that reads the live host
// process table consults under `go test`. On Windows that is windowsScanGuard,
// which an injected procfs root cannot lift.
func hostScanGuard() error {
	return windowsScanGuard()
}

func windowsScanGuard() error {
	if liveWindowsScanAllowedForTesting || !testing.Testing() {
		return nil
	}
	// scanRoot is deliberately not consulted. Pointing it at a fake procfs
	// confines the Linux scanner, but nothing confines this one, so honouring
	// it would lift the refusal while still reading the real host.
	return fmt.Errorf("proctable: refusing to scan the live Windows process table under go test; " +
		"a test that needs it must opt in with allowLiveWindowsScanForTesting (guards against reaping real agent runtimes — see gastownhall/gascity#2839)")
}
