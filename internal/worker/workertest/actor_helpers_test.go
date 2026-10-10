package workertest

import (
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// managerActor is an Actor of kind on m's city.
func managerActor(t testing.TB, m *sessionpkg.Manager, kind sessionpkg.ActorKind) sessionpkg.Actor {
	t.Helper()
	city, err := m.CityDir()
	if err != nil {
		t.Fatal(err)
	}
	return sessionpkg.Actor{Kind: kind, City: city}
}

// testAgent is an agent on a fresh t.TempDir() city, for a runtime-only
// handle, which has no Manager to share one with.
func testAgent(t testing.TB) sessionpkg.Actor {
	t.Helper()
	city, err := sessionpkg.NewCityDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return sessionpkg.Actor{Kind: sessionpkg.ActorAgent, City: city}
}
