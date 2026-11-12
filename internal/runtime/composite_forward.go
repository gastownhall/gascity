package runtime

import (
	"errors"
	"fmt"
	"sort"
)

// ErrSessionRosterUnsupported reports that no backend of a composite provider
// reports a session roster.
var ErrSessionRosterUnsupported = errors.New("runtime does not report a session roster")

// ErrEnvironmentBatchUnsupported reports that the backend serving a session
// has no batched environment read; fall back to per-key GetMeta. It is
// deliberately not [ErrMetaUnsupported], which means the runtime has no
// metadata at all, so a token read through it is absent by construction.
var ErrEnvironmentBatchUnsupported = errors.New("runtime has no batched environment read; fall back to per-key GetMeta")

// The helpers below let composite providers (auto, hybrid) forward optional
// capabilities that are not per-session, so callers asserting them on the
// city provider still reach the backends that have them.

// ConfigureServers calls ConfigureServer on every backend that implements
// [ServerLifecycleProvider]. A failure does not skip the remaining backends;
// errors are labeled by backend and joined.
func ConfigureServers(backends []Backend) error {
	return eachServer(backends, ServerLifecycleProvider.ConfigureServer)
}

// TeardownServers calls TeardownServer on every backend that implements
// [ServerLifecycleProvider], as [ConfigureServers] does.
func TeardownServers(backends []Backend) error {
	return eachServer(backends, ServerLifecycleProvider.TeardownServer)
}

func eachServer(backends []Backend, op func(ServerLifecycleProvider) error) error {
	var errs []error
	for _, b := range backends {
		if s, ok := b.Provider.(ServerLifecycleProvider); ok {
			if err := op(s); err != nil {
				errs = append(errs, fmt.Errorf("%s backend: %w", b.Label, err))
			}
		}
	}
	return errors.Join(errs...)
}

// MergeSessionRosters merges the rosters of the backends that report one. A
// name two backends report keeps the entry of the backend r routes it to. A
// backend without a roster contributes no entries, so its running sessions
// are absent: unlike a leaf's roster, absence from the merged roster does not
// mean not running, and callers fall back to per-session reads. Any backend
// error fails the merge; with no roster-reporting backend it returns
// [ErrSessionRosterUnsupported].
func MergeSessionRosters(r Router, backends []Backend) (map[string]SessionRosterEntry, error) {
	var merged map[string]SessionRosterEntry
	var errs []error
	for _, b := range backends {
		rp, ok := b.Provider.(SessionRosterProvider)
		if !ok {
			continue
		}
		roster, err := rp.SessionRoster()
		if errors.Is(err, ErrSessionRosterUnsupported) {
			continue // a nested composite with no roster-reporting backend
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s backend: %w", b.Label, err))
			continue
		}
		if merged == nil {
			merged = make(map[string]SessionRosterEntry, len(roster))
		}
		for name, entry := range roster {
			if _, dup := merged[name]; dup && r.RouteFor(name).Label != b.Label {
				continue
			}
			merged[name] = entry
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if merged == nil {
		return nil, ErrSessionRosterUnsupported
	}
	return merged, nil
}

// BackendScanner is a backend's process-table scanner, as
// [ScanningBackends] returns it.
type BackendScanner struct {
	Label   string
	Scanner ProcessTableScanner
}

// ScanningBackends returns the scanners of the backends that can scan the
// process table ([AsProcessTableScanner]), in order. It returns none when the
// first backend cannot scan: a later backend's scanner tracks only the
// sessions it hosts, so behind a scannerless first backend it would report
// every live runtime that backend hosts as an untracked orphan, and orphan
// reaping would kill it.
func ScanningBackends(backends []Backend) []BackendScanner {
	var out []BackendScanner
	for i, b := range backends {
		s, ok := AsProcessTableScanner(b.Provider)
		if !ok && i == 0 {
			return nil
		}
		if ok {
			out = append(out, BackendScanner{Label: b.Label, Scanner: s})
		}
	}
	return out
}

// TmuxSubtreeFence is an optional capability of a [ProcessTableScanner]
// whose sessions never run under a tmux server, such as an exec pack whose
// boxes share the host. Its own scan never reports a process with a tmux
// server or client among its ancestors: a tmux server in such a session's
// picture is one an agent or a user founded inside it, which carries the
// session's GC_SESSION_ID, so every pane would otherwise read as one of the
// session's roots. A composite whose default backend has it filters every
// later backend's roots through it ([FindRuntimesAcross]), since a later
// backend's scan (ACP's) still reports those panes.
type TmuxSubtreeFence interface {
	// FenceTmuxSubtrees returns the roots that have no tmux server or client
	// among their ancestors. A root whose ancestry cannot be read is left
	// out, with an error naming it.
	FenceTmuxSubtrees(roots []LiveRuntime) ([]LiveRuntime, error)
}

// FindRuntimesAcross implements [ProcessTableScanner.FindRuntimesBySessionID]
// for a composite by querying every backend [ScanningBackends] returns and
// merging the results by PID. When the default backend's scanner is a
// [TmuxSubtreeFence], every later backend's roots pass through its fence
// before the merge, and a PID the fence leaves out is dropped from the whole
// merge: no backend's record of it is reported. The fence leaves out a root
// it could not decide (with an error) as well as one under tmux, and keeping
// another backend's record of such a PID could report a root a later backend
// tracks as untracked, for an orphan kill to end. Dropping it only makes
// callers terminate less.
//
// Local backends scan the same host process table, so one root commonly
// appears in several results, each backend marking tracked only the sessions
// it hosts. A root is therefore tracked when ANY backend tracks it, and the
// tracking backend's record (with its ProviderName) is kept. An untracked root
// is LiveUnlisted when ANY backend claims it so. Results are ordered by PID.
// Backend errors are labeled and joined; partial results from a failing
// backend are still merged, per the best-effort contract.
func FindRuntimesAcross(backends []Backend, id string) ([]LiveRuntime, error) {
	merged := make(map[int]LiveRuntime)
	fenced := make(map[int]bool)
	var errs []error
	scanners := ScanningBackends(backends)
	var fence TmuxSubtreeFence
	if len(scanners) > 0 {
		fence, _ = scanners[0].Scanner.(TmuxSubtreeFence)
	}
	for i, b := range scanners {
		found, err := b.Scanner.FindRuntimesBySessionID(id)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s backend: %w", b.Label, err))
		}
		if i > 0 && fence != nil && len(found) > 0 {
			kept, fenceErr := fence.FenceTmuxSubtrees(found)
			if fenceErr != nil {
				errs = append(errs, fmt.Errorf("%s backend: %w", b.Label, fenceErr))
			}
			keptPIDs := make(map[int]bool, len(kept))
			for _, r := range kept {
				keptPIDs[r.PID] = true
			}
			for _, r := range found {
				if !keptPIDs[r.PID] {
					fenced[r.PID] = true
				}
			}
			found = kept
		}
		for _, r := range found {
			existing, ok := merged[r.PID]
			switch {
			case !ok || (r.IsTracked && !existing.IsTracked):
				merged[r.PID] = r
			case r.LiveUnlisted && !existing.IsTracked:
				existing.LiveUnlisted = true
				merged[r.PID] = existing
			}
		}
	}
	out := make([]LiveRuntime, 0, len(merged))
	for pid, r := range merged {
		if !fenced[pid] {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, errors.Join(errs...)
}
