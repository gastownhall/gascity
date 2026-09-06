package main

import (
	"bytes"
	"os"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// TestCmdNudgeStatusDoesNotBlockOnHeldQueueLock pins that `gc nudge status` is a
// lock-free reader of the persisted nudge queue.
//
// Regression test for ga-cn8dkj: cmdNudgeStatus reached the queue through
// listQueuedNudgesForTarget -> withNudgeQueueState -> nudgequeue.WithState,
// which takes a city-wide *exclusive* flock (internal/nudgequeue/state.go:111)
// and then runs the full maintenance sweep -- spawning serial `bd` subprocesses
// -- under that lock. On a busy city the lock is permanently contended, so a
// read-only status call blocked in flock(2) and returned NO output and NO
// error until the caller's own timeout killed it.
//
// Status only reads. It must never wait on the queue's writer lock.
func TestCmdNudgeStatusDoesNotBlockOnHeldQueueLock(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)

	// Seed the queue so nudgeQueueHasWork() is true (an empty queue would skip
	// the maintenance path and could mask the block).
	now := time.Now().Add(-time.Minute)
	if err := enqueueQueuedNudge(cityDir, newQueuedNudge("mayor", "review queued work", now)); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	// Hold the queue's exclusive lock, exactly as a concurrent `gc nudge poll`
	// tick does while it drains the backlog. flock conflicts are per open file
	// description, so this conflicts with the command's own separate open.
	lockFile, err := os.OpenFile(nudgequeue.LockPath(cityDir), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("opening queue lock: %v", err)
	}
	defer lockFile.Close() //nolint:errcheck
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatalf("holding queue lock: %v", err)
	}
	defer syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) //nolint:errcheck

	type result struct {
		code   int
		stdout string
		stderr string
	}
	done := make(chan result, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := cmdNudgeStatus([]string{"mayor"}, true, &stdout, &stderr)
		done <- result{code: code, stdout: stdout.String(), stderr: stderr.String()}
	}()

	select {
	case got := <-done:
		if got.code != 0 {
			t.Fatalf("cmdNudgeStatus = %d, want 0; stderr=%s", got.code, got.stderr)
		}
		if got.stdout == "" {
			t.Fatalf("cmdNudgeStatus produced no output while the queue lock was held")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cmdNudgeStatus blocked on the held nudge-queue lock: status is a read and must not wait on the writer flock (ga-cn8dkj)")
	}
}

// TestListQueuedNudgesForTargetSnapshotMatchesMaintainedBuckets pins that the
// lock-free reader is a faithful projection of the maintaining reader.
//
// listQueuedNudgesForTargetSnapshot re-buckets in memory what
// recoverExpiredInFlightNudges/pruneExpiredQueuedNudges do on disk. Re-bucketing
// alone is not enough: those passes also project the fields the bucket implies
// (a recovered item loses its lease and becomes deliverable now; a newly-dead
// item gets a DeadAt) and re-sort, and Item's lease fields are `omitempty`, so
// skipping the projection would show `gc nudge status --json` a pending item
// still carrying the lease it just lost.
func TestListQueuedNudgesForTargetSnapshotMatchesMaintainedBuckets(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	writeNamedSessionCityTOML(t, dir)
	t.Setenv("GC_CITY", dir)

	now := time.Now()
	seed := func(id string, mutate func(*queuedNudge)) queuedNudge {
		item := queuedNudge{
			ID:           id,
			Agent:        "worker",
			Source:       "test",
			Message:      id,
			CreatedAt:    now.Add(-time.Hour),
			DeliverAfter: now.Add(-time.Minute),
			ExpiresAt:    now.Add(time.Hour),
		}
		mutate(&item)
		return item
	}
	// No BeadID on any item: dead-letter retention pruning only considers
	// items with a backing bead, so the maintaining path leaves these alone
	// and the two readers stay comparable.
	if err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
		state.Pending = append(state.Pending,
			seed("nudge-pending-live", func(_ *queuedNudge) {}),
			seed("nudge-pending-expired", func(i *queuedNudge) { i.ExpiresAt = now.Add(-time.Minute) }),
		)
		state.InFlight = append(state.InFlight,
			seed("nudge-inflight-live", func(i *queuedNudge) {
				i.ClaimedAt = now.Add(-time.Minute)
				i.LeaseUntil = now.Add(time.Hour)
			}),
			seed("nudge-inflight-lease-expired", func(i *queuedNudge) {
				i.ClaimedAt = now.Add(-2 * time.Hour)
				i.LeaseUntil = now.Add(-time.Minute)
			}),
			seed("nudge-inflight-expired", func(i *queuedNudge) {
				i.ClaimedAt = now.Add(-2 * time.Hour)
				i.LeaseUntil = now.Add(time.Hour)
				i.ExpiresAt = now.Add(-time.Minute)
			}),
		)
		state.Dead = append(state.Dead, seed("nudge-dead-existing", func(i *queuedNudge) {
			i.DeadAt = now.Add(-2 * time.Hour)
			i.LastError = "boom"
		}))
		return nil
	}); err != nil {
		t.Fatalf("seeding nudge queue: %v", err)
	}

	target := nudgeTarget{cityPath: dir, identity: "worker"}

	// Snapshot first: it must not mutate state, so the maintaining read below
	// still sees the seeded queue.
	snapPending, snapInFlight, snapDead, err := listQueuedNudgesForTargetSnapshot(dir, target, now)
	if err != nil {
		t.Fatalf("listQueuedNudgesForTargetSnapshot: %v", err)
	}
	wantPending, wantInFlight, wantDead, err := listQueuedNudgesForTarget(dir, target, now)
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}

	ids := func(items []queuedNudge) []string {
		out := make([]string, 0, len(items))
		for _, item := range items {
			out = append(out, item.ID)
		}
		return out
	}
	for _, bucket := range []struct {
		name string
		got  []queuedNudge
		want []queuedNudge
	}{
		{"pending", snapPending, wantPending},
		{"in-flight", snapInFlight, wantInFlight},
		{"dead", snapDead, wantDead},
	} {
		gotIDs, wantIDs := ids(bucket.got), ids(bucket.want)
		if !slices.Equal(gotIDs, wantIDs) {
			t.Fatalf("snapshot %s = %v, want %v (order included)", bucket.name, gotIDs, wantIDs)
		}
	}

	byID := func(items []queuedNudge, id string) queuedNudge {
		t.Helper()
		for _, item := range items {
			if item.ID == id {
				return item
			}
		}
		t.Fatalf("item %q not found in bucket", id)
		return queuedNudge{}
	}

	recovered := byID(snapPending, "nudge-inflight-lease-expired")
	if !recovered.ClaimedAt.IsZero() || !recovered.LeaseUntil.IsZero() {
		t.Fatalf("recovered item kept its lease: claimed_at=%v lease_until=%v", recovered.ClaimedAt, recovered.LeaseUntil)
	}
	if !recovered.DeliverAfter.Equal(now.UTC()) {
		t.Fatalf("recovered DeliverAfter = %v, want %v", recovered.DeliverAfter, now.UTC())
	}

	for _, id := range []string{"nudge-pending-expired", "nudge-inflight-expired"} {
		item := byID(snapDead, id)
		if item.DeadAt.IsZero() {
			t.Fatalf("newly-dead item %q has zero DeadAt", id)
		}
		if item.LastError != "expired" {
			t.Fatalf("newly-dead item %q LastError = %q, want expired", id, item.LastError)
		}
	}
	if got := byID(snapDead, "nudge-dead-existing"); got.LastError != "boom" {
		t.Fatalf("existing dead item LastError = %q, want boom", got.LastError)
	}
	if got := byID(snapInFlight, "nudge-inflight-live"); got.LeaseUntil.IsZero() {
		t.Fatalf("live in-flight item lost its lease")
	}
}
