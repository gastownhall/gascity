package api

import "github.com/gastownhall/gascity/internal/session"

// testOperator is an Actor with no City, for the package's tests of
// leaseless Managers (runtime_lease_optout_test.go). A test of the server's
// own Managers uses Server.actor.
var testOperator = session.Actor{Kind: session.ActorOperator}
