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
	return newReadAnnounceFixtureOver(t, func(backing *MemStore) Store { return backing })
}

// newReadAnnounceFixtureOver is newReadAnnounceFixture with the cache reading
// the MemStore through the store wrap returns.
func newReadAnnounceFixtureOver(t *testing.T, wrap func(*MemStore) Store) *readAnnounceFixture {
	t.Helper()
	var clock atomic.Int64
	clock.Store(time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	backing := NewMemStore()
	rec := &readAnnounceRecorder{}
	cache := NewCachingStoreForTest(wrap(backing), rec.record, WithClock(now))
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

// newRacingReadAnnounceFixture is newReadAnnounceFixture with the cache reading
// the MemStore through a casBackingStore, whose hooks run a second read inside
// the first one's backing read window.
func newRacingReadAnnounceFixture(t *testing.T) (*readAnnounceFixture, *casBackingStore) {
	t.Helper()
	var racing *casBackingStore
	f := newReadAnnounceFixtureOver(t, func(backing *MemStore) Store {
		racing = &casBackingStore{Store: backing}
		return racing
	})
	return f, racing
}

// readAnnounceSlowRead is a read of the step that race overtakes: race runs
// after the backing answered the read and before the cache installs the
// answer, the window in which a faster read of the same row lands first.
type readAnnounceSlowRead struct {
	name string
	run  func(t *testing.T, f *readAnnounceFixture, racing *casBackingStore, race func())
}

var readAnnounceSlowReads = []readAnnounceSlowRead{
	{"live_parent_list", func(t *testing.T, f *readAnnounceFixture, racing *casBackingStore, race func()) {
		racing.onListOnce = race
		if _, err := f.cache.List(ListQuery{ParentID: f.root.ID, IncludeClosed: true}); err != nil {
			t.Fatalf("slow List: %v", err)
		}
	}},
	{"dirty_get", func(t *testing.T, f *readAnnounceFixture, racing *casBackingStore, race func()) {
		f.markStepDirty()
		racing.onGetOnce = race
		if _, err := f.cache.Get(f.step.ID); err != nil {
			t.Fatalf("slow Get: %v", err)
		}
	}},
	{"dirty_overlay", func(t *testing.T, f *readAnnounceFixture, racing *casBackingStore, race func()) {
		f.markStepDirty()
		racing.onGetOnce = race
		if _, err := f.cache.List(ListQuery{Metadata: map[string]string{"gc.root_bead_id": f.root.ID}}); err != nil {
			t.Fatalf("slow overlay List: %v", err)
		}
	}},
}

// runOvertakenRead runs slow with race as the faster read and fails when race
// never ran: the slow read then overlapped nothing.
func runOvertakenRead(t *testing.T, f *readAnnounceFixture, racing *casBackingStore, slow readAnnounceSlowRead, race func()) {
	t.Helper()
	ran := false
	slow.run(t, f, racing, func() {
		ran = true
		race()
	})
	if !ran {
		t.Fatal("the faster read did not run inside the slow read's backing read window; the race is vacuous")
	}
}

// requireCachedStepStatus requires the cache to hold the claimed step at
// wantStatus.
func requireCachedStepStatus(t *testing.T, f *readAnnounceFixture, wantStatus string) {
	t.Helper()
	got, held, _ := cachedRowState(f.cache, f.step.ID)
	if !held || got.Status != wantStatus || got.Assignee != readAnnounceClaimant {
		t.Fatalf("cached step = held=%v status %q assignee %q, want status %q assignee %q: an older read was installed over a newer one",
			held, got.Status, got.Assignee, wantStatus, readAnnounceClaimant)
	}
}

// Two reads of a step overlap. The slow one takes the step's pre-claim row
// from the backing; before it installs that row, another process claims the
// step and a faster read installs and announces the claim. The slow read's row
// is older than the one the cache now holds: installing it would move the
// cache, and through the bead.updated it announces every run view, back to the
// unclaimed step, and the reconcile that follows would find the cache stale
// rather than settled. It installs nothing: the claim is announced once and
// nothing after it, and the cache keeps the claimed row.
func TestCachingStoreOverlappingReadsNeverAnnounceAnOlderClaimState(t *testing.T) {
	for _, slow := range readAnnounceSlowReads {
		t.Run(slow.name, func(t *testing.T) {
			f, racing := newRacingReadAnnounceFixture(t)
			runOvertakenRead(t, f, racing, slow, func() {
				f.claimOutOfProcess(t)
				if _, err := f.cache.List(ListQuery{ParentID: f.root.ID}); err != nil {
					t.Fatalf("fast List: %v", err)
				}
			})

			requireOneClaimAnnouncement(t, f.rec, f.step.ID)
			requireCachedStepStatus(t, f, "in_progress")
			f.cache.ReconcileNowForTest()
			requireOneClaimAnnouncement(t, f.rec, f.step.ID)
			requireCachedStepStatus(t, f, "in_progress")
		})
	}
}

// The close shape of the same race: the slow read takes the claimed row, the
// step closes, and a faster read installs and announces the close. The slow
// read's row would announce the closed step as running again.
func TestCachingStoreOverlappingReadsNeverAnnounceAnUpdateAfterTheClose(t *testing.T) {
	for _, slow := range readAnnounceSlowReads {
		t.Run(slow.name, func(t *testing.T) {
			f, racing := newRacingReadAnnounceFixture(t)
			f.claimOutOfProcess(t)
			if _, err := f.cache.List(ListQuery{ParentID: f.root.ID}); err != nil {
				t.Fatalf("List: %v", err)
			}
			requireOneClaimAnnouncement(t, f.rec, f.step.ID)
			f.rec.reset()

			runOvertakenRead(t, f, racing, slow, func() {
				if err := f.backing.Close(f.step.ID); err != nil {
					t.Fatalf("out-of-process close: %v", err)
				}
				if _, err := f.cache.List(ListQuery{ParentID: f.root.ID, IncludeClosed: true}); err != nil {
					t.Fatalf("fast List: %v", err)
				}
			})

			rows := f.rec.forBead(f.step.ID)
			if len(rows) != 1 || rows[0].eventType != "bead.closed" {
				t.Fatalf("notifications for %s = %+v, want exactly the one bead.closed", f.step.ID, rows)
			}
			requireCachedStepStatus(t, f, "closed")
		})
	}
}

// The unheld shape: another process created the step after the cache primed,
// so the faster read installs it as a row the cache did not hold, which a read
// does not announce. The slow read's pre-claim row, installed over it, would
// be announced as a change: the claimer announces the claim, and the cache
// then announces the step unclaimed.
func TestCachingStoreOverlappingReadsNeverAnnounceAnOlderStateOfAnUnheldRow(t *testing.T) {
	for _, slow := range readAnnounceSlowReads {
		t.Run(slow.name, func(t *testing.T) {
			f, racing := newRacingReadAnnounceFixture(t)
			late, err := f.backing.Create(Bead{
				Title:    "late step",
				Type:     "task",
				ParentID: f.root.ID,
				Metadata: map[string]string{"gc.root_bead_id": f.root.ID},
			})
			if err != nil {
				t.Fatalf("out-of-process create: %v", err)
			}
			if _, held, _ := cachedRowState(f.cache, late.ID); held {
				t.Fatal("the cache holds the late step; the unheld shape is vacuous")
			}
			f.step = late
			runOvertakenRead(t, f, racing, slow, func() {
				f.claimOutOfProcess(t)
				if _, err := f.cache.List(ListQuery{ParentID: f.root.ID}); err != nil {
					t.Fatalf("fast List: %v", err)
				}
			})

			if rows := f.rec.forBead(late.ID); len(rows) != 0 {
				t.Fatalf("notifications for %s = %+v, want none: the cache never held it unclaimed", late.ID, rows)
			}
			requireCachedStepStatus(t, f, "in_progress")
		})
	}
}

// childrenBatchMemStore gives a MemStore the batched children read, answered
// one parent at a time, so the cache's ChildrenOfAny takes its batch path.
type childrenBatchMemStore struct {
	*MemStore
}

func (s childrenBatchMemStore) ChildrenOfAny(parentIDs []string, opts ...QueryOpt) ([]Bead, error) {
	return ChildrenOfAnyPerParent(s.MemStore, parentIDs, opts...)
}

// A batched children read (a molecule walk: finalize, run cancel) installs each
// parent's children as a ParentID List does, so a claim it is the first reader
// to see is announced as that List announces it.
func TestCachingStoreChildrenOfAnyAnnouncesOutOfProcessClaimItInstalls(t *testing.T) {
	f := newReadAnnounceFixtureOver(t, func(backing *MemStore) Store { return childrenBatchMemStore{backing} })
	if _, ok := any(f.cache.backing).(ChildrenBatchLister); !ok {
		t.Fatal("the backing has no batched children read; ChildrenOfAny would read each parent through List")
	}
	f.claimOutOfProcess(t)

	children, err := f.cache.ChildrenOfAny([]string{f.root.ID})
	if err != nil {
		t.Fatalf("ChildrenOfAny: %v", err)
	}
	if len(children) != 1 || children[0].Status != "in_progress" {
		t.Fatalf("ChildrenOfAny = %+v, want the claimed step", children)
	}
	requireOneClaimAnnouncement(t, f.rec, f.step.ID)
	f.cache.ReconcileNowForTest()
	requireOneClaimAnnouncement(t, f.rec, f.step.ID)
}
