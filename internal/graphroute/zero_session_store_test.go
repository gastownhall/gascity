package graphroute

import (
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestZeroSessionStoreReadsNoStore pins graphroute's absent sessions store
// (typed-stores T5a): the zero handle reads no store, as a nil store did. A
// direct-session lookup over it reports no binding without asking the
// resolver, and the session-name
// lookup falls back to the configured name instead of listing through a nil
// store.
func TestZeroSessionStoreReadsNoStore(t *testing.T) {
	resolved := false
	deps := Deps{DirectSessionResolver: func(beads.SessionStore, string, string, *config.City, string, string) (string, bool, error) {
		resolved = true
		return "materialized", true, nil
	}}
	binding, ok, err := ResolveGraphDirectSessionBinding(beads.SessionStore{}, "city", &config.City{}, "worker", "", deps)
	if err != nil || ok || binding != (GraphRouteBinding{}) {
		t.Errorf("ResolveGraphDirectSessionBinding over the zero handle = %+v, %v, %v; want no binding", binding, ok, err)
	}
	if resolved {
		t.Error("the direct-session resolver ran over the zero handle; an absent store resolves nothing")
	}
	if got, want := lookupSessionName(beads.SessionStore{}, "city", "rig/worker", ""), agent.SessionNameFor("city", "rig/worker", ""); got != want {
		t.Errorf("lookupSessionName over the zero handle = %q, want the configured name %q", got, want)
	}
}
