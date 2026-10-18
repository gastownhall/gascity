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
// configDirty is not a reason to refuse. It records that a reload is due, for
// instance after the config watcher has seen a file change, after `gc reload`,
// or after an API config mutation has already published, and a reconcile tick
// clears it when it starts that reload. It says nothing about whether the
// config and the stores agree: they are one generation whatever it reads.
func (cs *controllerState) ReadyWork(_ context.Context, q readywork.Query) ([]readywork.ReadyBead, error) {
	if err := q.Validate(); err != nil {
		return nil, err
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
