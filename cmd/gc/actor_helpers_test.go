package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// testOperator is an operator on a fresh t.TempDir() city. A test whose
// Manager leases in a city it shares with another holder uses testActorIn.
func testOperator(t testing.TB) session.Actor {
	t.Helper()
	return testActorIn(t, session.ActorOperator, t.TempDir())
}

// testActorIn is an Actor of kind on city, whose runtime dir holds the lease.
func testActorIn(t testing.TB, kind session.ActorKind, city string) session.Actor {
	t.Helper()
	dir, err := session.NewCityDir(city)
	if err != nil {
		t.Fatal(err)
	}
	return session.Actor{Kind: kind, City: dir}
}
