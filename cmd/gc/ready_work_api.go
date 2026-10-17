package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/readywork"
)

var _ api.ReadyWorkReader = (*controllerState)(nil)

// errReadyWorkConfigPending refuses a ready-work request while a city config
// change is staged but not yet reloaded. The controller's legs are built from
// its loaded config, and a rig added, removed or rebound since would make its
// answer read a different set of stores than a fresh `gc ready` reads. The
// refusal is a 503, and the CLI then reads in-process.
var errReadyWorkConfigPending = errors.New("the city config changed and the controller has not reloaded it yet")

// ReadyWork answers `gc ready` (GET /v0/city/{cityName}/work/ready) from the
// stores this controller already holds open, with the reader the CLI runs
// in-process (internal/readywork).
//
// The legs are the ones a fresh `gc ready` assembles, from the same functions:
// the serving frame (readyServingRigs), the same topology constructor over this
// controller's own class-binding routes (assembleResidencyTopology over
// residencyBindingsFromRoutes, which is how the controller's census builds its
// topology), and the same plan. readywork reads every leg through its live
// handle, so a CachingStore leg answers from its backing store and never from
// its read cache: the answer is as fresh as an in-process read, whoever wrote
// the bead. Opening nothing is the whole saving.
func (cs *controllerState) ReadyWork(_ context.Context, q readywork.Query) ([]readywork.ReadyBead, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if cs.configDirty != nil && cs.configDirty.Load() {
		return nil, errReadyWorkConfigPending
	}
	legs, err := cs.readyWorkLegs()
	if err != nil {
		return nil, err
	}
	return readywork.Read(legs, q)
}

// readyWorkLegs assembles the ready legs over this controller's open stores.
//
// A serving rig whose store this controller does not hold fails the request
// rather than shrinking it: a missing leg is a short answer, the fail-open
// `gc ready` exists to close. A rig whose store failed to open is held as an
// unavailableStore, which fails its read the same way. An unbound rig (no
// .gc/site.toml binding, empty path) has no store anywhere and is skipped, as
// the in-process reader skips it.
func (cs *controllerState) readyWorkLegs() ([]readywork.Leg, error) {
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
	return readyLegsForTopology(assembleResidencyTopology(cfg, cityStore, rigLegsExcludingCityName(cityName, rigStores), bindings, refused))
}
