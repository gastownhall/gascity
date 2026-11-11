package storeref

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// fakeView is one side of fakeProjection over a front.
type fakeView struct {
	beads.Store
	front beads.Store
	class bool
}

// fakeProjection views a store as a fakeView and sees through its own views.
type fakeProjection struct{}

func (fakeProjection) WorkView(s beads.Store) beads.Store {
	return &fakeView{Store: s, front: s}
}

func (fakeProjection) ClassView(s beads.Store) beads.Store {
	return &fakeView{Store: s, front: s, class: true}
}

// unviewingProjection is fakeProjection with the optional Unviewer half.
type unviewingProjection struct{ fakeProjection }

func (unviewingProjection) ViewFront(s beads.Store) (beads.Store, func(beads.Store) beads.Store, bool) {
	v, ok := s.(*fakeView)
	if !ok {
		return s, nil, false
	}
	return v.front, func(r beads.Store) beads.Store { return &fakeView{Store: r, front: r, class: v.class} }, true
}

func TestUnviewRebuildsBehindTheSameSide(t *testing.T) {
	p := unviewingProjection{}
	for _, class := range []bool{false, true} {
		front := beads.NewMemStore()
		viewed := p.WorkView(front)
		if class {
			viewed = p.ClassView(front)
		}
		got, rewrap := Unview(p, viewed)
		if got != beads.Store(front) {
			t.Fatalf("class=%v: Unview front = %v, want the viewed store", class, got)
		}
		rebuilt := beads.NewMemStore()
		v, ok := rewrap(rebuilt).(*fakeView)
		if !ok || v.front != beads.Store(rebuilt) || v.class != class {
			t.Fatalf("class=%v: rewrap = %#v, want a view of the rebuilt store on the same side", class, rewrap(rebuilt))
		}
	}
}

func TestUnviewIsIdentityWithoutAnUnviewerOrAView(t *testing.T) {
	store := beads.NewMemStore()
	for name, p := range map[string]Projection{
		"nil projection":       nil,
		"no Unviewer":          fakeProjection{},
		"not one of its views": unviewingProjection{},
	} {
		got, rewrap := Unview(p, store)
		if got != beads.Store(store) || rewrap(store) != beads.Store(store) {
			t.Fatalf("%s: Unview changed the store", name)
		}
	}
}
