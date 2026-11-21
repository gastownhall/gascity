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
// whose scan can leave out every process that a tmux server or client
// carrying a session's GC_SESSION_ID hosts, as the exec scanner does for a
// host-sharing pack whose agents run outside tmux: a tmux in such a box is
// one an agent or a user founded there, which copied the session's
// GC_SESSION_ID, so each of its panes would otherwise read as one of the
// session's roots. A composite whose default backend fences reports only the
// PIDs that backend reports ([FindRuntimesAcross]), since a later backend's
// scan (ACP's) still reports those panes.
type TmuxSubtreeFence interface {
	// FencesTmuxSubtrees reports whether the scanner's scan leaves out the
	// processes a tmux carrying a session's GC_SESSION_ID hosts.
	FencesTmuxSubtrees() bool
}

// FindRuntimesAcross implements [ProcessTableScanner.FindRuntimesBySessionID]
// for a composite by querying every backend [ScanningBackends] returns and
// merging the results by PID.
//
// Local backends scan the same host process table, so one root commonly
// appears in several results, each backend marking tracked only the sessions
// it hosts. A root is therefore tracked when ANY backend tracks it, and the
// tracking backend's record (with its ProviderName) is kept. An untracked root
// is LiveUnlisted when ANY backend claims it so. Results are ordered by PID.
// Backend errors are labeled and joined; partial results from a failing
// backend are still merged, per the best-effort contract.
//
// When the default backend's scan fences tmux subtrees ([TmuxSubtreeFence]),
// that scan decides which PIDs the composite reports: a later backend's
// record is merged only for a PID the default backend reported, adding its
// tracking or claim, never a root. A later backend scans the same table
// without the fence, so a PID only it reports is one a session-carrying tmux
// hosts, one the default's fence could not decide, or one that appeared
// after the default's scan, and its untracked record would hand that PID to
// an orphan kill. Leaving it out only makes callers terminate less.
func FindRuntimesAcross(backends []Backend, id string) ([]LiveRuntime, error) {
	merged := make(map[int]LiveRuntime)
	var errs []error
	// fencedPIDs, once set, is the fencing default backend's PIDs: the only
	// ones a later backend's records are merged for.
	var fencedPIDs map[int]bool
	for i, b := range ScanningBackends(backends) {
		found, err := b.Scanner.FindRuntimesBySessionID(id)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s backend: %w", b.Label, err))
		}
		for _, r := range found {
			if fencedPIDs == nil || fencedPIDs[r.PID] {
				mergeLiveRuntime(merged, r)
			}
		}
		if i == 0 && fencesTmuxSubtrees(b.Scanner) {
			fencedPIDs = make(map[int]bool, len(found))
			for _, r := range found {
				fencedPIDs[r.PID] = true
			}
		}
	}
	out := make([]LiveRuntime, 0, len(merged))
	for _, r := range merged {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, errors.Join(errs...)
}

// fencesTmuxSubtrees reports whether s is a [TmuxSubtreeFence] whose scan
// fences.
func fencesTmuxSubtrees(s ProcessTableScanner) bool {
	fence, ok := s.(TmuxSubtreeFence)
	return ok && fence.FencesTmuxSubtrees()
}

// mergeLiveRuntime merges r into merged by PID: a tracked record replaces an
// untracked one, and an untracked root keeps any record's LiveUnlisted
// claim.
func mergeLiveRuntime(merged map[int]LiveRuntime, r LiveRuntime) {
	existing, ok := merged[r.PID]
	switch {
	case !ok || (r.IsTracked && !existing.IsTracked):
		merged[r.PID] = r
	case r.LiveUnlisted && !existing.IsTracked:
		existing.LiveUnlisted = true
		merged[r.PID] = existing
	}
}
