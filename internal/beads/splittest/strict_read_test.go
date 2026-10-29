package splittest

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/storeref"
)

func TestStrictWorkLeafRecordsAByIDReadOfAClassNamespaceID(t *testing.T) {
	work := NewWorkStore(t, "gc")
	// storeref.Resolve is a production by-id probe: handing the work leaf a
	// graph id asks the wrong store.
	if _, err := storeref.Resolve("gcg-7", []beads.Store{work}); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Resolve error = %v, want ErrNotFound", err)
	}
	got := TakeSplitViolations(work)
	if len(got) == 0 {
		t.Fatal("no split violation for a graph id read through the work leaf")
	}
	v := got[0]
	if v.Op != "get" || v.Class != "graph" || v.ID != "gcg-7" || v.Site.File != "internal/storeref/storeref.go" {
		t.Fatalf("violation = %+v", v)
	}
}

func TestStrictWorkLeafIgnoresAFederatedProbe(t *testing.T) {
	work := NewWorkStore(t, "gc")
	if _, err := beads.FederatedGet(work, "gcg-7"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("FederatedGet error = %v", err)
	}
	if got := TakeSplitViolations(work); len(got) != 0 {
		t.Fatalf("a federated probe was recorded: %+v", got)
	}
}

func TestStrictWorkLeafIgnoresWorkIDs(t *testing.T) {
	work := NewWorkStore(t, "gc")
	if _, err := work.Get("gc-404"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get error = %v", err)
	}
	if got := TakeSplitViolations(work); len(got) != 0 {
		t.Fatalf("a work id was recorded: %+v", got)
	}
}

func TestStrictClassLeafDoesNotPoliceRelicIDs(t *testing.T) {
	graph := NewClassStore(t, config.BeadClassGraph)
	if _, err := graph.Get("gc-12"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get error = %v", err)
	}
	if got := TakeSplitViolations(graph); len(got) != 0 {
		t.Fatalf("a relic-shaped probe of a class leaf was recorded: %+v", got)
	}
}

func TestStrictWorkLeafRecordsAByIDWriteToAClassNamespaceID(t *testing.T) {
	work := NewWorkStore(t, "gc")
	// convoy.SetConvoyFields is a production by-id metadata write.
	_ = convoy.SetConvoyFields(work, "gcs-3", convoy.ConvoyFields{Owner: "x"})
	got := TakeSplitViolations(work)
	if len(got) != 1 || got[0].Op != "set-metadata" || got[0].Class != "sessions" || got[0].Site.File != "internal/convoy/convoy_fields.go" {
		t.Fatalf("violations = %+v, want one set-metadata of a sessions id charged to convoy_fields.go", got)
	}
}

func TestStrictDepAddReportsACrossStoreEdgeOnce(t *testing.T) {
	work := NewWorkStore(t, "gc")
	task, err := work.Create(beads.Bead{Title: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(task.ID, "gcg-1", "blocks"); err == nil {
		t.Fatal("cross-store dep accepted on a BdSemantics leaf")
	}
	if got := TakeSplitViolations(work); len(got) != 0 {
		t.Fatalf("DepAdd's own endpoint resolution was recorded as a wrong-store read: %+v", got)
	}
}

func TestLeafClasses(t *testing.T) {
	if got := leafClasses(BdSemantics, nil); !got[coordclass.ClassWork] || len(got) != 1 {
		t.Fatalf("unfenced bd leaf classes = %v, want work", got)
	}
	if got := leafClasses(SQLiteSemantics, nil); got != nil {
		t.Fatalf("unfenced sqlite leaf classes = %v, want nil", got)
	}
	union := storeref.ReservedPrefixesFor([]coordclass.Class{coordclass.ClassGraph, coordclass.ClassSessions, coordclass.ClassMessaging, coordclass.ClassOrders, coordclass.ClassNudges})
	got := leafClasses(SQLiteSemantics, normalizeNamespaces(union))
	if len(got) != 5 || got[coordclass.ClassWork] {
		t.Fatalf("whole-binding leaf classes = %v, want the five infra classes", got)
	}
}

func TestUnclaimedSplitViolationsMessage(t *testing.T) {
	if unclaimedSplitViolationsMessage("gc", nil) != "" {
		t.Fatal("empty message for no violations expected")
	}
	msg := unclaimedSplitViolationsMessage("gc", []SplitViolation{{View: "gc", Op: "get", Class: "graph", ID: "gcg-1", Site: Site{File: "a.go", Line: 1, Func: "a.f"}}})
	for _, want := range []string{"1 split violation", "beads.FederatedRead", "TakeSplitViolations", "a.go:1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q: %s", want, msg)
		}
	}
}

func TestStrictWorkLeafIgnoresTheTestsOwnProbe(t *testing.T) {
	work := NewWorkStore(t, "gc")
	if _, err := work.Get("gcg-9"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get error = %v", err)
	}
	if got := TakeSplitViolations(work); len(got) != 0 {
		t.Fatalf("a test-code probe (no production frame) was recorded: %+v", got)
	}
}

func TestStrictLeavesRecordAProductionWrongClassCreate(t *testing.T) {
	work, graph := NewSplitStores(t)
	// beadmail is production code: a message sent into the graph leaf lands a
	// messaging-class row in a store that holds only graph rows.
	if _, err := beadmail.New(graph).Send("a", "b", "s", "body"); err != nil {
		t.Fatal(err)
	}
	got := TakeSplitViolations(graph)
	if len(got) != 1 || got[0].Op != "create" || got[0].Class != "messaging" {
		t.Fatalf("graph leaf violations = %+v, want one messaging create", got)
	}
	// Test code seeding a stand-in row records nothing.
	if _, err := work.Create(sessionBead("seed")); err != nil {
		t.Fatal(err)
	}
	if got := TakeSplitViolations(work); len(got) != 0 {
		t.Fatalf("a test-code seed was recorded: %+v", got)
	}
}

func TestNewSingleStoreHoldsEveryClass(t *testing.T) {
	single := NewSingleStore(t, "hq")
	if _, err := beadmail.New(single).Send("a", "b", "s", "body"); err != nil {
		t.Fatal(err)
	}
	if got := TakeSplitViolations(single); len(got) != 0 {
		t.Fatalf("a single-store city's store recorded a class: %+v", got)
	}
}

func TestNewBindingStoreHoldsEveryInfrastructureClass(t *testing.T) {
	binding := NewBindingStore(t)
	if _, err := beadmail.New(binding).Send("a", "b", "s", "body"); err != nil {
		t.Fatal(err)
	}
	if got := TakeSplitViolations(binding); len(got) != 0 {
		t.Fatalf("a mail message in the whole binding was recorded: %+v", got)
	}
	if got := leafClasses(SQLiteSemantics, binding.(*StrictStore).namespaces); len(got) != 5 || got[coordclass.ClassWork] {
		t.Fatalf("binding classes = %v, want the five infrastructure classes", got)
	}
}
