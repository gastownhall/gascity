package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// newTestManager is NewManagerWithOptions with a city, as every production
// Manager has: t.TempDir(), unless opts set another. Only a test of the
// no-city refusal builds a Manager without one.
func newTestManager(t testing.TB, store beads.Store, sp runtime.Provider, opts ...ManagerOption) *Manager {
	t.Helper()
	return NewManagerWithOptions(store, sp, append([]ManagerOption{WithCityPath(t.TempDir())}, opts...)...)
}
