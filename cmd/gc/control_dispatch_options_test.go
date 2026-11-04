package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
)

// TestControlDispatchStoreOptions pins the stores a control dispatch hands
// internal/dispatch (typed-store T5b). Work is the scope store, which is the
// store dispatch's work-class reads took before the work handle was named: the
// member tail on a split city, the control's own store otherwise. Sessions is
// the control's graph store, as attempt routing has always read it. The member
// tail is on exactly when the caller asks for it; controlMemberTail decides
// that (TestControlMemberTail).
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

		// What dispatch's work-class reads took before T5b: the member tail
		// (the scope store), else the control's own (graph) store.
		was := tc.graph
		if opts.MemberTail {
			was = scope
		}
		if !beads.SameStore(opts.Work, was) || !beads.SameStore(opts.Work, scope) {
			t.Errorf("%s: Work is not the scope store dispatch's work reads took", name)
		}
		if !beads.SameStore(opts.Sessions, tc.graph) {
			t.Errorf("%s: Sessions is not the control's graph store", name)
		}
		if opts.MemberTail != tc.memberTail {
			t.Errorf("%s: MemberTail = %v, want %v", name, opts.MemberTail, tc.memberTail)
		}
	}
}

// TestControlMemberTail pins which control kinds read their work beads
// through the scope store as a member tail: the four that read beads they did
// not create, and only when the graph class relocated.
func TestControlMemberTail(t *testing.T) {
	scope := splittest.NewWorkStore(t, "hq")
	binding := splittest.NewBindingStore(t)
	tailKinds := map[string]bool{
		beadmeta.KindDrain:     true,
		beadmeta.KindRetryEval: true,
		beadmeta.KindRetry:     true,
		beadmeta.KindRalph:     true,
	}
	for _, kind := range beadmeta.ControlKinds {
		if got := controlMemberTail(kind, beads.GraphStore{Store: scope}, beads.WorkStore{Store: scope}); got {
			t.Errorf("%s on a city that relocates nothing: member tail = true, want false", kind)
		}
		if got, want := controlMemberTail(kind, beads.GraphStore{Store: binding}, beads.WorkStore{Store: scope}), tailKinds[kind]; got != want {
			t.Errorf("%s on a split city: member tail = %v, want %v", kind, got, want)
		}
	}
}
