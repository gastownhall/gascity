package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func gateTestSessionBead(t *testing.T, store beads.Store) Info {
	t.Helper()
	meta := map[string]string{
		"session_name":     "gate-worker-1",
		"template":         "worker",
		"pool_managed":     "true",
		"state":            "active",
		"generation":       "7",
		"instance_token":   "tok-7",
		"awake_started_at": "2026-08-14T10:00:00Z",
	}
	b, err := store.Create(beads.Bead{
		Title: "session", Type: BeadType, Labels: []string{LabelSession}, Status: "open", Metadata: meta,
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

func gateTestWorkBead(t *testing.T, store beads.Store, assignee string) beads.Bead {
	t.Helper()
	w, err := store.Create(beads.Bead{Title: "work", Type: "task", Metadata: map[string]string{}})
	if err != nil {
		t.Fatalf("seeding work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(w.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("claiming work: %v", err)
	}
	claimed, err := store.Get(w.ID)
	if err != nil {
		t.Fatalf("re-reading work: %v", err)
	}
	return claimed
}

func TestHookActivityGateAcquireValidateReleaseLifecycle(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	coordinates := HookActivityCoordinates{
		SessionID:         info.ID,
		Generation:        info.Generation,
		InstanceToken:     info.InstanceToken,
		ContinuationEpoch: "",
	}
	lease, current, err := AcquireHookActivityLease(store, coordinates)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if lease == nil || current.SessionHookActivityGate != lease.Value() {
		t.Fatalf("acquire did not install the exact gate value: %q", current.SessionHookActivityGate)
	}
	if _, err := lease.Validate(); err != nil {
		t.Fatalf("validate under ownership: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	raw, _ := beads.HandlesFor(store).Live.Get(info.ID)
	gate := raw.Metadata[SessionHookActivityGateMetadataKey]
	if !IsReleasedSessionHookActivityTombstone(gate) {
		t.Fatalf("released gate = %q, want released tombstone", gate)
	}
	if IsExecutionStalledActivityFence(gate) {
		t.Fatal("released tombstone must not report as a stalled fence")
	}
}

func TestHookActivityGateBlocksSecondAcquireAndSupersedesStaleCoordinates(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	first := HookActivityCoordinates{SessionID: info.ID, Generation: info.Generation, InstanceToken: info.InstanceToken}
	if _, _, err := AcquireHookActivityLease(store, first); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if _, _, err := AcquireHookActivityLease(store, first); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("second acquire err = %v, want busy", err)
	}
	stale := first
	stale.InstanceToken = "tok-other"
	if _, _, err := AcquireHookActivityLease(store, stale); err == nil {
		t.Fatal("stale coordinates acquired the gate")
	}
}

func TestHookActivityGateIncompleteCoordinatesRefused(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	incomplete := HookActivityCoordinates{SessionID: info.ID, Generation: info.Generation}
	if _, _, err := AcquireHookActivityLease(store, incomplete); err == nil {
		t.Fatal("incomplete coordinates acquired the gate")
	}
}

func TestHookActivityGateEpochDefaultMatchesManagerCreatedRows(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	if info.ContinuationEpoch != "" {
		t.Fatalf("precondition: expected empty persisted epoch, got %q", info.ContinuationEpoch)
	}
	coordinates := HookActivityCoordinates{SessionID: info.ID, Generation: info.Generation, InstanceToken: info.InstanceToken}
	lease, _, err := AcquireHookActivityLease(store, coordinates)
	if err != nil {
		t.Fatalf("acquire with implicit default epoch: %v", err)
	}
	var wire sessionHookActivityGateValue
	if err := json.Unmarshal([]byte(lease.Value()), &wire); err != nil {
		t.Fatalf("decoding gate value: %v", err)
	}
	if wire.ContinuationEpoch != "1" {
		t.Fatalf("gate epoch = %q, want the defaulted first epoch", wire.ContinuationEpoch)
	}
}

func TestHookActivityGateReleaseCannotEraseSuccessorLease(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	coordinates := HookActivityCoordinates{SessionID: info.ID, Generation: info.Generation, InstanceToken: info.InstanceToken}
	first, _, err := AcquireHookActivityLease(store, coordinates)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// Simulate a successor: force-clear through the raw store, then re-acquire.
	if err := store.SetMetadata(info.ID, SessionHookActivityGateMetadataKey, ""); err != nil {
		t.Fatalf("clearing gate: %v", err)
	}
	second, _, err := AcquireHookActivityLease(store, coordinates)
	if err != nil {
		t.Fatalf("successor acquire: %v", err)
	}
	if err := first.Release(); err == nil {
		t.Fatal("delayed first release erased or reported success against a successor lease")
	}
	if _, err := second.Validate(); err != nil {
		t.Fatalf("successor invalidated by delayed first release: %v", err)
	}
}

func TestAcknowledgeSessionHookActivityAfterProviderBoundaryRequiresExactValue(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	coordinates := HookActivityCoordinates{SessionID: info.ID, Generation: info.Generation, InstanceToken: info.InstanceToken}
	lease, _, err := AcquireHookActivityLease(store, coordinates)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// The primitive admits both the active kind and the released tombstone:
	// proving the provider boundary is the CALLER's policy (the cmd/gc
	// acknowledger clears active kinds only for a stopped runtime). Here the
	// primitive-level contract is pinned: exact value + live coordinates.
	if ok, err := AcknowledgeSessionHookActivityAfterProviderBoundary(store, info.ID, lease.Value()); err != nil || !ok {
		t.Fatalf("primitive acknowledgment of the exact active value: ok=%v err=%v", ok, err)
	}

	// Released tombstone acknowledgment and stale replay refusal.
	lease2, _, err := AcquireHookActivityLease(store, coordinates)
	if err != nil {
		t.Fatalf("re-acquire after primitive clear: %v", err)
	}
	if err := lease2.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	raw, _ := beads.HandlesFor(store).Live.Get(info.ID)
	tombstone := raw.Metadata[SessionHookActivityGateMetadataKey]
	if !IsReleasedSessionHookActivityTombstone(tombstone) {
		t.Fatalf("gate = %q after release, want released tombstone", tombstone)
	}
	if ok, err := AcknowledgeSessionHookActivityAfterProviderBoundary(store, info.ID, tombstone); err != nil || !ok {
		t.Fatalf("acknowledge released tombstone: ok=%v err=%v", ok, err)
	}
	raw, _ = beads.HandlesFor(store).Live.Get(info.ID)
	if raw.Metadata[SessionHookActivityGateMetadataKey] != "" {
		t.Fatalf("gate = %q after acknowledgment, want cleared", raw.Metadata[SessionHookActivityGateMetadataKey])
	}
	// A stale replay of the acknowledged value is refused.
	if ok, err := AcknowledgeSessionHookActivityAfterProviderBoundary(store, info.ID, tombstone); err == nil || ok {
		t.Fatal("stale tombstone acknowledgment replayed")
	}
}

func TestExecutionStalledActivityFenceAcquireRecoverAndRelease(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	work := gateTestWorkBead(t, store, "gate-worker-1")
	coordinates := ExecutionStalledActivityFenceCoordinates{
		HookActivityCoordinates: HookActivityCoordinates{
			SessionID: info.ID, Generation: info.Generation, InstanceToken: info.InstanceToken,
		},
		AwakeStartedAt:     info.AwakeStartedAt,
		LifecycleAuthority: ExecutionStalledLifecycleAuthority(info),
		WorkID:             work.ID,
		WorkStoreRef:       "city",
		WorkRevision:       work.Revision,
		Assignee:           "gate-worker-1",
	}
	fence, current, err := AcquireExecutionStalledActivityFence(store, coordinates)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !current.Closed && !IsExecutionStalledActivityFence(current.SessionHookActivityGate) {
		t.Fatalf("stalled fence not installed: %q", current.SessionHookActivityGate)
	}
	if !ExecutionStalledActivityFenceMatches(current.SessionHookActivityGate, coordinates) {
		t.Fatal("installed fence does not match its coordinates (nonce aside)")
	}
	// Recovery: a second acquire with the same coordinates adopts, not blocks.
	recovered, _, err := AcquireExecutionStalledActivityFence(store, coordinates)
	if err != nil {
		t.Fatalf("crash-recovery adopt: %v", err)
	}
	if recovered.Value() != fence.Value() {
		t.Fatal("recovery did not adopt the exact prior fence value")
	}
	// A different stalled authority is blocked, not adopted.
	other := coordinates
	other.WorkID = "work-other"
	if _, _, err := AcquireExecutionStalledActivityFence(store, other); err == nil {
		t.Fatal("divergent stalled authority adopted the fence")
	}
	// Terminal release clears exactly.
	if err := ReleaseExecutionStalledActivityFenceValue(store, info.ID, fence.Value()); err != nil {
		t.Fatalf("release: %v", err)
	}
	raw, _ := beads.HandlesFor(store).Live.Get(info.ID)
	if raw.Metadata[SessionHookActivityGateMetadataKey] != "" {
		t.Fatal("stalled fence not cleared by exact release")
	}
}

func TestExecutionStalledActivityFenceBlocksHookLeaseAndViceVersa(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	work := gateTestWorkBead(t, store, "gate-worker-1")
	stalled := ExecutionStalledActivityFenceCoordinates{
		HookActivityCoordinates: HookActivityCoordinates{
			SessionID: info.ID, Generation: info.Generation, InstanceToken: info.InstanceToken,
		},
		AwakeStartedAt:     info.AwakeStartedAt,
		LifecycleAuthority: ExecutionStalledLifecycleAuthority(info),
		WorkID:             work.ID,
		WorkStoreRef:       "city",
		WorkRevision:       work.Revision,
		Assignee:           "gate-worker-1",
	}
	if _, _, err := AcquireExecutionStalledActivityFence(store, stalled); err != nil {
		t.Fatalf("stalled fence: %v", err)
	}
	hook := stalled.HookActivityCoordinates
	if _, _, err := AcquireHookActivityLease(store, hook); err == nil {
		t.Fatal("hook lease acquired over an installed stalled fence")
	}
	// A stalled fence cannot be released through the hook-lease release path.
	raw, _ := beads.HandlesFor(store).Live.Get(info.ID)
	if err := ReleaseExecutionStalledActivityFenceValue(store, info.ID, "not-a-gate"); err == nil {
		t.Fatal("non-gate value released")
	}
	if err := ReleaseExecutionStalledActivityFenceValue(store, info.ID, raw.Metadata[SessionHookActivityGateMetadataKey]); err != nil {
		t.Fatalf("exact stalled release: %v", err)
	}
	if _, _, err := AcquireHookActivityLease(store, hook); err != nil {
		t.Fatalf("hook lease after fence release: %v", err)
	}
}

func TestReleaseExecutionStalledActivityFenceValueRefusesHookKinds(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	lease, _, err := AcquireHookActivityLease(store, HookActivityCoordinates{
		SessionID: info.ID, Generation: info.Generation, InstanceToken: info.InstanceToken,
	})
	if err != nil {
		t.Fatalf("hook lease: %v", err)
	}
	if err := ReleaseExecutionStalledActivityFenceValue(store, info.ID, lease.Value()); err == nil {
		t.Fatal("hook-kind lease released through the stalled-fence path")
	}
}

func TestStaleOccupantEvictionUnjamsHookLeaseAfterIncarnationChurn(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	coordinates := HookActivityCoordinates{SessionID: info.ID, Generation: info.Generation, InstanceToken: info.InstanceToken}
	lease, _, err := AcquireHookActivityLease(store, coordinates)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// Incarnation churn: the row moves to a new token while the tombstone
	// stays (the hook died before any acknowledgment).
	if err := store.SetMetadata(info.ID, "instance_token", "token-rotated"); err != nil {
		t.Fatalf("rotating token: %v", err)
	}
	next := coordinates
	next.InstanceToken = "token-rotated"
	fresh, current, err := AcquireHookActivityLease(store, next)
	if err != nil {
		t.Fatalf("acquire after churn: %v (gate=%q)", err, current.SessionHookActivityGate)
	}
	if fresh == nil {
		t.Fatal("no lease after eviction")
	}
}

func TestStaleReceiptFenceEvictionAfterTokenConsumed(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	// Strand a receipt fence exactly as a post-Update crash would: token
	// cleared, gate still holding the (now stale) nonce.
	fence := sessionHookActivityGateValue{
		Version: 1, Kind: "receipt-consume", Nonce: "consumed-token",
		SessionID: info.ID, Generation: info.Generation,
		ContinuationEpoch: "1", InstanceToken: info.InstanceToken,
	}
	encoded, err := encodeSessionHookActivityGate(fence)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(info.ID, SessionHookActivityGateMetadataKey, encoded); err != nil {
		t.Fatal(err)
	}
	// With no pending token, the consume lane itself must evict the residue.
	if _, applied, err := ConsumeProviderSessionKeyReceipts(store, info.ID); err != nil || applied {
		t.Fatalf("consume: applied=%v err=%v", applied, err)
	}
	raw, _ := beads.HandlesFor(store).Live.Get(info.ID)
	if raw.Metadata[SessionHookActivityGateMetadataKey] != "" {
		t.Fatalf("stranded receipt fence not evicted: %q", raw.Metadata[SessionHookActivityGateMetadataKey])
	}
	// And a fresh hook lease succeeds on the unjammed gate.
	lease, _, err := AcquireHookActivityLease(store, HookActivityCoordinates{
		SessionID: info.ID, Generation: info.Generation, InstanceToken: info.InstanceToken,
	})
	if err != nil {
		t.Fatalf("hook lease after eviction: %v", err)
	}
	_ = lease
}

func TestLiveOccupantIsNeverEvicted(t *testing.T) {
	store := beads.NewMemStore()
	info := gateTestSessionBead(t, store)
	coordinates := HookActivityCoordinates{SessionID: info.ID, Generation: info.Generation, InstanceToken: info.InstanceToken}
	lease, _, err := AcquireHookActivityLease(store, coordinates)
	if err != nil {
		t.Fatalf("first lease: %v", err)
	}
	// The SAME live coordinates must be blocked, not evicted.
	if _, _, err := AcquireHookActivityLease(store, coordinates); err == nil {
		t.Fatal("live hook lease evicted by a second acquire")
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	// The released tombstone is not janitor-evicted while its coordinates
	// still match the live row; the stalled acquire proceeds only after the
	// documented provider-boundary acknowledgment (the cmd/gc installer's
	// role), mirroring the production layering.
	raw, _ := beads.HandlesFor(store).Live.Get(info.ID)
	tombstone := raw.Metadata[SessionHookActivityGateMetadataKey]
	if ok, err := AcknowledgeSessionHookActivityAfterProviderBoundary(store, info.ID, tombstone); err != nil || !ok {
		t.Fatalf("acknowledge released tombstone: ok=%v err=%v", ok, err)
	}
	// A stalled-kind gate is never janitor-evicted for a different authority.
	work := gateTestWorkBead(t, store, "gate-worker-1")
	stalled := ExecutionStalledActivityFenceCoordinates{
		HookActivityCoordinates: coordinates,
		AwakeStartedAt:          info.AwakeStartedAt,
		LifecycleAuthority:      ExecutionStalledLifecycleAuthority(info),
		WorkID:                  work.ID,
		WorkStoreRef:            "city",
		WorkRevision:            work.Revision,
		Assignee:                "gate-worker-1",
	}
	fence, _, err := AcquireExecutionStalledActivityFence(store, stalled)
	if err != nil {
		t.Fatalf("stalled fence: %v", err)
	}
	if evicted, err := evictStaleSessionHookActivityOccupant(store, mustGateInfo(t, store, info.ID)); err != nil || evicted {
		t.Fatalf("stalled fence janitor-evicted: %v %v", evicted, err)
	}
	_ = fence
}

func mustGateInfo(t *testing.T, store beads.Store, id string) Info {
	t.Helper()
	raw, err := beads.HandlesFor(store).Live.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return InfoFromPersistedBead(raw)
}
