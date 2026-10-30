package beadstest

import (
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// The test-side mint for typed class handles (typed-stores design §3.1,
// §7.3). Tests build handles here rather than by literal, so the day the
// handles are sealed (T10) the literal rewrite is a call that already exists.
// Production code may not import this package: the storeclass lint reports
// it (test-mint-import).

// Work is a work-class handle over s, for a test.
func Work(s beads.Store) beads.WorkStore { return beads.WorkStore{Store: s} }

// Graph is a graph-class handle over s, for a test.
func Graph(s beads.Store) beads.GraphStore { return beads.GraphStore{Store: s} }

// Sessions is a sessions-class handle over s, for a test.
func Sessions(s beads.Store) beads.SessionStore { return beads.SessionStore{Store: s} }

// Mail is a messaging-class handle over s, for a test.
func Mail(s beads.Store) beads.MailStore { return beads.MailStore{Store: s} }

// Orders is an orders-class handle over s, for a test.
func Orders(s beads.Store) beads.OrdersStore { return beads.OrdersStore{Store: s} }

// Nudges is a nudges-class handle over s, for a test.
func Nudges(s beads.Store) beads.NudgesStore { return beads.NudgesStore{Store: s} }

// ClassStores is every class's handle, for a test that hands a fixture city
// to code taking several classes.
type ClassStores struct {
	Work     beads.WorkStore
	Graph    beads.GraphStore
	Sessions beads.SessionStore
	Mail     beads.MailStore
	Orders   beads.OrdersStore
	Nudges   beads.NudgesStore
}

// SingleStore is every class's handle over one store: a single-store city,
// where every class is served by the work store.
func SingleStore(s beads.Store) ClassStores {
	return ClassStores{
		Work: Work(s), Graph: Graph(s), Sessions: Sessions(s),
		Mail: Mail(s), Orders: Orders(s), Nudges: Nudges(s),
	}
}

// All is every handle of c, keyed by class name, each as a beads.Store.
func (c ClassStores) All() map[string]beads.Store {
	return map[string]beads.Store{
		"work": c.Work, "graph": c.Graph, "sessions": c.Sessions,
		"mail": c.Mail, "orders": c.Orders, "nudges": c.Nudges,
	}
}

// capabilityProbe looks one capability up through beads.As and names its
// dynamic type, or "" when the store lacks it.
type capabilityProbe struct {
	name string
	look func(beads.Store) (any, bool)
}

func probe[T any](name string) capabilityProbe {
	return capabilityProbe{name: name, look: func(s beads.Store) (any, bool) {
		v, ok := beads.As[T](s)
		return v, ok
	}}
}

// capabilityProbes is every optional capability a call site reaches for
// through a store today: the XFor-backed ones and the directly asserted ones
// (design §2, "Direct capability assertions").
var capabilityProbes = []capabilityProbe{
	probe[beads.ConditionalWriter]("ConditionalWriter"),
	probe[beads.GuardedBatcher]("GuardedBatcher"),
	probe[beads.GraphApplyStore]("GraphApplyStore"),
	probe[beads.MetadataCASWriter]("MetadataCASWriter"),
	probe[beads.AtomicConditionalCloser]("AtomicConditionalCloser"),
	probe[beads.NamespaceCensus]("NamespaceCensus"),
	probe[beads.DependencyBatchLister]("DependencyBatchLister"),
	probe[beads.GraphOnlyReadyStore]("GraphOnlyReadyStore"),
	probe[beads.AssignmentGuardedUpdater]("AssignmentGuardedUpdater"),
	probe[beads.AbsentBlockedReader]("AbsentBlockedReader"),
	probe[beads.Counter]("Counter"),
	probe[beads.ForeignIDCreator]("ForeignIDCreator"),
	probe[beads.StorageCreateStore]("StorageCreateStore"),
	probe[beads.StorageGraphApplyStore]("StorageGraphApplyStore"),
	probe[beads.EphemeralGraphApplyStore]("EphemeralGraphApplyStore"),
	probe[beads.ConditionalAssignmentReleaser]("ConditionalAssignmentReleaser"),
	probe[beads.ConditionalAssigneeTransferer]("ConditionalAssigneeTransferer"),
	probe[beads.DepMetadataReader]("DepMetadataReader"),
	probe[*beads.CachingStore]("*CachingStore"),
}

// RunClassHandleCapabilityConformance checks, for a store from newStore, that
// every class handle over it reaches every optional capability exactly as the
// store does: the same answer, the same dynamic type, and the same value where
// the type is comparable. A handle that hid a capability, or found a
// different one, would change behavior with no compile error (design §7.4);
// run it for every engine kind.
func RunClassHandleCapabilityConformance(t *testing.T, newStore func(t *testing.T) beads.Store) {
	t.Helper()
	raw := newStore(t)
	handles := SingleStore(raw).All()
	handles["nested"] = Graph(Work(raw))
	checkHandleCapabilities(t, raw, handles)
}

// errorReporter is the part of *testing.T the conformance reports through.
type errorReporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// checkHandleCapabilities reports each handle, keyed by class, that is not
// over raw or does not reach a capability exactly as raw does.
func checkHandleCapabilities(t errorReporter, raw beads.Store, handles map[string]beads.Store) {
	t.Helper()
	for class, h := range handles {
		if !beads.SameStore(h, raw) {
			t.Errorf("%s handle: SameStore with its store = false", class)
		}
		for _, p := range capabilityProbes {
			want, wantOK := p.look(raw)
			got, gotOK := p.look(h)
			if gotOK != wantOK {
				t.Errorf("%s handle, %s: found = %v, the store's = %v", class, p.name, gotOK, wantOK)
				continue
			}
			if !gotOK {
				continue
			}
			if reflect.TypeOf(got) != reflect.TypeOf(want) {
				t.Errorf("%s handle, %s: %T, the store's is %T", class, p.name, got, want)
				continue
			}
			if reflect.TypeOf(got).Comparable() && !sameValue(got, want) {
				t.Errorf("%s handle, %s: a different %T than the store's", class, p.name, got)
			}
		}
	}
}

// sameValue is got == want, false when the comparison panics (a comparable
// type holding a non-comparable interface value).
func sameValue(got, want any) (same bool) {
	defer func() {
		if recover() != nil {
			same = false
		}
	}()
	return got == want
}
