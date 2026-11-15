package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// registeredSessionFrontDoor returns the session front door over the
// session-class store of the runtime this process registered for cityPath
// (registerResidencyRoutes): the class binding when its routes relocate
// sessions, and its city work store otherwise.
//
// It is for code that runs INSIDE a controller or supervisor, such as the
// controller socket's commands, and it never reaches the one-shot funnel.
// cliStorageRoutes memoizes its answer for the life of the process. That is
// right for a one-shot command and wrong here: a supervisor that resolved a city
// as single-store before `gc storage migrate` keeps answering "work" after the
// city's runtime has re-booted onto the binding, so every session bead the
// migration moved reads as not found. The registration is replaced on every
// boot, so it is what this process serves now, and it hands back the runtime's
// own stores rather than a second handle on the same roots.
//
// No registration is an error, not a fallback: a standalone controller starts
// its socket before its runtime registers (and keeps it open while it shuts
// down after unregistering), and the only other answer available then is the
// funnel.
func registeredSessionFrontDoor(cityPath string) (*session.Store, error) {
	entry, ok := registeredResidencyEntry(cityPath)
	if !ok {
		return nil, fmt.Errorf("no runtime in this controller is serving city %s right now (it is starting or stopping); retry once it is running", cityPath)
	}
	var work beads.Store
	if entry.work != nil {
		work = entry.work()
	}
	sessions := scopeStores(cityPath, entry.routes, work).Sessions()
	if sessions == (beads.SessionStore{}) {
		return nil, fmt.Errorf("the runtime serving city %s has no session store: its city work store did not open (see the controller log); restart the controller once the store is reachable", cityPath)
	}
	return session.NewStore(sessions), nil
}
