package storebinding

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
)

func TestBeadsAdaptersExposeClosedFrontDoorsOverOneCanonicalStore(t *testing.T) {
	store := beads.NewMemStore()
	adapters, err := NewBeadsAdapters(store, BeadsAdapterIdentity{
		OpenerID:    "canonical-beads",
		ComponentID: "work-component",
		PhysicalID:  "work-physical",
	})
	if err != nil {
		t.Fatal(err)
	}

	if adapters.Work != store {
		t.Fatal("Work did not retain the canonical store")
	}
	if adapters.Identity.OpenerID != "canonical-beads" || adapters.Identity.ComponentID != "work-component" || adapters.Identity.PhysicalID != "work-physical" {
		t.Fatalf("identity = %#v", adapters.Identity)
	}

	graph, err := adapters.Graph.Create(beads.Bead{ID: "graph-root", Title: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if err := adapters.Graph.Tx("graph update", func(tx GraphTx) error {
		title := "updated"
		return tx.Update(graph.ID, beads.UpdateOpts{Title: &title})
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := adapters.Graph.Get(graph.ID); err != nil || got.Title != "updated" {
		t.Fatalf("graph Get = %#v, %v", got, err)
	}

	created, err := adapters.Sessions.CreateSessionInfo(session.CreateSpec{ID: "session-1", Title: "one", AgentName: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := adapters.Sessions.Get(created.ID); err != nil || got.ID != created.ID {
		t.Fatalf("session Get = %#v, %v", got, err)
	}
	if err := adapters.Sessions.SetLocalString(created.ID, "adapter", "value"); err != nil {
		t.Fatal(err)
	}
	if got, err := adapters.Sessions.GetLocalString(created.ID, "adapter"); err != nil || got != "value" {
		t.Fatalf("GetLocalString() = %q, %v", got, err)
	}

	if _, err := adapters.Messaging.Mail.Send(created.ID, created.ID, "subject", "body"); err != nil {
		t.Fatal(err)
	}
	run, err := adapters.Orders.CreateRun("scoped-order", orders.RunOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := adapters.Orders.CloseRunsSwept(context.Background(), []string{run.ID}, "swept", "test"); err != nil {
		t.Fatal(err)
	}

	item := nudgequeue.Item{ID: "nudge-1", Agent: "agent", Source: "test", CreatedAt: time.Now(), DeliverAfter: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if _, _, err := adapters.Nudges.Shadows.Save(item); err != nil {
		t.Fatal(err)
	}
	if shadow, found, err := adapters.Nudges.Shadows.Find(item.ID); err != nil || !found || shadow.ID != item.ID {
		t.Fatalf("nudge shadow = %#v, %v, %v", shadow, found, err)
	}
	if err := adapters.Nudges.Shadows.Terminalize(item, "injected", "", "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if history, err := adapters.Nudges.Shadows.ShadowHistorySince(time.Now().Add(-time.Minute)); err != nil || len(history) != 1 || history[0].CreatedAt.IsZero() || history[0].TerminalAt.IsZero() {
		t.Fatalf("ShadowHistorySince() = %#v, %v", history, err)
	}
}

func TestBeadsAdaptersKeepNudgeQueueExplicit(t *testing.T) {
	queue := nudgeQueueFake{}
	adapters, err := NewBeadsAdapters(beads.NewMemStore(), testBeadsAdapterIdentity(), queue)
	if err != nil {
		t.Fatal(err)
	}
	if adapters.Nudges.Queue != queue {
		t.Fatal("Nudges.Queue did not retain the supplied queue front door")
	}
}

func TestBindBeadsMessagingUsesSeparateTypedSessionsDirectory(t *testing.T) {
	messagingStore := beads.NewMemStore()
	sessionsStore := beads.NewMemStore()
	sessions := newBeadsSessionsAdapter(beads.SessionStore{Store: sessionsStore})
	created, err := sessions.CreateSessionInfo(session.CreateSpec{
		ID:    "session-split",
		Title: "split session",
		Metadata: map[string]string{
			"alias":        "split-alias",
			"session_name": "split-canonical",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	binder, err := BindBeadsMessaging(messagingStore)
	if err != nil {
		t.Fatalf("BindBeadsMessaging: %v", err)
	}
	messaging, err := binder.BindSessions(sessions)
	if err != nil {
		t.Fatalf("BindSessions: %v", err)
	}
	message, err := messaging.Mail.Send("split-alias", created.ID, "subject", "body")
	if err != nil {
		t.Fatalf("Mail.Send: %v", err)
	}
	if _, err := messagingStore.Get(message.ID); err != nil {
		t.Fatalf("message not persisted in Messaging store: %v", err)
	}
	rows, err := sessionsStore.List(beads.ListQuery{Type: "message", IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list messages in Sessions store: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("messages unexpectedly persisted in Sessions store: %#v", rows)
	}
}

func TestBeadsSessionsAdapterDynamicMethodSetCannotRecoverRawStore(t *testing.T) {
	sessions := newBeadsSessionsAdapter(beads.SessionStore{Store: beads.NewMemStore()})
	if _, ok := any(sessions).(interface{ Store() beads.SessionStore }); ok {
		t.Fatal("dynamic Sessions adapter exposes raw beads.SessionStore through Store()")
	}

	dynamic := reflect.TypeOf(sessions)
	contract := reflect.TypeOf((*SessionsStore)(nil)).Elem()
	if dynamic.NumMethod() != contract.NumMethod() {
		t.Fatalf("dynamic Sessions methods = %d, contract methods = %d", dynamic.NumMethod(), contract.NumMethod())
	}
	for index := 0; index < dynamic.NumMethod(); index++ {
		method := dynamic.Method(index)
		if _, ok := contract.MethodByName(method.Name); !ok {
			t.Fatalf("dynamic Sessions adapter exposes non-contract method %s", method.Name)
		}
	}
	value := reflect.ValueOf(sessions).Elem()
	for index := 0; index < value.NumField(); index++ {
		if value.Type().Field(index).PkgPath == "" {
			t.Fatalf("dynamic Sessions adapter exposes exported field %s", value.Type().Field(index).Name)
		}
	}
}

func TestBeadsGraphAdapterResolvesCachingGraphApplyHandle(t *testing.T) {
	backing := &graphApplyRecordingStore{Store: beads.NewMemStore()}
	cache := beads.NewCachingStoreForTest(backing, nil)
	adapters, err := NewBeadsAdapters(cache, testBeadsAdapterIdentity())
	if err != nil {
		t.Fatal(err)
	}
	plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{{Key: "root", Title: "Root"}}}
	result, err := adapters.Graph.ApplyGraphPlanWithStorage(t.Context(), plan, beads.StorageDefault)
	if err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage(default): %v", err)
	}
	if backing.applyWithStorageCalls != 0 || backing.applyCalls != 1 {
		t.Fatalf("apply calls = ordinary:%d storage:%d, want ordinary only", backing.applyCalls, backing.applyWithStorageCalls)
	}
	if _, err := cache.Get(result.IDs["root"]); err != nil {
		t.Fatalf("cached Get(graph result): %v", err)
	}
}

type graphApplyRecordingStore struct {
	beads.Store
	applyCalls            int
	applyWithStorageCalls int
}

func (s *graphApplyRecordingStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	s.applyCalls++
	return s.apply(plan)
}

func (s *graphApplyRecordingStore) ApplyGraphPlanWithStorage(_ context.Context, plan *beads.GraphApplyPlan, _ beads.StorageClass) (*beads.GraphApplyResult, error) {
	s.applyWithStorageCalls++
	return s.apply(plan)
}

func (s *graphApplyRecordingStore) apply(plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	ids := make(map[string]string, len(plan.Nodes))
	for _, node := range plan.Nodes {
		created, err := s.Create(beads.Bead{Title: node.Title})
		if err != nil {
			return nil, err
		}
		ids[node.Key] = created.ID
	}
	return &beads.GraphApplyResult{IDs: ids}, nil
}

func TestBeadsOrdersAdapterFindsGraphDependencyDescendantsAcrossSplitStores(t *testing.T) {
	ordersStore := beads.NewMemStore()
	graphStore := beads.NewMemStore()
	graph := &beadsGraphAdapter{store: graphStore}
	adapter := newBeadsOrdersAdapter(beads.OrdersStore{Store: ordersStore}, graph)
	root, err := graphStore.Create(beads.Bead{ID: "root", Type: "molecule", Labels: []string{"order-run:daily"}})
	if err != nil {
		t.Fatal(err)
	}
	child, err := graphStore.Create(beads.Bead{ID: "child", ParentID: root.ID, Metadata: map[string]string{"gc.root_bead_id": root.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := graphStore.DepAdd(child.ID, root.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if got, err := adapter.HasOpenWork("daily"); err != nil || !got {
		t.Fatalf("HasOpenWork() = %v, %v", got, err)
	}
	if err := graphStore.Close(child.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := adapter.HasOpenWork("daily"); err != nil || got {
		t.Fatalf("HasOpenWork() after close = %v, %v", got, err)
	}
}

func TestBeadsOrdersAdapterFindsOpenRootChildOnUnifiedWork(t *testing.T) {
	store := beads.NewMemStore()
	adapter := newUnifiedBeadsOrdersAdapter(beads.OrdersStore{Store: store}, &beadsGraphAdapter{store: store})
	root, err := store.Create(beads.Bead{ID: "root", Type: "molecule", Labels: []string{"order-run:unified"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{ID: "child", ParentID: root.ID}); err != nil {
		t.Fatal(err)
	}
	if got, err := adapter.HasOpenWork("unified"); err != nil || !got {
		t.Fatalf("HasOpenWork() = %v, %v", got, err)
	}
}

func TestBeadsOrdersAdapterBindGraphUsesSelectedSplitGraph(t *testing.T) {
	ordersStore := beads.NewMemStore()
	initialGraph := beads.NewMemStore()
	selectedGraph := beads.NewMemStore()
	adapter := newBeadsOrdersAdapter(beads.OrdersStore{Store: ordersStore}, &beadsGraphAdapter{store: initialGraph})
	rebound := adapter.BindGraph(graphNoRaw{GraphStore: &beadsGraphAdapter{store: selectedGraph}})
	if rebound == nil {
		t.Fatal("BindGraph() = nil")
	}
	if _, err := initialGraph.Create(beads.Bead{ID: "misleading", Labels: []string{"order-run:selected", "order-tracking", "seq:99"}}); err != nil {
		t.Fatal(err)
	}
	root, err := selectedGraph.Create(beads.Bead{ID: "selected-root", Type: "molecule", Labels: []string{"order-run:selected", "seq:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := selectedGraph.Close(root.ID); err != nil {
		t.Fatal(err)
	}
	if got := rebound.Cursor("selected"); got != 1 {
		t.Fatalf("rebound Cursor() = %d, want selected graph cursor", got)
	}
	if got, err := rebound.HasOpenWork("selected"); err != nil || got {
		t.Fatalf("rebound HasOpenWork() = %v, %v", got, err)
	}
}

func TestBeadsOrdersAdapterConsumesPartialGraphRows(t *testing.T) {
	partial := partialGraph{items: []beads.Bead{{
		ID:        "partial",
		CreatedAt: time.Now(),
		Labels:    []string{"order-run:partial", "seq:7"},
	}}, err: errors.New("partial graph read")}
	adapter := newBeadsOrdersAdapter(beads.OrdersStore{Store: beads.NewMemStore()}, partial)
	if got := adapter.Cursor("partial"); got != 7 {
		t.Fatalf("Cursor() = %d, want partial graph cursor", got)
	}
	if got, err := adapter.LastRun("partial"); err != nil || got.IsZero() {
		t.Fatalf("LastRun() = %v, %v", got, err)
	}
	if got := adapter.BindGraph(nil); got != nil {
		t.Fatalf("BindGraph(nil) = %#v, want nil", got)
	}
}

func TestBeadsOrdersAdapterBindGraphRejectsTypedNilWithoutPanic(t *testing.T) {
	adapter := newBeadsOrdersAdapter(
		beads.OrdersStore{Store: beads.NewMemStore()},
		&beadsGraphAdapter{store: beads.NewMemStore()},
	)
	var typedNilGraph *beadsGraphAdapter
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("BindGraph(typed nil) panicked: %v", recovered)
		}
	}()
	if got := adapter.BindGraph(typedNilGraph); got != nil {
		t.Fatalf("BindGraph(typed nil) = %#v, want nil rejection", got)
	}
}

func TestNewBeadsAdaptersRejectsMissingStoreOrIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		store    beads.Store
		identity BeadsAdapterIdentity
	}{
		{name: "missing store", identity: testBeadsAdapterIdentity()},
		{name: "missing opener", store: beads.NewMemStore(), identity: BeadsAdapterIdentity{ComponentID: "component", PhysicalID: "physical"}},
		{name: "missing component", store: beads.NewMemStore(), identity: BeadsAdapterIdentity{OpenerID: "opener", PhysicalID: "physical"}},
		{name: "missing physical", store: beads.NewMemStore(), identity: BeadsAdapterIdentity{OpenerID: "opener", ComponentID: "component"}},
		{name: "multiple queues", store: beads.NewMemStore(), identity: testBeadsAdapterIdentity()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var queue []NudgeQueue
			if tc.name == "multiple queues" {
				queue = []NudgeQueue{nudgeQueueFake{}, nudgeQueueFake{}}
			}
			if _, err := NewBeadsAdapters(tc.store, tc.identity, queue...); err == nil {
				t.Fatal("NewBeadsAdapters() error = nil")
			}
		})
	}
}

func TestNewBeadsAdaptersRejectsTypedNilStoreAndQueue(t *testing.T) {
	var typedNilStore *typedNilBeadsStore
	if _, err := NewBeadsAdapters(typedNilStore, testBeadsAdapterIdentity()); err == nil {
		t.Fatal("NewBeadsAdapters accepted typed-nil beads.Store")
	}
	var typedNilQueue *typedNilNudgeQueue
	if _, err := NewBeadsAdapters(beads.NewMemStore(), testBeadsAdapterIdentity(), typedNilQueue); err == nil {
		t.Fatal("NewBeadsAdapters accepted typed-nil NudgeQueue")
	}
}

func TestBindBeadsMessagingRejectsTypedNilSessionsDirectory(t *testing.T) {
	var sessions *beadsSessionsAdapter
	binder, err := BindBeadsMessaging(beads.NewMemStore())
	if err != nil {
		t.Fatalf("BindBeadsMessaging: %v", err)
	}
	if _, err := binder.BindSessions(sessions); !errors.Is(err, ErrInvalidMessagingBinding) {
		t.Fatalf("BindSessions(typed nil) error = %v, want ErrInvalidMessagingBinding", err)
	}
}

type typedNilBeadsStore struct{ beads.Store }

func testBeadsAdapterIdentity() BeadsAdapterIdentity {
	return BeadsAdapterIdentity{OpenerID: "opener", ComponentID: "component", PhysicalID: "physical"}
}

// graphNoRaw proves OrdersGraphBinder needs only the closed GraphStore
// contract, never an adapter-specific raw-store accessor.
type graphNoRaw struct{ GraphStore }

type partialGraph struct {
	GraphStore
	items []beads.Bead
	err   error
}

func (g partialGraph) List(beads.ListQuery) ([]beads.Bead, error) { return g.items, g.err }
