package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// testActorIn is an Actor of kind on city, whose runtime dir holds the lease.
func testActorIn(t testing.TB, kind session.ActorKind, city string) session.Actor {
	t.Helper()
	dir, err := session.NewCityDir(city)
	if err != nil {
		t.Fatal(err)
	}
	return session.Actor{Kind: kind, City: dir}
}

func testOperatorIn(t testing.TB, city string) session.Actor {
	t.Helper()
	return testActorIn(t, session.ActorOperator, city)
}

// leasedStart is item with the lease the planned start takes on its
// candidate before the wave (executePlannedStartsTraced): a test that runs
// the wave or one candidate directly holds it too. release ends it.
func leasedStart(t testing.TB, city string, item preparedStart) (preparedStart, func()) {
	t.Helper()
	lease, release, err := tryRuntimeLease(nil, city, startLeaseName(item.candidate), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	item.candidate.lease = lease
	return item, release
}
