package worker

import (
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// testActor is an Actor of kind on a fresh t.TempDir() city.
func testActor(t testing.TB, kind sessionpkg.ActorKind) sessionpkg.Actor {
	t.Helper()
	city, err := sessionpkg.NewCityDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return sessionpkg.Actor{Kind: kind, City: city}
}

func testOperator(t testing.TB) sessionpkg.Actor {
	t.Helper()
	return testActor(t, sessionpkg.ActorOperator)
}

func testAgent(t testing.TB) sessionpkg.Actor {
	t.Helper()
	return testActor(t, sessionpkg.ActorAgent)
}

func testController(t testing.TB) sessionpkg.Actor {
	t.Helper()
	return testActor(t, sessionpkg.ActorController)
}
