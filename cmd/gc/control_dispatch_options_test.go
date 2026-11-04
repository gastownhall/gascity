package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// TestControlDispatchStoreOptions pins the stores a control dispatch hands
// internal/dispatch (typed-store T5b). Work is the scope store, which is the
// store dispatch's work-class reads took before the work handle was named: the
// member tail on a split city, the control's own store otherwise. Sessions is
// the control's graph store, as attempt routing has always read it. The
// member tail names the scope store exactly when the caller asks for it; the
// caller asks only on a split city (TestControlDispatchDrainNamesTheWorkLegForConvoyMembership
// pins that end to end, and the drain refuses a tail that disagrees).
func TestControlDispatchStoreOptions(t *testing.T) {
	scope := splittest.NewWorkStore(t, "hq")
	binding := splittest.NewBindingStore(t)
	for name, tc := range map[string]struct {
		graph      beads.Store
		memberTail bool
	}{
		"a city that relocates nothing": {graph: scope},
		"a split city":                  {graph: binding, memberTail: true},
	} {
		opts := controlDispatchOptions("/city", "/city", beads.GraphStore{Store: tc.graph}, beads.WorkStore{Store: scope}, tc.memberTail)

		// What dispatch's work-class reads took before T5b: the first member
		// store, else the control's own (graph) store.
		was := tc.graph
		if len(opts.MemberStores) > 0 {
			was = opts.MemberStores[0]
		}
		if !beads.SameStore(opts.Work, was) || !beads.SameStore(opts.Work, scope) {
			t.Errorf("%s: Work is not the scope store dispatch's work reads took", name)
		}
		if !beads.SameStore(opts.Sessions, tc.graph) {
			t.Errorf("%s: Sessions is not the control's graph store", name)
		}
		if got := len(opts.MemberStores) > 0; got != tc.memberTail {
			t.Errorf("%s: member tail named = %v, want %v", name, got, tc.memberTail)
		}
	}
}
