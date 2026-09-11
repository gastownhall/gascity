package proctable

// ProcessRecord is one process in a host-wide snapshot used for descendant
// liveness matching.
type ProcessRecord struct {
	PID       int
	PPID      int
	Name      string // basename of the process's command
	SessionID string // GC_SESSION_ID from the process's env, if present ("" if absent/unreadable)
}

// SnapshotProcesses returns a host-wide process snapshot (pid, ppid, command
// basename) for descendant-liveness matching. Unlike ScanBySessionID, this is
// a plain read of the process table with no GC_SESSION_ID filtering and no
// liveScanGuard: it powers read-only liveness checks (e.g. a runtime
// provider's ProcessAlive), not the orphan sweep that guard protects against.
func SnapshotProcesses() ([]ProcessRecord, error) {
	return snapshotProcesses()
}

// DescendantAlive reports whether any process reachable from roots (each root
// pid included) has a Name matching one of names. It exists because a pane's
// foreground process can be a wrapper around the process a caller actually
// cares about — e.g. the agent runs as a child of macOS caffeinate, which
// stays the pane's foreground the whole time the agent is alive — so matching
// against the foreground/root alone misreports a live agent as dead.
func DescendantAlive(records []ProcessRecord, roots []int, names []string) bool {
	if len(names) == 0 || len(roots) == 0 {
		return false
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	byPID := make(map[int]ProcessRecord, len(records))
	children := make(map[int][]int, len(records))
	for _, r := range records {
		byPID[r.PID] = r
		if r.PPID != r.PID {
			children[r.PPID] = append(children[r.PPID], r.PID)
		}
	}
	visited := make(map[int]bool, len(records))
	stack := append([]int(nil), roots...)
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if visited[pid] {
			continue
		}
		visited[pid] = true
		if r, ok := byPID[pid]; ok && want[r.Name] {
			return true
		}
		stack = append(stack, children[pid]...)
	}
	return false
}
