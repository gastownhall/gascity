package beads

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// readAnnounceRecorder collects a CachingStore's change notifications.
type readAnnounceRecorder struct {
	mu   sync.Mutex
	rows []readAnnounceRow
}

type readAnnounceRow struct {
	eventType string
	bead      Bead
}

func (r *readAnnounceRecorder) record(eventType, _ string, payload json.RawMessage) {
	b, ok := DecodeBeadEventPayload(payload)
	if !ok {
		panic("undecodable bead notification payload")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = append(r.rows, readAnnounceRow{eventType: eventType, bead: b})
}

func (r *readAnnounceRecorder) forBead(id string) []readAnnounceRow {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []readAnnounceRow
	for _, row := range r.rows {
		if row.bead.ID == id {
			out = append(out, row)
		}
	}
	return out
}

func (r *readAnnounceRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = nil
}

// readAnnounceFixture is a primed cache over a MemStore holding a run root and
// one open step, created through the cache as the supervisor creates them, with
// the clock moved past the recent-local-write window so a later read is free to
// absorb what the backing says.
type readAnnounceFixture struct {
	backing *MemStore
	cache   *CachingStore
	rec     *readAnnounceRecorder
	root    Bead
	step    Bead
}

func newReadAnnounceFixture(t *testing.T) *readAnnounceFixture {
	t.Helper()
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	backing := NewMemStore()
	rec := &readAnnounceRecorder{}
	cache := NewCachingStoreForTest(backing, rec.record, WithClock(now))
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	root, err := cache.Create(Bead{Title: "run root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	step, err := cache.Create(Bead{
		Title:    "step",
		Type:     "task",
		ParentID: root.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	clock.Add(int64(time.Minute))
	rec.reset()
	return &readAnnounceFixture{backing: backing, cache: cache, rec: rec, root: root, step: step}
}

// readAnnounceClaimant is the worker every out-of-process claim in these tests names.
const readAnnounceClaimant = "worker"

// claimOutOfProcess claims the step in the backing directly, the way
// `gc hook --claim` does from a one-shot process: a bd write that this cache
// never sees and that announces nothing.
func (f *readAnnounceFixture) claimOutOfProcess(t *testing.T) {
	t.Helper()
	status := "in_progress"
	assignee := readAnnounceClaimant
	if err := f.backing.Update(f.step.ID, UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("out-of-process claim: %v", err)
	}
}

func (f *readAnnounceFixture) markStepDirty() {
	f.cache.mu.Lock()
	f.cache.markDirtyLocked(f.step.ID)
	f.cache.mu.Unlock()
}

func requireOneClaimAnnouncement(t *testing.T, rec *readAnnounceRecorder, id string) {
	t.Helper()
	assignee := readAnnounceClaimant
	rows := rec.forBead(id)
	if len(rows) != 1 {
		t.Fatalf("notifications for %s = %+v, want exactly one bead.updated carrying the claim", id, rows)
	}
	got := rows[0]
	if got.eventType != "bead.updated" || got.bead.Status != "in_progress" || got.bead.Assignee != assignee {
		t.Fatalf("notification = %s status=%q assignee=%q, want bead.updated status=in_progress assignee=%q",
			got.eventType, got.bead.Status, got.bead.Assignee, assignee)
	}
}

// A live list (a parent-children read, which is what every workflow reader and
// GET /bead/{id}/deps issue) is often the first reader to see a claim another
// process made. It installs the claimed row; before this fix it announced
// nothing, so the reconcile pass that followed found the cache already agreeing
// with the backing and never announced the claim either. Every bead.* consumer
// — the event-sourced run projection behind GET /runs and /runs/{id}/steps
// among them — then saw the step pending until it closed.
func TestCachingStoreLiveListAnnouncesOutOfProcessClaimItInstalls(t *testing.T) {
	f := newReadAnnounceFixture(t)
	f.claimOutOfProcess(t)

	if _, err := f.cache.List(ListQuery{ParentID: f.root.ID}); err != nil {
		t.Fatalf("List: %v", err)
	}
	cached, err := f.cache.Get(f.step.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cached.Status != "in_progress" || cached.Assignee != "worker" {
		t.Fatalf("cached step = status %q assignee %q, want the claim installed", cached.Status, cached.Assignee)
	}
	requireOneClaimAnnouncement(t, f.rec, f.step.ID)

	// The reconcile that follows finds nothing new: the claim is announced
	// once, not lost and not repeated.
	f.cache.ReconcileNowForTest()
	requireOneClaimAnnouncement(t, f.rec, f.step.ID)
}

func TestCachingStoreDirtyGetAnnouncesOutOfProcessClaimItInstalls(t *testing.T) {
	f := newReadAnnounceFixture(t)
	f.markStepDirty()
	f.claimOutOfProcess(t)

	got, err := f.cache.Get(f.step.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "worker" {
		t.Fatalf("Get = status %q assignee %q, want the backing's claim", got.Status, got.Assignee)
	}
	requireOneClaimAnnouncement(t, f.rec, f.step.ID)
	f.cache.ReconcileNowForTest()
	requireOneClaimAnnouncement(t, f.rec, f.step.ID)
}

func TestCachingStoreDirtyOverlayAnnouncesOutOfProcessClaimItInstalls(t *testing.T) {
	f := newReadAnnounceFixture(t)
	f.markStepDirty()
	f.claimOutOfProcess(t)

	rows, err := f.cache.List(ListQuery{Metadata: map[string]string{"gc.root_bead_id": f.root.ID}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Status != "in_progress" {
		t.Fatalf("overlay list = %+v, want the claimed step", rows)
	}
	requireOneClaimAnnouncement(t, f.rec, f.step.ID)
	f.cache.ReconcileNowForTest()
	requireOneClaimAnnouncement(t, f.rec, f.step.ID)
}

// Refetching a row that did not change announces nothing: the announcement
// tracks a transition the cache had not seen, not every read.
func TestCachingStoreReadRefetchOfUnchangedRowAnnouncesNothing(t *testing.T) {
	f := newReadAnnounceFixture(t)
	for range 3 {
		if _, err := f.cache.List(ListQuery{ParentID: f.root.ID}); err != nil {
			t.Fatalf("List: %v", err)
		}
	}
	f.markStepDirty()
	if _, err := f.cache.Get(f.step.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rows := f.rec.forBead(f.step.ID); len(rows) != 0 {
		t.Fatalf("notifications for an unchanged step = %+v, want none", rows)
	}
}

// A close another process made keeps its single bead.closed announcement
// (gastownhall/gascity#6860); it is not announced again as an update.
func TestCachingStoreLiveListAnnouncesOutOfProcessCloseOnlyAsClose(t *testing.T) {
	f := newReadAnnounceFixture(t)
	if err := f.backing.Close(f.step.ID); err != nil {
		t.Fatalf("out-of-process close: %v", err)
	}
	if _, err := f.cache.List(ListQuery{ParentID: f.root.ID, IncludeClosed: true}); err != nil {
		t.Fatalf("List: %v", err)
	}
	rows := f.rec.forBead(f.step.ID)
	if len(rows) != 1 || rows[0].eventType != "bead.closed" {
		t.Fatalf("notifications = %+v, want exactly one bead.closed", rows)
	}
}

// A queued read announcement is superseded, not replayed, when a write through
// the cache installs a different row before the drain: the write announces its
// own row, and replaying the older read would move consumers backwards.
func TestCachingStoreQueuedReadAnnouncementYieldsToLaterDifferentInstall(t *testing.T) {
	f := newReadAnnounceFixture(t)
	f.claimOutOfProcess(t)
	items, err := f.backing.List(ListQuery{ParentID: f.root.ID})
	if err != nil {
		t.Fatalf("backing list: %v", err)
	}
	f.cache.mu.Lock()
	startSeq, startScan := f.cache.mutationSeq, f.cache.scanGen
	f.cache.mu.Unlock()
	f.cache.refreshCachedBeads(ListQuery{ParentID: f.root.ID}, startSeq, startScan, items)

	f.cache.mu.Lock()
	_, queued := f.cache.unannouncedUpdates[f.step.ID]
	f.cache.mu.Unlock()
	if !queued {
		t.Fatal("the list refetch queued no announcement for the claim it installed")
	}

	status := "open"
	empty := ""
	if err := f.cache.Update(f.step.ID, UpdateOpts{Status: &status, Assignee: &empty}); err != nil {
		t.Fatalf("release through cache: %v", err)
	}
	f.cache.announceUnannouncedUpdates()

	rows := f.rec.forBead(f.step.ID)
	if len(rows) != 1 || rows[0].bead.Status != "open" {
		t.Fatalf("notifications = %+v, want only the release the write announced", rows)
	}
}
