package storebinding

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/session"
)

func TestWorkTopologyOrdersAndSelectsScopes(t *testing.T) {
	hq := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "hq-1"}}, nil)
	alpha := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "alpha-1"}}, nil)
	beta := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "beta-1"}}, nil)

	topology, err := NewWorkTopology(workspace(HQScope(), hq, "hq", false), []Workspace{
		workspace(RigScope("beta"), beta, "beta", true),
		workspace(RigScope("alpha"), alpha, "alpha", false),
	})
	if err != nil {
		t.Fatal(err)
	}

	if got := topology.RigsInConfigOrder(false); len(got) != 1 || got[0].Scope != RigScope("alpha") {
		t.Fatalf("RigsInConfigOrder(false) = %#v", got)
	}
	if got := topology.RigsInLexicalOrder(true); len(got) != 2 || got[0].Scope != RigScope("alpha") || got[1].Scope != RigScope("beta") {
		t.Fatalf("RigsInLexicalOrder(true) = %#v", got)
	}
	if got := topology.All(); len(got) != 3 || got[0].Scope != HQScope() || got[1].Scope != RigScope("alpha") || got[2].Scope != RigScope("beta") {
		t.Fatalf("All() = %#v", got)
	}
	if got, err := topology.ForScope(RigScope("alpha")); err != nil || got.Store != alpha {
		t.Fatalf("ForScope(alpha) = %#v, %v", got, err)
	}
	if got, err := topology.ScopeForID("beta-1"); err != nil || got != RigScope("beta") {
		t.Fatalf("ScopeForID(beta-1) = %v, %v", got, err)
	}
}

func TestWorkTopologyReportsTypedNotFoundAndDuplicateResidence(t *testing.T) {
	store := beads.NewMemStore()
	topology, err := NewWorkTopology(workspace(HQScope(), store, "shared", false), []Workspace{
		workspace(RigScope("one"), store, "shared", false),
		workspace(RigScope("two"), store, "shared", false),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := topology.ForScope(RigScope("missing")); !errors.Is(err, ErrWorkScopeNotFound) {
		t.Fatalf("ForScope(missing) error = %v", err)
	} else if typed := new(WorkScopeNotFoundError); !errors.As(err, &typed) || typed.Scope != RigScope("missing") {
		t.Fatalf("ForScope(missing) typed error = %#v", err)
	}
	if _, err := topology.ScopeForID("missing"); !errors.Is(err, ErrWorkResidenceNotFound) {
		t.Fatalf("ScopeForID(missing) error = %v", err)
	} else if typed := new(WorkResidenceNotFoundError); !errors.As(err, &typed) || typed.ID != "missing" {
		t.Fatalf("ScopeForID(missing) typed error = %#v", err)
	}
	created, err := store.Create(beads.Bead{ID: "same"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := topology.ScopeForID(created.ID); !errors.Is(err, ErrDuplicateWorkResidence) {
		t.Fatalf("ScopeForID(duplicate) error = %v", err)
	} else if typed := new(DuplicateWorkResidenceError); !errors.As(err, &typed) || len(typed.Candidates) != 3 {
		t.Fatalf("ScopeForID(duplicate) typed error = %#v", err)
	}
	if got := topology.PhysicalWorkspaces(); len(got) != 1 {
		t.Fatalf("PhysicalWorkspaces() = %#v, want one grouped workspace", got)
	} else if len(got[0].Scopes) != 3 {
		t.Fatalf("grouped scopes = %#v, want all three retained scopes", got[0].Scopes)
	}
}

func TestWorkTopologyPinsAndDefensivelyCopiesViews(t *testing.T) {
	store := beads.NewMemStore()
	opener := "bootstrap-opener"
	prefix := "bootstrap"
	topology, err := NewWorkTopology(Workspace{Scope: HQScope(), Store: store, Prefix: prefix, OpenerID: opener, ComponentID: "bootstrap-component", PhysicalID: "bootstrap-physical"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := topology.ForScope(HQScope())
	if err != nil || workspace.OpenerID != "bootstrap-opener" || workspace.Prefix != "bootstrap" {
		t.Fatalf("pinned workspace = %#v, %v", workspace, err)
	}
	groups := topology.PhysicalWorkspaces()
	groups[0].Scopes[0] = RigScope("mutated")
	if got := topology.PhysicalWorkspaces()[0].Scopes[0]; got != HQScope() {
		t.Fatalf("topology group mutated through returned slice: %v", got)
	}
	if got := topology.RigsInConfigOrder(true); len(got) != 0 {
		t.Fatalf("unexpected rigs: %#v", got)
	}
}

func TestWorkTopologyRejectsUnpinnedAndInvalidScopes(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := NewWorkTopology(Workspace{Scope: HQScope(), Store: store}, nil); err == nil {
		t.Fatal("NewWorkTopology accepted unpinned HQ workspace")
	}
	if _, err := NewWorkTopology(Workspace{Scope: HQScope(), Store: store, OpenerID: "opener", ComponentID: "component", PhysicalID: "physical"}, nil); err == nil {
		t.Fatal("NewWorkTopology accepted HQ workspace without a pinned prefix")
	}
	if _, err := NewWorkTopology(workspace(WorkScope{}, store, "hq", false), nil); err == nil {
		t.Fatal("NewWorkTopology accepted zero WorkScope as HQ")
	}
}

func TestWorkTopologyUsesPinnedPrefixWithoutLiveStoreLookup(t *testing.T) {
	store := &mutablePrefixStore{Store: beads.NewMemStore(), prefix: "mutable"}
	topology, err := NewWorkTopology(Workspace{
		Scope:       HQScope(),
		Store:       store,
		Prefix:      "pinned",
		OpenerID:    "opener",
		ComponentID: "component",
		PhysicalID:  "physical",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	store.prefix = "changed-after-open"

	scope, err := topology.ScopeForID("pinned-123")
	if err != nil || scope != HQScope() {
		t.Fatalf("ScopeForID(pinned-123) = %v, %v; want pinned HQ scope", scope, err)
	}
	if store.prefixCalls != 0 {
		t.Fatalf("ScopeForID() called mutable store IDPrefix %d times", store.prefixCalls)
	}
}

func TestWorkTopologyPhysicalGroupingDoesNotConcatenateIdentityParts(t *testing.T) {
	store := beads.NewMemStore()
	hq := workspace(HQScope(), store, "hq", false)
	hq.OpenerID, hq.ComponentID, hq.PhysicalID = "a\x00b", "c", "d"
	rig := workspace(RigScope("rig"), store, "rig", false)
	rig.OpenerID, rig.ComponentID, rig.PhysicalID = "a", "b\x00c", "d"
	topology, err := NewWorkTopology(hq, []Workspace{rig})
	if err != nil {
		t.Fatal(err)
	}
	if got := topology.PhysicalWorkspaces(); len(got) != 2 {
		t.Fatalf("PhysicalWorkspaces() grouped distinct structured identities: %#v", got)
	}
}

func TestStoreSetHasTypedImmutableFrontDoors(t *testing.T) {
	store := beads.NewMemStore()
	topology, err := NewWorkTopology(workspace(HQScope(), store, "hq", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	adapters, err := NewBeadsAdapters(store, testBeadsAdapterIdentity(), nudgeQueueFake{})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := newUnpublishedStoreSet(testStoreSetFronts(topology, adapters), 1, testCandidateDescriptors())
	if err != nil {
		t.Fatal(err)
	}
	set := candidate.set
	if set.Work().All()[0].Scope != HQScope() || set.Graph() == nil || set.Sessions() == nil || set.Orders() == nil {
		t.Fatalf("StoreSet getters did not preserve values")
	}
	if got := reflect.TypeOf(StoreSet{}).NumField(); got != 6 {
		t.Fatalf("StoreSet fields = %d, want exactly 6", got)
	}
}

// testStoreSetFronts is the complete front set the adapters serve.
func testStoreSetFronts(topology WorkTopology, adapters BeadsAdapters) storeSetFronts {
	return storeSetFronts{
		work:      topology,
		graph:     adapters.Graph,
		sessions:  adapters.Sessions,
		messaging: adapters.Messaging,
		orders:    adapters.Orders,
		nudges:    adapters.Nudges,
	}
}

// testCandidateDescriptors is a minimal identity set; assembly records it
// verbatim and only Publish checks it against the active manifest.
func testCandidateDescriptors() map[BindingName]BindingIdentity {
	return map[BindingName]BindingIdentity{"binding": BindingIdentity(canonicalDigest([]byte("binding")))}
}

func TestStoreSetAssemblyRejectsIncompleteAndTypedNilFronts(t *testing.T) {
	store := beads.NewMemStore()
	topology, err := NewWorkTopology(workspace(HQScope(), store, "hq", false), nil)
	if err != nil {
		t.Fatal(err)
	}
	adapters, err := NewBeadsAdapters(store, testBeadsAdapterIdentity(), nudgeQueueFake{})
	if err != nil {
		t.Fatal(err)
	}
	var typedNilGraph *beadsGraphAdapter
	var typedNilSessions *beadsSessionsAdapter
	var typedNilQueue *typedNilNudgeQueue
	typedNilNudges := adapters.Nudges
	typedNilNudges.Queue = typedNilQueue
	corruptTopology := topology
	corruptTopology.byScope = nil

	cases := []struct {
		name   string
		mutate func(storeSetFronts) storeSetFronts
	}{
		{"typed-nil GraphStore", func(f storeSetFronts) storeSetFronts { f.graph = typedNilGraph; return f }},
		{"typed-nil SessionsStore", func(f storeSetFronts) storeSetFronts { f.sessions = typedNilSessions; return f }},
		{"typed-nil NudgeQueue", func(f storeSetFronts) storeSetFronts { f.nudges = typedNilNudges; return f }},
		{"zero WorkTopology without required HQ", func(f storeSetFronts) storeSetFronts { f.work = WorkTopology{}; return f }},
		{"WorkTopology with missing scope index", func(f storeSetFronts) storeSetFronts { f.work = corruptTopology; return f }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fronts := testCase.mutate(testStoreSetFronts(topology, adapters))
			if _, err := newUnpublishedStoreSet(fronts, 1, testCandidateDescriptors()); err == nil {
				t.Fatalf("assembly accepted %s", testCase.name)
			}
		})
	}
}

type typedNilNudgeQueue struct{ nudgeQueueFake }

func TestFrontDoorContractCensusRejectsInfrastructureEscapes(t *testing.T) {
	contracts := []reflect.Type{
		reflect.TypeOf((*GraphStore)(nil)).Elem(),
		reflect.TypeOf((*SessionsAddressDirectory)(nil)).Elem(),
		reflect.TypeOf((*SessionsStore)(nil)).Elem(),
		reflect.TypeOf((*session.AddressDirectory)(nil)).Elem(),
		reflect.TypeOf((*MessagingFrontDoorBinder)(nil)).Elem(),
		reflect.TypeOf((*OrdersStore)(nil)).Elem(),
		reflect.TypeOf((*NudgeQueue)(nil)).Elem(),
		reflect.TypeOf((*NudgeShadows)(nil)).Elem(),
	}
	for _, contract := range contracts {
		if contract.NumMethod() == 0 {
			t.Fatalf("%s has no enumerated contract methods", contract)
		}
		for index := 0; index < contract.NumMethod(); index++ {
			method := contract.Method(index)
			forbidden := []string{"beads.Store", "beads.SessionStore", "beads.NudgesStore", "sql.", "provider", "descriptor", "registry", "io.Writer"}
			for _, word := range forbidden {
				if strings.Contains(method.Type.String(), word) {
					t.Fatalf("%s.%s leaks forbidden infrastructure %q through %s", contract, method.Name, word, method.Type)
				}
			}
			if method.Name == "Close" && method.Type.NumIn() == 0 {
				t.Fatalf("%s exposes lifecycle Close()", contract)
			}
			for _, lifecycle := range []string{"Export", "Import", "Migrate", "Inspect"} {
				if strings.HasPrefix(method.Name, lifecycle) {
					t.Fatalf("%s.%s leaks migration lifecycle", contract, method.Name)
				}
			}
			if method.Name == "Open" {
				t.Fatalf("%s.Open leaks provider lifecycle", contract)
			}
		}
	}
	for _, frontDoor := range []reflect.Type{reflect.TypeOf(MessagingFrontDoors{}), reflect.TypeOf(NudgeFrontDoors{})} {
		for index := 0; index < frontDoor.NumField(); index++ {
			field := frontDoor.Field(index)
			for _, word := range []string{"beads.Store", "sql.", "provider", "descriptor", "registry", "io.Writer"} {
				if strings.Contains(field.Type.String(), word) {
					t.Fatalf("%s.%s leaks %q", frontDoor, field.Name, word)
				}
			}
		}
	}
	storeSet := reflect.TypeOf(StoreSet{})
	wantGetters := map[string]bool{"Work": true, "Graph": true, "Sessions": true, "Messaging": true, "Orders": true, "Nudges": true}
	if storeSet.NumMethod() != len(wantGetters) {
		t.Fatalf("StoreSet has %d public methods, want exactly six getters", storeSet.NumMethod())
	}
	for index := 0; index < storeSet.NumMethod(); index++ {
		if !wantGetters[storeSet.Method(index).Name] {
			t.Fatalf("StoreSet exposes non-front-door method %s", storeSet.Method(index).Name)
		}
	}
	for index := 0; index < storeSet.NumField(); index++ {
		if storeSet.Field(index).PkgPath == "" {
			t.Fatalf("StoreSet field %q is exported", storeSet.Field(index).Name)
		}
	}
}

func workspace(scope WorkScope, store beads.Store, physical string, suspended bool) Workspace {
	return Workspace{Scope: scope, Store: store, Prefix: physical, OpenerID: "pinned-opener", ComponentID: physical + "-component", PhysicalID: physical, Suspended: suspended}
}

type mutablePrefixStore struct {
	beads.Store
	prefix      string
	prefixCalls int
}

func (s *mutablePrefixStore) IDPrefix() string {
	s.prefixCalls++
	return s.prefix
}

type nudgeQueueFake struct{}

func (nudgeQueueFake) Enqueue(nudgequeue.Item) error                              { return nil }
func (nudgeQueueFake) EnqueueDeferred(nudgequeue.Item) error                      { return nil }
func (nudgeQueueFake) ClaimDue(ClaimTarget, time.Time) ([]nudgequeue.Item, error) { return nil, nil }
func (nudgeQueueFake) ListForAgent(string, time.Time) ([]nudgequeue.Item, []nudgequeue.Item, []nudgequeue.Item, error) {
	return nil, nil, nil, nil
}

func (nudgeQueueFake) ListFor(ClaimTarget, time.Time) ([]nudgequeue.Item, []nudgequeue.Item, []nudgequeue.Item, error) {
	return nil, nil, nil, nil
}
func (nudgeQueueFake) Snapshot() (nudgequeue.State, error)        { return nudgequeue.State{}, nil }
func (nudgeQueueFake) Ack([]string, string, string, string) error { return nil }
func (nudgeQueueFake) ReleaseClaims([]string) error               { return nil }
func (nudgeQueueFake) RecordFailure([]string, error, time.Time) ([]nudgequeue.Item, error) {
	return nil, nil
}
func (nudgeQueueFake) Rollback(nudgequeue.Item, string) error  { return nil }
func (nudgeQueueFake) WithdrawQueuedWaitNudges([]string) error { return nil }

var _ NudgeQueue = nudgeQueueFake{}
