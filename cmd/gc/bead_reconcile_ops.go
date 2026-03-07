package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// beadReconcileOps implements reconcileOps by storing config/live hashes
// in session bead metadata instead of tmux environment variables. This
// makes beads the single source of truth for agent lifecycle state.
//
// Hash keys use the "started_" prefix to distinguish them from the
// observational hashes written by syncSessionBeads:
//   - syncSessionBeads writes config_hash / live_hash (CURRENT config)
//   - beadReconcileOps writes started_config_hash / started_live_hash
//     (what config the agent was STARTED with)
//
// After a config change, these diverge: config_hash reflects the new
// config, while started_config_hash reflects the old config. The
// reconciler detects drift by comparing started_config_hash to the
// current config's fingerprint.
//
// listRunning and runLive delegate to the underlying provider — these
// are session-level operations that beads cannot replace.
type beadReconcileOps struct {
	provider reconcileOps      // delegate for listRunning, runLive
	store    beads.Store       // bead store for hash persistence
	index    map[string]string // session_name → bead_id (open beads)
}

// newBeadReconcileOps creates a beadReconcileOps wrapping the given provider.
// The index is initially empty and must be populated via updateIndex before
// hash operations will succeed.
func newBeadReconcileOps(provider reconcileOps, store beads.Store) *beadReconcileOps {
	return &beadReconcileOps{
		provider: provider,
		store:    store,
	}
}

// updateIndex replaces the session_name → bead_id index. Called after
// syncSessionBeads to reflect the current set of open session beads.
func (o *beadReconcileOps) updateIndex(idx map[string]string) {
	o.index = idx
}

func (o *beadReconcileOps) listRunning(prefix string) ([]string, error) {
	return o.provider.listRunning(prefix)
}

func (o *beadReconcileOps) storeConfigHash(name, hash string) error {
	id, ok := o.index[name]
	if !ok {
		// No bead for this session — fall back to provider.
		return o.provider.storeConfigHash(name, hash)
	}
	return o.store.SetMetadata(id, "started_config_hash", hash)
}

func (o *beadReconcileOps) configHash(name string) (string, error) {
	id, ok := o.index[name]
	if !ok {
		// No bead — graceful upgrade, same as empty hash.
		return "", nil
	}
	b, err := o.store.Get(id)
	if err != nil {
		return "", nil
	}
	return b.Metadata["started_config_hash"], nil
}

func (o *beadReconcileOps) storeLiveHash(name, hash string) error {
	id, ok := o.index[name]
	if !ok {
		return o.provider.storeLiveHash(name, hash)
	}
	return o.store.SetMetadata(id, "started_live_hash", hash)
}

func (o *beadReconcileOps) liveHash(name string) (string, error) {
	id, ok := o.index[name]
	if !ok {
		return "", nil
	}
	b, err := o.store.Get(id)
	if err != nil {
		return "", nil
	}
	return b.Metadata["started_live_hash"], nil
}

func (o *beadReconcileOps) runLive(name string, cfg runtime.Config) error {
	return o.provider.runLive(name, cfg)
}
