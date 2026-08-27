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
// # One target per process, and every process has one
//
// Emission is not a property of a store; it is a property of the PROCESS that
// opened it, so the emit target is set on the ROUTES, once, at the construction
// that belongs to that process: resolveCLIStorageRoutes for a one-shot command,
// newCityRuntime for the controller. Each gets the target it can carry — a
// one-shot command has no live bus and opens the journal per batch, the
// controller records through the bus it already owns.
//
// The controller used to be left out on the theory that its CachingStore
// covered these writes. It does not: that cache sits on the city's WORK ledger,
// and a relocated class is served from a binding no cache wraps, so every
// session-class write a split city's controller made emitted nothing —
// admitSessionStartEvent never fired for a session row and keyed admission fell
// back to patrol cadence (ga-f7v2ft.161 Q4, .164 step 2). Emission is required
// plumbing on a relocated class, not a capability a process may skip, so there
// is no arm here that declines to wire one.
//
// Double emission is what the per-process rule prevents, and it is prevented by
// arithmetic rather than by care: a store carries at most one wrapper because
// exactly one construction per process installs it, and the layer that would
// have been the second emitter (the CachingStore) is never on a relocated
// class. TestClassStoreEmitTargetIsInjectedOncePerProcess pins the injectors.
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
// The payload is the canonical bead snapshot CachingStore.notifyChange emits —
// json.Marshal of the post-write bead, decodable by beads.DecodeBeadEventPayload
// and foldable by the run projection — with the run/session/step correlation
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
	"encoding/json"
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

// classStoreEmitTarget is where one process writes the bead.* rows its
// relocated class stores produce, and who those rows are attributed to.
//
// It exists because the two processes that open a binding differ in exactly one
// respect: whether they already hold a recorder. Everything else about emission
// — when, what, in which shape — is the same on both sides and lives below.
type classStoreEmitTarget interface {
	// openRecorder returns the recorder for one emission batch and the release
	// that finishes it. A target that cannot open one returns the error, and
	// the batch is dropped with a diagnostic rather than failing the mutation
	// it describes.
	openRecorder() (events.Recorder, func(), error)
	// actor attributes the rows to whoever performed the mutation.
	actor() string
}

// cityJournalEmitTarget appends to a city's event log, opening it per batch. It
// is the one-shot CLI's target: a command has no live bus of its own, and its
// rows have to reach the same journal every other writer uses.
type cityJournalEmitTarget struct{ cityPath string }

func (t cityJournalEmitTarget) openRecorder() (events.Recorder, func(), error) {
	rec, err := newClassStoreEmitRecorder(t.cityPath)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the event log of %s: %w", t.cityPath, err)
	}
	return rec, func() { _ = rec.Close() }, nil
}

// actor names the agent or command that made the write, which is what a
// one-shot mutation is genuinely attributable to.
func (cityJournalEmitTarget) actor() string { return eventActor() }

// liveRecorderEmitTarget records through a recorder the process already owns:
// the controller's event provider, which is also the journal its own bead-event
// watcher tails.
type liveRecorderEmitTarget struct{ rec events.Recorder }

func (t liveRecorderEmitTarget) openRecorder() (events.Recorder, func(), error) {
	return t.rec, func() {}, nil
}

// actor attributes a controller-side relocated write to the store layer, which
// is what the controller's CachingStore already does for the identical write on
// a city that relocates nothing. The value decides what ELSE the event does —
// a tick poke, a routed-work demand contribution — so a split city emitting
// under a different actor would not merely be labeled differently, it would
// behave differently from the composition the campaign certified.
func (liveRecorderEmitTarget) actor() string { return beadStoreLayerActor }

// withCLIEmission gives these routes the one-shot CLI's emit target: the city's
// own event log, opened per batch.
func (r *storageRoutes) withCLIEmission(cityPath string) *storageRoutes {
	if strings.TrimSpace(cityPath) == "" {
		return r
	}
	return r.withEmission(cityJournalEmitTarget{cityPath: strings.TrimSpace(cityPath)})
}

// withControllerEmission gives these routes the controller's emit target: the
// live recorder it already holds. Without it a split city's controller writes to
// its relocated classes in silence — the defect this seam exists to close.
func (r *storageRoutes) withControllerEmission(rec events.Recorder) *storageRoutes {
	if rec == nil {
		return r
	}
	return r.withEmission(liveRecorderEmitTarget{rec: rec})
}

// withEmission makes every class store these routes serve append a canonical
// bead.* event through target after each successful mutation, and returns the
// routes.
//
// Nil routes — a city that relocates nothing — are returned untouched, so a
// single-store city reaches none of this and its class resolvers keep returning
// the caller's own store value.
//
// Each distinct leaf store is wrapped exactly once, and every class it serves
// gets that same wrapper value. Store identity is load-bearing: callers dedup
// scan candidates in a map[beads.Store], so a wrapper per class would turn one
// binding into five and re-scan it five times.
func (r *storageRoutes) withEmission(target classStoreEmitTarget) *storageRoutes {
	if r == nil || target == nil || len(r.stores) == 0 {
		return r
	}
	if r.emit != nil {
		// Routes already claimed by a process. Wrapping a wrapper is the double
		// row this file's header is about, and it costs nothing to make the
		// "one target per process" rule arithmetic rather than discipline.
		return r
	}
	emitting := make(map[beads.Store]beads.Store, 1)
	for class, store := range r.stores {
		if store == nil {
			continue
		}
		wrapped, ok := emitting[store]
		if !ok {
			wrapped = &emittingClassStore{Store: store, target: target}
			emitting[store] = wrapped
		}
		r.stores[class] = wrapped
	}
	r.emit = target
	return r
}

// emittingClassStore is a relocated class store that appends a bead.* event
// after each successful mutation. The embedded Store carries the required
// surface; every optional capability either bead engine declares is forwarded
// explicitly below.
type emittingClassStore struct {
	beads.Store
	target classStoreEmitTarget
}

// EmitsChangeEvents reports that this store appends a bead.* event after its own
// mutations. It answers the contract's recorder-wiring question about the store
// rather than about its type, which is the only honest way to ask it: a wrapper
// holding no target and one holding a live recorder are the same Go type.
func (s *emittingClassStore) EmitsChangeEvents() bool { return s != nil && s.target != nil }

// ConditionalWritesCapabilityTarget points every capability question at the
// engine, while this wrapper stays the writer.
//
// The wrapper forwards the whole fenced trio, so asking whether IT can fence
// answers yes for a store that may not be able to fence at all — the vacuous
// capability the status wire must never report. Pointing the question down is
// not the same as pointing the WRITE down: a resolve target would hand callers
// the bare engine and every fenced write, terminal close included, would land
// with no event at all.
func (s *emittingClassStore) ConditionalWritesCapabilityTarget() beads.Store { return s.Store }

// ---------------------------------------------------------------------------
// Emission.

// classStoreEmission is one pending row: the event type and the bead snapshot
// to carry as its payload.
type classStoreEmission struct {
	eventType string
	bead      beads.Bead
}

// emit appends one row per emission through a single recorder from the target,
// which owns the cross-process sequence and locking. The batch is the unit
// because a target may pay to open one.
func (s *emittingClassStore) emit(emissions ...classStoreEmission) {
	if len(emissions) == 0 || s.target == nil {
		return
	}
	rec, release, err := s.target.openRecorder()
	if err != nil {
		warnClassStoreEmit(err)
		return
	}
	defer release()
	actor := s.target.actor()
	for _, emission := range emissions {
		if strings.TrimSpace(emission.bead.ID) == "" {
			continue
		}
		payload, err := json.Marshal(emission.bead)
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

func (s *emittingClassStore) DepMetadata(issueID, dependsOnID string) (string, bool, error) {
	reader, ok := s.Store.(interface {
		DepMetadata(string, string) (string, bool, error)
	})
	if !ok {
		return "", false, nil
	}
	return reader.DepMetadata(issueID, dependsOnID)
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
//
// events.WithoutStartupSweep is what makes a per-mutation open safe: the sweep
// exists to recover rotating-* files a crash stranded, it belongs to the
// supervisor's long-lived recorder, and running it here would race that
// recorder mid-rotation. It does not make the open free — NewFileRecorder reads
// the log directory either way, to continue the sequence past the archives —
// only unraced.
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
