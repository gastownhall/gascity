package herdr

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime/herdr/herdrtest"
)

// requireLiveHerdr gates this package's live journeys. The decision itself,
// and the reasoning behind making the tier opt-in, live in
// internal/runtime/herdr/herdrtest: the controller's own live journeys under
// cmd/gc need the same gate, and one predicate cannot drift from itself.
func requireLiveHerdr(t *testing.T) {
	t.Helper()
	herdrtest.RequireLive(t)
}
