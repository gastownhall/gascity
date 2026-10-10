package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// testOperator is an operator on a fresh t.TempDir() city. A test of the
// server's own Managers uses Server.actor.
func testOperator(t testing.TB) session.Actor {
	t.Helper()
	return testOperatorIn(t, t.TempDir())
}

// testOperatorIn is an operator on city, whose runtime dir holds the lease.
func testOperatorIn(t testing.TB, city string) session.Actor {
	t.Helper()
	dir, err := session.NewCityDir(city)
	if err != nil {
		t.Fatal(err)
	}
	return session.Actor{Kind: session.ActorOperator, City: dir}
}
