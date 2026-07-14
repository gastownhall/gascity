package main

import (
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
// The recorder is nil: a one-shot CLI command has no live event bus, matching
// today's behavior where these paths emit no bead events. Threading a recorder
// so relocated CLI writes emit bead.* is a separate follow-up.
func cliSessionStore(store beads.Store, cfg *config.City, cityPath string) beads.Store {
	return resolveSessionStore(store, cfg, cityPath, nil)
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
