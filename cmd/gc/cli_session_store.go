package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// cliSessionStore routes a generic CLI one-shot work store to the session
// coordination-class store, so a [beads.classes.sessions] relocation reaches
// one-shot commands the same way it reaches the running controller (which routes
// through resolveSessionStore via CityRuntime.sessionsBeadStore). Identity to the
// input store at the default single-store backend (resolveSessionStore returns
// the store verbatim there), so wrapping is byte-identical until a session
// relocation is configured.
//
// The recorder is nil, and passing one would change nothing: resolveSessionStore
// ignores it. What makes a relocated one-shot write observable is the emit
// target the funnel puts on the ROUTES (class_store_emit.go), which
// cliStorageRoutes has already applied by the time this returns.
func cliSessionStore(store beads.Store, cfg *config.City, cityPath string) beads.Store {
	return resolveSessionStore(cliStorageRoutes(cityPath), store, cfg, cityPath, nil)
}

// cliSessionFrontDoor builds the typed session write front door over the
// session-class store for a CLI one-shot command. It is the relocation-safe
// replacement for sessionFrontDoor(store) at CLI command roots. The name
// deliberately does not contain the substring "sessionFrontDoor(" so the
// relocation guard (TestSessionRelocationRootsRouteThroughSessionClassStore) can
// forbid the unrouted form while allowing this one.
func cliSessionFrontDoor(store beads.Store, cfg *config.City, cityPath string) *session.Store {
	return sessionFrontDoor(cliSessionStore(store, cfg, cityPath))
}

// cliSessionStoreForCity resolves the session-class store for a CLI one-shot
// command that has a city path but no store handle of the right class yet. It
// opens the work store itself rather than accepting one from the caller, so
// it can never be handed another class's store as the base the way
// cliSessionStore can. Every nudge-CLI session-bead access point did exactly
// that: each had only opened the NUDGES store, so it became the accidental
// base for a session-class read -- correct while nudges and sessions share
// the work store, silently wrong the moment a city relocates
// [beads.classes.nudges] without also relocating [beads.classes.sessions]
// (gastownhall/gascity#6348).
func cliSessionStoreForCity(cityPath string, cfg *config.City) (beads.Store, error) {
	work, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return nil, fmt.Errorf("opening the city store at %q: %w", cityPath, err)
	}
	return cliSessionStore(work, cfg, cityPath), nil
}

// cliNudgeSessionStore is cliSessionStoreForCity for the nudge command tree
// specifically: every one of its session-bead access points held only a
// nudges-class store (see cliSessionStoreForCity's doc comment), so this
// derives the session store from the target's own city path instead of from
// whatever store the caller happens to be holding. A nil store preserves the
// store-less WithProvider caller's identity path -- some nudge call sites are
// deliberately reachable without a real store, and that shape must survive
// unchanged.
func cliNudgeSessionStore(store beads.Store, target nudgeTarget) (beads.Store, error) {
	if store == nil {
		return nil, nil
	}
	return cliSessionStoreForCity(target.cityPath, target.cfg)
}
