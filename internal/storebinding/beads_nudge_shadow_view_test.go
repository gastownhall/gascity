package storebinding

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// viewClaimingMemStore is beads.MemStore with the two-argument assignment
// claim the beads queue requires, built on MemStore's own revision fence.
type viewClaimingMemStore struct {
	*beads.MemStore
}

func (s viewClaimingMemStore) Claim(id, assignee string) (beads.Bead, bool, error) {
	for {
		current, err := s.Get(id)
		if err != nil {
			return beads.Bead{}, false, err
		}
		if current.Status != "open" && current.Status != "in_progress" {
			return beads.Bead{}, false, nil
		}
		if current.Assignee != "" && current.Assignee != assignee {
			return beads.Bead{}, false, nil
		}
		inProgress := "in_progress"
		err = s.UpdateIfMatch(id, current.Revision, beads.UpdateOpts{Assignee: &assignee, Status: &inProgress})
		var stale *beads.PreconditionFailedError
		if errors.As(err, &stale) {
			continue
		}
		if err != nil {
			return beads.Bead{}, false, err
		}
		held, err := s.Get(id)
		return held, err == nil, err
	}
}

// shadowViewStores are the stores every view case runs over: MemStore and
// the SQLite engine.
var shadowViewStores = []struct {
	name string
	open func(t *testing.T) beads.Store
}{
	{"memstore", func(*testing.T) beads.Store {
		memory := beads.NewMemStore()
		memory.HonorExplicitIDs = true
		return viewClaimingMemStore{MemStore: memory}
	}},
	{"sqlite", func(t *testing.T) beads.Store { return openNudgeQueueStore(t, t.TempDir()) }},
}

type shadowViewFixture struct {
	store beads.Store
	fd    NudgeFrontDoors
}

func runShadowViewCase(t *testing.T, name string, fn func(t *testing.T, f shadowViewFixture)) {
	t.Helper()
	for _, backend := range shadowViewStores {
		t.Run(name+"/"+backend.name, func(t *testing.T) {
			store := backend.open(t)
			fd, err := NewBeadsNudgeFrontDoors(beads.NudgesStore{Store: store})
			if err != nil {
				t.Fatalf("NewBeadsNudgeFrontDoors: %v", err)
			}
			fn(t, shadowViewFixture{store: store, fd: fd})
		})
	}
}

func mustFindTerminal(t *testing.T, shadows NudgeShadows, id string) nudgequeue.NudgeShadow {
	t.Helper()
	shadow, ok, err := shadows.FindIncludingTerminal(id)
	if err != nil {
		t.Fatalf("FindIncludingTerminal(%s): %v", id, err)
	}
	if !ok {
		t.Fatalf("FindIncludingTerminal(%s) found nothing", id)
	}
	return shadow
}

func assertNotOpenFind(t *testing.T, shadows NudgeShadows, id string) {
	t.Helper()
	if shadow, ok, err := shadows.Find(id); err != nil || ok {
		t.Fatalf("Find(%s) = %+v, %v, %v; want a retired record not found", id, shadow, ok, err)
	}
}

func TestNudgeShadowViewRefusesAStoreWithoutTheClaimCAS(t *testing.T) {
	if _, err := NewBeadsNudgeFrontDoors(beads.NudgesStore{Store: beads.NewMemStore()}); !errors.Is(err, ErrBeadsAdapterCapability) {
		t.Fatalf("NewBeadsNudgeFrontDoors over a store without Claim = %v, want ErrBeadsAdapterCapability", err)
	}
}

func TestNudgeShadowViewProjectsALiveRecordAsQueued(t *testing.T) {
	runShadowViewCase(t, "pending-and-in-flight", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		pending := testNudgeItem("n-pending", "", now)
		pending.Reference = &nudgequeue.Reference{Kind: "bead", ID: "gc-1"}
		claimed := testNudgeItem("n-claimed", "", now)
		claimed.Agent = "beta/other"
		for _, item := range []nudgequeue.Item{pending, claimed} {
			if err := f.fd.Queue.Enqueue(item); err != nil {
				t.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		if got, err := f.fd.Queue.ClaimDue(ClaimTarget{QueueKeys: []string{"beta/other"}}, now); err != nil || len(got) != 1 {
			t.Fatalf("ClaimDue = %v, %v; want the one item claimed", nudgeItemIDs(got), err)
		}
		for _, item := range []nudgequeue.Item{pending, claimed} {
			shadow, ok, err := f.fd.Shadows.Find(item.ID)
			if err != nil || !ok {
				t.Fatalf("Find(%s) = %v, %v; want the live record found", item.ID, ok, err)
			}
			if !shadow.Open || shadow.State != "queued" {
				t.Errorf("%s projected Open=%v State=%q, want open and queued", item.ID, shadow.Open, shadow.State)
			}
			if shadow.TerminalReason != "" || shadow.CommitBoundary != "" || !shadow.TerminalAt.IsZero() {
				t.Errorf("%s carries terminal fields %+v while live", item.ID, shadow)
			}
			if shadow.ID != item.ID || shadow.BeadID != nudgeQueueBeadID(item.ID) || shadow.Agent != item.Agent ||
				shadow.Source != item.Source || shadow.Message != item.Message ||
				!shadow.CreatedAt.Equal(item.CreatedAt) || !shadow.DeliverAfter.Equal(item.DeliverAfter) || !shadow.ExpiresAt.Equal(item.ExpiresAt) {
				t.Errorf("%s projected identity %+v, want the item's", item.ID, shadow)
			}
			if again := mustFindTerminal(t, f.fd.Shadows, item.ID); again.State != "queued" || !again.Open {
				t.Errorf("FindIncludingTerminal(%s) = %+v, want the same live projection", item.ID, again)
			}
		}
		if shadow, _, _ := f.fd.Shadows.Find(pending.ID); shadow.Reference == nil || *shadow.Reference != *pending.Reference {
			t.Errorf("reference = %+v, want %+v", shadow.Reference, pending.Reference)
		}
	})
}

func TestNudgeShadowViewProjectsEveryAckOutcome(t *testing.T) {
	outcomes := []string{"injected", "accepted_for_injection", "injected_unobserved", "failed", "expired", "superseded"}
	runShadowViewCase(t, "ack", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		for _, outcome := range outcomes {
			item := testNudgeItem("n-"+outcome, "", now)
			if err := f.fd.Queue.Enqueue(item); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			before := time.Now().UTC().Add(-time.Second)
			if err := f.fd.Queue.Ack([]string{item.ID}, outcome, "reason-"+outcome, "boundary-"+outcome); err != nil {
				t.Fatalf("Ack(%s): %v", outcome, err)
			}
			assertNotOpenFind(t, f.fd.Shadows, item.ID)
			shadow := mustFindTerminal(t, f.fd.Shadows, item.ID)
			if shadow.Open || shadow.State != outcome || shadow.TerminalReason != "reason-"+outcome || shadow.CommitBoundary != "boundary-"+outcome {
				t.Errorf("Ack %s projected %+v", outcome, shadow)
			}
			if shadow.TerminalAt.Before(before) {
				t.Errorf("Ack %s TerminalAt = %s, want the ack time", outcome, shadow.TerminalAt)
			}
		}
	})
}

// The view test of NQ-F1: a wait finalizes on injected_unobserved, which the
// shared classifier reads as delivered.
func TestNudgeShadowViewDeliveredOutcomesClassifyAsDelivered(t *testing.T) {
	runShadowViewCase(t, "delivered", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		for _, outcome := range []string{"injected", "accepted_for_injection", "injected_unobserved"} {
			item := testNudgeItem("n-d-"+outcome, "", now)
			if err := f.fd.Queue.Enqueue(item); err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if err := f.fd.Queue.Ack([]string{item.ID}, outcome, "", ""); err != nil {
				t.Fatalf("Ack: %v", err)
			}
			if got := nudgequeue.ClassifyTerminalState(mustFindTerminal(t, f.fd.Shadows, item.ID).State); got != nudgequeue.OutcomeDelivered {
				t.Errorf("%s classifies as %s, want delivered", outcome, got)
			}
		}
	})
}

func TestNudgeShadowViewProjectsDeadByAttempts(t *testing.T) {
	runShadowViewCase(t, "attempts", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		item := testNudgeItem("n-attempts", "", now)
		if err := f.fd.Queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		cause := errors.New("pane gone")
		var dead []nudgequeue.Item
		for attempt := 0; attempt < nudgeQueueMaxAttempts; attempt++ {
			var err error
			if dead, err = f.fd.Queue.RecordFailure([]string{item.ID}, cause, now); err != nil {
				t.Fatalf("RecordFailure: %v", err)
			}
		}
		if len(dead) != 1 {
			t.Fatalf("dead = %v, want the item dead-lettered by attempts", nudgeItemIDs(dead))
		}
		assertNotOpenFind(t, f.fd.Shadows, item.ID)
		shadow := mustFindTerminal(t, f.fd.Shadows, item.ID)
		if shadow.Open || shadow.State != "failed" || shadow.TerminalReason != "pane gone" || !shadow.TerminalAt.Equal(now) {
			t.Errorf("dead by attempts projected %+v, want failed with the stamped reason and time", shadow)
		}

		// Aging the dead letter into a retired record keeps its verdict.
		if _, err := f.fd.Queue.Maintain(context.Background(), now.Add(nudgeQueueDeadRetention+time.Minute)); err != nil {
			t.Fatalf("Maintain: %v", err)
		}
		if aged := mustFindTerminal(t, f.fd.Shadows, item.ID); aged.Open || aged.State != "failed" || aged.TerminalReason != "pane gone" || !aged.TerminalAt.Equal(now) {
			t.Errorf("retired dead letter projected %+v, want the dead verdict kept", aged)
		}
	})
}

func TestNudgeShadowViewProjectsExpiry(t *testing.T) {
	runShadowViewCase(t, "expiry", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		item := testNudgeItem("n-expired", "", now.Add(-time.Hour))
		item.ExpiresAt = now.Add(-time.Minute)
		if err := f.fd.Queue.EnqueueDeferred(item); err != nil {
			t.Fatalf("EnqueueDeferred: %v", err)
		}
		want := func(stage string) {
			t.Helper()
			assertNotOpenFind(t, f.fd.Shadows, item.ID)
			shadow := mustFindTerminal(t, f.fd.Shadows, item.ID)
			if shadow.Open || shadow.State != "expired" || shadow.TerminalReason != "expired" || !shadow.TerminalAt.Equal(item.ExpiresAt) {
				t.Errorf("%s: expired item projected %+v, want expired at its deadline", stage, shadow)
			}
		}
		want("by arithmetic")
		if _, err := f.fd.Queue.Maintain(context.Background(), now); err != nil {
			t.Fatalf("Maintain: %v", err)
		}
		want("recorded by Maintain")
	})
}

func TestNudgeShadowViewProjectsSupersession(t *testing.T) {
	runShadowViewCase(t, "supersession", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		ref := &nudgequeue.Reference{Kind: "bead", ID: "gc-7"}
		older := testNudgeItem("n-older", "", now)
		older.Reference = ref
		newer := testNudgeItem("n-newer", "", now)
		newer.Reference = ref
		for _, item := range []nudgequeue.Item{older, newer} {
			if err := f.fd.Queue.Enqueue(item); err != nil {
				t.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		assertNotOpenFind(t, f.fd.Shadows, older.ID)
		shadow := mustFindTerminal(t, f.fd.Shadows, older.ID)
		if shadow.Open || shadow.State != "superseded" || shadow.TerminalReason != "superseded" || shadow.TerminalAt.IsZero() {
			t.Errorf("superseded item projected %+v", shadow)
		}
		if _, ok, err := f.fd.Shadows.Find(newer.ID); err != nil || !ok {
			t.Errorf("Find(newer) = %v, %v; want the superseding item live", ok, err)
		}
	})
}

func TestNudgeShadowViewProjectsWithdrawal(t *testing.T) {
	runShadowViewCase(t, "withdrawal", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		item := testNudgeItem("n-wait", "", now)
		if err := f.fd.Queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := f.fd.Queue.WithdrawQueuedWaitNudges([]string{item.ID}); err != nil {
			t.Fatalf("WithdrawQueuedWaitNudges: %v", err)
		}
		assertNotOpenFind(t, f.fd.Shadows, item.ID)
		shadow := mustFindTerminal(t, f.fd.Shadows, item.ID)
		if shadow.Open || shadow.State != "failed" || shadow.TerminalReason != "wait-canceled" || shadow.CommitBoundary != "delivery-withdrawn" {
			t.Errorf("withdrawn item projected %+v, want the file queue's withdrawal vocabulary", shadow)
		}
	})
}

func TestNudgeShadowViewProjectsRollback(t *testing.T) {
	runShadowViewCase(t, "rollback", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		landed := testNudgeItem("n-landed", "", now)
		if err := f.fd.Queue.Enqueue(landed); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		for _, item := range []nudgequeue.Item{landed, testNudgeItem("n-never", "", now)} {
			if err := f.fd.Queue.Rollback(item, "sling failed"); err != nil {
				t.Fatalf("Rollback(%s): %v", item.ID, err)
			}
			assertNotOpenFind(t, f.fd.Shadows, item.ID)
			if shadow := mustFindTerminal(t, f.fd.Shadows, item.ID); shadow.Open || shadow.State != "failed" || shadow.TerminalReason != "sling failed" {
				t.Errorf("rolled back %s projected %+v", item.ID, shadow)
			}
		}
	})
}

func TestNudgeShadowViewFallsBackToLegacyShadows(t *testing.T) {
	runShadowViewCase(t, "legacy", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		legacy := nudgequeue.NewStore(beads.NudgesStore{Store: f.store})
		open := testNudgeItem("n-legacy-open", "", now)
		closed := testNudgeItem("n-legacy-closed", "", now)
		for _, item := range []nudgequeue.Item{open, closed} {
			if _, created, err := legacy.Save(item); err != nil || !created {
				t.Fatalf("legacy Save(%s) = %v, %v", item.ID, created, err)
			}
		}
		if err := legacy.Terminalize(closed, "injected", "delivered", "provider", now); err != nil {
			t.Fatalf("legacy Terminalize: %v", err)
		}

		if shadow, ok, err := f.fd.Shadows.Find(open.ID); err != nil || !ok || !shadow.Open || shadow.State != "queued" {
			t.Errorf("Find(open legacy) = %+v, %v, %v; want the open shadow", shadow, ok, err)
		}
		if shadow, ok, err := f.fd.Shadows.Find(closed.ID); err != nil || ok {
			t.Errorf("Find(closed legacy) = %+v, %v, %v; want open shadows only", shadow, ok, err)
		}
		if shadow := mustFindTerminal(t, f.fd.Shadows, closed.ID); shadow.Open || shadow.State != "injected" || shadow.TerminalReason != "delivered" {
			t.Errorf("FindIncludingTerminal(closed legacy) = %+v", shadow)
		}
		if _, ok, err := f.fd.Shadows.Find("n-unknown"); err != nil || ok {
			t.Errorf("Find(unknown) = %v, %v; want not found", ok, err)
		}
		if _, ok, err := f.fd.Shadows.FindIncludingTerminal("n-unknown"); err != nil || ok {
			t.Errorf("FindIncludingTerminal(unknown) = %v, %v; want not found", ok, err)
		}

		// A queue record answers for its id even beside an open legacy
		// shadow: the record is the observable record in beads mode.
		if err := f.fd.Queue.Enqueue(open); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := f.fd.Queue.Ack([]string{open.ID}, "injected", "", ""); err != nil {
			t.Fatalf("Ack: %v", err)
		}
		assertNotOpenFind(t, f.fd.Shadows, open.ID)
		if shadow := mustFindTerminal(t, f.fd.Shadows, open.ID); shadow.State != "injected" || shadow.BeadID != nudgeQueueBeadID(open.ID) {
			t.Errorf("FindIncludingTerminal with a record and a legacy shadow = %+v, want the record", shadow)
		}

		// The stale-shadow sweep sees legacy shadows only.
		stale, err := f.fd.Shadows.StaleShadowsBefore(now.Add(time.Hour), 0, nil)
		if err != nil {
			t.Fatalf("StaleShadowsBefore: %v", err)
		}
		for _, shadow := range stale {
			if shadow.BeadID == nudgeQueueBeadID(shadow.ID) {
				t.Errorf("StaleShadowsBefore returned queue record %s", shadow.BeadID)
			}
		}
		if len(stale) != 1 || stale[0].ID != open.ID {
			t.Errorf("StaleShadowsBefore = %+v, want only the open legacy shadow", stale)
		}
		openShadow, _, _ := legacy.Find(open.ID)
		if err := f.fd.Shadows.SweepStale(openShadow.BeadID, "nudge swept by the retention window", now); err != nil {
			t.Fatalf("SweepStale: %v", err)
		}
		if _, ok, _ := legacy.Find(open.ID); ok {
			t.Error("SweepStale left the legacy shadow open")
		}
	})
}

// A record the queue-mode shim retired migrated is no record to the view: the
// nudge moved into the file queue, whose gc:nudge shadow answers for it from
// then on, open while it is queued and terminal once it is delivered. With no
// legacy shadow either, the nudge is not found.
func TestNudgeShadowViewReadsAMigratedRecordAsNoRecord(t *testing.T) {
	runShadowViewCase(t, "migrated", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		legacy := nudgequeue.NewStore(beads.NudgesStore{Store: f.store})
		moved := testNudgeItem("n-moved", "", now)
		bare := testNudgeItem("n-bare", "", now)
		for _, item := range []nudgequeue.Item{moved, bare} {
			if err := f.fd.Queue.Enqueue(item); err != nil {
				t.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		if _, created, err := legacy.Save(moved); err != nil || !created {
			t.Fatalf("legacy Save = %v, %v", created, err)
		}
		if err := f.fd.Queue.Ack([]string{moved.ID, bare.ID}, NudgeQueueMigratedOutcome, "imported into the file queue", "queue-mode-switch"); err != nil {
			t.Fatalf("Ack migrated: %v", err)
		}

		shadow, ok, err := f.fd.Shadows.Find(moved.ID)
		if err != nil || !ok || !shadow.Open || shadow.State != "queued" || shadow.BeadID == nudgeQueueBeadID(moved.ID) {
			t.Fatalf("Find(moved) = %+v, %v, %v; want the open legacy shadow", shadow, ok, err)
		}
		if again := mustFindTerminal(t, f.fd.Shadows, moved.ID); again.BeadID != shadow.BeadID || again.State != "queued" {
			t.Fatalf("FindIncludingTerminal(moved) = %+v, want the legacy shadow %s", again, shadow.BeadID)
		}
		if err := legacy.Terminalize(moved, "injected", "delivered", "provider", now); err != nil {
			t.Fatalf("legacy Terminalize: %v", err)
		}
		assertNotOpenFind(t, f.fd.Shadows, moved.ID)
		if delivered := mustFindTerminal(t, f.fd.Shadows, moved.ID); delivered.BeadID != shadow.BeadID || delivered.State != "injected" {
			t.Fatalf("FindIncludingTerminal(moved) after the delivery = %+v, want the delivered legacy shadow %s", delivered, shadow.BeadID)
		}

		if got, ok, err := f.fd.Shadows.Find(bare.ID); err != nil || ok {
			t.Errorf("Find(bare) = %+v, %v, %v; want no record and no shadow", got, ok, err)
		}
		if got, ok, err := f.fd.Shadows.FindIncludingTerminal(bare.ID); err != nil || ok {
			t.Errorf("FindIncludingTerminal(bare) = %+v, %v, %v; want no record and no shadow", got, ok, err)
		}
	})
}

func TestNudgeShadowViewShadowHistorySince(t *testing.T) {
	runShadowViewCase(t, "history", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		for _, id := range []string{"n-live", "n-dead", "n-acked"} {
			if err := f.fd.Queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
				t.Fatalf("Enqueue(%s): %v", id, err)
			}
		}
		if _, err := f.fd.Queue.RecordFailure([]string{"n-dead"}, ErrNudgeSessionFenceMismatch, now); err != nil {
			t.Fatalf("RecordFailure: %v", err)
		}
		if err := f.fd.Queue.Ack([]string{"n-acked"}, "injected", "", ""); err != nil {
			t.Fatalf("Ack: %v", err)
		}
		if _, _, err := nudgequeue.NewStore(beads.NudgesStore{Store: f.store}).Save(testNudgeItem("n-legacy", "", now)); err != nil {
			t.Fatalf("legacy Save: %v", err)
		}

		ids := func(since time.Time) map[string]bool {
			t.Helper()
			history, err := f.fd.Shadows.ShadowHistorySince(since)
			if err != nil {
				t.Fatalf("ShadowHistorySince: %v", err)
			}
			got := map[string]bool{}
			for _, shadow := range history {
				got[shadow.ID] = true
			}
			return got
		}
		recent := ids(now.Add(-time.Minute))
		for _, id := range []string{"n-live", "n-dead", "n-acked", "n-legacy"} {
			if !recent[id] {
				t.Errorf("history since a minute ago lacks %s: %v", id, recent)
			}
		}
		later := ids(now.Add(time.Hour))
		if !later["n-live"] || !later["n-dead"] {
			t.Errorf("history since a later time = %v, want live and dead records kept whatever the cutoff", later)
		}
		if later["n-acked"] || later["n-legacy"] {
			t.Errorf("history since a later time = %v, want records retired before it left out", later)
		}
	})
}

// ShadowHistorySince lists each nudge once, from where Find answers for it: a
// nudge with both a queue record and a legacy shadow from the record, and one
// whose record was retired migrated from its legacy shadow.
func TestNudgeShadowViewShadowHistorySinceListsEachNudgeOnce(t *testing.T) {
	runShadowViewCase(t, "history-once", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		legacy := nudgequeue.NewStore(beads.NudgesStore{Store: f.store})
		toBeads := testNudgeItem("n-to-beads", "", now)
		toFile := testNudgeItem("n-to-file", "", now)
		for _, item := range []nudgequeue.Item{toBeads, toFile} {
			if _, _, err := legacy.Save(item); err != nil {
				t.Fatalf("legacy Save(%s): %v", item.ID, err)
			}
			if err := f.fd.Queue.Enqueue(item); err != nil {
				t.Fatalf("Enqueue(%s): %v", item.ID, err)
			}
		}
		if err := f.fd.Queue.Ack([]string{toFile.ID}, NudgeQueueMigratedOutcome, "imported into the file queue", "queue-mode-switch"); err != nil {
			t.Fatalf("Ack migrated: %v", err)
		}

		history, err := f.fd.Shadows.ShadowHistorySince(now.Add(-time.Minute))
		if err != nil {
			t.Fatalf("ShadowHistorySince: %v", err)
		}
		rows := map[string][]nudgequeue.NudgeShadow{}
		for _, shadow := range history {
			rows[shadow.ID] = append(rows[shadow.ID], shadow)
		}
		if got := rows[toBeads.ID]; len(got) != 1 || got[0].BeadID != nudgeQueueBeadID(toBeads.ID) {
			t.Errorf("history rows of %s = %+v, want the queue record only", toBeads.ID, got)
		}
		if got := rows[toFile.ID]; len(got) != 1 || got[0].BeadID == nudgeQueueBeadID(toFile.ID) || got[0].State != "queued" {
			t.Errorf("history rows of %s = %+v, want its open legacy shadow only", toFile.ID, got)
		}
	})
}

func TestNudgeShadowViewWritersAreReadOnly(t *testing.T) {
	runShadowViewCase(t, "read-only", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		item := testNudgeItem("n-ro", "", now)
		if err := f.fd.Queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := f.fd.Shadows.Terminalize(item, "injected", "", "", now); !errors.Is(err, ErrNudgeShadowViewReadOnly) {
			t.Errorf("Terminalize = %v, want ErrNudgeShadowViewReadOnly", err)
		}
		if err := f.fd.Shadows.RollbackEnqueue(nudgeQueueBeadID(item.ID)); !errors.Is(err, ErrNudgeShadowViewReadOnly) {
			t.Errorf("RollbackEnqueue = %v, want ErrNudgeShadowViewReadOnly", err)
		}
		if shadow, ok, err := f.fd.Shadows.Find(item.ID); err != nil || !ok || !shadow.Open {
			t.Errorf("after refused writes Find = %+v, %v, %v; want the record untouched", shadow, ok, err)
		}
	})
}

func TestNudgeShadowViewSaveConfirmsAnExistingRecord(t *testing.T) {
	runShadowViewCase(t, "save", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		item := testNudgeItem("n-save", "", now)
		if _, _, err := f.fd.Shadows.Save(item); err == nil {
			t.Fatal("Save of an item with no record succeeded, want an error naming enqueue first")
		} else if want := "enqueue first"; !strings.Contains(err.Error(), want) {
			t.Fatalf("Save error = %q, want it to name %q", err, want)
		}
		if _, _, err := f.fd.Shadows.Save(nudgequeue.Item{}); err == nil {
			t.Fatal("Save of an empty id succeeded")
		}
		if err := f.fd.Queue.Enqueue(item); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		for _, stage := range []string{"live", "retired"} {
			beadID, created, err := f.fd.Shadows.Save(item)
			if err != nil || created || beadID != nudgeQueueBeadID(item.ID) {
				t.Fatalf("%s: Save = %q, %v, %v; want the record's bead id and created=false", stage, beadID, created, err)
			}
			if err := f.fd.Queue.Ack([]string{item.ID}, "injected", "", ""); err != nil {
				t.Fatalf("Ack: %v", err)
			}
		}
		if records := queueRecordIDs(t, f.store); len(records) != 1 {
			t.Fatalf("queue records = %v, want Save to create nothing", records)
		}
	})
}

func TestIsNudgeQueueRecord(t *testing.T) {
	runShadowViewCase(t, "predicate", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		if err := f.fd.Queue.Enqueue(testNudgeItem("n-rec", "", now)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		record, err := f.store.Get(nudgeQueueBeadID("n-rec"))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !IsNudgeQueueRecord(record) {
			t.Errorf("IsNudgeQueueRecord(%+v) = false, want true", record)
		}
		shadowID, _, err := nudgequeue.NewStore(beads.NudgesStore{Store: f.store}).Save(testNudgeItem("n-shadow", "", now))
		if err != nil {
			t.Fatalf("legacy Save: %v", err)
		}
		shadow, err := f.store.Get(shadowID)
		if err != nil {
			t.Fatalf("Get shadow: %v", err)
		}
		if IsNudgeQueueRecord(shadow) {
			t.Error("IsNudgeQueueRecord(gc:nudge shadow) = true, want false")
		}
	})
	for _, b := range []beads.Bead{
		{Type: "task", Labels: []string{nudgeQueueLabel}},
		{Type: nudgeQueueBeadType, Labels: []string{"gc:nudge"}},
		{Type: nudgeQueueBeadType},
	} {
		if IsNudgeQueueRecord(b) {
			t.Errorf("IsNudgeQueueRecord(%+v) = true, want false", b)
		}
	}
}

// A queue record that does not decode is reported, never answered from the
// legacy shadow: the record exists, so a legacy answer would be wrong. The
// error wraps ErrBeadsNudgeQueueCorrupt so a caller that looks up many nudges
// can tell it apart and skip only the one lookup it fails.
func TestNudgeShadowViewReportsACorruptRecordAsCorrupt(t *testing.T) {
	runShadowViewCase(t, "corrupt", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		seedCorruptNudgeRecord(t, f.store, "n-corrupt", testNudgeAgent)
		// A legacy shadow under the same id must not answer for the record.
		if _, _, err := nudgequeue.NewStore(beads.NudgesStore{Store: f.store}).Save(testNudgeItem("n-corrupt", "", now)); err != nil {
			t.Fatalf("legacy Save: %v", err)
		}
		if _, ok, err := f.fd.Shadows.Find("n-corrupt"); ok || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
			t.Fatalf("Find(corrupt) = %v, %v; want an error wrapping ErrBeadsNudgeQueueCorrupt", ok, err)
		}
		if _, ok, err := f.fd.Shadows.FindIncludingTerminal("n-corrupt"); ok || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
			t.Fatalf("FindIncludingTerminal(corrupt) = %v, %v; want an error wrapping ErrBeadsNudgeQueueCorrupt", ok, err)
		}
	})
}

// A retired record whose stamped terminal_at does not parse is corrupt to the
// view's projection, the one reader of terminal_at. Find and
// FindIncludingTerminal report it wrapping ErrBeadsNudgeQueueCorrupt, so a
// caller that looks up many nudges can skip only that one.
func TestNudgeShadowViewReportsAnUnparsableTerminalTimeAsCorrupt(t *testing.T) {
	runShadowViewCase(t, "corrupt-terminal-at", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		if err := f.fd.Queue.Enqueue(testNudgeItem("n-retired", "", now)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := f.fd.Queue.Ack([]string{"n-retired"}, "injected", "", ""); err != nil {
			t.Fatalf("Ack: %v", err)
		}
		if err := f.store.SetMetadata(nudgeQueueBeadID("n-retired"), nudgeMetaTerminalAt, "not-a-time"); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
		if _, ok, err := f.fd.Shadows.FindIncludingTerminal("n-retired"); ok || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
			t.Fatalf("FindIncludingTerminal = %v, %v; want an error wrapping ErrBeadsNudgeQueueCorrupt", ok, err)
		}
		if _, ok, err := f.fd.Shadows.Find("n-retired"); ok || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
			t.Fatalf("Find = %v, %v; want an error wrapping ErrBeadsNudgeQueueCorrupt", ok, err)
		}
	})
}

// Every queue transition that retires or dead-letters a record stamps
// terminal_at, so a retired or dead record with neither terminal_at nor dead_at
// is corrupt to the view's projection, not a record retired at the zero time
// that ShadowHistorySince would drop as older than any cutoff. Find,
// FindIncludingTerminal and ShadowHistorySince all report it wrapping
// ErrBeadsNudgeQueueCorrupt, and its legacy shadow does not answer in its
// place.
func TestNudgeShadowViewReportsAMissingTerminalTimeAsCorrupt(t *testing.T) {
	runShadowViewCase(t, "missing-terminal-at", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		for _, id := range []string{"n-retired", "n-dead"} {
			if err := f.fd.Queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
				t.Fatalf("Enqueue(%s): %v", id, err)
			}
			if _, _, err := nudgequeue.NewStore(beads.NudgesStore{Store: f.store}).Save(testNudgeItem(id, "", now)); err != nil {
				t.Fatalf("legacy Save(%s): %v", id, err)
			}
		}
		if err := f.fd.Queue.Ack([]string{"n-retired"}, "injected", "", ""); err != nil {
			t.Fatalf("Ack: %v", err)
		}
		if dead, err := f.fd.Queue.RecordFailure([]string{"n-dead"}, ErrNudgeSessionFenceMismatch, now); err != nil || len(dead) != 1 {
			t.Fatalf("RecordFailure = %d dead, %v; want n-dead dead-lettered", len(dead), err)
		}
		for _, id := range []string{"n-retired", "n-dead"} {
			for _, key := range []string{nudgeMetaTerminalAt, nudgeMetaDeadAt} {
				if err := f.store.SetMetadata(nudgeQueueBeadID(id), key, ""); err != nil {
					t.Fatalf("SetMetadata(%s, %s): %v", id, key, err)
				}
			}
			if _, ok, err := f.fd.Shadows.FindIncludingTerminal(id); ok || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
				t.Fatalf("FindIncludingTerminal(%s) = %v, %v; want an error wrapping ErrBeadsNudgeQueueCorrupt", id, ok, err)
			}
			if _, ok, err := f.fd.Shadows.Find(id); ok || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
				t.Fatalf("Find(%s) = %v, %v; want an error wrapping ErrBeadsNudgeQueueCorrupt", id, ok, err)
			}
		}
		history, err := f.fd.Shadows.ShadowHistorySince(now.Add(-time.Minute))
		if !beads.IsPartialResult(err) || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
			t.Fatalf("ShadowHistorySince error = %v, want a partial result wrapping ErrBeadsNudgeQueueCorrupt", err)
		}
		if len(history) != 0 {
			t.Fatalf("ShadowHistorySince = %+v, want neither corrupt record nor its legacy shadow", history)
		}
	})
}

// ShadowHistorySince skips a record it cannot decode or project and returns
// the rest beside a partial result naming it. Such a record still answers for
// its id, so a legacy shadow of that id is not listed in its place.
func TestNudgeShadowViewShadowHistorySinceReportsACorruptRecordAsPartial(t *testing.T) {
	runShadowViewCase(t, "corrupt-history", func(t *testing.T, f shadowViewFixture) {
		now := time.Now().UTC()
		if err := f.fd.Queue.Enqueue(testNudgeItem("n-good", "", now)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		seedCorruptNudgeRecord(t, f.store, "n-corrupt", testNudgeAgent)
		if err := f.fd.Queue.Enqueue(testNudgeItem("n-retired", "", now)); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if err := f.fd.Queue.Ack([]string{"n-retired"}, "injected", "", ""); err != nil {
			t.Fatalf("Ack: %v", err)
		}
		if err := f.store.SetMetadata(nudgeQueueBeadID("n-retired"), nudgeMetaTerminalAt, "not-a-time"); err != nil {
			t.Fatalf("SetMetadata: %v", err)
		}
		legacy := nudgequeue.NewStore(beads.NudgesStore{Store: f.store})
		for _, id := range []string{"n-corrupt", "n-retired"} {
			if _, _, err := legacy.Save(testNudgeItem(id, "", now)); err != nil {
				t.Fatalf("legacy Save(%s): %v", id, err)
			}
		}
		history, err := f.fd.Shadows.ShadowHistorySince(now.Add(-time.Minute))
		if !beads.IsPartialResult(err) || !errors.Is(err, ErrBeadsNudgeQueueCorrupt) {
			t.Fatalf("ShadowHistorySince error = %v, want a partial result wrapping ErrBeadsNudgeQueueCorrupt", err)
		}
		if len(history) != 1 || history[0].ID != "n-good" {
			t.Fatalf("ShadowHistorySince = %+v, want the good record kept", history)
		}
	})
}

// ShadowHistorySince reads the queue family by lifecycle state, never by the
// family label alone (DES-NQ D9.3, invariant 7): live and dead records open,
// retired ones closed too.
func TestNudgeShadowViewShadowHistorySinceReadsAreIndexed(t *testing.T) {
	_, recorder, backing := newRecordedNudgeQueue(t)
	fd, err := NewBeadsNudgeFrontDoors(beads.NudgesStore{Store: recorder})
	if err != nil {
		t.Fatalf("NewBeadsNudgeFrontDoors: %v", err)
	}
	now := time.Now().UTC()
	seedUnrelatedNudges(t, backing, 3, now)
	recorder.reset()
	if _, err := fd.Shadows.ShadowHistorySince(now.Add(-time.Minute)); err != nil {
		t.Fatalf("ShadowHistorySince: %v", err)
	}
	var queueLists []beads.ListQuery
	for _, op := range recorder.Ops() {
		if op.Kind == beadstest.OpList && op.Query.Label == nudgeQueueLabel {
			queueLists = append(queueLists, op.Query)
		}
	}
	states := map[string]bool{}
	for _, query := range queueLists {
		state := query.Metadata[nudgeMetaQueueState]
		if state == "" {
			t.Errorf("ShadowHistorySince listed %+v; want a read indexed on queue_state, never the family label alone", query)
			continue
		}
		if !query.Live {
			t.Errorf("ShadowHistorySince listed %+v without Live", query)
		}
		if query.IncludeClosed != (state == nudgeQueueStateTerminal) {
			t.Errorf("ShadowHistorySince listed %+v; want only the terminal read to include closed records", query)
		}
		states[state] = true
	}
	for _, state := range []string{nudgeQueueStateLive, nudgeQueueStateDead, nudgeQueueStateTerminal} {
		if !states[state] {
			t.Errorf("ShadowHistorySince read states %v, want %s read", states, state)
		}
	}
}
