package worker

import sessionpkg "github.com/gastownhall/gascity/internal/session"

// Actors with no City, for the package's tests of leaseless Managers
// (runtime_lease_optout_test.go).
var (
	testOperator   = sessionpkg.Actor{Kind: sessionpkg.ActorOperator}
	testAgent      = sessionpkg.Actor{Kind: sessionpkg.ActorAgent}
	testController = sessionpkg.Actor{Kind: sessionpkg.ActorController}
)
