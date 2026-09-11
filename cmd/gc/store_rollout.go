package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

// resolvedConditionalWritesFlags resolves the rollout flags from an
// already-loaded config for store-open threading. A nil config or a resolve
// error yields (zero, false) — the factory maps the unset mode to off with a
// defaulted marker, so a best-effort open can never RAISE enforcement. The
// loud surfaces for a resolve error are the controller boot latch and gc
// doctor; store-open helpers stay best-effort, matching their existing
// config-error tolerance. Resolution is per-process by design (the env
// break-glass is per-process; the supported whole-city change is config edit
// plus restart).
func resolvedConditionalWritesFlags(cfg *config.City) (rollout.Flags, bool) {
	if cfg == nil {
		return rollout.Flags{}, false
	}
	flags, err := rollout.Resolve(cfg, rollout.ResolveOptions{})
	if err != nil {
		return rollout.Flags{}, false
	}
	return flags, true
}

// resolvedConditionalWritesMode is the mode-only view of
// resolvedConditionalWritesFlags for store-open threading.
func resolvedConditionalWritesMode(cfg *config.City) gate.Mode {
	flags, ok := resolvedConditionalWritesFlags(cfg)
	if !ok {
		return gate.ModeUnset
	}
	return flags.BeadsConditionalWrites()
}

// lazyConditionalWritesDegradeEmitter builds the factory degrade callback for
// open paths that have no live event provider in hand (the shared CLI open
// helper and the control dispatcher). The recorder is constructed INSIDE the
// callback — which the factory latches to at most one invocation per store —
// so routine opens pay nothing and an auto-degrade still lands in the city's
// event log instead of persisting unnoticed.
func lazyConditionalWritesDegradeEmitter(cityPath, storeID string, flags rollout.Flags, resolved bool) func(beads.ConditionalWritesDegrade) {
	if !resolved || strings.TrimSpace(cityPath) == "" {
		return nil
	}
	return func(d beads.ConditionalWritesDegrade) {
		cb := conditionalWritesDegradedRecorder(openCityRecorderAt(cityPath, io.Discard), flags, storeID)
		if cb != nil {
			cb(d)
		}
	}
}

// conditionalWritesStoreID labels a store scope for the degraded event
// (matching the DESIGN examples: "city", "rig/<name>").
func conditionalWritesStoreID(scopeRoot, cityPath string) string {
	if samePath(scopeRoot, cityPath) {
		return "city"
	}
	return "rig/" + filepath.Base(scopeRoot)
}

// openControlBdStoreThroughFactory routes a control-plane bd store through
// the beads factory so it carries the conditional-writes stamp. No
// PreflightChecker is supplied, so the factory can never select the native
// store for the control path (the zero checker fails preflight and the
// factory takes the bd fallback — pinned by
// TestOpenStoreAtForCityNilPreflightCheckerFallsBackToBd); the store comes
// back raw, matching the control path's deliberately unwrapped handles.
func openControlBdStoreThroughFactory(scopeRoot, cityPath, provider string, cfg *config.City, openBd func() (beads.Store, error)) (beads.Store, error) {
	flags, resolved := resolvedConditionalWritesFlags(cfg)
	mode := gate.ModeUnset
	if resolved {
		mode = flags.BeadsConditionalWrites()
	}
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         scopeRoot,
		CityPath:          cityPath,
		Provider:          provider,
		ConditionalWrites: mode,
		OnConditionalWritesDegraded: lazyConditionalWritesDegradeEmitter(
			cityPath, conditionalWritesStoreID(scopeRoot, cityPath), flags, resolved),
		OpenBdStore: openBd,
	})
	if err != nil {
		return nil, err
	}
	return result.Store, nil
}

// conditionalWritesEventStoreKind maps internal store-kind names onto the
// beads.conditional_writes.degraded wire vocabulary
// (bd | native | sqlite-graph | caching | mem | file).
func conditionalWritesEventStoreKind(kind string) string {
	switch kind {
	case beads.BeadsStoreNameBdStore:
		return "bd"
	case beads.BeadsStoreNameNativeDoltStore:
		return "native"
	case beads.BeadsStoreNameFileStore:
		return "file"
	case "MemStore":
		return "mem"
	case "CachingStore":
		return "caching"
	case "*beads.DoltliteReadStore":
		// DoltliteReadStore only exists under the gascity_native_beads build
		// tag, so beads.conditionalStoreKind cannot name it and it arrives as
		// the %T spelling. It embeds *BdStore and its entire conditional-write
		// surface IS bd's, so on the wire it is a bd store.
		return "bd"
	default:
		return kind
	}
}

// conditionalWritesDegradedRecorder converts the beads factory's degrade
// notification into the typed beads.conditional_writes.degraded event,
// attaching what only the composition root knows: the store scope and the
// resolved mode's origin. The factory latches invocation once per store
// instance, so this cannot storm.
func conditionalWritesDegradedRecorder(rec events.Recorder, flags rollout.Flags, storeID string) func(beads.ConditionalWritesDegrade) {
	if rec == nil {
		return nil
	}
	return func(d beads.ConditionalWritesDegrade) {
		payload, err := json.Marshal(events.ConditionalWritesDegradedPayload{
			StoreID:   storeID,
			StoreKind: conditionalWritesEventStoreKind(d.StoreKind),
			Mode:      d.Mode,
			Origin:    string(flags.OriginOf(rollout.KeyBeadsConditionalWrites)),
			Reason:    d.Reason,
		})
		if err != nil {
			return
		}
		rec.Record(events.Event{
			Type:    events.BeadsConditionalWritesDegraded,
			Actor:   "gc",
			Subject: storeID,
			Message: fmt.Sprintf("conditional_writes degraded: store=%s mode=%s reason=%q", storeID, d.Mode, d.Reason),
			Payload: payload,
		})
	}
}
