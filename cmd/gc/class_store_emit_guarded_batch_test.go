package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// The emitter's guarded batches are discovered only over a backing that has
// them, resolve to the emitter itself so the batch still emits, and record one
// event per item once the batch lands: bead.created for a create, bead.closed
// for an update that closes.
func TestEmittingClassStoreGuardedBatchEmitsEachItem(t *testing.T) {
	cityPath := t.TempDir()
	leaf := beads.NewMemStore()
	row, err := leaf.Create(beads.Bead{Title: "row"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := &emittingClassStore{Store: leaf, cityPath: cityPath}
	batcher, ok := beads.GuardedBatcherFor(store)
	if !ok {
		t.Fatal("GuardedBatcherFor(emitter over MemStore) = false")
	}
	if batcher != beads.GuardedBatcher(store) {
		t.Fatalf("GuardedBatcherFor resolved %T, want the emitter so the batch emits", batcher)
	}
	closed := "closed"
	if _, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gbemitted", Title: "emitted"}},
		{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Status: &closed}}},
	}}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	got := map[string]string{}
	for _, evt := range beadEvents(readCityJournal(t, cityPath)) {
		got[evt.Subject] = evt.Type
	}
	if got["gc-gbemitted"] != events.BeadCreated || got[row.ID] != events.BeadClosed {
		t.Fatalf("events = %v, want bead.created for gc-gbemitted and bead.closed for %s", got, row.ID)
	}

	stale := row.Revision
	if _, err := batcher.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gbrefused", Title: "refused"}},
		{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &stale, Opts: beads.UpdateOpts{Title: &closed}}},
	}}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("ApplyGuarded on a stale revision = %v, want a precondition failure", err)
	}
	for _, evt := range beadEvents(readCityJournal(t, cityPath)) {
		if evt.Subject == "gc-gbrefused" {
			t.Fatalf("a refused batch emitted %s for gc-gbrefused", evt.Type)
		}
	}

	if b, ok := beads.GuardedBatcherFor(&emittingClassStore{Store: storeWithoutBatchedDepRead{Store: beads.NewMemStore()}, cityPath: cityPath}); ok {
		t.Fatalf("GuardedBatcherFor(emitter over an incapable backing) = (%T, true), want no capability", b)
	}
}

// getCountingGuardedStore counts the row reads the emitter makes around a
// guarded batch.
type getCountingGuardedStore struct {
	*beads.MemStore
	gets int
}

func (s *getCountingGuardedStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.MemStore.Get(id)
}

func (s *getCountingGuardedStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	return s.MemStore, true
}

// The emitter reads each row once after the batch, and before it only the
// rows whose status the batch writes (an update that leaves the status alone
// cannot open or close the row), and every item still emits.
func TestEmittingClassStoreGuardedBatchReadsEachRowOnce(t *testing.T) {
	cityPath := t.TempDir()
	leaf := &getCountingGuardedStore{MemStore: beads.NewMemStore()}
	tagged, err := leaf.Create(beads.Bead{Title: "tagged"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	closing, err := leaf.Create(beads.Bead{Title: "closing"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := &emittingClassStore{Store: leaf, cityPath: cityPath}
	closed := "closed"
	if _, err := store.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gbone", Title: "one"}},
		{Create: &beads.Bead{ID: "gc-gbtwo", Title: "two"}},
		{Update: &beads.GuardedUpdate{ID: tagged.ID, ExpectedRevision: &tagged.Revision, Opts: beads.UpdateOpts{Metadata: map[string]string{"k": "v"}}}},
		{Update: &beads.GuardedUpdate{ID: closing.ID, ExpectedRevision: &closing.Revision, Opts: beads.UpdateOpts{Status: &closed}}},
	}}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	if want := 4 + 1; leaf.gets != want {
		t.Errorf("emitter read rows %d times, want %d: one snapshot per item and one pre-read for the status write", leaf.gets, want)
	}
	got := map[string]string{}
	for _, evt := range beadEvents(readCityJournal(t, cityPath)) {
		got[evt.Subject] = evt.Type
	}
	want := map[string]string{
		"gc-gbone": events.BeadCreated, "gc-gbtwo": events.BeadCreated,
		tagged.ID: events.BeadUpdated, closing.ID: events.BeadClosed,
	}
	for id, typ := range want {
		if got[id] != typ {
			t.Errorf("event for %s = %q, want %q (all events %v)", id, got[id], typ, got)
		}
	}
}

// A batch hands back revisions, not rows, so a create the emitter cannot
// re-read after the batch has nothing true to journal: the request carries an
// empty status where the store wrote "open", and the caller's timestamps and
// revision. Journaling it would clobber the fold, so the create is skipped with
// a diagnostic, while a close the same batch landed still rides bead.closed
// from the write's own status.
func TestEmittingClassStoreGuardedBatchSkipsACreateItCannotReRead(t *testing.T) {
	cityPath := t.TempDir()
	leaf := &readMissAfterWriteStore{MemStore: beads.NewMemStore()}
	row, err := leaf.Create(beads.Bead{Title: "row"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var warned []string
	restore := classStoreEmitWarn
	classStoreEmitWarn = func(err error) { warned = append(warned, err.Error()) }
	t.Cleanup(func() { classStoreEmitWarn = restore })

	store := &emittingClassStore{Store: leaf, cityPath: cityPath}
	leaf.missing = true
	closed := "closed"
	if _, err := store.ApplyGuarded(context.Background(), beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gbunread", Title: "unread"}},
		{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Status: &closed}}},
	}}); err != nil {
		t.Fatalf("ApplyGuarded: %v", err)
	}
	if _, err := leaf.MemStore.Get("gc-gbunread"); err != nil {
		t.Fatalf("the batch create did not land: %v", err)
	}

	got := beadEvents(readCityJournal(t, cityPath))
	if len(got) != 1 || got[0].Type != events.BeadClosed || got[0].Subject != row.ID {
		t.Fatalf("got %s, want only bead.closed for %s: a create that cannot be re-read must not journal the request", eventSummary(got), row.ID)
	}
	if snapshot, ok := beads.DecodeBeadEventPayload(got[0].Payload); !ok || !strings.EqualFold(snapshot.Status, "closed") {
		t.Errorf("bead.closed payload = %s, want a closed status from the write's own transition", got[0].Payload)
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "gc-gbunread") {
		t.Errorf("diagnostics = %q, want one naming the skipped create gc-gbunread", warned)
	}
}

// ctxEndingReadStore ends the batch's context on its first row read after a
// batch landed, standing in for re-reads that run past the caller's deadline.
type ctxEndingReadStore struct {
	*beads.MemStore
	cancel context.CancelFunc
	// landed is set once a batch has landed; reads counts the row reads
	// since.
	landed bool
	reads  int
}

func (s *ctxEndingReadStore) Get(id string) (beads.Bead, error) {
	if s.landed {
		s.reads++
		s.cancel()
	}
	return s.MemStore.Get(id)
}

func (s *ctxEndingReadStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	return s, true
}

func (s *ctxEndingReadStore) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	res, err := s.MemStore.ApplyGuarded(ctx, b)
	s.landed = err == nil
	return res, err
}

// A batch that lands before its context ends still succeeds, but the
// emitter's re-reads stop once that context ends: no more rows are read and
// nothing is journaled, with one diagnostic, so a caller bounded by the
// context (the in-band hook pass) is not held past it.
func TestEmittingClassStoreGuardedBatchStopsOnceItsContextEnds(t *testing.T) {
	cityPath := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	leaf := &ctxEndingReadStore{MemStore: beads.NewMemStore(), cancel: cancel}
	row, err := leaf.Create(beads.Bead{Title: "row"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	var warned []string
	restore := classStoreEmitWarn
	classStoreEmitWarn = func(err error) { warned = append(warned, err.Error()) }
	t.Cleanup(func() { classStoreEmitWarn = restore })

	store := &emittingClassStore{Store: leaf, cityPath: cityPath}
	closed := "closed"
	if _, err := store.ApplyGuarded(ctx, beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gblate", Title: "late"}},
		{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Status: &closed}}},
	}}); err != nil {
		t.Fatalf("ApplyGuarded = %v; want the landed batch's success", err)
	}
	if got, err := leaf.MemStore.Get(row.ID); err != nil || got.Status != closed {
		t.Fatalf("row after the batch = %+v, %v; want it closed", got, err)
	}
	if leaf.reads != 1 {
		t.Errorf("emitter read %d rows after the batch, want the one that ended the context", leaf.reads)
	}
	if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 0 {
		t.Errorf("journaled %s after the context ended, want nothing", eventSummary(got))
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "bead.* skipped") || !strings.Contains(warned[0], context.Canceled.Error()) {
		t.Errorf("diagnostics = %q, want one saying the events were skipped for the ended context", warned)
	}
}

// A batch whose context has already ended is not applied: the emitter reads
// nothing and returns the context's cause, and nothing is written.
func TestEmittingClassStoreGuardedBatchAppliesNothingPastItsContext(t *testing.T) {
	cityPath := t.TempDir()
	leaf := &getCountingGuardedStore{MemStore: beads.NewMemStore()}
	row, err := leaf.Create(beads.Bead{Title: "row"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("the hook's settle-by time passed")
	cancel(cause)
	store := &emittingClassStore{Store: leaf, cityPath: cityPath}
	closed := "closed"
	leaf.gets = 0
	if _, err := store.ApplyGuarded(ctx, beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gbpast", Title: "past"}},
		{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Status: &closed}}},
	}}); !errors.Is(err, cause) {
		t.Fatalf("ApplyGuarded = %v; want the context's cause", err)
	}
	if _, err := leaf.MemStore.Get("gc-gbpast"); err == nil {
		t.Fatal("the batch landed though its context had ended")
	}
	if leaf.gets != 0 {
		t.Errorf("emitter read %d rows, want none", leaf.gets)
	}
	if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 0 {
		t.Errorf("journaled %s, want nothing", eventSummary(got))
	}
}

// ctxEndingPreReadStore ends the batch's context on the emitter's row read
// before the batch, as a pre-read that runs past the caller's deadline would,
// and counts the batches handed to it. It applies them without consulting
// ctx, as a backing that starts work before it looks at ctx does.
type ctxEndingPreReadStore struct {
	*beads.MemStore
	cancel  context.CancelCauseFunc
	cause   error
	batches int
}

func (s *ctxEndingPreReadStore) Get(id string) (beads.Bead, error) {
	s.cancel(s.cause)
	return s.MemStore.Get(id)
}

func (s *ctxEndingPreReadStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	return s, true
}

func (s *ctxEndingPreReadStore) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	s.batches++
	return s.MemStore.ApplyGuarded(context.WithoutCancel(ctx), b)
}

// A batch whose context ends during the emitter's last read before it is not
// handed to the backing, whatever the backing does with an ended context: the
// emitter returns the context's cause, and nothing is written or journaled.
func TestEmittingClassStoreGuardedBatchAppliesNothingOnceAPreReadEndsItsContext(t *testing.T) {
	cityPath := t.TempDir()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	leaf := &ctxEndingPreReadStore{MemStore: beads.NewMemStore(), cancel: cancel, cause: errors.New("the hook's settle-by time passed")}
	row, err := leaf.Create(beads.Bead{Title: "row"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := &emittingClassStore{Store: leaf, cityPath: cityPath}
	closed := "closed"
	if _, err := store.ApplyGuarded(ctx, beads.GuardedBatch{Items: []beads.GuardedItem{
		{Create: &beads.Bead{ID: "gc-gbpreread", Title: "pre-read"}},
		{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Status: &closed}}},
	}}); !errors.Is(err, leaf.cause) {
		t.Fatalf("ApplyGuarded = %v; want the context's cause", err)
	}
	if leaf.batches != 0 {
		t.Fatalf("the backing was handed %d batches after the context ended, want none", leaf.batches)
	}
	if got, err := leaf.MemStore.Get(row.ID); err != nil || got.Status == closed {
		t.Fatalf("row = %+v, %v; want it unwritten", got, err)
	}
	if got := beadEvents(readCityJournal(t, cityPath)); len(got) != 0 {
		t.Errorf("journaled %s, want nothing", eventSummary(got))
	}
}
