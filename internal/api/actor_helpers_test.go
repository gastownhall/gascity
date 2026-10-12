package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// testOperatorIn is an operator on city, whose runtime dir holds the lease: a
// test of the server's own Managers uses Server.actor.
func testOperatorIn(t testing.TB, city string) session.Actor {
	t.Helper()
	dir, err := session.NewCityDir(city)
	if err != nil {
		t.Fatal(err)
	}
	return session.Actor{Kind: session.ActorOperator, City: dir}
}
