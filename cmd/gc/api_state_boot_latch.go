package main

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout"
)

func (cs *controllerState) assertConditionalWritesBootReady() error {
	mode := cs.rolloutFlags.BeadsConditionalWrites()
	origin := cs.rolloutFlags.OriginOf(rollout.KeyBeadsConditionalWrites)

	if origin != rollout.OriginConfig && origin != rollout.OriginEnv {
		return fmt.Errorf("conditional-writes boot latch: beads.conditional_writes is unset (origin=%s, resolved mode=%q); refusing to start until it is explicitly set to \"require\" via config (beads.conditional_writes) or env", origin, string(mode))
	}
	if mode != rollout.Require {
		return fmt.Errorf("conditional-writes boot latch: beads.conditional_writes=%q (origin=%s); refusing to start until it resolves to \"require\"", string(mode), origin)
	}

	stores := map[string]beads.Store{"city": cs.cityBeadStore}
	for name, store := range cs.beadStores {
		stores["rig/"+name] = store
	}
	ids := make([]string, 0, len(stores))
	for id := range stores {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		store := stores[id]
		if store == nil {
			return fmt.Errorf("conditional-writes boot latch: scope %s has no open store; refusing to start rather than assume it is fenced", id)
		}
		if capable, reason := beads.MetadataCASCapableFor(store); !capable {
			return fmt.Errorf("conditional-writes boot latch: scope %s does not have a runtime-capable metadata CAS: %s", id, reason)
		}
	}
	return nil
}
