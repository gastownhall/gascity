package main

import (
	"fmt"

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
// its socket before its runtime registers, and the only other answer available
// then is the funnel.
func registeredSessionFrontDoor(cityPath string) (*session.Store, error) {
	entry, ok := registeredResidencyEntry(cityPath)
	if !ok {
		return nil, fmt.Errorf("this controller is not serving city %s yet; retry once it has started", cityPath)
	}
	if entry.work == nil || entry.work() == nil {
		return nil, fmt.Errorf("the controller serving city %s has no city work store open yet; retry once it has started", cityPath)
	}
	return session.NewStore(scopeStores(cityPath, entry.routes, entry.work()).Sessions()), nil
}
