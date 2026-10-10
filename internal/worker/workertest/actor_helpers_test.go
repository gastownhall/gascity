package workertest

import (
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// testAgent is an agent on a fresh t.TempDir() city.
func testAgent(t testing.TB) sessionpkg.Actor {
	t.Helper()
	city, err := sessionpkg.NewCityDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return sessionpkg.Actor{Kind: sessionpkg.ActorAgent, City: city}
}
