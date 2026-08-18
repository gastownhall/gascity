package main

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout"
)

// assertConditionalWritesBootReady is the controller's fail-closed startup
// gate for the beads conditional-writes rollout (ADR-0019 / dr-89vak, step 8).
// It runs once, synchronously, at the end of construction — before cs is
// published to any other goroutine — so it reads cs.rolloutFlags,
// cs.beadStores and cs.cityBeadStore without cs.mu, mirroring
// preflightConditionalWrites, its pre-existing warn-only sibling.
//
// Two conditions refuse startup, each with its own distinct message:
//
//  1. beads.conditional_writes does not resolve to "require" from an
//     explicit source (config or env). An unconfigured gate (origin=builtin)
//     is deliberately reported differently from an explicit non-require
//     value: absence means nobody has opted the fencing discipline in yet,
//     which is a different operator mistake from having explicitly set it to
//     "off" or "auto".
//  2. Any scope in the controller's own store map (the city store plus every
//     bound rig, exactly the set ConditionalWritesStatus walks) lacks a
//     runtime-capable metadata CAS, per beads.MetadataCASCapableFor. That is
//     a real, live capability probe — not a static MetadataCASWriter type
//     assertion, which would false-green on every BdStore scope regardless
//     of whether the installed bd CLI actually supports fenced writes (see
//     MetadataCASCapableFor's doc). On today's fleet, before bd gains
//     --if-revision, every BdStore-backed scope is expected to fail this
//     check — that is the correct, intended behavior this latch exists to
//     surface, not a regression to work around.
//
// Unlike ConditionalWritesStatus, this assertion does NOT return early when
// the gate is off: off/unset is itself condition (1)'s failure, so arm 2 must
// still be reachable in principle (it is short-circuited by arm 1 failing
// first, not by an early return keyed on mode).
//
// The now-struck beads.guarded_release rollout gate is deliberately NOT
// asserted here. It has zero consumers anywhere in this tree's history;
// asserting an unused gate would be a silent ADR amendment, not an
// implementation of the one that exists.
//
// Rigs opened AFTER boot (e.g. a runtime config reload that adds a new rig)
// are NOT covered by this latch: it runs once, over the store map as built at
// construction time. Closing that gap is out of scope for this change.
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
			continue
		}
		if capable, reason := beads.MetadataCASCapableFor(store); !capable {
			return fmt.Errorf("conditional-writes boot latch: scope %s does not have a runtime-capable metadata CAS: %s", id, reason)
		}
	}
	return nil
}
