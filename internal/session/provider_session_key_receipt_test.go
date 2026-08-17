package session

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func receiptTestSession(t *testing.T, store beads.Store) Info {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title: "session", Type: BeadType, Status: "open",
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "receipt-worker-1", "template": "worker",
			"state": "active", "generation": "3", "instance_token": "tok-3", "continuation_epoch": "1",
			"provider": "codex",
		},
	})
	if err != nil {
		t.Fatalf("seeding session: %v", err)
	}
	raw, err := beads.HandlesFor(store).Live.Get(b.ID)
	if err != nil {
		t.Fatalf("reading back session: %v", err)
	}
	return InfoFromPersistedBead(raw)
}

func TestProviderSessionKeyReceiptConsumeInstallsAndClearsFence(t *testing.T) {
	store := beads.NewMemStore()
	info := receiptTestSession(t, store)

	issued, env, err := IssueProviderSessionKeyReceipt(store, info, time.Now().UTC())
	if err != nil {
		t.Fatalf("issuing receipt: %v", err)
	}
	if env == nil || env[ProviderSessionKeyReceiptTokenEnv] == "" {
		t.Fatal("receipt environment missing the launch token")
	}
	if _, err := PublishProviderSessionKeyReceipt(store, ProviderSessionKeyReceiptInput{
		SessionID:          issued.ID,
		Generation:         issued.Generation,
		ContinuationEpoch:  issued.ContinuationEpoch,
		InstanceToken:      issued.InstanceToken,
		ProviderFamily:     "codex",
		ReceiptToken:       issued.ProviderSessionKeyReceiptToken,
		LaunchAuthority:    issued.ProviderSessionKeyReceiptAuthority,
		ProviderSessionKey: "provider-key-1",
	}); err != nil {
		t.Fatalf("publishing receipt: %v", err)
	}

	// While the consume authority is live, the deterministic receipt fence is
	// adoptable and idempotent.
	fence, fenced, err := acquireProviderSessionKeyReceiptFence(store, issued.ID)
	if err != nil || fence == nil {
		t.Fatalf("receipt fence acquire: %v", err)
	}
	if fenced.ProviderSessionKeyReceiptToken == "" {
		t.Fatal("fenced info lost the pending token")
	}
	again, _, err := acquireProviderSessionKeyReceiptFence(store, issued.ID)
	if err != nil || again == nil || again.Value() != fence.Value() {
		t.Fatalf("receipt fence re-adopt: err=%v same=%v", err, again != nil && again.Value() == fence.Value())
	}
	if err := fence.Release(); err != nil {
		t.Fatalf("fence release: %v", err)
	}

	consumed, applied, err := ConsumeProviderSessionKeyReceipts(store, issued.ID)
	if err != nil {
		t.Fatalf("consuming receipts: %v", err)
	}
	if !applied || consumed.SessionKey != "provider-key-1" {
		t.Fatalf("consume applied=%v key=%q", applied, consumed.SessionKey)
	}
	if strings.TrimSpace(consumed.SessionHookActivityGate) != "" {
		t.Fatalf("consume left a gate behind: %q", consumed.SessionHookActivityGate)
	}
}

func TestProviderSessionKeyReceiptConsumeWithoutPendingTokenIsFenceFree(t *testing.T) {
	store := beads.NewMemStore()
	info := receiptTestSession(t, store)
	fence, _, err := acquireProviderSessionKeyReceiptFence(store, info.ID)
	if err != nil || fence != nil {
		t.Fatalf("no-token acquire: fence=%v err=%v", fence != nil, err)
	}
	if _, applied, err := ConsumeProviderSessionKeyReceipts(store, info.ID); err != nil || applied {
		t.Fatalf("consume without receipts: applied=%v err=%v", applied, err)
	}
}

func TestProviderSessionKeyReceiptFenceBlocksConsumeSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	info := receiptTestSession(t, store)
	issued, _, err := IssueProviderSessionKeyReceipt(store, info, time.Now().UTC())
	if err != nil {
		t.Fatalf("issuing receipt: %v", err)
	}
	if _, err := PublishProviderSessionKeyReceipt(store, ProviderSessionKeyReceiptInput{
		SessionID:          issued.ID,
		Generation:         issued.Generation,
		ContinuationEpoch:  issued.ContinuationEpoch,
		InstanceToken:      issued.InstanceToken,
		ProviderFamily:     "codex",
		ReceiptToken:       issued.ProviderSessionKeyReceiptToken,
		LaunchAuthority:    issued.ProviderSessionKeyReceiptAuthority,
		ProviderSessionKey: "provider-key-1",
	}); err != nil {
		t.Fatalf("publishing receipt: %v", err)
	}
	// A foreign hook lease on the same row blocks the consume snapshot; the
	// consume treats it as a silent level-triggered retry.
	hook, _, err := AcquireHookActivityLease(store, HookActivityCoordinates{
		SessionID: issued.ID, Generation: issued.Generation, InstanceToken: issued.InstanceToken,
	})
	if err != nil {
		t.Fatalf("hook lease: %v", err)
	}
	current, applied, err := ConsumeProviderSessionKeyReceipts(store, issued.ID)
	if err != nil {
		t.Fatalf("blocked consume errored instead of deferring: %v", err)
	}
	if applied || current.SessionKey != "" {
		t.Fatalf("blocked consume applied=%v key=%q", applied, current.SessionKey)
	}
	// After the hook releases AND the tombstone is acknowledged, the consume
	// completes.
	if err := hook.Release(); err != nil {
		t.Fatalf("hook release: %v", err)
	}
	raw, _ := beads.HandlesFor(store).Live.Get(issued.ID)
	if ok, err := AcknowledgeSessionHookActivityAfterProviderBoundary(store, issued.ID, raw.Metadata[SessionHookActivityGateMetadataKey]); err != nil || !ok {
		t.Fatalf("acknowledge: ok=%v err=%v", ok, err)
	}
	consumed, applied, err := ConsumeProviderSessionKeyReceipts(store, issued.ID)
	if err != nil || !applied || consumed.SessionKey != "provider-key-1" {
		t.Fatalf("deferred consume: applied=%v key=%q err=%v", applied, consumed.SessionKey, err)
	}
}
