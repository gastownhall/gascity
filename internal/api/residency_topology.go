package api

// The API plane's residency topology is the epoch's: State.Stores().Topology(),
// the topology the controller's router minted this plane's class handles
// from (typed-stores T7c). The API holds no storageRoutes and no topology of
// its own: before T7c it derived one from the identity of the class handles,
// observing four of the five infrastructure classes and rounding a whole
// split back up to the binding the other planes name (class:gmnos). The
// minted topology sees every class handle, so the round-up and the identity
// grouping are gone with it; TestFakeStateTopologyMatchesProduction pins the
// two against each other through a frozen copy of the derivation.

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// configuredPrefixes is the work leg's and each rig leg's configured id prefix
// from cfg, keyed by rig ref: the prefixes a State's holder completes the
// topology it mints its Stores from with, so Stores().Topology() carries the
// prefixes this plane routes by-id reads on (seededState, and the test fakes).
func configuredPrefixes(cfg *config.City) (hq string, rigs map[storeref.StoreRef]string) {
	if cfg == nil {
		return "", nil
	}
	rigs = make(map[storeref.StoreRef]string, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rigs[storeref.RigRef(rig.Name)] = strings.TrimSpace(rig.EffectivePrefix())
	}
	return strings.TrimSpace(config.EffectiveHQPrefix(cfg)), rigs
}
