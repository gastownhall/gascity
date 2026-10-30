package main

// Leaked isolated test-supervisor process sweep (ga-rlnmsp).
//
// The service-file sweep in cmd_supervisor_stale_services.go removes unit
// files whose GC_HOME is gone, but a supervisor that an integration test
// spawned directly (no service file) outlives its deleted temp tree too: the
// drift-restart path under test replaces the harness-launched supervisor with
// a detached one that no test ever learns the PID of. This sweep finds those
// `supervisor run` processes by process-table identity and SIGTERMs them.
//
// Selection is deliberately narrow, so the real supervisor can never match:
// argv must be exactly `<abs-path>/gc-int-env-*/.../<binary> supervisor run`
// with a clean, absolute argv[0] under a known temp root (the unsuffixed
// /home/.../go/bin/gc is not under one), and the scope must be PROVABLY gone
// (cwd readlink ends in " (deleted)", or GC_HOME from the process environment
// definitively does not exist). Unknown state is never a reap signal. Identity
// is re-read from /proc immediately before signaling, and a process with no
// start-time tick is never signaled because PID reuse cannot be ruled out.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// leakedSupervisorTestEnvPrefix is the directory prefix the integration
// harness gives every isolated environment root (os.MkdirTemp("", ...)).
const leakedSupervisorTestEnvPrefix = "gc-int-env-"

// leakedSupervisorProc is one `supervisor run` candidate read from /proc.
// CWDState and GCHomeState use the procPathState* values.
type leakedSupervisorProc struct {
	PID            int
	Argv           []string
	CWDState       string
	GCHomeState    string
	StartTimeTicks uint64
}

// leakedSupervisorOps is the process-table surface the sweep uses; tests
// replace it, production uses defaultLeakedSupervisorOps.
type leakedSupervisorOps struct {
	// Discover lists every live `supervisor run` process.
	Discover func() ([]leakedSupervisorProc, error)
	// Reread re-reads one process from the process table, reporting false
	// when it is gone or no longer a `supervisor run`.
	Reread func(pid int) (leakedSupervisorProc, bool)
	// Signal delivers sig to pid.
	Signal func(pid int, sig syscall.Signal) error
}

// sweepLeakedTestSupervisorsFn is the Linux leaked-process sweep run by
// sweepStaleIsolatedSupervisorServices; a variable so tests can observe the
// wiring without touching the host process table.
var sweepLeakedTestSupervisorsFn = func(stderr io.Writer) {
	sweepLeakedTestSupervisors(stderr, defaultLeakedSupervisorOps(), os.TempDir())
}

// classifyLeakedTestSupervisor reports whether p is a leaked isolated test
// supervisor that is safe to SIGTERM, with the reason for either answer.
func classifyLeakedTestSupervisor(p leakedSupervisorProc, tempDir string) (bool, string) {
	if len(p.Argv) < 3 || p.Argv[1] != "supervisor" || p.Argv[2] != "run" {
		return false, "not a `supervisor run` process"
	}
	if !underTestEnvDir(p.Argv[0], []string{"/tmp", tempDir}) {
		return false, fmt.Sprintf("binary %q is not under a %s* test environment", p.Argv[0], leakedSupervisorTestEnvPrefix)
	}
	switch {
	case p.CWDState == procPathStateDeleted:
		return true, "working directory deleted (test environment removed)"
	case p.GCHomeState == procPathStateDeleted:
		return true, "GC_HOME deleted (test environment removed)"
	}
	return false, "test environment still present or its state is unknown"
}

// underTestEnvDir reports whether argv0 is a clean absolute path to a file
// inside a gc-int-env-* directory that is a direct child of one of roots.
func underTestEnvDir(argv0 string, roots []string) bool {
	if !filepath.IsAbs(argv0) || filepath.Clean(argv0) != argv0 {
		return false
	}
	for _, root := range roots {
		if root == "" || filepath.Clean(root) == string(filepath.Separator) {
			continue
		}
		rootPrefix := filepath.Clean(root) + string(filepath.Separator)
		if !strings.HasPrefix(argv0, rootPrefix) {
			continue
		}
		child := strings.TrimPrefix(argv0, rootPrefix)
		dir, _, nested := strings.Cut(child, string(filepath.Separator))
		if nested && strings.HasPrefix(dir, leakedSupervisorTestEnvPrefix) {
			return true
		}
	}
	return false
}

// sweepLeakedTestSupervisors SIGTERMs every leaked test supervisor and
// returns the PIDs it signaled. Failures are warnings on stderr; the sweep
// never blocks its caller.
func sweepLeakedTestSupervisors(stderr io.Writer, ops leakedSupervisorOps, tempDir string) []int {
	procs, err := ops.Discover()
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor: warning: scanning for leaked test supervisors: %v\n", err) //nolint:errcheck // best-effort stderr
		return nil
	}
	var reaped []int
	for _, p := range procs {
		reap, reason := classifyLeakedTestSupervisor(p, tempDir)
		if !reap || p.StartTimeTicks == 0 {
			continue
		}
		fresh, ok := ops.Reread(p.PID)
		if !ok || fresh.StartTimeTicks != p.StartTimeTicks {
			continue
		}
		if again, _ := classifyLeakedTestSupervisor(fresh, tempDir); !again {
			continue
		}
		if err := ops.Signal(p.PID, syscall.SIGTERM); err != nil {
			if !errors.Is(err, syscall.ESRCH) {
				fmt.Fprintf(stderr, "gc supervisor: warning: signaling leaked test supervisor pid %d: %v\n", p.PID, err) //nolint:errcheck // best-effort stderr
			}
			continue
		}
		fmt.Fprintf(stderr, "gc supervisor: stopped leaked test supervisor pid %d (%s)\n", p.PID, reason) //nolint:errcheck // best-effort stderr
		reaped = append(reaped, p.PID)
	}
	return reaped
}

// defaultLeakedSupervisorOps reads the real /proc.
func defaultLeakedSupervisorOps() leakedSupervisorOps {
	return leakedSupervisorOps{
		Discover: discoverLeakedSupervisorProcs,
		Reread:   readLeakedSupervisorProc,
		Signal:   syscall.Kill,
	}
}

// discoverLeakedSupervisorProcs lists live `supervisor run` processes other
// than the caller. A host without /proc yields none.
func discoverLeakedSupervisorProcs() ([]leakedSupervisorProc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil //nolint:nilerr // no /proc (non-Linux): nothing to sweep
	}
	var out []leakedSupervisorProc
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if p, ok := readLeakedSupervisorProc(pid); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// readLeakedSupervisorProc reads pid from /proc, reporting false when it is
// gone or its argv is not `<bin> supervisor run`.
func readLeakedSupervisorProc(pid int) (leakedSupervisorProc, bool) {
	pidDir := filepath.Join("/proc", strconv.Itoa(pid))
	data, err := readWithTimeout(filepath.Join(pidDir, "cmdline"))
	if err != nil {
		return leakedSupervisorProc{}, false
	}
	argv := splitCmdline(data)
	if len(argv) < 3 || argv[1] != "supervisor" || argv[2] != "run" {
		return leakedSupervisorProc{}, false
	}
	return leakedSupervisorProc{
		PID:            pid,
		Argv:           argv,
		CWDState:       doltProcCWDState(pid),
		GCHomeState:    procGCHomeState(pidDir),
		StartTimeTicks: readProcStartTimeTicks(pid),
	}, true
}

// procGCHomeState classifies the GC_HOME in a process's environment: deleted
// only when an absolute path definitively no longer exists; unknown when the
// environment is unreadable (other uid), GC_HOME is unset or relative, or the
// stat fails any other way.
func procGCHomeState(pidDir string) string {
	data, err := readWithTimeout(filepath.Join(pidDir, "environ"))
	if err != nil {
		return procPathStateUnknown
	}
	for _, kv := range strings.Split(string(data), "\x00") {
		if v, ok := strings.CutPrefix(kv, "GC_HOME="); ok {
			if !filepath.IsAbs(v) {
				return procPathStateUnknown
			}
			return statConfigPathState(v)
		}
	}
	return procPathStateUnknown
}
