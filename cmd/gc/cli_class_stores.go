package main

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// cliSessionsRelocated reports whether the city at cityPath binds the sessions
// coordination class to a store of its own. Commands that sweep city + rig
// scopes need this: class routing is city-keyed, so under a relocation every
// scope's cliSessionStore resolves to the SAME sessions store and a per-scope
// session pass repeats itself once per rig.
func cliSessionsRelocated(cityPath string) bool {
	_, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassSessions)
	return relocated
}

// cliNudgesStore routes a generic CLI one-shot work store to the nudges
// coordination-class store, so a [beads.classes.nudges] relocation reaches
// one-shot commands the same way it reaches the running controller (which routes
// through CityRuntime.nudgesBeadStore). It is the nudges twin of cliSessionStore,
// and exists because the controller and the CLI had drifted: the nudge-mail
// sweep watchdog at city_runtime.go routed while `gc order sweep-nudge-mail` did
// not, so the same sweep read different stores depending on who ran it.
//
// The handle wraps the input store itself on a city that relocates nothing.
func cliNudgesStore(store beads.Store, _ *config.City, cityPath string) beads.NudgesStore {
	return cliStores(cityPath, storeref.Leg{Store: store}).Nudges()
}

// cliGraphStore routes a generic CLI one-shot work store to the graph
// coordination-class store. Graph twin of cliSessionStore.
func cliGraphStore(store beads.Store, _ *config.City, cityPath string) beads.GraphStore {
	return cliStores(cityPath, storeref.Leg{Store: store}).Graph(storeref.WorkRef)
}

// cliMailStore routes a generic CLI one-shot work store to the messaging
// coordination-class store. It is the messaging twin of cliSessionStore; see
// cliNudgesStore for why the CLI needs its own seam.
//
// Messaging and sessions are distinct classes even though today's only servable
// split shape (storageSplitWhole) puts them in the same binding: a mail bead is
// ClassMessaging, the mailbox identity it resolves against is ClassSessions, and
// the two route independently.
func cliMailStore(store beads.Store, _ *config.City, cityPath string) beads.MailStore {
	return cliStores(cityPath, storeref.Leg{Store: store}).Mail()
}
