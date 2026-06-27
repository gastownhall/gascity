package api

import (
	"reflect"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// classStoreFor returns the bead store that owns beads of the given
// coordination class. This is the API-side seam of the per-class store
// refactor: every coordination class is given a named entry point so that a
// future per-class backend becomes a change in this one function rather than
// at every call site.
//
// On a single-store city every class collapses to the same concrete store, so
// this is an identity helper today: graph, session, wait, nudge, order, mail,
// and wisp beads resolve to the city-level store, and work beads resolve to the
// named rig store (or the city store when no rig is given). The returned value
// is the exact beads.Store the caller would have fetched directly, never a
// wrapping adapter, so optional-capability type assertions on it still succeed.
//
// classStoreFor is a free function over State rather than a new interface
// method so that every State implementation and test mock keeps working
// without growing the interface.
func classStoreFor(st State, class coordclass.Class, rig string) beads.Store {
	if class == coordclass.ClassWork && rig != "" {
		return st.BeadStore(rig)
	}
	return st.CityBeadStore()
}

// classBeadStoresForID returns the ordered set of stores to probe for a bead of
// the given coordination class, prepending the class store ahead of the
// generic by-id resolution order. Duplicates are removed by store identity, so
// on a single-store city the class store and the by-id fallback collapse to one
// entry. The class store is prepended (not substituted) so a bead that has been
// relocated to a per-class store is found first while the legacy scan order
// still resolves anything the class store does not own.
func (s *Server) classBeadStoresForID(class coordclass.Class, rig, id string) []beads.Store {
	fallback := s.beadStoresForID(id)
	classStore := classStoreFor(s.state, class, rig)
	if classStore == nil {
		return fallback
	}
	out := make([]beads.Store, 0, len(fallback)+1)
	seen := make(map[uintptr]struct{}, len(fallback)+1)
	out = append(out, classStore)
	if key, ok := storeIdentityKey(classStore); ok {
		seen[key] = struct{}{}
	}
	for _, st := range fallback {
		if st == nil {
			continue
		}
		if key, ok := storeIdentityKey(st); ok {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
		}
		out = append(out, st)
	}
	return out
}

// storeIdentityKey returns a pointer-identity key for a store and whether one
// could be derived. Non-pointer stores (none in production) yield ok=false and
// are never deduplicated, matching the conservative behavior of the controller
// store-pointer helper.
func storeIdentityKey(store beads.Store) (uintptr, bool) {
	value := reflect.ValueOf(store)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
		return 0, false
	}
	return value.Pointer(), true
}
