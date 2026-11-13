package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/readywork"
)

var _ api.ReadyWorkReader = (*controllerState)(nil)

// configFreshness lets a controller read show that the config it answers from,
// and so the set of stores it reads, is the one the controller has published
// and is not being replaced or left behind by a failed reload.
//
// A reload publishes its new config and stores only as it ends, and an API
// config write changes the files before its reload runs; either may swap the
// stores a read holds. So each reload and API config write is marked in
// progress from before it clears the dirty flag or writes a file until it has
// published, a failed reload is remembered until a later one applies or finds
// no change, and a read answers only when no change began during it, from
// assembling its legs to its last store read. A failed reload is remembered
// here, not by setting the dirty flag again, which would rerun a failing
// reload every tick.
//
// The dirty flag itself is not consulted. It records only that a reload is
// due, and the config and stores a read holds are one generation whatever it
// reads; an edit on disk reaches the read at the next reload.
type configFreshness struct {
	changing     atomic.Int64  // reloads and API config writes in progress
	began        atomic.Uint64 // reloads and API config writes ever begun
	reloadFailed atomic.Bool   // the last reload neither applied nor found no change
}

// beginChange marks a reload or an API config write in progress. The change is
// marked in progress before it is counted, so a read that sees the count also
// sees the change in progress, or ended. A nil marker (a runtime without an API
// state) keeps nothing.
func (f *configFreshness) beginChange() {
	if f == nil {
		return
	}
	f.changing.Add(1)
	f.began.Add(1)
}

// endChange ends an API config write, which leaves the flag set for its reload
// when it changed the config.
func (f *configFreshness) endChange() {
	if f == nil {
		return
	}
	f.changing.Add(-1)
}

// endReload ends a reload with its outcome. One that neither applied nor found
// no change, including a reload that panicked and has no outcome, leaves the
// controller on a config disk may no longer hold.
func (f *configFreshness) endReload(outcome reloadOutcome) {
	if f == nil {
		return
	}
	f.reloadFailed.Store(outcome != reloadOutcomeApplied && outcome != reloadOutcomeNoChange)
	f.changing.Add(-1)
}

// read runs answer and reports whether it ran under the published config: no
// change in progress, no failed reload, and no change begun while answer ran.
func (f *configFreshness) read(answer func()) bool {
	began := f.began.Load()
	if f.changing.Load() != 0 || f.reloadFailed.Load() {
		return false
	}
	answer()
	return f.began.Load() == began
}

// freshness is the ready-work freshness marker of this runtime's API state, or
// nil when it has none.
func (cr *CityRuntime) freshness() *configFreshness {
	if cr.cs == nil {
		return nil
	}
	return &cr.cs.freshness
}

// ReadyWork answers `gc ready` (GET /v0/city/{cityName}/work/ready) from the
// config this controller has published and the stores it already holds open,
// with the reader the CLI runs in-process (internal/readywork).
//
// The legs are assembled over that config by the functions a fresh `gc ready`
// uses: the serving frame (readyServingRigs), the same topology constructor
// over the class-binding routes this controller resolved when it started
// (assembleResidencyTopology over residencyBindingsFromRoutes, which is how the
// controller's census builds its topology), and the same plan. readywork reads
// every leg through its live handle, so a CachingStore leg answers from its
// backing store and never from its read cache: the answer is as fresh as an
// in-process read, whoever wrote the bead. Opening nothing is the whole saving.
//
// The config is read under cs.mu with the stores controllerState.update
// published alongside it, so the two are one generation. The answer is
// therefore not a fresh read of the config on disk: a rig added, removed or
// rebound there reaches it at the next config reload, as it reaches the
// controller's sessions.
//
// It refuses with api.ErrReadyWorkConfigNotCurrent, which the CLI answers by
// reading in-process, while a reload or API config write is in progress, after
// a reload failed until one applies or finds no change, and when a change
// began during the read (configFreshness). The check spans the store reads as
// well as the leg assembly. A change that begins mid-read may publish a
// different set of stores and close the ones the legs are reading
// (controllerStateStoreCloseDelay); every store swap happens inside such a
// change, so a read one overlapped is refused, whatever the closed stores
// returned. Each leg is read bound to ctx as far as its store allows
// (readyLegsBoundTo).
//
// configDirty is not a reason to refuse. It records that a reload is due, for
// instance after the config watcher has seen a file change, after `gc reload`,
// or after an API config mutation has already published, and a reconcile tick
// clears it when it starts that reload. It says nothing about whether the
// config and the stores agree: they are one generation whatever it reads.
func (cs *controllerState) ReadyWork(ctx context.Context, q readywork.Query) ([]readywork.ReadyBead, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	var rows []readywork.ReadyBead
	var err error
	// The refusal outranks an error from the same read: a read taken under a
	// changing config shows nothing, and the in-process read the refusal sends
	// the CLI to meets any failure that persists.
	if !cs.freshness.read(func() {
		var legs []readywork.Leg
		if legs, err = cs.readyWorkLegs(ctx); err == nil {
			rows, err = readywork.ReadContext(ctx, legs, q)
		}
	}) {
		return nil, api.ErrReadyWorkConfigNotCurrent
	}
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// readyWorkScopedStore binds one leg's store to a request; a variable so a test
// can watch every leg pass through it.
var readyWorkScopedStore = scopedStoreLike

// readyWorkLegs assembles the ready legs over this controller's open stores,
// each bound to ctx (readyLegsBoundTo).
//
// A serving rig whose store this controller does not hold fails the request
// rather than shrinking it: a missing leg is a short answer, the fail-open
// `gc ready` exists to close. A rig whose store failed to open is held as an
// unavailableStore, which fails its read the same way. An unbound rig (no
// .gc/site.toml binding, empty path) has no store anywhere and is skipped, as
// the in-process reader skips it.
func (cs *controllerState) readyWorkLegs(ctx context.Context) ([]readywork.Leg, error) {
	cs.mu.RLock()
	cfg := cs.cfg
	cityStore := cs.cityBeadStore
	rigs := cs.beadStores
	routes := cs.storageRoutes
	cityName := cs.cityName
	cityPath := cs.cityPath
	cs.mu.RUnlock()
	if cityStore == nil {
		return nil, errors.New("city store: not open in the controller")
	}
	serving, err := readyServingRigs(cfg, cityPath)
	if err != nil {
		return nil, err
	}
	rigStores := make(map[string]beads.Store, len(serving)) // residency:allow — the topology constructor's input, built from the serving frame exactly as readyRigLegStores builds it; reads go through the planned legs
	for _, rig := range serving {
		if strings.TrimSpace(rig.Path) == "" {
			continue
		}
		store := rigs[rig.Name]
		if store == nil {
			return nil, fmt.Errorf("rig %q store: not open in the controller", rig.Name)
		}
		rigStores[rig.Name] = store
	}
	bindings, refused := residencyBindingsFromRoutes(routes)
	planned, err := readyLegsForTopology(assembleResidencyTopology(cfg, cityStore, rigLegsExcludingCityName(cityName, rigStores), bindings, refused))
	if err != nil {
		return nil, err
	}
	return readyLegsBoundTo(ctx, cityPath, cfg, planned)
}

// readyLegsBoundTo returns legs with each store bound to ctx, so a request that
// ends (the CLI's timeout, a client that hangs up) stops its leg reads. A
// bd-backed store is read through a clone whose bd subprocesses ctx kills
// (scopedStoreLike). A store that runs no subprocess, such as a native one, has
// no clone and is read as held: readywork starts no read on it once ctx ends,
// but a read already inside it runs to its end, beside the in-process read the
// CLI falls back to. The legs are bound after they are planned, because the
// plan tells legs apart by the identity of the stores held.
func readyLegsBoundTo(ctx context.Context, cityPath string, cfg *config.City, legs []readywork.Leg) ([]readywork.Leg, error) {
	bound := make([]readywork.Leg, len(legs))
	for i, leg := range legs {
		scoped, err := readyWorkScopedStore(ctx, cityPath, cfg, leg.Store)
		if err != nil {
			return nil, fmt.Errorf("%s store: binding to the request: %w", leg.Label, err)
		}
		if scoped != nil {
			leg.Store = scoped
		}
		bound[i] = leg
	}
	return bound, nil
}
