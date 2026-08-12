package sqlite

// The Graph class front door over the SQLite graph engine. It adopts the deployed
// .gc/store/graph/beads.sqlite in place — same schema, same gcg IDs, same
// graph.seqfloor — and projects it into the closed storebinding.GraphStore
// contract. It ports nothing: every behavior here is either a delegation to
// internal/beads' SQLiteStore or a method the contract needs and that engine
// does not have.
//
// Two rules shape the file.
//
// No generic store escapes. The front door delegates method by method instead
// of embedding, so *beads.SQLiteStore is reachable only through the private
// field. That matters more here than for the other classes: the graph engine
// IS a beads.Store, so an embedded field would hand every caller the whole
// generic surface — Create on any class's rows, CloseStore, IDPrefix — through
// a value that exposes exactly the graph class.
//
// No capability degrades at call time. The engine is checked against
// graphEngine once, when the component opens, and a store that does not
// satisfy it is refused. The three contract methods the engine genuinely lacks
// — ReadyContext, Count, WaitForParentProjection — are implemented here over
// the engine rather than answered with a runtime "capability unavailable"
// error. A front door that returns a capability error at the moment a caller
// needs readiness is a silent degradation: the caller sees an error that looks
// like a store fault, and nothing in the composition ever said the class was
// incomplete.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// graphIDPrefix is the reserved namespace only the Graph class mints. It is
// spelled here as well as in the engine because the front door is what pins it
// for this provider: opening the deployed database under any other prefix
// would mint IDs a the legacy combined layout binary could not route back.
const graphIDPrefix = "gcg"

var (
	// ErrInvalidGraphComponent reports a Graph component that is not this
	// provider's exact deployed target, or an engine that does not satisfy the
	// deployed Graph surface the closed contract is built from.
	ErrInvalidGraphComponent = errors.New("invalid SQLite Graph component")

	// ErrGraphParentProjectionDiverged reports a parent-child listing that does
	// not reflect a reparent the caller believed had committed. The deployed
	// SQLite engine has no asynchronous projection to wait for, so divergence
	// is a real inconsistency rather than a timing artifact.
	ErrGraphParentProjectionDiverged = errors.New("SQLite Graph parent projection diverged")
)

// graphEngine is the exact deployed Graph engine surface this front door is
// built from. It is unexported and never returned: naming it pins the
// dependency at compile time (the assertion below fails the build if the graph engine drops
// or renames a method) and lets OpenGraph refuse an engine that cannot serve
// the whole class, instead of discovering the gap at the first call.
type graphEngine interface {
	Create(beads.Bead) (beads.Bead, error)
	CreateWithStorage(beads.Bead, beads.StorageClass) (beads.Bead, error)
	Get(string) (beads.Bead, error)
	List(beads.ListQuery) ([]beads.Bead, error)
	Ready(...beads.ReadyQuery) ([]beads.Bead, error)
	Children(string, ...beads.QueryOpt) ([]beads.Bead, error)
	Update(string, beads.UpdateOpts) error
	SetMetadata(string, string, string) error
	SetMetadataBatch(string, map[string]string) error
	UpdateIfMatch(string, int64, beads.UpdateOpts) error
	CloseIfMatch(string, int64) error
	DeleteIfMatch(string, int64) error
	CompareAndSetMetadataKey(string, string, string, string) (bool, error)
	Close(string) error
	Reopen(string) error
	CloseAll([]string, map[string]string) (int, error)
	Delete(string) error
	Claim(string, string) (beads.Bead, bool, error)
	ReleaseIfCurrent(string, string) (bool, error)
	DepAdd(string, string, string) error
	DepRemove(string, string) error
	DepList(string, string) ([]beads.Dep, error)
	DepMetadata(string, string) (string, bool, error)
	Tx(string, func(beads.Tx) error) error
	ApplyGraphPlan(context.Context, *beads.GraphApplyPlan) (*beads.GraphApplyResult, error)
	ApplyGraphPlanWithStorage(context.Context, *beads.GraphApplyPlan, beads.StorageClass) (*beads.GraphApplyResult, error)
	Ping() error
	SequenceFloor() (int64, error)
	SetSequenceFloor(int64) error
	StoreHealthPath() string
	CloseStore() error
}

// GraphComponent is one opened Graph component of a SQLite binding. It owns
// the physical handle and closes it; callers see only the typed contract.
type GraphComponent struct {
	engine graphEngine
	front  storebinding.GraphStore
}

// OpenGraph opens (adopting in place, creating only when absent) the deployed
// Graph database for one SQLite binding and returns its typed front door.
//
// The store is opened with the reserved gcg prefix and with generic terminal
// retention left at its default-disabled setting: the deployed sweeper is
// root-blind and would delete closed members of live workflow roots, so
// retention stays off until root-aware retention exists.
func OpenGraph(spec storebinding.BindingSpec) (*GraphComponent, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if spec.Provider != ProviderID {
		return nil, fmt.Errorf("%w: provider %q", ErrInvalidGraphComponent, spec.Provider)
	}
	path, err := GraphPath(BindingRoot(spec))
	if err != nil {
		return nil, err
	}
	opened, err := beads.OpenSQLiteStore(filepath.Dir(path), beads.WithSQLiteStoreIDPrefix(graphIDPrefix))
	if err != nil {
		return nil, fmt.Errorf("opening SQLite Graph component: %w", err)
	}
	return newGraphComponent(opened)
}

// newGraphComponent binds an already-opened engine, refusing one whose method
// set cannot serve the whole class. The refusal closes the handle it was
// handed: an engine that fails admission must not leak an open database.
func newGraphComponent(opened beads.Store) (*GraphComponent, error) {
	engine, ok := opened.(graphEngine)
	if !ok {
		closeErr := error(nil)
		if closer, isCloser := opened.(interface{ CloseStore() error }); isCloser {
			closeErr = closer.CloseStore()
		}
		return nil, errors.Join(fmt.Errorf("%w: store %T does not implement the deployed Graph engine surface", ErrInvalidGraphComponent, opened), closeErr)
	}
	return &GraphComponent{engine: engine, front: &graphFrontDoor{engine: engine}}, nil
}

// Graph returns the closed graph-class persistence contract.
func (c *GraphComponent) Graph() storebinding.GraphStore { return c.front }

// Path returns the deployed database file this component owns.
func (c *GraphComponent) Path() string { return c.engine.StoreHealthPath() }

// SequenceFloor returns the persisted Graph ID floor.
func (c *GraphComponent) SequenceFloor() (int64, error) { return c.engine.SequenceFloor() }

// ApplyGenesisSequenceFloor raises this component's allocator to floor and
// persists it beside the database in graph.seqfloor, so the next mint clears
// every gcg suffix the caller has evidence was already minted — across a
// restart, and across a rollback to a binary that knows only the sidecar.
//
// It is the front door's only floor writer. The read side (SequenceFloor) and
// the sidecar itself are both physical facts the migration manifest pins, so
// the write that produces them belongs on the component rather than in some
// caller reaching past it to the engine.
//
// The engine never lowers a floor, so applying a stale value is safe and the
// call is idempotent.
func (c *GraphComponent) ApplyGenesisSequenceFloor(floor int64) error {
	if floor < 0 {
		return fmt.Errorf("applying genesis Graph sequence floor: negative value %d", floor)
	}
	if err := c.engine.SetSequenceFloor(floor); err != nil {
		return fmt.Errorf("applying genesis Graph sequence floor: %w", err)
	}
	return nil
}

// Close releases the physical handle. Idempotent.
func (c *GraphComponent) Close() error { return c.engine.CloseStore() }

// graphFrontDoor is the closed contract the component hands out.
type graphFrontDoor struct{ engine graphEngine }

// Create persists a new graph bead, minting a gcg ID when none is pinned.
func (f *graphFrontDoor) Create(b beads.Bead) (beads.Bead, error) { return f.engine.Create(b) }

// CreateWithStorage persists a new graph bead on an already-selected tier.
func (f *graphFrontDoor) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	return f.engine.CreateWithStorage(b, storage)
}

// Get reads one graph bead by ID.
func (f *graphFrontDoor) Get(id string) (beads.Bead, error) { return f.engine.Get(id) }

// List runs one graph query.
func (f *graphFrontDoor) List(query beads.ListQuery) ([]beads.Bead, error) {
	return f.engine.List(query)
}

// Ready returns open, unblocked, actionable graph beads.
func (f *graphFrontDoor) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	return f.engine.Ready(query...)
}

// ReadyContext is the deadline-aware readiness read.
//
// The deployed engine's Ready is one indexed statement against a local read
// pool, and it takes no context; it is not interruptible mid-statement without
// changing the graph engine. So this honors the context at the call boundary: a context that
// is already done returns its error and never touches the store, and a context
// that expires while the statement runs discards the rows rather than handing
// a caller that has given up an answer it can no longer use.
//
// That is a narrower guarantee than a remote store's cancellable read, and it
// is deliberately a real answer rather than a capability veto — a veto here
// would make every deadline-sensitive caller of the graph class fail with what
// looks like a store fault.
func (f *graphFrontDoor) ReadyContext(ctx context.Context, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ready, err := f.engine.Ready(query...)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ready, nil
}

// Children returns the graph beads whose parent is id.
func (f *graphFrontDoor) Children(id string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	return f.engine.Children(id, opts...)
}

// Update applies a field mutation to one graph bead.
func (f *graphFrontDoor) Update(id string, opts beads.UpdateOpts) error {
	return f.engine.Update(id, opts)
}

// SetMetadata sets one metadata key.
func (f *graphFrontDoor) SetMetadata(id, key, value string) error {
	return f.engine.SetMetadata(id, key, value)
}

// SetMetadataBatch atomically sets several metadata keys.
func (f *graphFrontDoor) SetMetadataBatch(id string, kvs map[string]string) error {
	return f.engine.SetMetadataBatch(id, kvs)
}

// UpdateIfMatch applies opts only when the bead's revision is unchanged.
func (f *graphFrontDoor) UpdateIfMatch(id string, expected int64, opts beads.UpdateOpts) error {
	return f.engine.UpdateIfMatch(id, expected, opts)
}

// CloseIfMatch closes a bead only when its revision is unchanged.
func (f *graphFrontDoor) CloseIfMatch(id string, expected int64) error {
	return f.engine.CloseIfMatch(id, expected)
}

// DeleteIfMatch deletes a bead only when its revision is unchanged.
func (f *graphFrontDoor) DeleteIfMatch(id string, expected int64) error {
	return f.engine.DeleteIfMatch(id, expected)
}

// CompareAndSetMetadataKey sets one metadata key only when its current value
// matches expected.
func (f *graphFrontDoor) CompareAndSetMetadataKey(id, key, expected, value string) (bool, error) {
	return f.engine.CompareAndSetMetadataKey(id, key, expected, value)
}

// Close closes one graph bead.
func (f *graphFrontDoor) Close(id string) error { return f.engine.Close(id) }

// Reopen reopens one closed graph bead.
func (f *graphFrontDoor) Reopen(id string) error { return f.engine.Reopen(id) }

// CloseAll closes a batch of graph beads, stamping shared metadata.
func (f *graphFrontDoor) CloseAll(ids []string, metadata map[string]string) (int, error) {
	return f.engine.CloseAll(ids, metadata)
}

// Delete permanently removes one graph bead.
func (f *graphFrontDoor) Delete(id string) error { return f.engine.Delete(id) }

// Claim is the acquire half of the ownership pair. The deployed semantics are
// preserved exactly: same-owner reclaim is a no-op that consumes neither a
// revision nor a claim fence, a foreign holder or a terminal status is a
// conflict reported as ok=false rather than an error, and the engine's single
// write connection makes concurrent claims single-winner.
func (f *graphFrontDoor) Claim(id, assignee string) (beads.Bead, bool, error) {
	return f.engine.Claim(id, assignee)
}

// ReleaseIfCurrent releases an assignment only while assignee still holds it.
func (f *graphFrontDoor) ReleaseIfCurrent(id, assignee string) (bool, error) {
	return f.engine.ReleaseIfCurrent(id, assignee)
}

// DepAdd records one typed dependency edge.
func (f *graphFrontDoor) DepAdd(id, dependsOnID, depType string) error {
	return f.engine.DepAdd(id, dependsOnID, depType)
}

// DepRemove removes one dependency edge.
func (f *graphFrontDoor) DepRemove(id, dependsOnID string) error {
	return f.engine.DepRemove(id, dependsOnID)
}

// DepList lists dependency edges in one direction.
func (f *graphFrontDoor) DepList(id, direction string) ([]beads.Dep, error) {
	return f.engine.DepList(id, direction)
}

// DepMetadata reads the opaque payload one graph-apply edge carried.
func (f *graphFrontDoor) DepMetadata(id, dependsOnID string) (string, bool, error) {
	return f.engine.DepMetadata(id, dependsOnID)
}

// Tx runs fn inside one real atomic engine transaction, exposing only the
// closed GraphTx contract — the engine's beads.Tx never reaches the caller.
func (f *graphFrontDoor) Tx(message string, fn func(storebinding.GraphTx) error) error {
	if fn == nil {
		return f.engine.Tx(message, nil)
	}
	return f.engine.Tx(message, func(tx beads.Tx) error { return fn(graphFrontDoorTx{tx: tx}) })
}

// ApplyGraphPlan atomically materializes a whole plan.
func (f *graphFrontDoor) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	return f.engine.ApplyGraphPlan(ctx, plan)
}

// ApplyGraphPlanWithStorage atomically materializes a plan on a selected tier.
func (f *graphFrontDoor) ApplyGraphPlanWithStorage(ctx context.Context, plan *beads.GraphApplyPlan, storage beads.StorageClass) (*beads.GraphApplyResult, error) {
	return f.engine.ApplyGraphPlanWithStorage(ctx, plan, storage)
}

// Count reports how many beads match query once the excluded types are
// removed. The deployed engine has no count statement, so this counts the
// matched rows here rather than reporting the capability as missing: a caller
// that asked how many ready roots exist gets a number, not a store error.
func (f *graphFrontDoor) Count(ctx context.Context, query beads.ListQuery, excludeTypes ...string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	items, err := f.engine.List(query)
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	excluded := make(map[string]struct{}, len(excludeTypes))
	for _, excludedType := range excludeTypes {
		excluded[excludedType] = struct{}{}
	}
	count := 0
	for _, item := range items {
		if _, skip := excluded[item.Type]; !skip {
			count++
		}
	}
	return count, nil
}

// WaitForParentProjection verifies that the parent-child listing reflects a
// reparent of id from oldParentID to newParentID.
//
// The deployed engine has no lagging projection to wait for: Children is a
// direct query over the same committed parent_id column the reparent wrote, so
// either the move is already visible or it did not commit. Polling would
// therefore convert a real inconsistency into a timeout, so this reads once
// and reports divergence immediately. Returning nil unconditionally would be
// the silent version of the same thing — the caller would proceed on a
// projection nobody checked.
func (f *graphFrontDoor) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	bead, err := f.engine.Get(id)
	if err != nil {
		return fmt.Errorf("reading %s for parent projection: %w", id, err)
	}
	if bead.ParentID != newParentID {
		return fmt.Errorf("%w: %s has parent %q, want %q", ErrGraphParentProjectionDiverged, id, bead.ParentID, newParentID)
	}
	if newParentID != "" {
		listed, err := f.graphChildContains(newParentID, id)
		if err != nil {
			return err
		}
		if !listed {
			return fmt.Errorf("%w: %s is not listed under new parent %s", ErrGraphParentProjectionDiverged, id, newParentID)
		}
	}
	if oldParentID != "" && oldParentID != newParentID {
		listed, err := f.graphChildContains(oldParentID, id)
		if err != nil {
			return err
		}
		if listed {
			return fmt.Errorf("%w: %s is still listed under old parent %s", ErrGraphParentProjectionDiverged, id, oldParentID)
		}
	}
	return ctx.Err()
}

// graphChildContains reports whether id appears among parentID's children,
// counting closed children and both storage tiers: a reparent is a topology
// fact, and a closed or wisp-tier child is still reparented.
func (f *graphFrontDoor) graphChildContains(parentID, id string) (bool, error) {
	children, err := f.engine.Children(parentID, beads.IncludeClosed, beads.WithBothTiers)
	if err != nil {
		return false, fmt.Errorf("listing children of %s: %w", parentID, err)
	}
	for _, child := range children {
		if child.ID == id {
			return true, nil
		}
	}
	return false, nil
}

// Ping verifies that the graph component is reachable.
func (f *graphFrontDoor) Ping() error { return f.engine.Ping() }

// graphFrontDoorTx narrows the engine transaction to the closed GraphTx
// contract, by delegation rather than embedding for the same reason the front
// door itself delegates.
type graphFrontDoorTx struct{ tx beads.Tx }

// Create persists one bead inside the transaction.
func (t graphFrontDoorTx) Create(b beads.Bead) (beads.Bead, error) { return t.tx.Create(b) }

// Update applies one field mutation inside the transaction.
func (t graphFrontDoorTx) Update(id string, opts beads.UpdateOpts) error {
	return t.tx.Update(id, opts)
}

// SetMetadataBatch sets several metadata keys inside the transaction.
func (t graphFrontDoorTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return t.tx.SetMetadataBatch(id, kvs)
}

// Close closes one bead inside the transaction.
func (t graphFrontDoorTx) Close(id string) error { return t.tx.Close(id) }

var (
	_ storebinding.GraphStore = (*graphFrontDoor)(nil)
	_ storebinding.GraphTx    = graphFrontDoorTx{}
	_ graphEngine             = (*beads.SQLiteStore)(nil)
)
