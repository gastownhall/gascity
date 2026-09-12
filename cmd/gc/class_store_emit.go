package main

// bead.* emission for the one-shot CLI's relocated coordination classes.
//
// A split city serves graph, sessions, messaging, orders and nudges from a
// binding the controller opens at boot and one-shot commands open through the
// funnel (cli_storage_routes.go). The controller's copy of the city's work
// ledger sits under a CachingStore, whose notifyChange appends a bead.* row to
// <city>/.gc/events.jsonl after every mutation. The binding has no such layer
// on either side, so every write a one-shot command made to a relocated class —
// `gc bd close` of a gcg- step, `gc sling`, `gc formula cook`, the control
// dispatcher's one-shot arm, mail, nudges — landed in the store and appended
// nothing.
//
// What that costs is not the events: it is every consumer that reconstructs
// state by folding them. The event-sourced run projection never saw a
// worker-side close, so a molecule's steps rendered "Running" forever and the
// wisp reap made it permanent by deleting the rows the fold would have needed
// to recover from. The event-delta lanes read the same journal from a cursor,
// so on a split city they read an empty delta and conclude nothing happened.
//
// # One target, injected once
//
// Emission is not a property of a store; it is a property of the PROCESS that
// opened it. The controller has a live event bus and its own emitter, and a
// second one on the same mutation is a double row in the log — worse on the
// reconcile path, where the cache re-absorbs rows in bulk and a per-row emitter
// turns absorption into a flood.
//
// So the emit target is set on the ROUTES, at the one construction that belongs
// to a one-shot command (resolveCLIStorageRoutes), and never at the one that
// belongs to the controller (openStorageRoutes). The controller path is
// therefore untouched by construction rather than by a runtime test that could
// answer wrongly: it never reaches this file at all.
// TestClassStoreEmitTargetHasExactlyOneInjectionSite pins the single injector,
// because "the controller does not emit" is a claim about the call graph and
// nothing else keeps a second injector from appearing.
//
// # Why the wrapper forwards so much
//
// A relocated class store is a bare bead engine: openStorageRoutes hands back
// whatever the binding's provider opened (a *beads.SQLiteStore for the built-in
// binding, a *beads.NativeDoltStore for a workspace one) with no wrapper of any
// kind. Emission therefore has to arrive as one, and a wrapper is exactly how
// this repo loses capabilities: the optional interfaces are discovered by type
// assertion, so a method the wrapper does not carry does not fail — the caller
// simply stops matching and takes a slower or weaker path, silently. Every
// method either engine declares is forwarded here, and
// TestEmittingClassStoreKeepsEveryEngineCapability fails if either grows one
// this file does not.
//
// # What it emits, and what it deliberately does not
//
// The payload is the canonical bead snapshot from
// beads.EncodeBeadEventPayload, decodable by beads.DecodeBeadEventPayload and
// foldable by the run projection, with the run/session/step correlation
// resolved onto the typed envelope from the same metadata keys and through the
// same helpers. bead.closed rides only a genuine open→closed transition, because
// the export boundary drops bead.updated and a metadata write to a closed bead
// that rode bead.closed would re-close it in every replay.
//
// Tx is NOT shadowed. A transaction's effect is whatever its body did, and this
// layer cannot know which beads changed without re-reading the store around
// every call — so a Tx-shaped write on a relocated class stays event-dark, as it
// was before this file. That is a known gap rather than an oversight; closing it
// needs a touched-id set from the Tx itself.
//
// Emission is best-effort, per the one-shot recorder precedent: the mutation has
// already committed when the row is written, so a journal that cannot be opened
// must not turn a landed close into a failed command. Failures are surfaced once
// per process through classStoreEmitWarn rather than swallowed, because a
// dropped terminal event that nobody hears about is the failure this file
// exists to end.

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/events"
)

// withCLIEmission makes every class store these routes serve append a canonical
// bead.* event to cityPath's journal after each successful mutation, and
// returns the routes.
//
// It is the ONE injector. Nil routes (a city that relocates nothing) and an
// empty city path are returned untouched, so a single-store city reaches none
// of this and its class resolvers keep returning the caller's own store value.
//
// Each distinct leaf store is wrapped exactly once, and every class it serves
// gets that same wrapper value. Store identity is load-bearing: callers dedup
// scan candidates in a map[beads.Store], so a wrapper per class would turn one
// binding into five and re-scan it five times.
func (r *storageRoutes) withCLIEmission(cityPath string) *storageRoutes {
	cityPath = strings.TrimSpace(cityPath)
	if r == nil || cityPath == "" || len(r.stores) == 0 {
		return r
	}
	emitting := make(map[beads.Store]beads.Store, 1)
	for class, store := range r.stores {
		if store == nil {
			continue
		}
		wrapped, ok := emitting[store]
		if !ok {
			wrapped = &emittingClassStore{Store: store, cityPath: cityPath}
			emitting[store] = wrapped
		}
		r.stores[class] = wrapped
	}
	r.emitCityPath = cityPath
	return r
}

// emittingClassStore is a relocated class store that appends a bead.* event
// after each successful mutation. The embedded Store carries the required
// surface; every optional capability either bead engine declares is forwarded
// explicitly below.
type emittingClassStore struct {
	beads.Store
	cityPath string
}

// ---------------------------------------------------------------------------
// Emission.

// classStoreEmission is one pending row: the event type and the bead snapshot
// to carry as its payload.
type classStoreEmission struct {
	eventType string
	bead      beads.Bead
}

// emit appends one row per emission to the city's journal through a single
// recorder, which owns the cross-process sequence and locking.
//
// events.WithoutStartupSweep is what makes a per-mutation open safe: the sweep
// exists to recover rotating-* files a crash stranded, it belongs to the
// supervisor's long-lived recorder, and running it here would race that
// recorder mid-rotation. It does not make the open free — NewFileRecorder reads
// the log directory either way, to continue the sequence past the archives —
// only unraced.
func (s *emittingClassStore) emit(emissions ...classStoreEmission) {
	if len(emissions) == 0 {
		return
	}
	rec, err := newClassStoreEmitRecorder(s.cityPath)
	if err != nil {
		warnClassStoreEmit(fmt.Errorf("opening the event log of %s: %w", s.cityPath, err))
		return
	}
	defer rec.Close() //nolint:errcheck // best-effort: emission never surfaces I/O errors
	actor := eventActor()
	for _, emission := range emissions {
		if strings.TrimSpace(emission.bead.ID) == "" {
			continue
		}
		payload, err := beads.EncodeBeadEventPayload(emission.bead)
		if err != nil {
			warnClassStoreEmit(fmt.Errorf("marshaling the %s payload of %s: %w", emission.eventType, emission.bead.ID, err))
			continue
		}
		stepID := emission.bead.Metadata[beadmeta.StepIDMetadataKey]
		rec.Record(events.Event{
			Type:             emission.eventType,
			Actor:            actor,
			Subject:          emission.bead.ID,
			RunID:            beadmeta.ResolveRunID(emission.bead.Metadata, emission.bead.ID, ""),
			SessionID:        emission.bead.Metadata[beadmeta.SessionIDMetadataKey],
			StepID:           stepID,
			DependsOnStepIDs: beads.NativeStepDependencies(emission.bead.Metadata, stepID),
			Payload:          payload,
		})
	}
}

// snapshot re-reads the post-write bead so the payload is the authoritative
// fresh state, which is what the controller's emitter does for the same reason.
//
// A read miss reports ok=false and the caller skips the row rather than
// emitting a bare id: an empty snapshot does not merely say less, it CLOBBERS
// the fold, because a projection applying it overwrites the row's title, status
// and run membership with nothing.
//
// Dependency edges are hydrated because the controller's payload carries them
// and a consumer comparing the two shapes would otherwise read the CLI's as an
// edge removal.
func (s *emittingClassStore) snapshot(id string) (beads.Bead, bool) {
	bead, err := s.Get(id)
	if err != nil || strings.TrimSpace(bead.ID) == "" {
		return beads.Bead{}, false
	}
	if deps, err := s.DepList(id, "down"); err == nil {
		bead.Dependencies = deps
		bead.Needs = nil
	}
	return bead, true
}

// emitCreated records bead.created for a landed create. A post-write read miss
// falls back to what the store returned, which is a full snapshot rather than a
// bare id, so a create is never dropped on a store with read-after-write lag.
func (s *emittingClassStore) emitCreated(created beads.Bead, err error) {
	if err != nil {
		return
	}
	if strings.TrimSpace(created.ID) == "" {
		warnClassStoreEmit(errors.New("bead.created skipped: the create returned an empty id"))
		return
	}
	bead, ok := s.snapshot(created.ID)
	if !ok {
		bead = created
	}
	s.emit(classStoreEmission{eventType: events.BeadCreated, bead: bead})
}

// emitUpdated records bead.updated for a write that cannot have changed the
// bead's terminal state — metadata, assignment, dependency edges, a reopen. A
// read miss skips the row rather than emitting a bare id.
func (s *emittingClassStore) emitUpdated(id string) {
	bead, ok := s.snapshot(id)
	if !ok {
		warnClassStoreEmit(fmt.Errorf("bead.updated skipped: %s could not be re-read after the write", id))
		return
	}
	s.emit(classStoreEmission{eventType: events.BeadUpdated, bead: bead})
}

// emitAfterUpdate records the lifecycle edge a landed Update produced.
//
// bead.closed rides only a genuine open→closed transition. A metadata write to
// an already-closed bead is an update, matching the controller's emitter: the
// export boundary drops bead.updated, so a close edge must ride bead.closed —
// and a false one would re-close the bead in every replay of the log.
//
// On a read miss the row is synthesized from the write's own status when it
// carried one, so a committed close still rides bead.closed with a non-empty
// status, and skipped otherwise.
func (s *emittingClassStore) emitAfterUpdate(id string, opts beads.UpdateOpts, wasClosed bool) {
	bead, ok := s.snapshot(id)
	if !ok {
		if opts.Status == nil {
			warnClassStoreEmit(fmt.Errorf("bead.updated skipped: %s could not be re-read after the update", id))
			return
		}
		bead = beads.Bead{ID: id, Status: *opts.Status, Metadata: maps.Clone(opts.Metadata)}
	}
	eventType := events.BeadUpdated
	if !wasClosed && beadStatusIsClosed(bead.Status) {
		eventType = events.BeadClosed
	}
	s.emit(classStoreEmission{eventType: eventType, bead: bead})
}

// emitClosed records bead.closed for a close this store proved committed.
//
// Unlike the update path a read miss still emits, with the id and the status the
// close established: the terminal transition is a fact, and dropping it is the
// exact silence the seam exists to end.
func (s *emittingClassStore) emitClosed(id string) {
	bead, ok := s.snapshot(id)
	if !ok {
		bead = beads.Bead{ID: id}
	}
	bead.Status = beadStatusClosed
	s.emit(classStoreEmission{eventType: events.BeadClosed, bead: bead})
}

// emitClosedBead records bead.closed from the committed row an atomic fenced
// close returned. That row IS the post-commit state (status closed, metadata
// merged), so unlike emitClosed it needs no re-read — mirroring emitCreated's
// "trust what the store returned" fallback. An empty id is the one thing it
// cannot emit, and a close that returned one is a store bug worth surfacing.
func (s *emittingClassStore) emitClosedBead(bead beads.Bead) {
	if strings.TrimSpace(bead.ID) == "" {
		warnClassStoreEmit(errors.New("bead.closed skipped: the atomic close returned an empty id"))
		return
	}
	bead.Status = beadStatusClosed
	s.emit(classStoreEmission{eventType: events.BeadClosed, bead: bead})
}

// closedBefore reports whether a bead was already closed before a write. A read
// that fails answers "not closed", which is the safe direction: the write's own
// post-state then decides, and an event that says closed when the bead is closed
// is never wrong.
func (s *emittingClassStore) closedBefore(id string) bool {
	bead, err := s.Get(id)
	return err == nil && beadStatusIsClosed(bead.Status)
}

// snapshotBeforeDelete captures the pre-delete beads, because there is nothing
// to read afterwards. Dependency edges are not hydrated: bead.deleted removes
// the row from the fold, so its edges cannot matter.
func (s *emittingClassStore) snapshotBeforeDelete(ids ...string) []beads.Bead {
	out := make([]beads.Bead, 0, len(ids))
	for _, id := range ids {
		if bead, err := s.Get(id); err == nil && strings.TrimSpace(bead.ID) != "" {
			out = append(out, bead)
			continue
		}
		out = append(out, beads.Bead{ID: id})
	}
	return out
}

// emitDeleted records bead.deleted for each pre-delete snapshot.
func (s *emittingClassStore) emitDeleted(snapshots []beads.Bead) {
	emissions := make([]classStoreEmission, 0, len(snapshots))
	for _, bead := range snapshots {
		emissions = append(emissions, classStoreEmission{eventType: events.BeadDeleted, bead: bead})
	}
	s.emit(emissions...)
}

// emitGraphApplyCreated records bead.created for every bead a graph apply
// materialized. A graph apply IS the create path for a molecule — it writes the
// root and its steps in one plan — so a relocated apply that emitted nothing
// would leave the run projection with steps it never saw begin. A per-bead read
// miss skips that bead rather than dropping the batch.
func (s *emittingClassStore) emitGraphApplyCreated(result *beads.GraphApplyResult, err error) {
	if err != nil || result == nil || len(result.IDs) == 0 {
		return
	}
	emissions := make([]classStoreEmission, 0, len(result.IDs))
	for _, id := range result.IDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		bead, ok := s.snapshot(id)
		if !ok {
			warnClassStoreEmit(fmt.Errorf("bead.created skipped: graph-applied %s could not be re-read", id))
			continue
		}
		emissions = append(emissions, classStoreEmission{eventType: events.BeadCreated, bead: bead})
	}
	s.emit(emissions...)
}

// ---------------------------------------------------------------------------
// The required surface. Each method delegates and then emits; the delegation is
// the whole behavior, so a failed write emits nothing.

func (s *emittingClassStore) Create(bead beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(bead)
	s.emitCreated(created, err)
	return created, err
}

func (s *emittingClassStore) Update(id string, opts beads.UpdateOpts) error {
	wasClosed := s.closedBefore(id)
	if err := s.Store.Update(id, opts); err != nil {
		return err
	}
	s.emitAfterUpdate(id, opts, wasClosed)
	return nil
}

func (s *emittingClassStore) Close(id string) error {
	if err := s.Store.Close(id); err != nil {
		return err
	}
	s.emitClosed(id)
	return nil
}

func (s *emittingClassStore) Reopen(id string) error {
	if err := s.Store.Reopen(id); err != nil {
		return err
	}
	s.emitUpdated(id)
	return nil
}

// CloseAll emits one bead.closed per bead that actually transitioned. The
// pre-state is captured first so a bead already closed produces no row, and the
// post-state is confirmed so a bead the backing store declined to close
// produces none either.
func (s *emittingClassStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	wasOpen := make(map[string]bool, len(ids))
	for _, id := range ids {
		wasOpen[id] = !s.closedBefore(id)
	}
	n, err := s.Store.CloseAll(ids, metadata)
	var emissions []classStoreEmission
	for _, id := range ids {
		if !wasOpen[id] {
			continue
		}
		bead, ok := s.snapshot(id)
		if !ok || !beadStatusIsClosed(bead.Status) {
			continue
		}
		emissions = append(emissions, classStoreEmission{eventType: events.BeadClosed, bead: bead})
	}
	s.emit(emissions...)
	return n, err
}

func (s *emittingClassStore) Delete(id string) error {
	snapshots := s.snapshotBeforeDelete(id)
	if err := s.Store.Delete(id); err != nil {
		return err
	}
	s.emitDeleted(snapshots)
	return nil
}

func (s *emittingClassStore) SetMetadata(id, key, value string) error {
	if err := s.Store.SetMetadata(id, key, value); err != nil {
		return err
	}
	s.emitUpdated(id)
	return nil
}

func (s *emittingClassStore) SetMetadataBatch(id string, values map[string]string) error {
	if err := s.Store.SetMetadataBatch(id, values); err != nil {
		return err
	}
	s.emitUpdated(id)
	return nil
}

// DepAdd emits for the bead whose edges changed. The snapshot hydrates edges
// through DepList, so the payload reflects the new one.
func (s *emittingClassStore) DepAdd(issueID, dependsOnID, depType string) error {
	if err := s.Store.DepAdd(issueID, dependsOnID, depType); err != nil {
		return err
	}
	s.emitUpdated(issueID)
	return nil
}

func (s *emittingClassStore) DepRemove(issueID, dependsOnID string) error {
	if err := s.Store.DepRemove(issueID, dependsOnID); err != nil {
		return err
	}
	s.emitUpdated(issueID)
	return nil
}

// ---------------------------------------------------------------------------
// Optional capabilities. Every method either bead engine declares appears here,
// so wrapping cannot silently narrow what a caller's type assertion finds. A
// method the underlying store does not implement answers with the sentinel the
// beads package publishes for it, or with the zero value where the question is
// a capability question and the honest answer is "no".

func (s *emittingClassStore) CreateWithStorage(bead beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	creator, ok := s.Store.(beads.StorageCreateStore)
	if !ok {
		return beads.Bead{}, fmt.Errorf("creating with a storage class: %T does not support it", s.Store)
	}
	created, err := creator.CreateWithStorage(bead, storage)
	s.emitCreated(created, err)
	return created, err
}

func (s *emittingClassStore) CreateWithForeignID(bead beads.Bead) (beads.Bead, error) {
	creator, ok := s.Store.(interface {
		CreateWithForeignID(beads.Bead) (beads.Bead, error)
	})
	if !ok {
		return beads.Bead{}, fmt.Errorf("creating with a foreign id: %T does not support it", s.Store)
	}
	created, err := creator.CreateWithForeignID(bead)
	s.emitCreated(created, err)
	return created, err
}

func (s *emittingClassStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	writer, ok := beads.ConditionalWriterFor(s.Store)
	if !ok {
		return beads.ErrConditionalWriteUnsupported
	}
	wasClosed := s.closedBefore(id)
	if err := writer.UpdateIfMatch(id, revision, opts); err != nil {
		return err
	}
	s.emitAfterUpdate(id, opts, wasClosed)
	return nil
}

func (s *emittingClassStore) CloseIfMatch(id string, revision int64) error {
	writer, ok := beads.ConditionalWriterFor(s.Store)
	if !ok {
		return beads.ErrConditionalWriteUnsupported
	}
	if err := writer.CloseIfMatch(id, revision); err != nil {
		return err
	}
	s.emitClosed(id)
	return nil
}

// CloseWithMetadataIfMatch forwards the atomic fenced terminal close (merge
// metadata and close in one transaction, guarded by the revision) to the
// backing capability, so AtomicConditionalCloserFor discovers it on the CLI's
// wrapped store instead of stopping at this wrapper. It emits bead.closed from
// the committed row the close returned.
func (s *emittingClassStore) CloseWithMetadataIfMatch(id string, revision int64, metadata map[string]string) (beads.Bead, error) {
	closer, ok := beads.AtomicConditionalCloserFor(s.Store)
	if !ok {
		return beads.Bead{}, beads.ErrConditionalWriteUnsupported
	}
	closed, err := closer.CloseWithMetadataIfMatch(id, revision, metadata)
	if err != nil {
		return beads.Bead{}, err
	}
	s.emitClosedBead(closed)
	return closed, nil
}

// AtomicConditionalCloserHandle keeps AtomicConditionalCloserFor honest over
// this wrapper. TestEmittingClassStoreKeepsEveryEngineCapability forces the
// wrapper to carry CloseWithMetadataIfMatch structurally for every engine, so a
// bare type assertion would advertise the capability even over a backing (for
// example the sqlite CLI engine) that cannot honor it — and that discovery is
// contractually a hard capability gate, not a rollout seam. Consulted first by
// AtomicConditionalCloserFor, this answers yes only when the resolved backing
// truly provides the atomic close, and returns the emitting wrapper (not the
// raw backing) so the discovered closer still emits bead.closed.
func (s *emittingClassStore) AtomicConditionalCloserHandle() (beads.AtomicConditionalCloser, bool) {
	if _, ok := beads.AtomicConditionalCloserFor(s.Store); !ok {
		return nil, false
	}
	return s, true
}

// HasResidentOutside forwards the relic census to the backing capability. It
// exists because TestEmittingClassStoreKeepsEveryEngineCapability holds this
// wrapper to every engine method, and a wrapper that carries the method must
// still be unable to answer for a backing that cannot: a store with no census
// gets beads.ErrNamespaceCensusUnsupported, never (false, nil). A false clean
// here would retire the binding's by-id probe over every relic sitting in it.
//
// Discovery does not reach this method — NamespaceCensusHandle below answers
// first — so this is the spelling for a caller holding the wrapper that asks
// directly.
func (s *emittingClassStore) HasResidentOutside(prefixes []string) (bool, error) {
	census, ok := beads.NamespaceCensusFor(s.Store)
	if !ok {
		return false, fmt.Errorf("censusing the id namespaces of the emitting class store over %T: %w", s.Store, beads.ErrNamespaceCensusUnsupported)
	}
	return census.HasResidentOutside(prefixes)
}

// NamespaceCensusHandle keeps beads.NamespaceCensusFor honest over this
// wrapper, exactly as AtomicConditionalCloserHandle does above and for the same
// reason: the reflective capability guard forces HasResidentOutside to EXIST
// for every engine, so a bare type assertion would advertise a census over a
// backing — a mem store, the native Dolt engine — that has none. The census
// contract makes that a hard gate rather than a rollout seam, because a store
// that answers the question inexactly strands every bead it failed to see.
//
// It hands back the backing's census rather than the wrapper: the wrapper adds
// emission, a census reads and mutates nothing, so there is nothing here for
// the answer to pass back through.
func (s *emittingClassStore) NamespaceCensusHandle() (beads.NamespaceCensus, bool) {
	return beads.NamespaceCensusFor(s.Store)
}

func (s *emittingClassStore) DeleteIfMatch(id string, revision int64) error {
	writer, ok := beads.ConditionalWriterFor(s.Store)
	if !ok {
		return beads.ErrConditionalWriteUnsupported
	}
	snapshots := s.snapshotBeforeDelete(id)
	if err := writer.DeleteIfMatch(id, revision); err != nil {
		return err
	}
	s.emitDeleted(snapshots)
	return nil
}

// CompareAndSetMetadataKey emits only when the swap actually happened: a
// refused compare changed nothing, and a row saying otherwise would make every
// fold disagree with the store.
func (s *emittingClassStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	swapper, ok := s.Store.(interface {
		CompareAndSetMetadataKey(string, string, string, string) (bool, error)
	})
	if !ok {
		return false, beads.ErrConditionalWriteUnsupported
	}
	swapped, err := swapper.CompareAndSetMetadataKey(id, key, expected, next)
	if err == nil && swapped {
		s.emitUpdated(id)
	}
	return swapped, err
}

func (s *emittingClassStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	claimer, ok := s.Store.(interface {
		Claim(string, string) (beads.Bead, bool, error)
	})
	if !ok {
		return beads.Bead{}, false, beads.ErrConditionalWriteUnsupported
	}
	bead, claimed, err := claimer.Claim(id, assignee)
	if err == nil && claimed {
		s.emitUpdated(id)
	}
	return bead, claimed, err
}

func (s *emittingClassStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := s.Store.(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, beads.ErrConditionalReleaseUnsupported
	}
	released, err := releaser.ReleaseIfCurrent(id, expectedAssignee)
	if err == nil && released {
		s.emitUpdated(id)
	}
	return released, err
}

func (s *emittingClassStore) DeleteBatch(ids []string) error {
	deleter, ok := s.Store.(beads.BatchDeleter)
	if !ok {
		return beads.ErrBatchDeleteUnsupported
	}
	snapshots := s.snapshotBeforeDelete(ids...)
	if err := deleter.DeleteBatch(ids); err != nil {
		return err
	}
	s.emitDeleted(snapshots)
	return nil
}

func (s *emittingClassStore) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	applier, ok := beads.GraphApplyFor(s.Store)
	if !ok {
		return nil, fmt.Errorf("applying a graph plan: %T does not support it", s.Store)
	}
	result, err := applier.ApplyGraphPlan(ctx, plan)
	s.emitGraphApplyCreated(result, err)
	return result, err
}

func (s *emittingClassStore) ApplyGraphPlanWithStorage(ctx context.Context, plan *beads.GraphApplyPlan, storage beads.StorageClass) (*beads.GraphApplyResult, error) {
	applier, ok := s.Store.(beads.StorageGraphApplyStore)
	if !ok {
		return nil, fmt.Errorf("applying a graph plan with a storage class: %T does not support it", s.Store)
	}
	result, err := applier.ApplyGraphPlanWithStorage(ctx, plan, storage)
	s.emitGraphApplyCreated(result, err)
	return result, err
}

func (s *emittingClassStore) ReadyContext(ctx context.Context, query ...beads.ReadyQuery) ([]beads.Bead, error) {
	reader, ok := s.Store.(beads.ContextReadyReader)
	if !ok {
		return nil, beads.ErrReadyContextUnsupported
	}
	return reader.ReadyContext(ctx, query...)
}

func (s *emittingClassStore) Count(ctx context.Context, query beads.ListQuery, excludeTypes ...string) (int, error) {
	counter, ok := s.Store.(beads.Counter)
	if !ok {
		return 0, beads.ErrCountUnsupported
	}
	return counter.Count(ctx, query, excludeTypes...)
}

func (s *emittingClassStore) WaitForParentProjection(ctx context.Context, parentID, childID, scope string) error {
	waiter, ok := s.Store.(beads.ParentProjectionWaiter)
	if !ok {
		return nil
	}
	return waiter.WaitForParentProjection(ctx, parentID, childID, scope)
}

// DepMetadata forwards the inner store's edge-payload read. Emission has
// nothing to say about what an edge carries, so the answer passes through.
//
// An inner store without the read gets an error rather than ("", false, nil).
// The lenient form is the one shape this capability must never take: a caller
// that refuses on uncertainty — the infra-class migration is one — would read
// a store that CANNOT be asked as one answering "carries nothing", which is
// exactly the conflation that let edge payloads drop silently for months.
func (s *emittingClassStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	reader, ok := s.Store.(beads.DepMetadataReader)
	if !ok {
		return "", false, fmt.Errorf("reading dependency metadata %s -> %s: emitting store %T exposes no edge-payload read", issueID, dependsOnID, s.Store)
	}
	return reader.DepMetadata(issueID, dependsOnID)
}

// DepAddWithMetadata is DepAdd for an edge that carries a payload, and emits
// the same bead.updated for the same endpoint the plain form does: the issue
// side, whose DepList the snapshot hydrates. A subscriber that saw the
// payloadless add but not this one would hold a stale view of exactly the edges
// formula gating depends on.
//
// An inner store without the write gets an error rather than falling back to
// DepAdd. The fallback is the shape this must never take: on a store that keeps
// the payload in a sidecar, a plain DepAdd over an edge that had one CLEARS it,
// so "carry it if you can, add it plainly if you cannot" is not a degraded carry
// but a destructive one.
func (s *emittingClassStore) DepAddWithMetadata(issueID, dependsOnID, depType, metadata string) error {
	writer, ok := s.Store.(beads.DepMetadataWriter)
	if !ok {
		return fmt.Errorf("writing dependency metadata %s -> %s: emitting store %T cannot carry an edge payload", issueID, dependsOnID, s.Store)
	}
	if err := writer.DepAddWithMetadata(issueID, dependsOnID, depType, metadata); err != nil {
		return err
	}
	s.emitUpdated(issueID)
	return nil
}

func (s *emittingClassStore) SequenceFloor() (int64, error) {
	floor, ok := s.Store.(interface{ SequenceFloor() (int64, error) })
	if !ok {
		return 0, fmt.Errorf("reading the sequence floor: %T does not keep one", s.Store)
	}
	return floor.SequenceFloor()
}

func (s *emittingClassStore) SetSequenceFloor(seq int64) error {
	floor, ok := s.Store.(interface{ SetSequenceFloor(int64) error })
	if !ok {
		return fmt.Errorf("setting the sequence floor: %T does not keep one", s.Store)
	}
	return floor.SetSequenceFloor(seq)
}

func (s *emittingClassStore) AdvanceSequenceFloor(seq int64) {
	if floor, ok := s.Store.(interface{ AdvanceSequenceFloor(int64) }); ok {
		floor.AdvanceSequenceFloor(seq)
	}
}

func (s *emittingClassStore) CloseStore() error {
	closer, ok := s.Store.(interface{ CloseStore() error })
	if !ok {
		return nil
	}
	return closer.CloseStore()
}

func (s *emittingClassStore) IDPrefix() string {
	prefixer, ok := s.Store.(interface{ IDPrefix() string })
	if !ok {
		return ""
	}
	return prefixer.IDPrefix()
}

func (s *emittingClassStore) StoreHealthPath() string {
	pather, ok := s.Store.(interface{ StoreHealthPath() string })
	if !ok {
		return ""
	}
	return pather.StoreHealthPath()
}

func (s *emittingClassStore) AtomicTx() bool {
	atomic, ok := s.Store.(interface{ AtomicTx() bool })
	return ok && atomic.AtomicTx()
}

func (s *emittingClassStore) SupportsEphemeralGraphApply() bool {
	supporter, ok := s.Store.(interface{ SupportsEphemeralGraphApply() bool })
	return ok && supporter.SupportsEphemeralGraphApply()
}

// ---------------------------------------------------------------------------
// Diagnostics and small shared helpers.

// beadStatusClosed is the one spelling of the terminal status this file writes.
const beadStatusClosed = "closed"

// beadStatusIsClosed reports whether a status is the terminal one, tolerating
// the whitespace and casing a hand-written status can carry.
func beadStatusIsClosed(status string) bool {
	return strings.EqualFold(strings.TrimSpace(status), beadStatusClosed)
}

// newClassStoreEmitRecorder opens the city's journal for one emission batch.
func newClassStoreEmitRecorder(cityPath string) (*events.FileRecorder, error) {
	return events.NewFileRecorder(
		filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl"),
		classStoreEmitWarnWriter{},
		events.WithoutStartupSweep(),
	)
}

// classStoreEmitWarnWriter funnels the recorder's own stderr diagnostics — a
// flock timeout that dropped a row, a short write, a rotation failure — into
// the warn-once path, so a dropped terminal event is visible rather than
// discarded. It always reports a full write, which is what a recorder's stderr
// sink has to do.
type classStoreEmitWarnWriter struct{}

func (classStoreEmitWarnWriter) Write(p []byte) (int, error) {
	if msg := strings.TrimSpace(string(p)); msg != "" {
		warnClassStoreEmit(errors.New(msg))
	}
	return len(p), nil
}

// classStoreEmitWarn is the emission-diagnostic sink. It is a package variable
// so a test can capture what would otherwise be a one-line warning; the default
// prints the first diagnostic and suppresses the rest, so a broken event log is
// neither silent nor a flood in a worker's stderr.
var classStoreEmitWarn = warnClassStoreEmitOncePerProcess()

func warnClassStoreEmitOncePerProcess() func(error) {
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			fmt.Fprintf(os.Stderr, "gc: class-store event emission: %v (further emission errors suppressed)\n", err) //nolint:errcheck // best-effort stderr
		})
	}
}

// warnClassStoreEmit routes one diagnostic through the current sink.
func warnClassStoreEmit(err error) { classStoreEmitWarn(err) }
