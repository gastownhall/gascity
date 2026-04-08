package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// BulkRoutedCounts holds precomputed counts of routed work beads,
// keyed by template (gc.routed_to value).
//
// Computing these once per reconcile cycle replaces the per-pool fan-out
// of bd subprocess calls with one pair of in-memory Store queries per
// rig (typically served from the CachingStore). This avoids the slow
// per-pool fan-out path that would otherwise make a reconcile cycle
// take 10+ minutes under contended dolt.
type BulkRoutedCounts struct {
	Ready      map[string]int
	InProgress map[string]int
	// OKRigs records which rigs had both queries succeed. Callers must
	// fall back to the per-pool path for rigs not in this set.
	OKRigs map[string]bool
}

// Covers reports whether the bulk path successfully queried the given rig.
func (b *BulkRoutedCounts) Covers(rig string) bool {
	if b == nil {
		return false
	}
	return b.OKRigs[rig]
}

// Total returns ready + in_progress for a given template.
func (b *BulkRoutedCounts) Total(template string) int {
	if b == nil {
		return 0
	}
	return b.Ready[template] + b.InProgress[template]
}

// Has reports whether the template has any ready or in-progress routed work.
func (b *BulkRoutedCounts) Has(template string) bool {
	if b == nil {
		return false
	}
	return b.Ready[template] > 0 || b.InProgress[template] > 0
}

// precomputeBulkRoutedCounts queries each rig's bead store once for
// Ready() and in-progress lists, filters unassigned entries, and groups
// them by the gc.routed_to metadata. Returns nil when the store map is
// empty. Per-rig errors are recorded by omitting the rig from OKRigs so
// callers fall back only for that rig.
func precomputeBulkRoutedCounts(rigStores map[string]beads.Store, cfg *config.City) *BulkRoutedCounts {
	if cfg == nil || len(rigStores) == 0 {
		return nil
	}
	out := &BulkRoutedCounts{
		Ready:      make(map[string]int),
		InProgress: make(map[string]int),
		OKRigs:     make(map[string]bool),
	}
	for rig, store := range rigStores {
		if store == nil {
			continue
		}
		ready, err := store.Ready()
		if err != nil {
			continue
		}
		inProg, err := store.List(beads.ListQuery{Status: "in_progress", AllowScan: true})
		if err != nil {
			continue
		}
		out.OKRigs[rig] = true
		// Mirror the default scale_check semantics from
		// config.Agent.EffectiveScaleCheck: count ready --unassigned
		// and in_progress --no-assignee, grouped by gc.routed_to. Both
		// filters exclude beads that already have an assignee — in-progress
		// beads without an assignee are orphaned/queued work that still
		// needs a worker.
		for _, b := range ready {
			if b.Assignee != "" {
				continue
			}
			if t := b.Metadata["gc.routed_to"]; t != "" {
				out.Ready[t]++
			}
		}
		for _, b := range inProg {
			if b.Assignee != "" {
				continue
			}
			if t := b.Metadata["gc.routed_to"]; t != "" {
				out.InProgress[t]++
			}
		}
	}
	return out
}
