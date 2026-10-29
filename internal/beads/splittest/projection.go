package splittest

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// Projection is the projection split over one physical test store: a work
// view and a class view of the same rows, each hiding the other side's.
//
// A physical split (NewSplitStores) moves every infrastructure bead into its
// own store, which means every fixture that seeds a session or a molecule into
// "the city store" has to move too. The projection split catches the same
// wrong-handle READS without moving any fixture: over the one physical test
// store it hands out two views.
//
//   - The WORK view hides every row coordclass.Classify puts in an
//     infrastructure class. Production's work accessors return it: the city
//     store, rig stores, the CLI's work store.
//   - The CLASS view hides every work row. Production's class accessors
//     return it: the sessions, graph, mail, nudges and orders stores.
//
// Fixtures keep seeding the raw store. Production code then sees what it would
// see on a split city, and every row a view hid from a caller that asked for it
// is a violation charged to the caller's production call site:
//
//   - a by-id read (Get, DepList, GetExactBatch) of a wrong-class row;
//   - a list read whose query SELECTED the hidden row: by id, parent, type,
//     label, metadata or assignee. An advisory predicate a store may ignore
//     (ListQuery.ParentIDs, ReadyQuery.MetadataEquals) selects only the rows
//     it matches. A broad scan (ListOpen, an unfiltered Ready, a status-only
//     List) also hides rows but is not flagged: it was not looking for them,
//     it merely filters what it finds. A list with a Limit is filtered BEFORE
//     the limit, as a split city's store would be;
//   - a write (Update, Close, SetMetadata, DepAdd, a conditional, CAS or
//     guarded-batch write, ...) that touches a wrong-class row, which then
//     fails with ErrNotFound as it would on a split city;
//   - a Create of a bead whose class belongs to the other side. The row lands,
//     but only the view that created it can see it afterwards, exactly as a
//     misrouted write on a split city is invisible to the class's readers.
//
// A READ inside beads.FederatedRead is a declared federation leg or residence
// probe: it is filtered but not flagged. A read inside beads.RelicRead is a
// leg the split plan has ruled against but still needs (pre-split rows in a
// scope store): filtered, not a failure, and counted per call site as debt.
// Neither marker covers a write.
//
// # Root and front
//
// A store reaches a view through whatever production wrapped around it: the
// cmd/gc bead policy store, the controller's CachingStore, the typed class
// wrappers. A projection is keyed by the ROOT under all of them (found by
// following views, Backing() and declared conditional-writes resolution
// targets), so the city cache and the policy store over the same MemStore
// share one projection, one set of misplaced-row pins and one mint hook. Each
// view delegates to the FRONT it was asked for, so the code under test keeps
// its policy, cache and capability paths. A view found nested inside a front
// (a CachingStore over a work view) is skipped: the outer view delegates to
// the inner view's front, so views never stack.
//
// # Refinements
//
// Infrastructure beads a MemStore (or FileStore) root mints get the class
// binding's "gcg-" prefix (MemStore.SetMintPrefixFunc), so by-id routers can
// route by reserved prefix the way they do on a born-split city; a strict kit
// leaf (StrictStore) keeps its own prefixes. Broad scans are not flagged (see
// above). Rig stores are work views like the city store: graph beads always
// live in the city binding, so a rig-store graph write is a violation of its
// own, and the sites that do it today sit in the baseline as known violations
// rather than on an allow-list.
type Projection struct {
	root beads.Store
	sink *violationSink

	mu sync.Mutex
	// misplaced pins the side of a row created through the wrong view: a
	// misrouted write is visible only where it was written.
	misplaced map[string]projectionSide
	// views memoizes one view per (front, side), so view identity is stable.
	views map[viewKey]beads.Store
	// defaultFront is the front WorkView and ClassView hand out views over:
	// the store the projection was first asked about.
	defaultFront beads.Store
}

type viewKey struct {
	front any
	side  projectionSide
}

type projectionSide int

const (
	sideWork projectionSide = iota + 1
	sideClass
)

func (s projectionSide) String() string {
	if s == sideWork {
		return "work"
	}
	return "class"
}

var (
	projectionsMu sync.Mutex
	// projections maps a root to its projection, held strongly for the life
	// of the test binary: a projection carries the misplaced-row pins, and a
	// projection collected mid-test (as a weakly held one could be) would
	// forget them and turn later reads into nondeterministic violations. The
	// stores tests open are small; a shard's worth is cheap to keep.
	projections = map[any]*Projection{}
)

// ProjectionOf returns the memoized projection over store's root, building it
// on first use. See Projection for root and front.
func ProjectionOf(store beads.Store) *Projection {
	return projectionOf(store, gateSink)
}

func projectionOf(store beads.Store, sink *violationSink) *Projection {
	if store == nil {
		return nil
	}
	root, inner := projectionRoot(store)
	if inner != nil {
		return inner
	}
	key, keyable := rootKey(root)
	projectionsMu.Lock()
	defer projectionsMu.Unlock()
	if keyable {
		if p, ok := projections[key]; ok {
			return p
		}
	}
	p := &Projection{
		root:         root,
		sink:         sink,
		misplaced:    map[string]projectionSide{},
		views:        map[viewKey]beads.Store{},
		defaultFront: frontOf(store),
	}
	if minter, ok := root.(mintPrefixSetter); ok {
		minter.SetMintPrefixFunc(classMintPrefix)
	}
	if keyable {
		projections[key] = p
	}
	return p
}

// rootKey returns root as a map key when its dynamic value is comparable.
func rootKey(root beads.Store) (any, bool) {
	if root == nil || !reflect.TypeOf(root).Comparable() {
		return nil, false
	}
	return root, true
}

// maxUnwrapDepth bounds the root walk so a self-referential wrapper cannot
// loop it.
const maxUnwrapDepth = 16

// projectionRoot follows store's wrappers to the physical root, returning the
// projection of a view met on the way instead when there is one.
func projectionRoot(store beads.Store) (beads.Store, *Projection) {
	current := store
	for range maxUnwrapDepth {
		if v, ok := current.(projectionView); ok {
			return nil, v.projection()
		}
		next, ok := unwrapOnce(current)
		if !ok {
			return current, nil
		}
		current = next
	}
	return current, nil
}

// unwrapOnce returns the store one wrapper layer down. A strict kit leaf is a
// boundary: it models a whole store of its own, so a projection over it keeps
// it as the root (and leaves its prefixes alone).
func unwrapOnce(store beads.Store) (beads.Store, bool) {
	switch s := store.(type) {
	case projectionView:
		return s.viewFront(), true
	case *StrictStore, *strictStorageStore:
		return nil, false
	}
	if backed, ok := store.(interface{ Backing() beads.Store }); ok {
		if next := backed.Backing(); next != nil && next != store {
			return next, true
		}
	}
	if targeter, ok := store.(beads.ConditionalWritesResolveTargeter); ok {
		if next := targeter.ConditionalWritesResolveTarget(); next != nil && next != store {
			return next, true
		}
	}
	return nil, false
}

// frontOf strips views from store: a view's own front, or, when a view sits
// nested inside store's wrappers, that view's front, so views never stack.
func frontOf(store beads.Store) beads.Store {
	current := store
	for range maxUnwrapDepth {
		if v, ok := current.(projectionView); ok {
			return v.viewFront()
		}
		next, ok := unwrapOnce(current)
		if !ok {
			return store
		}
		current = next
	}
	return store
}

// viewFor returns the memoized view of one side over front.
func (p *Projection) viewFor(front beads.Store, side projectionSide) beads.Store {
	front = frontOf(front)
	comparable := front != nil && reflect.TypeOf(front).Comparable()
	if comparable {
		p.mu.Lock()
		if v, ok := p.views[viewKey{front: front, side: side}]; ok {
			p.mu.Unlock()
			return v
		}
		p.mu.Unlock()
	}
	v := newProjectionView(p, front, side)
	if comparable {
		p.mu.Lock()
		defer p.mu.Unlock()
		if existing, ok := p.views[viewKey{front: front, side: side}]; ok {
			return existing
		}
		p.views[viewKey{front: front, side: side}] = v
	}
	return v
}

// mintPrefixSetter is the MemStore mint hook, also promoted through FileStore.
type mintPrefixSetter interface {
	SetMintPrefixFunc(func(beads.Bead) string)
}

// classMintPrefix is the gate's mint hook: infrastructure beads mint under the
// whole-split binding's prefix, work beads under the store's own.
func classMintPrefix(b beads.Bead) string {
	if coordclass.Classify(b).IsInfrastructure() {
		prefix, _ := config.ReservedClassPrefix(config.BeadClassGraph)
		return prefix
	}
	return ""
}

// WorkView returns the work view over the store the projection was first
// asked about.
func (p *Projection) WorkView() beads.Store { return p.viewFor(p.defaultFront, sideWork) }

// ClassView returns the class view over the store the projection was first
// asked about.
func (p *Projection) ClassView() beads.Store { return p.viewFor(p.defaultFront, sideClass) }

// WorkViewOf returns the work view over front, a store whose root is this
// projection's.
func (p *Projection) WorkViewOf(front beads.Store) beads.Store { return p.viewFor(front, sideWork) }

// ClassViewOf returns the class view over front.
func (p *Projection) ClassViewOf(front beads.Store) beads.Store { return p.viewFor(front, sideClass) }

// Root returns the physical store under every wrapper the views share.
func (p *Projection) Root() beads.Store { return p.root }

// sideOf reports which side of the split owns b.
func (p *Projection) sideOf(b beads.Bead) projectionSide {
	p.mu.Lock()
	pinned, ok := p.misplaced[b.ID]
	p.mu.Unlock()
	if ok {
		return pinned
	}
	return classSide(b)
}

func classSide(b beads.Bead) projectionSide {
	if coordclass.Classify(b).IsInfrastructure() {
		return sideClass
	}
	return sideWork
}

func (p *Projection) pin(id string, side projectionSide) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.misplaced[id] = side
}

// projectionView is implemented by every view variant.
type projectionView interface {
	projection() *Projection
	viewFront() beads.Store
}

// view is one side of a projection over one front. It embeds the front for
// the methods it does not need to police (Ping, local strings) and overrides
// every read and write that names or returns a row.
type view struct {
	beads.Store
	p    *Projection
	side projectionSide
}

// storageView is the StorageCreateStore-preserving variant, built only over a
// front that has CreateWithStorage, so the policy layer's storage fallback
// keeps firing for fronts without it.
type storageView struct {
	*view
	storage beads.StorageCreateStore
}

func newProjectionView(p *Projection, front beads.Store, side projectionSide) beads.Store {
	v := &view{Store: front, p: p, side: side}
	storage, hasStorage := front.(beads.StorageCreateStore)
	cache, isCache := front.(cacheProbes)
	switch {
	case hasStorage && isCache:
		return &storageCacheView{storageView: &storageView{view: v, storage: storage}, cache: cache}
	case hasStorage:
		return &storageView{view: v, storage: storage}
	case isCache:
		return &cacheView{view: v, cache: cache}
	}
	return v
}

var (
	_ beads.Store                                 = (*view)(nil)
	_ beads.StorageCreateStore                    = (*storageView)(nil)
	_ beads.ContextReadyReader                    = (*view)(nil)
	_ beads.Counter                               = (*view)(nil)
	_ beads.GraphApplyHandleProvider              = (*view)(nil)
	_ beads.ConditionalWriterHandleProvider       = (*view)(nil)
	_ beads.ConditionalWritesResolveTargeter      = (*view)(nil)
	_ beads.GuardedBatcherHandleProvider          = (*view)(nil)
	_ beads.MetadataCASWriterHandleProvider       = (*view)(nil)
	_ beads.AtomicConditionalCloserHandleProvider = (*view)(nil)
	_ beads.AtomicTxStore                         = (*view)(nil)
	_ beads.ParentProjectionWaiter                = (*view)(nil)
	_ beads.DepMetadataReader                     = (*view)(nil)
	_ beads.DepMetadataWriter                     = (*view)(nil)
	_ beads.BatchDeleter                          = (*view)(nil)
	_ beads.ConditionalAssignmentReleaser         = (*view)(nil)
	_ beads.ConditionalAssigneeTransferer         = (*view)(nil)
	_ beads.ExactBatchGetter                      = (*view)(nil)
	_ beads.ChildrenBatchLister                   = (*view)(nil)
	_ beads.DependencyBatchLister                 = (*view)(nil)
	_ cachedLister                                = (*view)(nil)
	_ cacheProbes                                 = (*cacheView)(nil)
	_ cacheProbes                                 = (*storageCacheView)(nil)
)

// cachedLister is internal/api's cache-first list (cache_read_model.go).
type cachedLister interface {
	CachedList(beads.ListQuery) ([]beads.Bead, bool)
}

func (v *view) projection() *Projection { return v.p }
func (v *view) viewFront() beads.Store  { return v.Store }

func (v *view) visible(b beads.Bead) bool { return v.p.sideOf(b) == v.side }

// flag charges one hidden row to the caller's production call site. A READ
// under beads.FederatedRead is not charged; a read under beads.RelicRead is
// counted as relic debt instead. A write is charged whatever marker it runs
// under: the markers vouch for a miss, never for a misplaced write.
func (v *view) flag(op string, b beads.Bead, read bool) {
	facts := captureStack()
	violation := SplitViolation{
		View:  v.side.String(),
		Op:    op,
		Class: coordclass.Classify(b).String(),
		ID:    b.ID,
		Site:  facts.site,
		Via:   facts.via,
		Test:  facts.test,
		Pkg:   facts.pkg,
	}
	if read {
		switch facts.marker {
		case beads.FederatedReadMarker:
			return
		case beads.RelicReadMarker:
			v.p.sink.recordRelic(violation)
			return
		}
	}
	v.p.sink.record(violation)
}

// filter drops the rows this view cannot see, flagging the hidden rows the
// query selected (selects; nil for a broad scan, which selects none). Rows
// that come with an error are filtered the same way (a partial result,
// beads.PartialResultError, is rows callers use), and the error passes
// through unchanged.
func (v *view) filter(op string, selects func(beads.Bead) bool, rows []beads.Bead, err error) ([]beads.Bead, error) {
	if len(rows) == 0 {
		return rows, err
	}
	out := make([]beads.Bead, 0, len(rows))
	for _, b := range rows {
		if v.visible(b) {
			out = append(out, b)
			continue
		}
		if selects != nil && selects(b) {
			v.flag(op, b, true)
		}
	}
	return out, err
}

// notHere is the error a split city's store returns for a row it does not hold.
func notHere(id string) error {
	return fmt.Errorf("bead %q is not in this store: %w", id, beads.ErrNotFound)
}

// guardID answers whether a by-id write may proceed: nil when the row is this
// view's (or does not exist, so the front reports its own error), ErrNotFound
// after flagging when it belongs to the other side.
func (v *view) guardID(op, id string) error {
	b, err := v.Store.Get(id)
	if err != nil {
		return nil
	}
	if v.visible(b) {
		return nil
	}
	v.flag(op, b, false)
	return notHere(id)
}

// Get hides a wrong-class row.
func (v *view) Get(id string) (beads.Bead, error) {
	b, err := v.Store.Get(id)
	if err != nil {
		return b, err
	}
	if !v.visible(b) {
		v.flag("get", b, true)
		return beads.Bead{}, notHere(id)
	}
	return b, nil
}

// List hides wrong-class rows, flagging them when the query selected them. A
// limit applies after the filter.
func (v *view) List(q beads.ListQuery) ([]beads.Bead, error) {
	limit := q.Limit
	q.Limit = 0
	rows, err := v.Store.List(q)
	rows, err = v.filter("list", listSelects(q), rows, err)
	return limitRows(rows, limit), err
}

// CachedList is internal/api's cache-first list, filtered like List; a front
// without one answers "not cached" so the caller lists.
func (v *view) CachedList(q beads.ListQuery) ([]beads.Bead, bool) {
	cached, ok := v.Store.(cachedLister)
	if !ok {
		return nil, false
	}
	limit := q.Limit
	q.Limit = 0
	rows, hit := cached.CachedList(q)
	if !hit {
		return nil, false
	}
	rows, _ = v.filter("list", listSelects(q), rows, nil)
	return limitRows(rows, limit), true
}

// ListOpen is a broad scan: it hides wrong-class rows without flagging.
func (v *view) ListOpen(status ...string) ([]beads.Bead, error) {
	rows, err := v.Store.ListOpen(status...)
	return v.filter("list-open", nil, rows, err)
}

// Ready hides wrong-class rows, flagging them when the query selected them. A
// limit applies after the filter.
func (v *view) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) {
	unlimited, limit := unlimitedReady(q)
	rows, err := v.Store.Ready(unlimited...)
	rows, err = v.filter("ready", readySelects(q), rows, err)
	return limitRows(rows, limit), err
}

// ReadyContext is Ready with the front's deadline-aware read when it has one.
func (v *view) ReadyContext(ctx context.Context, q ...beads.ReadyQuery) ([]beads.Bead, error) {
	unlimited, limit := unlimitedReady(q)
	var rows []beads.Bead
	var err error
	if reader, ok := v.Store.(beads.ContextReadyReader); ok {
		rows, err = reader.ReadyContext(ctx, unlimited...)
	} else {
		rows, err = v.Store.Ready(unlimited...)
	}
	rows, err = v.filter("ready", readySelects(q), rows, err)
	return limitRows(rows, limit), err
}

// unlimitedReady strips the limit from a ready query so the view can filter
// before limiting.
func unlimitedReady(q []beads.ReadyQuery) ([]beads.ReadyQuery, int) {
	if len(q) == 0 || q[0].Limit == 0 {
		return q, 0
	}
	out := append([]beads.ReadyQuery(nil), q...)
	limit := out[0].Limit
	out[0].Limit = 0
	return out, limit
}

// Children hides wrong-class children; asking for a parent's children selects
// them, so every hidden child is flagged.
func (v *view) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	rows, err := v.Store.Children(parentID, opts...)
	return v.filter("children", selectsAll, rows, err)
}

// ChildrenOfAny is Children over several parents, through the front's batched
// read when it has one.
func (v *view) ChildrenOfAny(parentIDs []string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	batch, ok := v.Store.(beads.ChildrenBatchLister)
	if !ok {
		return beads.ChildrenOfAnyPerParent(v, parentIDs, opts...)
	}
	rows, err := batch.ChildrenOfAny(parentIDs, opts...)
	return v.filter("children", selectsAll, rows, err)
}

// ListByLabel hides and flags wrong-class rows.
func (v *view) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	rows, err := v.Store.ListByLabel(label, 0, opts...)
	rows, err = v.filter("list-by-label", selectsAll, rows, err)
	return limitRows(rows, limit), err
}

// ListByAssignee hides and flags wrong-class rows.
func (v *view) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	rows, err := v.Store.ListByAssignee(assignee, status, 0)
	rows, err = v.filter("list-by-assignee", selectsAll, rows, err)
	return limitRows(rows, limit), err
}

// ListByMetadata hides and flags wrong-class rows.
func (v *view) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	rows, err := v.Store.ListByMetadata(filters, 0, opts...)
	rows, err = v.filter("list-by-metadata", selectsAll, rows, err)
	return limitRows(rows, limit), err
}

func limitRows(rows []beads.Bead, limit int) []beads.Bead {
	if limit > 0 && len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

// GetExactBatch reads several ids, through the front's batch when it has one;
// a wrong-class row is reported unresolved, as a split city's store would.
func (v *view) GetExactBatch(ids []string) (map[string]beads.Bead, []string, error) {
	getter, ok := v.Store.(beads.ExactBatchGetter)
	if !ok {
		return nil, nil, beads.ErrExactBatchGetUnsupported
	}
	found, unresolved, err := getter.GetExactBatch(ids)
	if err != nil {
		return found, unresolved, err
	}
	for id, b := range found {
		if !v.visible(b) {
			v.flag("get", b, true)
			delete(found, id)
			unresolved = append(unresolved, id)
		}
	}
	return found, unresolved, nil
}

// Count is unsupported on a view: a count cannot be filtered by class, so
// callers fall back to List, which can.
func (v *view) Count(context.Context, beads.ListQuery, ...string) (int, error) {
	return 0, fmt.Errorf("counting beads through a split-strict view: %w", beads.ErrCountUnsupported)
}

// guardCreateEdges refuses a create whose edges name a row of the other side:
// a parent (ParentID), a dependency (Needs, Dependencies) the store asked to
// hold this row could not resolve on a split city. A Needs entry that is not
// a row id (a formula step ref) is not a row and passes.
func (v *view) guardCreateEdges(b beads.Bead) error {
	if b.ParentID != "" && !v.attachmentParent(b, b.ParentID) {
		if err := v.guardID("create-parent", b.ParentID); err != nil {
			return err
		}
	}
	for _, need := range b.Needs {
		target := need
		if _, after, ok := strings.Cut(need, ":"); ok && after != "" {
			target = after
		}
		if err := v.guardID("create-dep", target); err != nil {
			return err
		}
	}
	for _, dep := range b.Dependencies {
		if dep.DependsOnID != "" {
			if err := v.guardID("create-dep", dep.DependsOnID); err != nil {
				return err
			}
		}
	}
	return nil
}

// Create lands the row, flagging a bead of the other side's class and pinning
// it to this view, the only one that will see it. A create whose parent or
// dependency is the other side's row is refused (guardCreateEdges).
func (v *view) Create(b beads.Bead) (beads.Bead, error) {
	if err := v.guardCreateEdges(b); err != nil {
		return beads.Bead{}, err
	}
	created, err := v.Store.Create(b)
	if err != nil {
		return created, err
	}
	v.afterCreate(b, created)
	return created, nil
}

// CreateWithStorage is Create through the front's storage-tier create.
func (v *storageView) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	if err := v.guardCreateEdges(b); err != nil {
		return beads.Bead{}, err
	}
	created, err := v.storage.CreateWithStorage(b, storage)
	if err != nil {
		return created, err
	}
	v.afterCreate(b, created)
	return created, nil
}

// afterCreate flags and pins a row created through the wrong view. The class
// is the REQUESTED bead's (or the returned row's): a store may hand back a row
// without the labels the caller asked for, and the request decides where the
// row belongs.
func (v *view) afterCreate(requested, created beads.Bead) {
	owner := sideWork
	if classSide(requested) == sideClass || classSide(created) == sideClass {
		owner = sideClass
	}
	if owner == v.side {
		return
	}
	v.flag("create", created, false)
	v.p.pin(created.ID, v.side)
}

// guardUpdate refuses an update of a wrong-class row, and one that re-parents
// a row under the other side's row.
func (v *view) guardUpdate(id string, opts beads.UpdateOpts) error {
	if err := v.guardID("update", id); err != nil {
		return err
	}
	if opts.ParentID != nil && *opts.ParentID != "" {
		row, err := v.Store.Get(id)
		if err == nil && v.attachmentParent(row, *opts.ParentID) {
			return nil
		}
		return v.guardID("update-parent", *opts.ParentID)
	}
	return nil
}

// attachmentParent reports the one cross-store parent edge the design
// sanctions: a graph row (a molecule or wisp root) created through the class
// view under a WORK row, which is how a workflow attaches to its source bead
// (sling --on, formula cook --on; sling.CollectAttachedBeads reads exactly
// those children). ParentID is a weak, city-scoped reference (beads.Store
// Children), and the binding stores it without resolving it. Every other
// cross-store parent (a work row under an infrastructure row, a session or
// mail row under a work row) is refused.
func (v *view) attachmentParent(child beads.Bead, parentID string) bool {
	if v.side != sideClass || coordclass.Classify(child) != coordclass.ClassGraph {
		return false
	}
	parent, err := v.Store.Get(parentID)
	return err == nil && v.p.sideOf(parent) == sideWork
}

// Update refuses a wrong-class row, or a parent of the other side.
func (v *view) Update(id string, opts beads.UpdateOpts) error {
	if err := v.guardUpdate(id, opts); err != nil {
		return err
	}
	return v.Store.Update(id, opts)
}

// Close refuses a wrong-class row.
func (v *view) Close(id string) error {
	if err := v.guardID("close", id); err != nil {
		return err
	}
	return v.Store.Close(id)
}

// Reopen refuses a wrong-class row.
func (v *view) Reopen(id string) error {
	if err := v.guardID("reopen", id); err != nil {
		return err
	}
	return v.Store.Reopen(id)
}

// CloseAll closes this view's rows and flags the rest, which a split city's
// store would not hold.
func (v *view) CloseAll(ids []string, metadata map[string]string) (int, error) {
	mine := make([]string, 0, len(ids))
	for _, id := range ids {
		if v.guardID("close-all", id) == nil {
			mine = append(mine, id)
		}
	}
	if len(mine) == 0 {
		return 0, nil
	}
	return v.Store.CloseAll(mine, metadata)
}

// SetMetadata refuses a wrong-class row.
func (v *view) SetMetadata(id, key, value string) error {
	if err := v.guardID("set-metadata", id); err != nil {
		return err
	}
	return v.Store.SetMetadata(id, key, value)
}

// SetMetadataBatch refuses a wrong-class row.
func (v *view) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := v.guardID("set-metadata", id); err != nil {
		return err
	}
	return v.Store.SetMetadataBatch(id, kvs)
}

// Delete refuses a wrong-class row.
func (v *view) Delete(id string) error {
	if err := v.guardID("delete", id); err != nil {
		return err
	}
	return v.Store.Delete(id)
}

// DeleteBatch deletes this view's rows, refusing the batch when any row is the
// other side's.
func (v *view) DeleteBatch(ids []string) error {
	for _, id := range ids {
		if err := v.guardID("delete", id); err != nil {
			return err
		}
	}
	deleter, ok := v.Store.(beads.BatchDeleter)
	if !ok {
		return fmt.Errorf("split-strict view over %T: %w", v.Store, beads.ErrBatchDeleteUnsupported)
	}
	return deleter.DeleteBatch(ids)
}

// guardEdge refuses an edge from or to a wrong-class row: no split city's
// store can hold a dependency on a row it does not have. A parent-child edge
// that restates the bead's own ParentID passes, as in StrictStore.
func (v *view) guardEdge(op, issueID, dependsOnID, depType string) error {
	// The issue is always checked; only the parent of an edge that mirrors
	// the issue's own ParentID (checked when that was written) is exempt.
	if err := v.guardID(op, issueID); err != nil {
		return err
	}
	if depType == "parent-child" {
		if b, err := v.Store.Get(issueID); err == nil && b.ParentID == dependsOnID {
			return nil
		}
	}
	return v.guardID(op, dependsOnID)
}

// DepAdd refuses an edge touching a wrong-class row.
func (v *view) DepAdd(issueID, dependsOnID, depType string) error {
	if err := v.guardEdge("dep-add", issueID, dependsOnID, depType); err != nil {
		return err
	}
	return v.Store.DepAdd(issueID, dependsOnID, depType)
}

// DepAddWithMetadata is DepAdd carrying an edge payload, through the front's
// payload write. A front without one refuses rather than dropping the payload.
func (v *view) DepAddWithMetadata(issueID, dependsOnID, depType, metadata string) error {
	if err := v.guardEdge("dep-add", issueID, dependsOnID, depType); err != nil {
		return err
	}
	writer, ok := v.Store.(beads.DepMetadataWriter)
	if !ok {
		return fmt.Errorf("writing dependency metadata %s -> %s: split-strict view over %T cannot carry an edge payload", issueID, dependsOnID, v.Store)
	}
	return writer.DepAddWithMetadata(issueID, dependsOnID, depType, metadata)
}

// DepRemove refuses an edge whose source or target is a wrong-class row: no
// split city's store holds it.
func (v *view) DepRemove(issueID, dependsOnID string) error {
	if err := v.guardID("dep-remove", issueID); err != nil {
		return err
	}
	if err := v.guardID("dep-remove", dependsOnID); err != nil {
		return err
	}
	return v.Store.DepRemove(issueID, dependsOnID)
}

// DepList reports no edges for a wrong-class row, which a split city's store
// would not hold.
func (v *view) DepList(id, direction string) ([]beads.Dep, error) {
	if b, err := v.Store.Get(id); err == nil && !v.visible(b) {
		v.flag("dep-list", b, true)
		return nil, nil
	}
	return v.Store.DepList(id, direction)
}

// DepListBatch is DepList per id: a wrong-class anchor gets no entry, as a
// missing anchor does.
func (v *view) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	out := make(map[string][]beads.Dep, len(ids))
	var mine []string
	for _, id := range ids {
		if b, err := v.Store.Get(id); err == nil && !v.visible(b) {
			v.flag("dep-list", b, true)
			continue
		}
		mine = append(mine, id)
	}
	if batch, ok := v.Store.(beads.DependencyBatchLister); ok {
		got, err := batch.DepListBatch(mine)
		if err != nil {
			return nil, err
		}
		for id, deps := range got {
			out[id] = deps
		}
		return out, nil
	}
	for _, id := range mine {
		deps, err := v.Store.DepList(id, "down")
		if err != nil {
			return nil, fmt.Errorf("listing deps for %q: %w", id, err)
		}
		out[id] = deps
	}
	return out, nil
}

// DepMetadata forwards the front's edge-payload read.
func (v *view) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	reader, ok := v.Store.(beads.DepMetadataReader)
	if !ok {
		return "", false, fmt.Errorf("reading dependency metadata %s -> %s: front %T exposes no edge-payload read", issueID, dependsOnID, v.Store)
	}
	return reader.DepMetadata(issueID, dependsOnID)
}

// Tx polices creates and by-id writes inside the callback like their
// store-level twins.
func (v *view) Tx(commitMsg string, fn func(beads.Tx) error) error {
	return v.Store.Tx(commitMsg, func(tx beads.Tx) error {
		return fn(&viewTx{tx: tx, v: v})
	})
}

type viewTx struct {
	tx beads.Tx
	v  *view
}

func (t *viewTx) Create(b beads.Bead) (beads.Bead, error) {
	if err := t.v.guardCreateEdges(b); err != nil {
		return beads.Bead{}, err
	}
	created, err := t.tx.Create(b)
	if err != nil {
		return created, err
	}
	t.v.afterCreate(b, created)
	return created, nil
}

func (t *viewTx) Update(id string, opts beads.UpdateOpts) error {
	if err := t.v.guardUpdate(id, opts); err != nil {
		return err
	}
	return t.tx.Update(id, opts)
}

func (t *viewTx) SetMetadataBatch(id string, kvs map[string]string) error {
	if err := t.v.guardID("set-metadata", id); err != nil {
		return err
	}
	return t.tx.SetMetadataBatch(id, kvs)
}

func (t *viewTx) Close(id string) error {
	if err := t.v.guardID("close", id); err != nil {
		return err
	}
	return t.tx.Close(id)
}

// ReleaseIfCurrent refuses a wrong-class row, then forwards.
func (v *view) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	if err := v.guardID("release", id); err != nil {
		return false, err
	}
	releaser, ok := v.Store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, beads.ErrConditionalReleaseUnsupported
	}
	return releaser.ReleaseIfCurrent(id, expectedAssignee)
}

// TransferIfCurrent refuses a wrong-class row, then forwards.
func (v *view) TransferIfCurrent(id, fromAssignee, toAssignee string) (bool, error) {
	if err := v.guardID("transfer", id); err != nil {
		return false, err
	}
	mover, ok := v.Store.(beads.ConditionalAssigneeTransferer)
	if !ok {
		return false, beads.ErrConditionalTransferUnsupported
	}
	return mover.TransferIfCurrent(id, fromAssignee, toAssignee)
}

// GraphApplyHandle forwards the front's graph apply, flagging a plan whose
// class belongs to the other side and pinning the rows it creates to this view.
//
// Known gap: a plan's external references (a node's ParentID, an edge's
// FromID/ToID) are NOT checked against the other side, unlike Create's. They
// can carry one today: molecule's graph apply parents the steps to
// opts.ParentID and edges them to opts.RootID and to step dependencies'
// DependsOnID. Checking them needs the designed cross-store exemptions (the
// attachment link, S3b's cross-store blockers) worked out first, and a
// regenerated baseline; until then a plan wiring a row to the wrong side's
// row passes the gate.
func (v *view) GraphApplyHandle() (beads.GraphApplyStore, bool) {
	applier, ok := beads.GraphApplyFor(v.Store)
	if !ok {
		return nil, false
	}
	return viewGraphApplier{v: v, applier: applier}, true
}

type viewGraphApplier struct {
	v       *view
	applier beads.GraphApplyStore
}

var _ beads.EphemeralGraphApplyStore = viewGraphApplier{}

// SupportsEphemeralGraphApply forwards the applier's ephemeral-plan answer.
func (a viewGraphApplier) SupportsEphemeralGraphApply() bool {
	if eph, ok := a.applier.(beads.EphemeralGraphApplyStore); ok {
		return eph.SupportsEphemeralGraphApply()
	}
	return false
}

func (a viewGraphApplier) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	result, err := a.applier.ApplyGraphPlan(ctx, plan)
	if err != nil || result == nil {
		return result, err
	}
	owner := sideWork
	if coordclass.ClassifyGraphPlan(plan).IsInfrastructure() {
		owner = sideClass
	}
	if owner == a.v.side {
		return result, nil
	}
	flagged := false
	for _, id := range result.IDs {
		b, getErr := a.v.Store.Get(id)
		if getErr != nil {
			continue
		}
		if !flagged {
			a.v.flag("graph-apply", b, false)
			flagged = true
		}
		a.v.p.pin(id, a.v.side)
	}
	return result, nil
}

// ConditionalWriterHandle returns the front's conditional writer behind the
// view's checks.
func (v *view) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	writer, ok := beads.ConditionalWriterFor(v.Store)
	if !ok {
		return nil, false
	}
	return checkedConditionalWriter{v: v, writer: writer}, true
}

// MetadataCASWriterHandle returns the front's metadata CAS behind the view's
// checks.
func (v *view) MetadataCASWriterHandle() (beads.MetadataCASWriter, bool) {
	writer, ok := beads.MetadataCASWriterFor(v.Store)
	if !ok {
		return nil, false
	}
	return checkedCAS{v: v, writer: writer}, true
}

// AtomicConditionalCloserHandle returns the front's atomic terminal write
// behind the view's checks.
func (v *view) AtomicConditionalCloserHandle() (beads.AtomicConditionalCloser, bool) {
	closer, ok := beads.AtomicConditionalCloserFor(v.Store)
	if !ok {
		return nil, false
	}
	return checkedCloser{v: v, closer: closer}, true
}

// GuardedBatcherHandle returns the front's guarded batches behind the view's
// checks.
func (v *view) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	batcher, ok := beads.GuardedBatcherFor(v.Store)
	if !ok {
		return nil, false
	}
	return checkedBatcher{v: v, batcher: batcher}, true
}

// ConditionalWritesResolveTarget is the view's own checked target, never the
// raw front: everything that follows a resolve target (the metadata CAS, the
// atomic closer, the guarded update) lands on the view's checks. The target
// sources its conditional-writes MODE from the front, so mode and capability
// resolve exactly as they would without the view.
func (v *view) ConditionalWritesResolveTarget() beads.Store {
	target := &condTarget{view: v}
	if updater, ok := beads.AssignmentGuardedUpdaterFor(v.Store); ok {
		return &condTargetGuarded{condTarget: target, updater: updater}
	}
	return target
}

// condTarget is a view as a conditional-writes resolution target.
type condTarget struct {
	*view
}

var (
	_ beads.ConditionalWriter            = (*condTarget)(nil)
	_ beads.ConditionalWritesModeSourcer = (*condTarget)(nil)
	_ beads.AssignmentGuardedUpdater     = (*condTargetGuarded)(nil)
)

// ConditionalWritesResolveTarget ends the resolve walk here.
func (t *condTarget) ConditionalWritesResolveTarget() beads.Store { return nil }

// ConditionalWritesModeSource sends mode and capability resolution to the
// front.
func (t *condTarget) ConditionalWritesModeSource() beads.Store { return t.Store }

func (t *condTarget) writer() (beads.ConditionalWriter, error) {
	writer, ok := beads.ConditionalWriterForTarget(t.Store)
	if !ok {
		return nil, beads.ErrConditionalWriteUnsupported
	}
	return writer, nil
}

// UpdateIfMatch refuses a wrong-class row, then forwards.
func (t *condTarget) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	return checkedConditionalWriter{v: t.view, writer: lazyWriter{t}}.UpdateIfMatch(id, expectedRevision, opts)
}

// CloseIfMatch refuses a wrong-class row, then forwards.
func (t *condTarget) CloseIfMatch(id string, expectedRevision int64) error {
	return checkedConditionalWriter{v: t.view, writer: lazyWriter{t}}.CloseIfMatch(id, expectedRevision)
}

// DeleteIfMatch refuses a wrong-class row, then forwards.
func (t *condTarget) DeleteIfMatch(id string, expectedRevision int64) error {
	return checkedConditionalWriter{v: t.view, writer: lazyWriter{t}}.DeleteIfMatch(id, expectedRevision)
}

// CompareAndSetMetadataKey refuses a wrong-class row, then forwards.
func (t *condTarget) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	return checkedConditionalWriter{v: t.view, writer: lazyWriter{t}}.CompareAndSetMetadataKey(id, key, expected, next)
}

// condTargetGuarded adds the front's assignment-guarded update.
type condTargetGuarded struct {
	*condTarget
	updater beads.AssignmentGuardedUpdater
}

// UpdateIfAssignment refuses a wrong-class row, then forwards.
func (t *condTargetGuarded) UpdateIfAssignment(id, expectedStatus, expectedAssignee string, opts beads.UpdateOpts) (bool, error) {
	if err := t.guardUpdate(id, opts); err != nil {
		return false, err
	}
	return t.updater.UpdateIfAssignment(id, expectedStatus, expectedAssignee, opts)
}

// lazyWriter resolves the front's writer at call time, answering
// ErrConditionalWriteUnsupported when there is none (as the resolve-target
// contract allows a writer to).
type lazyWriter struct{ t *condTarget }

func (l lazyWriter) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	w, err := l.t.writer()
	if err != nil {
		return err
	}
	return w.UpdateIfMatch(id, rev, opts)
}

func (l lazyWriter) CloseIfMatch(id string, rev int64) error {
	w, err := l.t.writer()
	if err != nil {
		return err
	}
	return w.CloseIfMatch(id, rev)
}

func (l lazyWriter) DeleteIfMatch(id string, rev int64) error {
	w, err := l.t.writer()
	if err != nil {
		return err
	}
	return w.DeleteIfMatch(id, rev)
}

func (l lazyWriter) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	w, err := l.t.writer()
	if err != nil {
		return false, err
	}
	return w.CompareAndSetMetadataKey(id, key, expected, next)
}

// checkedConditionalWriter refuses conditional writes to wrong-class rows.
type checkedConditionalWriter struct {
	v      *view
	writer beads.ConditionalWriter
}

func (c checkedConditionalWriter) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if err := c.v.guardUpdate(id, opts); err != nil {
		return err
	}
	return c.writer.UpdateIfMatch(id, rev, opts)
}

func (c checkedConditionalWriter) CloseIfMatch(id string, rev int64) error {
	if err := c.v.guardID("close", id); err != nil {
		return err
	}
	return c.writer.CloseIfMatch(id, rev)
}

func (c checkedConditionalWriter) DeleteIfMatch(id string, rev int64) error {
	if err := c.v.guardID("delete", id); err != nil {
		return err
	}
	return c.writer.DeleteIfMatch(id, rev)
}

func (c checkedConditionalWriter) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if err := c.v.guardID("set-metadata", id); err != nil {
		return false, err
	}
	return c.writer.CompareAndSetMetadataKey(id, key, expected, next)
}

// checkedCAS refuses metadata CAS writes to wrong-class rows.
type checkedCAS struct {
	v      *view
	writer beads.MetadataCASWriter
}

func (c checkedCAS) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if err := c.v.guardID("set-metadata", id); err != nil {
		return false, err
	}
	return c.writer.CompareAndSetMetadataKey(id, key, expected, next)
}

// checkedCloser refuses atomic terminal writes to wrong-class rows.
type checkedCloser struct {
	v      *view
	closer beads.AtomicConditionalCloser
}

func (c checkedCloser) CloseWithMetadataIfMatch(id string, rev int64, metadata map[string]string) (beads.Bead, error) {
	if err := c.v.guardID("close", id); err != nil {
		return beads.Bead{}, err
	}
	return c.closer.CloseWithMetadataIfMatch(id, rev, metadata)
}

// checkedBatcher refuses a guarded batch that updates a wrong-class row, and
// flags and pins the rows it creates of the other side's class.
type checkedBatcher struct {
	v       *view
	batcher beads.GuardedBatcher
}

func (c checkedBatcher) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	for _, item := range b.Items {
		if item.Update != nil {
			if err := c.v.guardUpdate(item.Update.ID, item.Update.Opts); err != nil {
				return beads.GuardedResult{}, err
			}
		}
		if item.Create != nil {
			if err := c.v.guardCreateEdges(*item.Create); err != nil {
				return beads.GuardedResult{}, err
			}
		}
	}
	result, err := c.batcher.ApplyGuarded(ctx, b)
	if err != nil {
		return result, err
	}
	for _, item := range b.Items {
		if item.Create != nil {
			c.v.afterCreate(*item.Create, *item.Create)
		}
	}
	return result, nil
}

// AtomicTx reports the front's transactional guarantee.
func (v *view) AtomicTx() bool { return beads.StoreSupportsAtomicTx(v.Store) }

// WaitForParentProjection forwards the front's projection wait.
func (v *view) WaitForParentProjection(ctx context.Context, id, oldParentID, newParentID string) error {
	if waiter, ok := v.Store.(beads.ParentProjectionWaiter); ok {
		return waiter.WaitForParentProjection(ctx, id, oldParentID, newParentID)
	}
	return nil
}

// cacheProbes are a CachingStore front's liveness and statistics. Production
// asserts them (the v2 planner waits for IsLive before its first pass), so a
// view over a cache must answer them, and a view over anything else must not
// claim them: a view that answered IsLive()=false for a MemStore would turn
// every "is the cache live?" check into "not yet".
type cacheProbes interface {
	IsLive() bool
	Stats() beads.CacheStats
}

// cacheView is the variant built over a front that is a cache.
type cacheView struct {
	*view
	cache cacheProbes
}

// storageCacheView is the variant over a cache front with CreateWithStorage.
type storageCacheView struct {
	*storageView
	cache cacheProbes
}

// IsLive forwards the front cache's liveness.
func (v *cacheView) IsLive() bool { return v.cache.IsLive() }

// Stats forwards the front cache's statistics.
func (v *cacheView) Stats() beads.CacheStats { return v.cache.Stats() }

// IsLive forwards the front cache's liveness.
func (v *storageCacheView) IsLive() bool { return v.cache.IsLive() }

// Stats forwards the front cache's statistics.
func (v *storageCacheView) Stats() beads.CacheStats { return v.cache.Stats() }

// StoreHealthPath forwards the front's health path, "" for a front without
// one (the answer cmd/gc's emitting class store gives too).
func (v *view) StoreHealthPath() string {
	if health, ok := v.Store.(interface{ StoreHealthPath() string }); ok {
		return health.StoreHealthPath()
	}
	return ""
}

// IDPrefix reports the prefix the front declares, following declared
// resolution targets as opjournal's prefix discovery does, and "" when none
// does: every consumer (storeref.PrefixOwner, the caching store, opjournal)
// reads "" as "no prefix", so the view routes exactly as its front would.
func (v *view) IDPrefix() string {
	current := v.Store
	for range maxUnwrapDepth {
		if p, ok := current.(interface{ IDPrefix() string }); ok {
			if prefix := p.IDPrefix(); prefix != "" {
				return prefix
			}
		}
		targeter, ok := current.(beads.ConditionalWritesResolveTargeter)
		if !ok {
			return ""
		}
		next := targeter.ConditionalWritesResolveTarget()
		if next == nil || next == current {
			return ""
		}
		current = next
	}
	return ""
}

// CloseStore releases the front's backing handle when it has one.
func (v *view) CloseStore() error {
	if closer, ok := v.Store.(interface{ CloseStore() error }); ok {
		return closer.CloseStore()
	}
	return nil
}

// selectsAll is the row predicate of a read that selected every row it
// returned: by parent, label, assignee or metadata.
func selectsAll(beads.Bead) bool { return true }

// listSelects returns the predicate a hidden row must satisfy for a list query
// to have selected it, or nil for a broad scan. A query that picks rows by
// something a class owns (ids, a parent, a type, a label, metadata or an
// assignee) selected the rows the store returned, except that ParentIDs is
// advisory (beads.ListQuery): a store that ignores it answers a superset the
// caller re-filters, so a row counts only when its parent is listed. A query
// constrained only by status, time, limit or tier is a broad scan.
func listSelects(q beads.ListQuery) func(beads.Bead) bool {
	if len(q.IDs) == 0 && q.ParentID == "" && len(q.ParentIDs) == 0 && q.Type == "" &&
		q.Label == "" && len(q.Metadata) == 0 && q.Assignee == "" && len(q.Assignees) == 0 {
		return nil
	}
	if len(q.ParentIDs) == 0 {
		return selectsAll
	}
	parents := make(map[string]bool, len(q.ParentIDs))
	for _, id := range q.ParentIDs {
		parents[id] = true
	}
	return func(b beads.Bead) bool { return parents[b.ParentID] }
}

// readySelects returns the predicate a hidden row must satisfy for a ready
// query to have selected it, or nil for an unfiltered ready scan (broad). A
// query picks rows by assignee or metadata; MetadataEquals and Unassigned are
// advisory (beads.ReadyQuery), so a store may answer a superset, and a row
// counts only when it satisfies every predicate the query carries.
func readySelects(qs []beads.ReadyQuery) func(beads.Bead) bool {
	var selective []beads.ReadyQuery
	for _, q := range qs {
		if q.Assignee != "" || len(q.MetadataEquals) > 0 {
			selective = append(selective, q)
		}
	}
	if len(selective) == 0 {
		return nil
	}
	return func(b beads.Bead) bool {
		for _, q := range selective {
			if readyQueryMatches(q, b) {
				return true
			}
		}
		return false
	}
}

// readyQueryMatches applies a ready query's row predicates to b.
func readyQueryMatches(q beads.ReadyQuery, b beads.Bead) bool {
	if q.Assignee != "" && b.Assignee != q.Assignee {
		return false
	}
	if q.Unassigned && b.Assignee != "" {
		return false
	}
	for key, value := range q.MetadataEquals {
		if got, ok := b.Metadata[key]; !ok || got != value {
			return false
		}
	}
	return true
}
