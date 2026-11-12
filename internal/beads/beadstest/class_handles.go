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

// GraphMolecule is a graph-class molecule target over s, for a test.
func GraphMolecule(s beads.Store) beads.MoleculeTarget { return beads.GraphMolecule(Graph(s)) }

// WorkMolecule is a work-class molecule target over s, for a test.
func WorkMolecule(s beads.Store) beads.MoleculeTarget { return beads.WorkMolecule(Work(s)) }

// FederatedLeg is a federated leg over s, for a test: a planned federation
// leg or residence probe, whose reads the split-strict gate filters but does
// not charge.
func FederatedLeg(s beads.Store) beads.FederatedReader { return beads.MintFederatedLeg(s, "") }

// RelicLeg is a relic leg over s, for a test: its reads the split-strict gate
// filters and counts as relic debt.
func RelicLeg(s beads.Store) beads.FederatedReader { return beads.MintRelicLeg(s, "") }

// StrictLeg is a test's leg over s that declares no role
// (beads.MintStrictLeg), named "".
func StrictLeg(s beads.Store) beads.FederatedReader { return beads.MintStrictLeg(s, "") }

// Resident is a resident handle over s, for a test.
func Resident(s beads.Store) beads.ResidentStore { return beads.MintResident(s, "") }

// ResidentLeg is the writable handle over r's leg layer, for a test: a sweep
// leg, whose reads keep r's role.
func ResidentLeg(r beads.FederatedReader) beads.ResidentStore { return beads.MintResidentLeg(r) }

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

// capabilityProbe looks one capability up through beads.As, and through
// beads.ReaderAs on a leg reader. readable marks a read capability, the only
// kind ReaderAs answers: class-neutral, and writing nothing.
type capabilityProbe struct {
	name     string
	look     func(beads.Store) (any, bool)
	reader   func(beads.FederatedReader) (any, bool)
	readable bool
}

func probe[T any](name string) capabilityProbe {
	return capabilityProbe{name: name, look: func(s beads.Store) (any, bool) {
		v, ok := beads.As[T](s)
		return v, ok
	}, reader: func(r beads.FederatedReader) (any, bool) {
		v, ok := beads.ReaderAs[T](r)
		return v, ok
	}}
}

// readProbe is probe for a read capability.
func readProbe[T any](name string) capabilityProbe {
	p := probe[T](name)
	p.readable = true
	return p
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
	readProbe[beads.NamespaceCensus]("NamespaceCensus"),
	readProbe[beads.DependencyBatchLister]("DependencyBatchLister"),
	readProbe[beads.GraphOnlyReadyStore]("GraphOnlyReadyStore"),
	probe[beads.AssignmentGuardedUpdater]("AssignmentGuardedUpdater"),
	readProbe[beads.AbsentBlockedReader]("AbsentBlockedReader"),
	readProbe[beads.Counter]("Counter"),
	probe[beads.ForeignIDCreator]("ForeignIDCreator"),
	probe[beads.StorageCreateStore]("StorageCreateStore"),
	probe[beads.StorageGraphApplyStore]("StorageGraphApplyStore"),
	probe[beads.EphemeralGraphApplyStore]("EphemeralGraphApplyStore"),
	// Class-neutral (a class guard passes them), but writers: no leg reader
	// hands them out.
	probe[beads.ConditionalAssignmentReleaser]("ConditionalAssignmentReleaser"),
	probe[beads.ConditionalAssigneeTransferer]("ConditionalAssigneeTransferer"),
	readProbe[beads.DepMetadataReader]("DepMetadataReader"),
	probe[*beads.CachingStore]("*CachingStore"),
	readProbe[beads.ChildrenBatchLister]("ChildrenBatchLister"),
	probe[beads.AtomicTxStore]("AtomicTxStore"),
	readProbe[beads.CacheStatsReporter]("CacheStatsReporter"),
	readProbe[beads.CacheLivenessReporter]("CacheLivenessReporter"),
	readProbe[beads.CachedLister]("CachedLister"),
}

// RunClassHandleCapabilityConformance checks, for a store from newStore, that
// every class handle over it reaches every optional capability exactly as the
// store does: the same answer, the same dynamic type, and the same value where
// the type is comparable. A handle that hid a capability, or found a
// different one, would change behavior with no compile error (design §7.4);
// run it for every engine kind. It covers the resident handle and the three
// leg layers too, and checks that each leg reader reaches the class-neutral
// capabilities, and reads, exactly as the store does, and nothing else. A
// leg's role-carrying adapter (beads.LegAdapterBase) counts as the store's
// own capability.
func RunClassHandleCapabilityConformance(t *testing.T, newStore func(t *testing.T) beads.Store) {
	t.Helper()
	raw := newStore(t)
	handles := SingleStore(raw).All()
	handles["nested"] = Graph(Work(raw))
	handles["graph molecule"] = GraphMolecule(raw)
	handles["work molecule"] = WorkMolecule(raw)
	handles["resident"] = Resident(raw)
	handles["federated leg"] = ResidentLeg(FederatedLeg(raw))
	handles["relic leg"] = ResidentLeg(RelicLeg(raw))
	handles["strict leg"] = ResidentLeg(StrictLeg(raw))
	checkHandleCapabilities(t, raw, handles)
	readers := map[string]beads.FederatedReader{"federated leg": FederatedLeg(raw), "relic leg": RelicLeg(raw), "strict leg": StrictLeg(raw)}
	checkReaderCapabilities(t, raw, readers)
	checkLegReads(t, raw, readers)
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
		checkHelperAnswers(t, raw, class, h)
		for _, p := range capabilityProbes {
			want, wantOK := p.look(raw)
			got, gotOK := p.look(h)
			got = beads.LegAdapterBase(got) // a leg's role adapter over the store's own
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

// helperProbe asks one XFor helper that follows a wrapper's declared
// conditional-writes resolution target or its handle providers, rather than
// the class layers As walks: a layer that dropped
// ConditionalWritesResolveTarget, GuardedBatcherHandle or Handles would pass
// the As probes and still change these answers (a fenced close would resolve
// unset to legacy).
type helperProbe struct {
	name string
	look func(beads.Store) (any, bool)
}

var helperProbes = []helperProbe{
	{"MetadataCASWriterFor", func(s beads.Store) (any, bool) { return beads.MetadataCASWriterFor(s) }},
	{"AtomicConditionalCloserFor", func(s beads.Store) (any, bool) { return beads.AtomicConditionalCloserFor(s) }},
	{"AssignmentGuardedUpdaterFor", func(s beads.Store) (any, bool) { return beads.AssignmentGuardedUpdaterFor(s) }},
	{"ConditionalWriterForTarget", func(s beads.Store) (any, bool) { return beads.ConditionalWriterForTarget(s) }},
	{"GuardedBatcherFor", func(s beads.Store) (any, bool) { return beads.GuardedBatcherFor(s) }},
	{"ResolveConditionalWriter", resolvedConditionalWriter},
	{"IDPrefixFor", func(s beads.Store) (any, bool) { return beads.IDPrefixFor(s), true }},
	{"HandlesFor.Cached", func(s beads.Store) (any, bool) { return beads.HandlesFor(s).Cached, true }},
	{"HandlesFor.Live", func(s beads.Store) (any, bool) { return beads.HandlesFor(s).Live, true }},
}

// resolution is what ResolveConditionalWriter answers: the writer the
// store's stamped mode resolves to (nil under off or unset), whether it
// carried a diagnostic, and its error.
type resolution struct {
	writer     beads.ConditionalWriter
	diagnostic bool
	err        string
}

// resolvedConditionalWriter is the ResolveConditionalWriter probe. On a
// store stamped require it answers the store's writer; a layer that dropped
// its resolution target would read the mode as unset and answer none.
func resolvedConditionalWriter(s beads.Store) (any, bool) {
	writer, diag, err := beads.ResolveConditionalWriter(s)
	return resolution{writer: writer, diagnostic: diag != nil, err: errText(err)}, true
}

// checkHelperAnswers reports a handle on which a helperProbe answers other
// than on raw, or whose HandlesFor writer is not the handle itself.
func checkHelperAnswers(t errorReporter, raw beads.Store, class string, h beads.Store) {
	t.Helper()
	for _, p := range helperProbes {
		want, wantOK := p.look(raw)
		got, gotOK := p.look(h)
		got = beads.LegAdapterBase(got)
		switch {
		case gotOK != wantOK:
			t.Errorf("%s handle, %s: found = %v, the store's = %v", class, p.name, gotOK, wantOK)
		case !gotOK:
		case reflect.TypeOf(got) != reflect.TypeOf(want):
			t.Errorf("%s handle, %s: %T, the store's is %T", class, p.name, got, want)
		case reflect.TypeOf(got).Comparable() && !sameValue(got, want):
			t.Errorf("%s handle, %s: a different %T than the store's", class, p.name, got)
		}
	}
	if w := beads.HandlesFor(h).Writer; !sameValue(w, any(h)) {
		t.Errorf("%s handle: HandlesFor writer is %T, not the handle", class, w)
	}
}

// checkReaderCapabilities reports a leg reader that is not over raw, that
// reaches a read capability other than as raw does, or that reaches any
// other capability (a writer, the store) at all.
func checkReaderCapabilities(t errorReporter, raw beads.Store, readers map[string]beads.FederatedReader) {
	t.Helper()
	for leg, r := range readers {
		if !beads.SameStore(r, raw) {
			t.Errorf("%s reader: SameStore with its store = false", leg)
		}
		for _, p := range capabilityProbes {
			got, gotOK := p.reader(r)
			got = beads.LegAdapterBase(got)
			if !p.readable {
				if gotOK {
					t.Errorf("%s reader, %s: a read-only leg handed out %T", leg, p.name, got)
				}
				continue
			}
			want, wantOK := p.look(raw)
			switch {
			case gotOK != wantOK:
				t.Errorf("%s reader, %s: found = %v, the store's = %v", leg, p.name, gotOK, wantOK)
			case !gotOK:
			case reflect.TypeOf(got) != reflect.TypeOf(want):
				t.Errorf("%s reader, %s: %T, the store's is %T", leg, p.name, got, want)
			case reflect.TypeOf(got).Comparable() && !sameValue(got, want):
				t.Errorf("%s reader, %s: a different %T than the store's", leg, p.name, got)
			}
		}
	}
}

// checkLegReads seeds raw and reports a leg reader, or a resident handle over
// its leg, whose reads answer other than raw's.
func checkLegReads(t *testing.T, raw beads.Store, readers map[string]beads.FederatedReader) {
	t.Helper()
	parent, err := raw.Create(beads.Bead{Title: "parent", Type: "task", Labels: []string{"leg:parity"}, Assignee: "a", Metadata: map[string]string{"leg": "parity"}})
	if err != nil {
		t.Fatalf("seeding the leg-parity parent: %v", err)
	}
	child, err := raw.Create(beads.Bead{Title: "child", Type: "task", ParentID: parent.ID})
	if err != nil {
		t.Fatalf("seeding the leg-parity child: %v", err)
	}
	// A store that cannot add a dep (the fake bd) still has its DepList
	// answer compared.
	_ = raw.DepAdd(child.ID, parent.ID, "blocks")
	type answer struct {
		rows any
		err  string
	}
	reads := map[string]func(storeReads) answer{
		"Get": func(s storeReads) answer { b, err := s.Get(parent.ID); return answer{b, errText(err)} },
		"List": func(s storeReads) answer {
			b, err := s.List(beads.ListQuery{Label: "leg:parity"})
			return answer{b, errText(err)}
		},
		"Ready":          func(s storeReads) answer { b, err := s.Ready(); return answer{b, errText(err)} },
		"Children":       func(s storeReads) answer { b, err := s.Children(parent.ID); return answer{b, errText(err)} },
		"ListByLabel":    func(s storeReads) answer { b, err := s.ListByLabel("leg:parity", 0); return answer{b, errText(err)} },
		"ListByAssignee": func(s storeReads) answer { b, err := s.ListByAssignee("a", "open", 0); return answer{b, errText(err)} },
		"ListByMetadata": func(s storeReads) answer {
			b, err := s.ListByMetadata(map[string]string{"leg": "parity"}, 0)
			return answer{b, errText(err)}
		},
		"DepList": func(s storeReads) answer { d, err := s.DepList(child.ID, "down"); return answer{d, errText(err)} },
	}
	for leg, r := range readers {
		for name, read := range reads {
			want := read(raw)
			for via, s := range map[string]storeReads{"reader": r, "resident": ResidentLeg(r)} {
				if got := read(s); !reflect.DeepEqual(got, want) {
					t.Errorf("%s %s %s = %+v, the store's = %+v", leg, via, name, got, want)
				}
			}
		}
		want, wantErr := raw.ListOpen()
		got, gotErr := ResidentLeg(r).ListOpen()
		if !reflect.DeepEqual(got, want) || errText(gotErr) != errText(wantErr) {
			t.Errorf("%s resident ListOpen = %+v, %v; the store's = %+v, %v", leg, got, gotErr, want, wantErr)
		}
	}
}

// storeReads is the read side a FederatedReader shares with a Store.
type storeReads interface {
	Get(id string) (beads.Bead, error)
	List(query beads.ListQuery) ([]beads.Bead, error)
	Ready(query ...beads.ReadyQuery) ([]beads.Bead, error)
	Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error)
	ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error)
	ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error)
	ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error)
	DepList(id, direction string) ([]beads.Dep, error)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
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
