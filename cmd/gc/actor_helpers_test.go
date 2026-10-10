package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// Actors with no City, for the package's tests of leaseless Managers
// (runtime_lease_optout_test.go). A test whose Manager leases in a city uses
// testActorIn.
var (
	testOperator   = session.Actor{Kind: session.ActorOperator}
	testController = session.Actor{Kind: session.ActorController}
)

// testActorIn is an Actor of kind on city, whose runtime dir holds the lease.
func testActorIn(t *testing.T, kind session.ActorKind, city string) session.Actor {
	t.Helper()
	dir, err := session.NewCityDir(city)
	if err != nil {
		t.Fatal(err)
	}
	return session.Actor{Kind: kind, City: dir}
}
