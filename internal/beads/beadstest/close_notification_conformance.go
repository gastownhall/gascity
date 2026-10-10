package beadstest

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// RunCloseNotificationTests pins the change-notification contract a
// CachingStore gives its consumers over the backing newBacking returns: every
// mutation that leaves a bead closed, made through the cache or behind it,
// produces exactly one bead.closed notification for that bead, however the
// cache learns of it, and a later live list and reconcile pass never repeat
// it (gastownhall/gascity#6860, #2546).
//
// The backing is wrapped in a primed CachingStore, the only store that emits
// change notifications; the cases exercise it over each backing's own row
// shapes (MemStore, BdStore over a fake bd, ...). Cases for the optional
// conditional-write capabilities run only when the backing has them. Each
// case seeds its beads in the backing before the prime, so no local-write
// recency stamp delays the reconcile pass that finally evicts the closed row.
func RunCloseNotificationTests(t *testing.T, newBacking func(t *testing.T) beads.Store) {
	t.Helper()

	type mutation struct {
		name string
		// needs reports whether the backing supports the mutation.
		needs func(backing beads.Store) bool
		// close leaves seed closed, through the cache or behind it.
		close func(t *testing.T, cs *beads.CachingStore, backing beads.Store, seed beads.Bead)
	}
	always := func(beads.Store) bool { return true }
	conditional := func(backing beads.Store) bool {
		_, ok := beads.ConditionalWriterFor(backing)
		return ok
	}
	closedStatus := "closed"
	mutations := []mutation{
		{"Close", always, func(t *testing.T, cs *beads.CachingStore, _ beads.Store, seed beads.Bead) {
			mustNoErr(t, "Close", cs.Close(seed.ID))
		}},
		{"CloseAll", always, func(t *testing.T, cs *beads.CachingStore, _ beads.Store, seed beads.Bead) {
			_, err := cs.CloseAll([]string{seed.ID}, nil)
			mustNoErr(t, "CloseAll", err)
		}},
		{"UpdateStatusClosed", always, func(t *testing.T, cs *beads.CachingStore, _ beads.Store, seed beads.Bead) {
			mustNoErr(t, "Update(status=closed)", cs.Update(seed.ID, beads.UpdateOpts{Status: &closedStatus}))
		}},
		{"CloseIfMatch", conditional, func(t *testing.T, cs *beads.CachingStore, _ beads.Store, seed beads.Bead) {
			mustConditional(t, "CloseIfMatch", cs.CloseIfMatch(seed.ID, currentRevision(t, cs, seed.ID)))
		}},
		{"UpdateIfMatchStatusClosed", conditional, func(t *testing.T, cs *beads.CachingStore, _ beads.Store, seed beads.Bead) {
			mustConditional(t, "UpdateIfMatch(status=closed)", cs.UpdateIfMatch(seed.ID, currentRevision(t, cs, seed.ID), beads.UpdateOpts{Status: &closedStatus}))
		}},
		{"UpdateIfAssignmentStatusClosed", func(backing beads.Store) bool {
			_, ok := beads.AssignmentGuardedUpdaterFor(backing)
			return ok
		}, func(t *testing.T, cs *beads.CachingStore, _ beads.Store, seed beads.Bead) {
			updated, err := cs.UpdateIfAssignment(seed.ID, "open", "", beads.UpdateOpts{Status: &closedStatus})
			mustConditional(t, "UpdateIfAssignment(status=closed)", err)
			if !updated {
				t.Fatal("UpdateIfAssignment(status=closed) did not update")
			}
		}},
		{"ExternalCloseSeenByReconcile", always, func(t *testing.T, cs *beads.CachingStore, backing beads.Store, seed beads.Bead) {
			mustNoErr(t, "backing Close", backing.Close(seed.ID))
			cs.ReconcileNowForTest()
		}},
		{"ExternalCloseSeenByLiveList", always, func(t *testing.T, cs *beads.CachingStore, backing beads.Store, seed beads.Bead) {
			mustNoErr(t, "backing Close", backing.Close(seed.ID))
			_, err := cs.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, Live: true})
			mustNoErr(t, "live List", err)
		}},
		{"ExternalCloseSeenByRefreshRow", always, func(t *testing.T, cs *beads.CachingStore, backing beads.Store, seed beads.Bead) {
			mustNoErr(t, "backing Close", backing.Close(seed.ID))
			_, err := cs.RefreshRow(seed.ID)
			mustNoErr(t, "RefreshRow", err)
		}},
	}

	for _, m := range mutations {
		t.Run(m.name+"AnnouncesOneClose", func(t *testing.T) {
			backing := newBacking(t)
			if !m.needs(backing) {
				t.Skipf("backing %T lacks the capability %s needs", backing, m.name)
			}
			seed, err := backing.Create(beads.Bead{Title: "closed by " + m.name})
			mustNoErr(t, "Create seed", err)
			bystander, err := backing.Create(beads.Bead{Title: "stays open"})
			mustNoErr(t, "Create bystander", err)
			cs, rec := primedNotifyingCache(t, backing)

			m.close(t, cs, backing, seed)
			// Whatever path announced the close, a later live list and
			// reconcile pass (which evicts the closed row) must not repeat
			// it, nor announce a bead that stayed open.
			_, err = cs.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, Live: true})
			mustNoErr(t, "settling live List", err)
			cs.ReconcileNowForTest()
			cs.ReconcileNowForTest()

			if got := rec.closes(seed.ID); got != 1 {
				t.Fatalf("bead.closed notifications for %s = %d, want exactly 1; notifications=%s", seed.ID, got, rec)
			}
			if got := rec.closes(bystander.ID); got != 0 {
				t.Fatalf("bead.closed notifications for the open bystander %s = %d, want 0; notifications=%s", bystander.ID, got, rec)
			}
			got, err := cs.Get(seed.ID)
			if err == nil && got.Status != "closed" {
				t.Fatalf("Get(%s).Status = %q after %s, want closed", seed.ID, got.Status, m.name)
			}
		})
	}

	t.Run("ReclosingAClosedBeadAnnouncesNothingMore", func(t *testing.T) {
		backing := newBacking(t)
		seed, err := backing.Create(beads.Bead{Title: "closed twice"})
		mustNoErr(t, "Create seed", err)
		cs, rec := primedNotifyingCache(t, backing)
		mustNoErr(t, "Close", cs.Close(seed.ID))
		mustNoErr(t, "second Close", cs.Close(seed.ID))
		_, err = cs.CloseAll([]string{seed.ID}, nil)
		mustNoErr(t, "CloseAll", err)
		cs.ReconcileNowForTest()
		if got := rec.closes(seed.ID); got != 1 {
			t.Fatalf("bead.closed notifications = %d, want exactly 1 across re-closes; notifications=%s", got, rec)
		}
	})
}

// closeNotificationRecorder counts the notifications a CachingStore emits.
type closeNotificationRecorder struct {
	mu     sync.Mutex
	events []string
	counts map[string]int
}

func (r *closeNotificationRecorder) onChange(eventType, beadID string, _ json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, eventType+" "+beadID)
	if eventType == "bead.closed" {
		r.counts[beadID]++
	}
}

func (r *closeNotificationRecorder) closes(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[id]
}

func (r *closeNotificationRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return fmt.Sprint(r.events)
}

// primedNotifyingCache wraps backing in a CachingStore that records its
// notifications, primes it, and starts recording from a clean slate.
func primedNotifyingCache(t *testing.T, backing beads.Store) (*beads.CachingStore, *closeNotificationRecorder) {
	t.Helper()
	rec := &closeNotificationRecorder{counts: map[string]int{}}
	cs := beads.NewCachingStoreForTest(backing, rec.onChange)
	mustNoErr(t, "Prime", cs.Prime(context.Background()))
	rec.mu.Lock()
	rec.events, rec.counts = nil, map[string]int{}
	rec.mu.Unlock()
	return cs, rec
}

func currentRevision(t *testing.T, s beads.Store, id string) int64 {
	t.Helper()
	b, err := s.Get(id)
	mustNoErr(t, "Get for revision", err)
	return b.Revision
}

func mustNoErr(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// mustConditional fails on err, except that a backing whose type carries a
// conditional verb but reports it unsupported at run time (a BdStore over a bd
// without revisions) skips the case.
func mustConditional(t *testing.T, what string, err error) {
	t.Helper()
	if beads.IsConditionalWriteUnsupported(err) {
		t.Skipf("%s: %v", what, err)
	}
	mustNoErr(t, what, err)
}
