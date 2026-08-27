package main

import (
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// readyRoutedWorkEntry is one bead of the detector sweep's DECLARED routed-work
// view: the exact key the pool-allocation admission is enqueued under, plus the
// demand-relevant projection the demand snapshot invalidates on.
//
// PoolTarget is the resolved pool template — the same resolution
// admitReadyRoutedWorkEvent performs for event-carried work — and is empty for a
// ready bead no pool template answers for, or that a worker for that template
// would not be served (demand_serve_predicate.go). Those beads stay in the view
// because their appearance and disappearance still moves named-session and
// control-dispatcher demand; only the enqueue is target-gated.
type readyRoutedWorkEntry struct {
	SourceStore string
	WorkID      string
	PoolTarget  string
	Assigned    bool
	Status      string
	Type        string
	Kind        string
	Contract    string
}

// readyRoutedWorkView is one bounded ReadyLive read per store per patrol,
// promoted from the retired ready-demand fingerprint scan to the sweep's
// declared routed-work input (DETECTOR.md §2, Q2 resolved yes-with-promotion).
//
// It serves two consumers off ONE read: the demand snapshot invalidates when the
// view's fingerprint moves, and the sweep enqueues the unallocated entries by
// exact (workID, poolTarget, sourceStore) key. Event-carried routed work is
// already exact-key covered by admitReadyRoutedWorkEvent, so the view's residual
// value is event-silent raw-bd writes — the census-owed re-detection that
// replaces the pool-allocation channel's legacy overflow crutch.
type readyRoutedWorkView struct {
	Entries     []readyRoutedWorkEntry
	Fingerprint string
	Stores      int
	ObservedAt  time.Time
}

// unallocated returns the entries carrying a resolved pool target and no
// assignee — the routed work a pool member would have to be materialized for.
func (v readyRoutedWorkView) unallocated() []readyRoutedWorkEntry {
	entries := make([]readyRoutedWorkEntry, 0, len(v.Entries))
	for _, entry := range v.Entries {
		if entry.PoolTarget == "" || entry.Assigned {
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

// readyRoutedWorkViewFloor bounds how often consecutive patrol ticks re-run the
// cache-bypassing routed-work read. The read queries every store, so a
// sub-second patrol_interval reproduces exactly the query storm
// scaleCheckDemandMinInterval was introduced to stop. Reusing the previous view
// inside the floor is a no-op at any patrol_interval at or above it (the 30s
// default included) and bounds routed-work discovery latency to the floor only
// on pathologically fast cadences. Non-patrol triggers — including
// certified-readiness routed-work and wait-dependency pokes — carry their own
// exact keys and never wait on this path.
const readyRoutedWorkViewFloor = scaleCheckDemandMinInterval

// flooredReadyRoutedWorkView returns the memoized routed-work view when the
// previous read is younger than readyRoutedWorkViewFloor, and otherwise re-reads
// and re-memoizes. Every consumer inside one tick therefore shares ONE read per
// store, which is what makes the declared cost per patrol rather than per
// consumer.
func (cr *CityRuntime) flooredReadyRoutedWorkView() readyRoutedWorkView {
	if cr == nil {
		return readyRoutedWorkView{}
	}
	if !cr.readyRoutedWorkViewAt.IsZero() && time.Since(cr.readyRoutedWorkViewAt) < readyRoutedWorkViewFloor {
		return cr.readyRoutedWorkView
	}
	view := cr.readReadyRoutedWorkView()
	if !cr.readyRoutedWorkViewAt.IsZero() && cr.readyRoutedWorkView.Fingerprint != view.Fingerprint {
		// Edge-detect the change at the READ rather than comparing a fingerprint
		// carried on the demand snapshot. The snapshot no longer holds one: the
		// view is the declared input, so the view is where the change lives, and
		// the flag is consumed exactly once by the next refresh decision. The
		// FIRST observation raises no edge — there is nothing to have changed
		// from, and a runtime with no snapshot yet rebuilds for that reason
		// alone.
		cr.readyRoutedWorkViewChanged = true
	}
	cr.readyRoutedWorkView = view
	cr.readyRoutedWorkViewAt = time.Now()
	return view
}

// takeReadyRoutedWorkViewChanged reports whether the routed-work view has moved
// since the last demand-snapshot refresh consumed it, and clears the edge.
func (cr *CityRuntime) takeReadyRoutedWorkViewChanged() bool {
	if cr == nil {
		return false
	}
	changed := cr.readyRoutedWorkViewChanged
	cr.readyRoutedWorkViewChanged = false
	return changed
}

// workflowStoreEntry is one bead store paired with the canonical workflow store
// ref its consumers speak.
type workflowStoreEntry struct {
	ref   string
	store beads.Store
}

// canonicalWorkflowStoreEntries labels the runtime's bead stores with the ref
// workflowStoreRefForDir derives from each store's DIRECTORY, walking the same
// loop controllerState.routedWorkStore walks to resolve one. The two are
// mirrors, so a ref produced here is resolvable there by construction.
//
// The store maps are keyed by BARE name — buildStores and
// buildStandaloneRigStores index rigs by rig.Name, and BeadStores adds the city
// under cityName — and that vocabulary is private to the maps. A routed-work
// ref is not a label but a KEY, and every consumer of one resolves the canonical
// spelling only: routedWorkStore, agentutil.AgentReachesWorkflowStore behind the
// allocation policy, and the row-versus-lease scope comparisons in the
// pool-allocation start and drain-ack effect boundaries. Handing a map key out
// as a ref therefore emits work no allocation can act on and no acknowledgement
// can finalize (ga-f7v2ft.155), which is why refs leave this producer canonical
// rather than being repaired at each consumer.
//
// A RIG store whose ref does not resolve is dropped rather than emitted bare:
// its identity comes from cfg.Rigs, so a store that matches no configured rig
// has no ref any consumer could resolve and no consumer to resolve it. The CITY
// store is never dropped — it is always present and always readable, and losing
// its read would trade a labeling defect for a blind demand scan. Routing its
// ref through the canonicalizer gives the canonical spelling whenever the city
// path is known (always, in a running city) and the legacy "city" literal
// otherwise, which downstream canonicalization still maps to this same store.
func canonicalWorkflowStoreEntries(cfg *config.City, cityPath string, cityStore beads.Store, rigStores map[string]beads.Store) []workflowStoreEntry {
	entries := make([]workflowStoreEntry, 0, len(rigStores)+1)
	entries = append(entries, workflowStoreEntry{
		ref:   canonicalizeLegacyWorkflowStoreRef(cfg, cityPath, "city"),
		store: cityStore,
	})
	if cfg == nil {
		return entries
	}
	cityName := loadedCityName(cfg, cityPath)
	rigs := make([]workflowStoreEntry, 0, len(rigStores))
	for i := range cfg.Rigs {
		rig := &cfg.Rigs[i]
		store, ok := rigStores[rig.Name]
		if !ok {
			continue
		}
		rigPath := rig.Path
		if !filepath.IsAbs(rigPath) {
			rigPath = filepath.Join(cityPath, rigPath)
		}
		ref := workflowStoreRefForDir(rigPath, cityPath, cityName, cfg)
		if ref == "" {
			continue
		}
		rigs = append(rigs, workflowStoreEntry{ref: ref, store: store})
	}
	sort.Slice(rigs, func(i, j int) bool { return rigs[i].ref < rigs[j].ref })
	return append(entries, rigs...)
}

// readReadyRoutedWorkView performs the declared read: one bounded ReadyLive per
// store, in a stable store order, resolving each ready bead's pool target
// against the current config.
func (cr *CityRuntime) readReadyRoutedWorkView() readyRoutedWorkView {
	view := readyRoutedWorkView{ObservedAt: time.Now().UTC()}
	if cr == nil {
		return view
	}
	cr.serviceStateMu.RLock()
	cfg := cr.cfg
	cr.serviceStateMu.RUnlock()
	templates := poolRouteTemplateSet(cfg)

	stores := canonicalWorkflowStoreEntries(cfg, cr.cityPath, cr.cityBeadStore(), cr.rigBeadStores())
	view.Stores = len(stores)

	h := fnv.New64a()
	for _, entry := range stores {
		_, _ = io.WriteString(h, entry.ref)
		_, _ = io.WriteString(h, "\x00")
		if entry.store == nil {
			_, _ = io.WriteString(h, "<nil>")
			_, _ = io.WriteString(h, "\x00")
			continue
		}
		ready, err := beads.ReadyLive(entry.store, beads.ReadyQuery{TierMode: beads.TierBoth})
		if err != nil {
			// Fold a stable marker, never the error text. A real outage reports
			// varying connection ids, retry counts, or timestamps, and hashing
			// that text turns the outage into a changed fingerprint — and so a
			// full buildDesiredState rebuild — on every patrol tick. The marker
			// keeps an unreachable store distinguishable from an empty one while
			// degrading the outage to view reuse. The error itself is logged.
			log.Printf("readyRoutedWorkView: store %s: %v", entry.ref, err)
			_, _ = io.WriteString(h, "error")
			_, _ = io.WriteString(h, "\x00")
			continue
		}
		sort.Slice(ready, func(i, j int) bool {
			return ready[i].ID < ready[j].ID
		})
		for _, bead := range ready {
			// AGREEMENT: resolve a target only for a row a worker for that
			// template would actually be served. A routed epic or a row parked
			// on a dispatch hold is not capacity demand — enqueueing it mints a
			// seat whose own hook query reads empty, drains, and leaves the row
			// exactly as it found it. See demand_serve_predicate.go.
			target, _ := demandServableForTemplates(cfg, bead, templates)
			row := readyRoutedWorkEntry{
				SourceStore: entry.ref,
				WorkID:      bead.ID,
				PoolTarget:  target,
				Assigned:    strings.TrimSpace(bead.Assignee) != "",
				Status:      bead.Status,
				Type:        bead.Type,
				Kind:        strings.TrimSpace(bead.Metadata[beadmeta.KindMetadataKey]),
				Contract:    strings.TrimSpace(bead.Metadata[beadmeta.FormulaContractMetadataKey]),
			}
			view.Entries = append(view.Entries, row)
			writeReadyRoutedWorkViewEntry(h, row)
		}
	}
	view.Fingerprint = fmt.Sprintf("%x", h.Sum64())
	return view
}

// writeReadyRoutedWorkViewEntry folds one view row into the invalidation hash.
//
// The hash input is the DECLARED view, not the raw bead: the route target is the
// resolved value of gc.routed_to / gc.run_target, and kind + formula_contract are
// the two remaining metadata keys demand routing reads (control-dispatcher and
// workflow-root demand). A bead's UpdatedAt is deliberately absent — it moved the
// retired fingerprint on every touch and rebuilt desired state for a change no
// demand decision could see.
func writeReadyRoutedWorkViewEntry(w io.Writer, entry readyRoutedWorkEntry) {
	for _, field := range []string{
		entry.WorkID,
		entry.PoolTarget,
		entry.Status,
		entry.Type,
		entry.Kind,
		entry.Contract,
	} {
		_, _ = io.WriteString(w, field)
		_, _ = io.WriteString(w, "\x00")
	}
	if entry.Assigned {
		_, _ = io.WriteString(w, "assigned")
	}
	_, _ = io.WriteString(w, "\x00")
}

// poolRouteTemplateSet is the set of pool templates routed work may name. It is
// the same eligibility admitReadyRoutedWorkEvent applies to event-carried work,
// so the sweep's view and the event path resolve identical targets.
func poolRouteTemplateSet(cfg *config.City) map[string]struct{} {
	if cfg == nil {
		return nil
	}
	templates := make(map[string]struct{}, len(cfg.Agents))
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() {
			continue
		}
		templates[agent.QualifiedName()] = struct{}{}
	}
	return templates
}
