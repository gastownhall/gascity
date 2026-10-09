package runtime

import (
	"errors"
	"fmt"
	"sort"
)

// ErrSessionRosterUnsupported reports that no backend of a composite provider
// reports a session roster.
var ErrSessionRosterUnsupported = errors.New("runtime does not report a session roster")

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
// backend without a roster contributes no entries, so its sessions read as
// absent and callers fall back to per-session reads for them. Any backend
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

// ScanningBackends returns the backends that can scan the process table
// ([AsProcessTableScanner]), in order. It returns none when the first backend
// cannot scan: a later backend's scanner tracks only the sessions it hosts,
// so behind a scannerless first backend it would report every live runtime
// that backend hosts as an untracked orphan, and orphan reaping would kill it.
func ScanningBackends(backends []Backend) []Backend {
	if len(backends) == 0 {
		return nil
	}
	if _, ok := AsProcessTableScanner(backends[0].Provider); !ok {
		return nil
	}
	out := []Backend{backends[0]}
	for _, b := range backends[1:] {
		if _, ok := AsProcessTableScanner(b.Provider); ok {
			out = append(out, b)
		}
	}
	return out
}

// FindRuntimesAcross implements [ProcessTableScanner.FindRuntimesBySessionID]
// for a composite by querying every backend [ScanningBackends] returns and
// merging the results by PID.
//
// Local backends scan the same host process table, so one root commonly
// appears in several results, each backend marking tracked only the sessions
// it hosts. A root is therefore tracked when ANY backend tracks it, and the
// tracking backend's record (with its ProviderName) is kept. Results are
// ordered by PID. Backend errors are labeled and joined; partial results from
// a failing backend are still merged, per the best-effort contract.
func FindRuntimesAcross(backends []Backend, id string) ([]LiveRuntime, error) {
	merged := make(map[int]LiveRuntime)
	var errs []error
	for _, b := range ScanningBackends(backends) {
		scanner, _ := AsProcessTableScanner(b.Provider)
		found, err := scanner.FindRuntimesBySessionID(id)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s backend: %w", b.Label, err))
		}
		for _, r := range found {
			if existing, ok := merged[r.PID]; !ok || (r.IsTracked && !existing.IsTracked) {
				merged[r.PID] = r
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
