package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// namedSessionBeadWithTrigger builds a named session bead stamped with a
// trigger cluster pointing at workBeadID (same-store unless storeRef is
// non-empty).
func namedSessionBeadWithTrigger(workBeadID, storeRef string) beads.Bead {
	metadata := map[string]string{
		"session_name":                     "s-claude",
		"template":                         "city/claude",
		beadmeta.TriggerBeadIDMetadataKey:  workBeadID,
		beadmeta.BrainParentSIDMetadataKey: "brain-A",
	}
	if storeRef != "" {
		metadata[beadmeta.TriggerBeadStoreRefMetadataKey] = storeRef
	}
	return beads.Bead{
		Title:    "claude-named",
		Type:     sessionBeadType,
		Status:   "open",
		Labels:   []string{sessionBeadLabel},
		Metadata: metadata,
	}
}

// TestBindNamedSessionTriggerBead_ClearsStampWhenTargetBlocked pins the core
// gascity#4373 repro: a named session's trigger stamp must clear once its
// target is parked, exactly like the pool path's bindPoolSessionTriggerBead
// already does for the request-driven case.
func TestBindNamedSessionTriggerBead_ClearsStampWhenTargetBlocked(t *testing.T) {
	mem := beads.NewMemStore()
	target, err := mem.Create(beads.Bead{Title: "work"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	blocked := "blocked"
	if err := mem.Update(target.ID, beads.UpdateOpts{Status: &blocked}); err != nil {
		t.Fatalf("block target: %v", err)
	}
	sess, err := mem.Create(namedSessionBeadWithTrigger(target.ID, ""))
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	rec := beadstest.NewRecordingStore(mem)

	info, err := sessionFrontDoor(rec).Get(sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	bound, err := bindNamedSessionTriggerBead(rec, info)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.TriggerBeadID != "" {
		t.Errorf("TriggerBeadID = %q, want cleared", bound.TriggerBeadID)
	}
	if bound.BrainParentSID != "" {
		t.Errorf("BrainParentSID = %q, want cleared alongside the trigger", bound.BrainParentSID)
	}
	after, err := mem.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get after bind: %v", err)
	}
	if v := after.Metadata[beadmeta.TriggerBeadIDMetadataKey]; v != "" {
		t.Errorf("durable trigger stamp = %q, want cleared", v)
	}
}

// TestBindNamedSessionTriggerBead_ClearsStampWhenTargetClosed covers the
// second parked-state trigger from the issue's own gate: a closed target is
// just as stale as a blocked one.
func TestBindNamedSessionTriggerBead_ClearsStampWhenTargetClosed(t *testing.T) {
	mem := beads.NewMemStore()
	target, err := mem.Create(beads.Bead{Title: "work", Status: "open"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if err := mem.Close(target.ID); err != nil {
		t.Fatalf("close target: %v", err)
	}
	sess, err := mem.Create(namedSessionBeadWithTrigger(target.ID, ""))
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	rec := beadstest.NewRecordingStore(mem)

	info, err := sessionFrontDoor(rec).Get(sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	bound, err := bindNamedSessionTriggerBead(rec, info)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.TriggerBeadID != "" {
		t.Errorf("TriggerBeadID = %q, want cleared for a closed target", bound.TriggerBeadID)
	}
}

// TestBindNamedSessionTriggerBead_ClearsStampWhenTargetAbsent covers the
// case where the target bead no longer exists at all.
func TestBindNamedSessionTriggerBead_ClearsStampWhenTargetAbsent(t *testing.T) {
	mem := beads.NewMemStore()
	sess, err := mem.Create(namedSessionBeadWithTrigger("wb-GONE", ""))
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	rec := beadstest.NewRecordingStore(mem)

	info, err := sessionFrontDoor(rec).Get(sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	bound, err := bindNamedSessionTriggerBead(rec, info)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.TriggerBeadID != "" {
		t.Errorf("TriggerBeadID = %q, want cleared for an absent target", bound.TriggerBeadID)
	}
}

// TestBindNamedSessionTriggerBead_LeavesStampWhenTargetOpen is the negative
// case: a still-workable target must not be disturbed, matching the pool
// path's "no change" behavior when there is nothing stale to clear.
func TestBindNamedSessionTriggerBead_LeavesStampWhenTargetOpen(t *testing.T) {
	mem := beads.NewMemStore()
	target, err := mem.Create(beads.Bead{Title: "work", Status: "open"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	sess, err := mem.Create(namedSessionBeadWithTrigger(target.ID, ""))
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	rec := beadstest.NewRecordingStore(mem)

	info, err := sessionFrontDoor(rec).Get(sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	bound, err := bindNamedSessionTriggerBead(rec, info)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.TriggerBeadID != target.ID {
		t.Errorf("TriggerBeadID = %q, want unchanged %q for a still-open target", bound.TriggerBeadID, target.ID)
	}
	if n := len(rec.CallsForOp("Update")); n != 0 {
		t.Errorf("Update ops = %d, want 0 (nothing stale to clear)", n)
	}
}

// TestBindNamedSessionTriggerBead_LeavesCrossStoreTargetUntouched: a trigger
// stamped against a different store's bead is outside what this store can
// judge, so the stamp must survive rather than risk a wrong clear based on a
// same-ID coincidence in the wrong store.
func TestBindNamedSessionTriggerBead_LeavesCrossStoreTargetUntouched(t *testing.T) {
	mem := beads.NewMemStore()
	sess, err := mem.Create(namedSessionBeadWithTrigger("wb-A", "rig-a"))
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	rec := beadstest.NewRecordingStore(mem)

	info, err := sessionFrontDoor(rec).Get(sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	bound, err := bindNamedSessionTriggerBead(rec, info)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.TriggerBeadID != "wb-A" {
		t.Errorf("TriggerBeadID = %q, want untouched cross-store stamp %q", bound.TriggerBeadID, "wb-A")
	}
}
