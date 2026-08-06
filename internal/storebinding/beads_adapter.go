package storebinding

import (
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
)

// ErrBeadsAdapterCapability reports that a canonical store lacks a graph
// capability required by the graph class front door.
var ErrBeadsAdapterCapability = errors.New("beads adapter capability unavailable")

// BeadsAdapterIdentity is the stable physical identity of an already-opened
// canonical Beads binding. The resolver copies these values into Work
// workspaces, where WorkTopology uses them to deduplicate shared handles.
type BeadsAdapterIdentity struct {
	OpenerID    string
	ComponentID string
	PhysicalID  string
}

// BeadsAdapters is the class projection of one already-opened canonical Beads
// store. Work deliberately retains the full store; the remaining fields expose
// only their closed class contracts.
type BeadsAdapters struct {
	Identity  BeadsAdapterIdentity
	Work      beads.Store
	Graph     GraphStore
	Sessions  SessionsStore
	Messaging MessagingFrontDoors
	Orders    OrdersStore
	Nudges    NudgeFrontDoors
}

// NewBeadsAdapters projects store into the closed storage-class front doors.
// queue is optional because the legacy queue authority is independent of the
// canonical Beads ledger; when supplied it is retained verbatim while the
// nudge shadow projection remains Beads-backed.
func NewBeadsAdapters(store beads.Store, identity BeadsAdapterIdentity, queue ...NudgeQueue) (BeadsAdapters, error) {
	if nilInterface(store) {
		return BeadsAdapters{}, errors.New("adapting canonical beads store: store is required")
	}
	if identity.OpenerID == "" || identity.ComponentID == "" || identity.PhysicalID == "" {
		return BeadsAdapters{}, errors.New("adapting canonical beads store: complete physical identity is required")
	}
	if len(queue) > 1 {
		return BeadsAdapters{}, errors.New("adapting canonical beads store: at most one nudge queue is allowed")
	}
	graph := &beadsGraphAdapter{store: store}
	queueFront := NudgeQueue(nil)
	if len(queue) > 0 {
		queueFront = queue[0]
		if nilInterface(queueFront) {
			return BeadsAdapters{}, errors.New("adapting canonical beads store: nudge queue is required when supplied")
		}
	}
	sessions := newBeadsSessionsAdapter(beads.SessionStore{Store: store})
	messagingBinder, err := BindBeadsMessaging(store)
	if err != nil {
		return BeadsAdapters{}, err
	}
	messaging, err := messagingBinder.BindSessions(sessions)
	if err != nil {
		return BeadsAdapters{}, err
	}
	return BeadsAdapters{
		Identity:  identity,
		Work:      store,
		Graph:     graph,
		Sessions:  sessions,
		Messaging: messaging,
		Orders:    newUnifiedBeadsOrdersAdapter(beads.OrdersStore{Store: store}, graph),
		Nudges: NudgeFrontDoors{
			Queue:   queueFront,
			Shadows: nudgequeue.NewStore(beads.NudgesStore{Store: store}),
		},
	}, nil
}

// BindBeadsMessaging captures one already-open Beads-backed Messaging
// persistence edge without selecting Sessions or reopening either provider.
// The returned binder consumes the selected typed Sessions directory later.
func BindBeadsMessaging(messaging beads.Store) (MessagingFrontDoorBinder, error) {
	if nilInterface(messaging) {
		return nil, errors.New("binding beads messaging: messaging store is required")
	}
	return newManagedMessagingFrontDoorBinder(&beadsMessagingFrontDoorBinder{messaging: messaging})
}

type beadsMessagingFrontDoorBinder struct {
	messaging beads.Store
}

func (b *beadsMessagingFrontDoorBinder) BindSessions(sessions SessionsAddressDirectory) (MessagingFrontDoors, error) {
	if nilInterface(sessions) {
		return MessagingFrontDoors{}, errors.New("binding beads messaging: sessions directory is required")
	}
	services, err := extmsg.NewServicesWithSessionDirectory(b.messaging, sessions)
	if err != nil {
		return MessagingFrontDoors{}, fmt.Errorf("binding beads messaging services: %w", err)
	}
	return MessagingFrontDoors{
		Mail:             beadmail.NewWithSessionDirectory(b.messaging, sessions),
		Bindings:         services.Bindings,
		DeliveryContexts: services.Delivery,
		Groups:           services.Groups,
		Transcripts:      services.Transcript,
	}, nil
}

type beadsGraphAdapter struct{ store beads.Store }

type closedBeadsSessionsStore interface {
	SessionsStore
}

type beadsSessionsAdapter struct {
	closedBeadsSessionsStore
}

func newBeadsSessionsAdapter(store beads.SessionStore) *beadsSessionsAdapter {
	return &beadsSessionsAdapter{closedBeadsSessionsStore: session.NewStore(store)}
}

type beadsOrdersAdapter struct {
	*orders.Store
	ordersStore   beads.OrdersStore
	ordersLeg     *orders.Store
	graph         GraphStore
	graphDistinct bool
}

func newBeadsOrdersAdapter(store beads.OrdersStore, graph GraphStore) *beadsOrdersAdapter {
	return newBeadsOrdersAdapterWithGraph(store, graph, true)
}

func newUnifiedBeadsOrdersAdapter(store beads.OrdersStore, graph GraphStore) *beadsOrdersAdapter {
	return newBeadsOrdersAdapterWithGraph(store, graph, false)
}

func newBeadsOrdersAdapterWithGraph(store beads.OrdersStore, graph GraphStore, graphDistinct bool) *beadsOrdersAdapter {
	return &beadsOrdersAdapter{
		Store:         orders.NewStore(store),
		ordersStore:   store,
		ordersLeg:     orders.NewStore(store),
		graph:         graph,
		graphDistinct: graphDistinct,
	}
}

// BindGraph returns an immutable orders projection whose mixed-class reads use
// graph through the closed GraphStore contract.
func (a *beadsOrdersAdapter) BindGraph(graph GraphStore) OrdersStore {
	if nilInterface(graph) {
		return nil
	}
	return newBeadsOrdersAdapter(a.ordersStore, graph)
}

// HasOpenWork keeps the graph traversal inside the adapter edge. The typed
// OrdersStore contract exposes neither a raw store nor a callback to callers.
func (a *beadsOrdersAdapter) HasOpenWork(scoped string) (bool, error) {
	if a.graphDistinct {
		if open, err := a.ordersLeg.HasOpenWork(scoped, nil); err != nil || open {
			return open, err
		}
	}
	items, err := a.graph.List(beads.ListQuery{Label: orders.RunLabel(scoped), Sort: beads.SortCreatedDesc, TierMode: beads.TierBoth})
	if err != nil {
		return false, fmt.Errorf("listing graph order work beads: %w", err)
	}
	for _, item := range items {
		if item.Status == "closed" {
			continue
		}
		if orders.IsTrackingBead(item) {
			return true, nil
		}
		open, err := a.graphRootHasOpenWork(item)
		if err != nil {
			return false, err
		}
		if open {
			return true, nil
		}
	}
	return false, nil
}

func (a *beadsOrdersAdapter) LastRun(scoped string) (time.Time, error) {
	latest, err := a.ordersLeg.LastRun(scoped)
	if err != nil {
		return time.Time{}, err
	}
	if !a.graphDistinct {
		return latest, nil
	}
	items, err := a.graph.List(beads.ListQuery{
		Label:                    orders.RunLabel(scoped),
		Limit:                    1,
		IncludeClosed:            true,
		Sort:                     beads.SortCreatedDesc,
		TierMode:                 beads.TierBoth,
		AllowBackingCreatedLimit: true,
	})
	if err != nil {
		if len(items) == 0 {
			return time.Time{}, err
		}
		log.Printf("orders: last-run lookup partially failed for %s: %v", scoped, err)
	}
	for _, item := range items {
		if item.CreatedAt.After(latest) {
			latest = item.CreatedAt
		}
	}
	return latest, nil
}

func (a *beadsOrdersAdapter) Cursor(scoped string) orders.EventCursor {
	latest := a.ordersLeg.Cursor(scoped)
	if !a.graphDistinct {
		return latest
	}
	items, err := a.graph.List(beads.ListQuery{
		Label:         orders.RunLabel(scoped),
		Limit:         10,
		IncludeClosed: true,
		Sort:          beads.SortCreatedDesc,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		log.Printf("orders: cursor lookup failed for %s: %v", scoped, err)
	}
	if graphCursor := orders.MaxEventCursor(items); graphCursor > latest {
		return graphCursor
	}
	return latest
}

func (a *beadsOrdersAdapter) graphRootHasOpenWork(root beads.Bead) (bool, error) {
	if !beads.IsMoleculeType(root.Type) && root.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWorkflow && root.Metadata[beadmeta.KindMetadataKey] != beadmeta.KindWisp {
		return false, nil
	}
	if root.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWisp && !beads.IsMoleculeType(root.Type) {
		return true, nil
	}
	return a.graphHasOpenDescendants(root.ID)
}

func (a *beadsOrdersAdapter) graphHasOpenDescendants(rootID string) (bool, error) {
	members, err := a.graph.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		return false, fmt.Errorf("listing wisp members of %s: %w", rootID, err)
	}
	for _, member := range members {
		if member.ID != rootID && member.Status != "closed" && !isTransientOrderNotification(member) {
			return true, nil
		}
	}
	return a.graphHasOpenDescendantsByWalk(rootID)
}

func (a *beadsOrdersAdapter) graphHasOpenDescendantsByWalk(rootID string) (bool, error) {
	seen := map[string]struct{}{rootID: {}}
	queue := []string{rootID}
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]
		children, err := a.graph.Children(parentID, beads.IncludeClosed)
		if err != nil {
			return false, err
		}
		for _, child := range children {
			if child.ID == "" || child.ID == rootID {
				continue
			}
			if _, duplicate := seen[child.ID]; duplicate {
				continue
			}
			seen[child.ID] = struct{}{}
			if child.Status != "closed" && !isTransientOrderNotification(child) {
				return true, nil
			}
			queue = append(queue, child.ID)
		}
		parent, err := a.graph.Get(parentID)
		if errors.Is(err, beads.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("getting graph parent %s: %w", parentID, err)
		}
		if !mayHaveGraphDependents(parent) {
			continue
		}
		deps, err := a.graph.DepList(parentID, "up")
		if err != nil {
			return false, fmt.Errorf("listing graph dependents for %s: %w", parentID, err)
		}
		for _, dep := range deps {
			if !isOrderDescendantDependency(dep.Type) || dep.IssueID == "" {
				continue
			}
			child, err := a.graph.Get(dep.IssueID)
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			if err != nil {
				return false, fmt.Errorf("getting graph dependent %s: %w", dep.IssueID, err)
			}
			if child.ID != rootID && child.Metadata[beadmeta.RootBeadIDMetadataKey] != rootID {
				continue
			}
			if _, duplicate := seen[child.ID]; duplicate {
				continue
			}
			seen[child.ID] = struct{}{}
			if child.Status != "closed" && !isTransientOrderNotification(child) {
				return true, nil
			}
			queue = append(queue, child.ID)
		}
	}
	return false, nil
}

func isTransientOrderNotification(b beads.Bead) bool {
	if beadmail.IsMessageBead(b) {
		return true
	}
	return nudgequeue.IsShadowBead(b)
}

func mayHaveGraphDependents(b beads.Bead) bool {
	return beads.IsMoleculeType(b.Type) || b.Metadata[beadmeta.RootBeadIDMetadataKey] != "" ||
		b.Metadata[beadmeta.StepRefMetadataKey] != "" || b.Metadata[beadmeta.LogicalBeadIDMetadataKey] != ""
}

func isOrderDescendantDependency(kind string) bool {
	switch kind {
	case "parent-child", "tracks", "blocks":
		return true
	default:
		return false
	}
}

func (a *beadsGraphAdapter) Create(b beads.Bead) (beads.Bead, error) {
	return a.store.Create(b)
}

func (a *beadsGraphAdapter) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	if creator, ok := a.store.(beads.StorageCreateStore); ok {
		return creator.CreateWithStorage(b, storage)
	}
	if storage == beads.StorageDefault {
		return a.store.Create(b)
	}
	return beads.Bead{}, unsupportedBeadsCapability("create with storage")
}

func (a *beadsGraphAdapter) Get(id string) (beads.Bead, error) { return a.store.Get(id) }

func (a *beadsGraphAdapter) List(query beads.ListQuery) ([]beads.Bead, error) {
	return a.store.List(query)
}

func (a *beadsGraphAdapter) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return a.store.Ready(query...)
}

func (a *beadsGraphAdapter) ReadyContext(ctx context.Context, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	reader, ok := a.store.(beads.ContextReadyReader)
	if !ok {
		return nil, unsupportedBeadsCapability("context-ready query")
	}
	return reader.ReadyContext(ctx, query...)
}

func (a *beadsGraphAdapter) Children(id string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return a.store.Children(id, opts...)
}

func (a *beadsGraphAdapter) Update(id string, opts beads.UpdateOpts) error {
	return a.store.Update(id, opts)
}

func (a *beadsGraphAdapter) SetMetadata(id, key, value string) error {
	return a.store.SetMetadata(id, key, value)
}

func (a *beadsGraphAdapter) SetMetadataBatch(id string, kvs map[string]string) error {
	return a.store.SetMetadataBatch(id, kvs)
}

func (a *beadsGraphAdapter) UpdateIfMatch(id string, expected int64, opts beads.UpdateOpts) error {
	writer, ok := beads.ConditionalWriterFor(a.store)
	if !ok {
		return unsupportedBeadsCapability("conditional update")
	}
	return writer.UpdateIfMatch(id, expected, opts)
}

func (a *beadsGraphAdapter) CloseIfMatch(id string, expected int64) error {
	writer, ok := beads.ConditionalWriterFor(a.store)
	if !ok {
		return unsupportedBeadsCapability("conditional close")
	}
	return writer.CloseIfMatch(id, expected)
}

func (a *beadsGraphAdapter) DeleteIfMatch(id string, expected int64) error {
	writer, ok := beads.ConditionalWriterFor(a.store)
	if !ok {
		return unsupportedBeadsCapability("conditional delete")
	}
	return writer.DeleteIfMatch(id, expected)
}

func (a *beadsGraphAdapter) CompareAndSetMetadataKey(id, key, expected, value string) (bool, error) {
	writer, ok := beads.MetadataCASWriterFor(a.store)
	if !ok {
		return false, unsupportedBeadsCapability("metadata compare-and-set")
	}
	return writer.CompareAndSetMetadataKey(id, key, expected, value)
}

func (a *beadsGraphAdapter) Close(id string) error { return a.store.Close(id) }

func (a *beadsGraphAdapter) Reopen(id string) error { return a.store.Reopen(id) }

func (a *beadsGraphAdapter) CloseAll(ids []string, metadata map[string]string) (int, error) {
	return a.store.CloseAll(ids, metadata)
}

func (a *beadsGraphAdapter) Delete(id string) error { return a.store.Delete(id) }

// beadsAssignmentClaimer is the acquire half of the claim pair, discovered on
// the resolved store the same way ConditionalAssignmentReleaser is. It is
// declared here rather than in internal/beads because only the graph class
// front door needs it: the canonical Store surface deliberately has no claim
// method, and BdStore's subprocess claim takes a different shape (the assignee
// is implicit in the bd invocation).
type beadsAssignmentClaimer interface {
	Claim(id, assignee string) (beads.Bead, bool, error)
}

// Claim delegates to a store that implements the two-argument claim. A store
// without it reports the capability as unavailable rather than emulating the
// CAS with a read-then-write, which would lose the single-winner guarantee the
// contract promises.
func (a *beadsGraphAdapter) Claim(id, assignee string) (beads.Bead, bool, error) {
	claimer, ok := a.store.(beadsAssignmentClaimer)
	if !ok {
		return beads.Bead{}, false, unsupportedBeadsCapability("assignment claim")
	}
	return claimer.Claim(id, assignee)
}

func (a *beadsGraphAdapter) ReleaseIfCurrent(id, assignee string) (bool, error) {
	releaser, ok := a.store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, unsupportedBeadsCapability("conditional assignment release")
	}
	return releaser.ReleaseIfCurrent(id, assignee)
}

func (a *beadsGraphAdapter) DepAdd(id, dependsOnID, depType string) error {
	return a.store.DepAdd(id, dependsOnID, depType)
}

func (a *beadsGraphAdapter) DepRemove(id, dependsOnID string) error {
	return a.store.DepRemove(id, dependsOnID)
}

func (a *beadsGraphAdapter) DepList(id, direction string) ([]beads.Dep, error) {
	return a.store.DepList(id, direction)
}

// beadsGraphEdgeMetadataReader is the optional read side of graph-apply edge
// metadata, discovered on the resolved store like the other graph
// capabilities. Stores that never retained the payload report it as
// unavailable rather than as an edge that carried none.
type beadsGraphEdgeMetadataReader interface {
	DepMetadata(issueID, dependsOnID string) (string, bool, error)
}

// DepMetadata reads the opaque payload retained for one graph edge.
func (a *beadsGraphAdapter) DepMetadata(id, dependsOnID string) (string, bool, error) {
	reader, ok := a.store.(beadsGraphEdgeMetadataReader)
	if !ok {
		return "", false, unsupportedBeadsCapability("dependency metadata")
	}
	return reader.DepMetadata(id, dependsOnID)
}

func (a *beadsGraphAdapter) Tx(message string, fn func(GraphTx) error) error {
	if fn == nil {
		return a.store.Tx(message, nil)
	}
	return a.store.Tx(message, func(tx beads.Tx) error {
		return fn(beadsGraphTx{tx: tx})
	})
}

func (a *beadsGraphAdapter) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	applier, ok := beads.GraphApplyFor(a.store)
	if !ok {
		return nil, unsupportedBeadsCapability("graph apply")
	}
	return applier.ApplyGraphPlan(ctx, plan)
}

func (a *beadsGraphAdapter) ApplyGraphPlanWithStorage(ctx context.Context, plan *beads.GraphApplyPlan, storage beads.StorageClass) (*beads.GraphApplyResult, error) {
	applier, ok := beads.GraphApplyFor(a.store)
	if !ok {
		return nil, unsupportedBeadsCapability("graph apply with storage")
	}
	if storage == beads.StorageDefault {
		return applier.ApplyGraphPlan(ctx, plan)
	}
	storageApplier, ok := applier.(beads.StorageGraphApplyStore)
	if !ok {
		return nil, unsupportedBeadsCapability("graph apply with storage")
	}
	return storageApplier.ApplyGraphPlanWithStorage(ctx, plan, storage)
}

// nilInterface reports nil interfaces as well as typed-nil interface values.
// Store fronts are composition-time invariants, so accepting a typed nil would
// otherwise defer a deterministic startup error to an arbitrary consumer.
func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func (a *beadsGraphAdapter) Count(ctx context.Context, query beads.ListQuery, excludeTypes ...string) (int, error) {
	if counter, ok := a.store.(beads.Counter); ok {
		return counter.Count(ctx, query, excludeTypes...)
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	items, err := a.store.List(query)
	excluded := make(map[string]struct{}, len(excludeTypes))
	for _, typ := range excludeTypes {
		excluded[typ] = struct{}{}
	}
	count := 0
	for _, item := range items {
		if _, skip := excluded[item.Type]; !skip {
			count++
		}
	}
	return count, err
}

func (a *beadsGraphAdapter) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	waiter, ok := a.store.(beads.ParentProjectionWaiter)
	if !ok {
		return unsupportedBeadsCapability("parent projection wait")
	}
	return waiter.WaitForParentProjection(ctx, id, oldParentID, newParentID)
}

func (a *beadsGraphAdapter) Ping() error { return a.store.Ping() }

type beadsGraphTx struct{ tx beads.Tx }

func (tx beadsGraphTx) Create(b beads.Bead) (beads.Bead, error) { return tx.tx.Create(b) }
func (tx beadsGraphTx) Update(id string, opts beads.UpdateOpts) error {
	return tx.tx.Update(id, opts)
}

func (tx beadsGraphTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return tx.tx.SetMetadataBatch(id, kvs)
}
func (tx beadsGraphTx) Close(id string) error { return tx.tx.Close(id) }

func unsupportedBeadsCapability(capability string) error {
	return fmt.Errorf("%w: %s", ErrBeadsAdapterCapability, capability)
}

var (
	_ GraphStore        = (*beadsGraphAdapter)(nil)
	_ GraphTx           = beadsGraphTx{}
	_ SessionsStore     = (*beadsSessionsAdapter)(nil)
	_ OrdersStore       = (*beadsOrdersAdapter)(nil)
	_ OrdersGraphBinder = (*beadsOrdersAdapter)(nil)
	_ NudgeShadows      = (*nudgequeue.Store)(nil)
)
