package main

import (
	"errors"
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

// TestBindNamedSessionTriggerBead_ClearsStampWhenTargetBlocked covers the
// literal `blocked` status, which only a MemStore-backed (or otherwise
// unmapped) target can hold. The production shape is
// TestBindNamedSessionTriggerBead_ClearsStampWhenTargetDependencyBlocked
// below: every real store folds bd's raw `blocked` into "open" (gc-4zb/#4395)
// and reports the park through the IsBlocked projection instead.
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

// TestBindNamedSessionTriggerBead_ClearsStampWhenTargetDependencyBlocked is
// the production-shaped gascity#4373 repro. Through BdStore/DoltLite/
// NativeDolt, mapBdStatus folds bd's raw `blocked` into "open", so the parked
// target this reconciler actually sees is `open` + IsBlocked=true — bd's
// denormalized ready-work projection. The stamp must clear on that shape, not
// only on the literal status a MemStore fixture can hand back.
func TestBindNamedSessionTriggerBead_ClearsStampWhenTargetDependencyBlocked(t *testing.T) {
	mem := beads.NewMemStore()
	blocked := true
	target, err := mem.Create(beads.Bead{Title: "work", Status: "open", IsBlocked: &blocked})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	stored, err := mem.Get(target.ID)
	if err != nil {
		t.Fatalf("get target: %v", err)
	}
	if stored.Status != "open" || stored.IsBlocked == nil || !*stored.IsBlocked {
		t.Fatalf("fixture target = {Status:%q IsBlocked:%v}, want the production shape {open, &true}", stored.Status, stored.IsBlocked)
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
		t.Errorf("TriggerBeadID = %q, want cleared for a dependency-blocked target", bound.TriggerBeadID)
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

// TestBindNamedSessionTriggerBead_LeavesStampWhenTargetProjectionUnavailable
// pins the fail-open half of the projection read: a store that does not
// publish IsBlocked (native DoltLite snapshots, pre-1.0.5 bd) reports an open
// target with a nil projection, and a nil projection must never be read as
// "parked".
func TestBindNamedSessionTriggerBead_LeavesStampWhenTargetProjectionUnavailable(t *testing.T) {
	mem := beads.NewMemStore()
	target, err := mem.Create(beads.Bead{Title: "work", Status: "open"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	if stored, getErr := mem.Get(target.ID); getErr != nil || stored.IsBlocked != nil {
		t.Fatalf("fixture target IsBlocked = %v (err %v), want nil", stored.IsBlocked, getErr)
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
		t.Errorf("TriggerBeadID = %q, want unchanged %q when the projection is unavailable", bound.TriggerBeadID, target.ID)
	}
}

// failingGetStore fails Get for one bead ID and delegates everything else,
// standing in for a transient backend error on the target lookup.
type failingGetStore struct {
	beads.Store
	failID string
	err    error
}

func (s *failingGetStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

// TestBindNamedSessionTriggerBead_LeavesStampWhenTargetLookupFails: a Get
// failure that is not ErrNotFound says nothing about the target's state, so
// the stamp must survive. Clearing on a transient backend blip would silently
// unaim a live session from workable work.
func TestBindNamedSessionTriggerBead_LeavesStampWhenTargetLookupFails(t *testing.T) {
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
	failing := &failingGetStore{Store: rec, failID: target.ID, err: errors.New("backend unavailable")}

	info, err := sessionFrontDoor(rec).Get(sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	bound, err := bindNamedSessionTriggerBead(failing, info)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.TriggerBeadID != target.ID {
		t.Errorf("TriggerBeadID = %q, want unchanged %q after a transient lookup failure", bound.TriggerBeadID, target.ID)
	}
	after, err := mem.Get(sess.ID)
	if err != nil {
		t.Fatalf("Get after bind: %v", err)
	}
	if v := after.Metadata[beadmeta.TriggerBeadIDMetadataKey]; v != target.ID {
		t.Errorf("durable trigger stamp = %q, want unchanged %q", v, target.ID)
	}
	if n := len(rec.CallsForOp("Update")); n != 0 {
		t.Errorf("Update ops = %d, want 0 (an unreadable target is not a stale one)", n)
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
