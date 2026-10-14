//go:build integration

package storebinding

// The batch claim on the native store, against a real embedded workspace.
//
// The queue's records are durable chore items in the history plane. That is a
// decision (DES-NQ D9.5), not an accident of policy: the native store refuses
// Claim on wisp-plane rows, so ClaimDue only works because the records are
// history-plane rows, and ClaimBatch claims by guarded update, which is the
// write a future move to the wisp plane would have to rely on everywhere. This
// test proves both on the store that would host the queue: the record ClaimBatch
// claims lands in the history plane, the guarded write takes and pins it, and
// the Claim a hook drain issues is accepted on the same record.
//
// It is tagged and skips the same way every other real-workspace proof in this
// tree does: the linked beads library chooses the backend from the workspace's
// own configuration, and the only honest fixture is a workspace it opened.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// openNativeNudgeQueueWorkspace provisions an embedded workspace that mints
// under the queue's reserved prefix and opens the native store over it.
func openNativeNudgeQueueWorkspace(t *testing.T) *beads.NativeDoltStore {
	t.Helper()
	root := t.TempDir()
	stateDir := filepath.Join(root, ".beads")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("creating %s: %v", stateDir, err)
	}
	// An empty configuration keeps every default, which is the embedded
	// backend this fixture wants.
	if err := os.WriteFile(filepath.Join(stateDir, "metadata.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("writing the workspace configuration: %v", err)
	}
	ctx := context.Background()
	storage, err := beads.OpenNativeStorage(ctx, root, nil)
	if err != nil {
		t.Skipf("the linked beads library cannot open a workspace here: %v", err)
	}
	if err := storage.SetConfig(ctx, "issue_prefix", config.NudgeQueueIDPrefix); err != nil {
		t.Fatalf("configuring the workspace id prefix: %v", err)
	}
	if err := storage.Close(); err != nil {
		t.Fatalf("closing the workspace fixture: %v", err)
	}
	store, err := beads.OpenNativeDoltStoreAt(ctx, root, nil)
	if err != nil {
		t.Fatalf("opening the native store over %s: %v", root, err)
	}
	t.Cleanup(func() {
		if err := store.CloseStore(); err != nil {
			t.Errorf("closing the native store: %v", err)
		}
	})
	return store
}

func TestBeadsNudgeQueueClaimBatchOnTheNativeStore(t *testing.T) {
	store := openNativeNudgeQueueWorkspace(t)
	queue := newNudgeQueue(t, store)
	now := time.Now().UTC()
	target := ClaimTarget{QueueKeys: []string{testNudgeAgent}}
	for _, id := range []string{"n-a", "n-b"} {
		if err := queue.Enqueue(testNudgeItem(id, "", now)); err != nil {
			t.Fatalf("Enqueue(%s): %v", id, err)
		}
	}

	// The plane: a queue record is a durable history-plane row.
	for _, id := range []string{"n-a", "n-b"} {
		record, err := store.Get(nudgeQueueBeadID(id))
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if record.Ephemeral || record.NoHistory {
			t.Fatalf("queue record %s Ephemeral=%v NoHistory=%v, want a history-plane row", id, record.Ephemeral, record.NoHistory)
		}
	}

	// The guarded write takes and pins the batch.
	claimed, err := queue.ClaimBatch(target, now)
	if err != nil {
		t.Fatalf("ClaimBatch: %v", err)
	}
	if len(claimed) != 2 || claimed[0].DeliveryBatch == "" || claimed[0].DeliveryBatch != claimed[1].DeliveryBatch {
		t.Fatalf("ClaimBatch = %+v, want both nudges under one key", claimed)
	}
	key := claimed[0].DeliveryBatch
	for _, item := range claimed {
		record, err := store.Get(nudgeQueueBeadID(item.ID))
		if err != nil {
			t.Fatalf("Get(%s): %v", item.ID, err)
		}
		lease, ok := parseNudgeLease(record.Assignee)
		if !ok || record.Status != "in_progress" {
			t.Errorf("claimed %s assignee=%q status=%q, want a lease held in progress", item.ID, record.Assignee, record.Status)
		}
		if ok && !lease.until.Equal(item.LeaseUntil) {
			t.Errorf("claimed %s lease until %v, want the returned %v", item.ID, lease.until, item.LeaseUntil)
		}
		if got := record.Metadata[nudgeMetaDeliveryBatch]; got != key {
			t.Errorf("claimed %s delivery_batch = %q, want %q", item.ID, got, key)
		}
	}

	// Released with backoff, the batch resumes whole under its key.
	notBefore := now.Add(time.Minute)
	if err := queue.ReleaseClaims([]string{"n-a", "n-b"}, notBefore); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}
	if early, err := queue.ClaimBatch(target, now); err != nil || len(early) != 0 {
		t.Fatalf("ClaimBatch before notBefore = %v, %v; want nothing", nudgeItemIDs(early), err)
	}
	resumed, err := queue.ClaimBatch(target, notBefore)
	if err != nil {
		t.Fatalf("resumed ClaimBatch: %v", err)
	}
	if len(resumed) != 2 || resumed[0].DeliveryBatch != key || resumed[1].DeliveryBatch != key {
		t.Fatalf("resumed ClaimBatch = %+v, want both nudges under %q", resumed, key)
	}

	// The native Claim refuses wisp-plane rows, so a hook drain's Claim on
	// the same records succeeding is the plane proved by the store itself.
	if err := queue.ReleaseClaims([]string{"n-a", "n-b"}, time.Time{}); err != nil {
		t.Fatalf("ReleaseClaims: %v", err)
	}
	drained, err := queue.ClaimDue(target, notBefore)
	if err != nil {
		t.Fatalf("ClaimDue on the native store: %v", err)
	}
	if len(drained) != 2 {
		t.Fatalf("ClaimDue = %v, want both nudges claimed by the native Claim", nudgeItemIDs(drained))
	}
}
