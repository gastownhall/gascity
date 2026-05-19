package main

import (
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// closeSessionBeadIfUnassigned closes a session bead only when the live store
// confirms no open or in-progress work is assigned to it across the primary
// store AND any attached rig stores. Use this cross-store guard for cleanup
// paths that must not orphan work in any attached store. Reconciler paths that
// close a session according to its configured agent reachability should use
// closeSessionBeadIfReachableStoreUnassigned instead.
//
// Callers must NOT pass a pre-computed work snapshot — this helper queries the
// stores itself so its decision cannot be poisoned by a stale snapshot taken
// earlier in the tick (see the PR that retired the snapshot-based variant).
// Live-query failures fail closed: the bead stays open until assignment can be
// re-verified.
func closeSessionBeadIfUnassigned(
	store beads.Store,
	rigStores map[string]beads.Store,
	session beads.Bead,
	reason string,
	now time.Time,
	stderr io.Writer,
) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	hasAssignedWork, err := sessionHasOpenAssignedWork(store, rigStores, session)
	if err != nil {
		fmt.Fprintf(stderr, "session work guard: checking assigned work for %s: %v\n", session.ID, err) //nolint:errcheck
		return false
	}
	if hasAssignedWork {
		return false
	}
	return closeBead(store, session.ID, reason, now, stderr)
}

// closeSessionBeadIfReachableStoreUnassigned closes a session bead only when
// the live store scope its configured agent can query has no open or
// in-progress work assigned to the session. It returns whether the close
// succeeded, matching closeSessionBeadIfUnassigned's contract.
func closeSessionBeadIfReachableStoreUnassigned(
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	session beads.Bead,
	reason string,
	now time.Time,
	stderr io.Writer,
) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	hasAssignedWork, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, session)
	if err != nil {
		fmt.Fprintf(stderr, "session work guard: checking reachable assigned work for %s: %v\n", session.ID, err) //nolint:errcheck
		return false
	}
	if hasAssignedWork {
		return false
	}
	return closeBead(store, session.ID, reason, now, stderr)
}
