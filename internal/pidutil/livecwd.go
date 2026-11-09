package pidutil

// LiveState captures the working directories of every live process observed
// on this host, plus whether the enumeration itself succeeded. It backs
// internal/session's working-directory collision guard (ga-ighomh.1) and the
// bead-worktree reaper's liveness gate.
type LiveState struct {
	// CWDs is the set of canonicalized (symlink-resolved, absolute) working
	// directories of live processes. Deduplicated.
	CWDs []string
	// PIDCWDs maps each live process's PID to its canonicalized working
	// directory, so a caller can attribute a live cwd to a specific PID.
	PIDCWDs map[int]string
	// Scanned reports whether the process table was enumerated at all. False
	// means liveness is indeterminate and the caller must fail closed.
	Scanned bool
}

// LiveCWDs enumerates the working directory of every process on the host.
func LiveCWDs() LiveState {
	return LiveState{}
}

// liveCWDsFrom is LiveCWDs over the procfs mounted at procRoot, with lsof as
// the enumeration used when procRoot does not exist.
func liveCWDsFrom(_ string, _ func() LiveState) LiveState {
	return LiveState{}
}

// ParseLsofCWDs parses `lsof -a -d cwd -F0pn` output, with the error the run
// returned, into a LiveState.
func ParseLsofCWDs(_ []byte, _ error) LiveState {
	return LiveState{}
}
