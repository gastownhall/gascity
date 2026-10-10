package workertest

import sessionpkg "github.com/gastownhall/gascity/internal/session"

// testAgent is an Actor with no City, for the package's tests of leaseless
// Managers (runtime_lease_optout_test.go).
var testAgent = sessionpkg.Actor{Kind: sessionpkg.ActorAgent}
