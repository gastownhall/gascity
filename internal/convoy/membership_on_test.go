package convoy

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// failingStore fails the reads named in failOn.
type failingStore struct {
	beads.Store
	failOn map[string]bool
}

var errLegDown = errors.New("leg down")

func (s failingStore) Get(id string) (beads.Bead, error) {
	if s.failOn["get"] || s.failOn["get:"+id] {
		return beads.Bead{}, errLegDown
	}
	return s.Store.Get(id)
}

func (s failingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.failOn["list"] {
		return nil, errLegDown
	}
	return s.Store.List(q)
}

func (s failingStore) DepList(id, dir string) ([]beads.Dep, error) {
	if s.failOn["deplist"] {
		return nil, errLegDown
	}
	return s.Store.DepList(id, dir)
}

// membershipFixture is a convoy with a legacy child, a tracked open item, a
// tracked closed item, a tracked item no store holds, a non-tracks edge, and
// a second convoy tracking the open item.
func membershipFixture(t *testing.T) (*beads.MemStore, string, string) {
	t.Helper()
	mem := beads.NewMemStore()
	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		got, err := mem.Create(b)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	convoy := mk(beads.Bead{Title: "convoy", Type: "convoy"})
	other := mk(beads.Bead{Title: "other convoy", Type: "convoy"})
	mk(beads.Bead{Title: "legacy child", Type: "task", ParentID: convoy.ID})
	open := mk(beads.Bead{Title: "open", Type: "task"})
	closed := mk(beads.Bead{Title: "closed", Type: "task"})
	if err := mem.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	notConvoy := mk(beads.Bead{Title: "a task tracking it", Type: "task"})
	for _, d := range []struct{ from, to, typ string }{
		{convoy.ID, open.ID, TrackingDepType},
		{convoy.ID, closed.ID, TrackingDepType},
		{other.ID, open.ID, TrackingDepType},
		{notConvoy.ID, open.ID, TrackingDepType},
		{convoy.ID, other.ID, "blocks"},
	} {
		if err := mem.DepAdd(d.from, d.to, d.typ); err != nil {
			t.Fatal(err)
		}
	}
	return mem, convoy.ID, open.ID
}

// TestMembersOnAgreesWithMembersIn: MembersOn over a federated leg of the
// convoy's store answers what MembersIn over the convoy class alone answers,
// includeClosed either way, with the same errors when a read fails; and
// TrackingConvoysOn answers what TrackingConvoysForItem does.
func TestMembersOnAgreesWithMembersIn(t *testing.T) {
	for _, fail := range []string{"", "list", "deplist", "get"} {
		mem, convoyID, itemID := membershipFixture(t)
		var store beads.Store = mem
		if fail != "" {
			store = failingStore{Store: mem, failOn: map[string]bool{fail: true}}
		}
		for _, includeClosed := range []bool{true, false} {
			name := fmt.Sprintf("fail=%q/includeClosed=%v", fail, includeClosed)
			want, wantErr := MembersIn(MemberClasses{Convoy: beadstest.Work(store)}, convoyID, includeClosed)
			got, gotErr := MembersOn(beadstest.FederatedLeg(store), convoyID, includeClosed)
			if !reflect.DeepEqual(ids(got), ids(want)) || errText(gotErr) != errText(wantErr) {
				t.Errorf("%s: MembersOn = %v, %v; MembersIn = %v, %v", name, ids(got), gotErr, ids(want), wantErr)
			}
		}
		want, wantErr := TrackingConvoysForItem(beadstest.Work(store), itemID)
		got, gotErr := TrackingConvoysOn(beadstest.FederatedLeg(store), itemID)
		if !reflect.DeepEqual(ids(got), ids(want)) || errText(gotErr) != errText(wantErr) {
			t.Errorf("fail=%q: TrackingConvoysOn = %v, %v; TrackingConvoysForItem = %v, %v", fail, ids(got), gotErr, ids(want), wantErr)
		}
	}
}

// TestMembersOnListsLegacyAndTrackedMembers pins MembersOn's answer on its
// own (MembersIn shares the walk, so agreeing with it alone proves nothing
// about the walk): the legacy child and both tracked items, the closed one
// only with includeClosed, and not the non-tracks edge's target.
func TestMembersOnListsLegacyAndTrackedMembers(t *testing.T) {
	mem, convoyID, _ := membershipFixture(t)
	for includeClosed, want := range map[bool]int{true: 3, false: 2} {
		got, err := MembersOn(beadstest.FederatedLeg(mem), convoyID, includeClosed)
		if err != nil || len(got) != want {
			t.Errorf("includeClosed=%v: MembersOn = %v, %v; want %d members", includeClosed, ids(got), err, want)
		}
		for _, b := range got {
			if b.Type == "convoy" {
				t.Errorf("includeClosed=%v: the non-tracks edge's target %s is a member", includeClosed, b.ID)
			}
		}
	}
}

// TestMembersOnRefusesAZeroLeg: a zero reader is the missing convoy class,
// as a zero handle is to MembersIn.
func TestMembersOnRefusesAZeroLeg(t *testing.T) {
	if _, err := MembersOn(beads.FederatedReader{}, "c-1", true); !errors.Is(err, ErrNoConvoyClass) {
		t.Errorf("MembersOn(zero) = %v, want ErrNoConvoyClass", err)
	}
	if _, err := TrackingConvoysOn(beads.FederatedReader{}, "i-1"); !errors.Is(err, ErrNoConvoyClass) {
		t.Errorf("TrackingConvoysOn(zero) = %v, want ErrNoConvoyClass", err)
	}
}

func ids(bs []beads.Bead) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.ID+":"+b.Status)
	}
	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
