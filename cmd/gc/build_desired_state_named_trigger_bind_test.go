package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// namedSessionBeadWithTrigger builds a named session bead stamped with a
// trigger cluster pointing at workBeadID. A non-empty storeRef is stamped
// verbatim onto gc.trigger_bead_store_ref; whether that names this store is
// namedTriggerRefIsSameStore's call, not the fixture's.
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

	bound, err := bindNamedSessionTriggerBead(rec, info, "test-city")
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
	bound, err := bindNamedSessionTriggerBead(rec, info, "test-city")
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
	bound, err := bindNamedSessionTriggerBead(rec, info, "test-city")
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
	bound, err := bindNamedSessionTriggerBead(failing, info, "test-city")
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
	bound, err := bindNamedSessionTriggerBead(rec, info, "test-city")
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
	bound, err := bindNamedSessionTriggerBead(rec, info, "test-city")
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
	bound, err := bindNamedSessionTriggerBead(rec, info, "test-city")
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
// same-ID coincidence in the wrong store. Every ref here names a blocked bead
// that IS present in this store under the stamped id -- if the predicate
// widened to accept them, the target would read as stale and the stamp would
// clear, so the zero-Update assertion is what pins the boundary.
func TestBindNamedSessionTriggerBead_LeavesCrossStoreTargetUntouched(t *testing.T) {
	for _, tc := range []struct {
		name     string
		storeRef string
	}{
		{name: "rig scoped", storeRef: "rig:rig-a"},
		{name: "bare rig name", storeRef: "rig-a"},
		{name: "another city", storeRef: "city:other-city"},
		{name: "class ref", storeRef: "class:worker"},
		{name: "unrecognized", storeRef: "somewhere-else"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			blocked := true
			target, err := mem.Create(beads.Bead{Title: "work", Status: "open", IsBlocked: &blocked})
			if err != nil {
				t.Fatalf("create target: %v", err)
			}
			sess, err := mem.Create(namedSessionBeadWithTrigger(target.ID, tc.storeRef))
			if err != nil {
				t.Fatalf("create session bead: %v", err)
			}
			rec := beadstest.NewRecordingStore(mem)

			info, err := sessionFrontDoor(rec).Get(sess.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			bound, err := bindNamedSessionTriggerBead(rec, info, "test-city")
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			if bound.TriggerBeadID != target.ID {
				t.Errorf("TriggerBeadID = %q, want untouched cross-store stamp %q", bound.TriggerBeadID, target.ID)
			}
			if bound.TriggerBeadStoreRef != tc.storeRef {
				t.Errorf("TriggerBeadStoreRef = %q, want untouched %q", bound.TriggerBeadStoreRef, tc.storeRef)
			}
			if n := len(rec.CallsForOp("Update")); n != 0 {
				t.Errorf("Update ops = %d, want 0 (a target in another store is not this store's to judge)", n)
			}
		})
	}
}

// TestBindNamedSessionTriggerBead_ClearsStampForSameStoreCityRef is the
// gascity#4373 shape the reporter actually filed: the parked bead's stamp
// carries a *populated* gc.trigger_bead_store_ref naming this same city store
// ("city" or "city:<name>", the two spellings normalizeDemandStoreRef collapses
// onto the city). Bailing on every non-empty ref would no-op on exactly that
// case. Both keys must clear together -- a store ref left pointing at a trigger
// id that no longer exists is a dangling half-cluster, and the pool path
// (computePoolTriggerBindingPatch) clears both.
func TestBindNamedSessionTriggerBead_ClearsStampForSameStoreCityRef(t *testing.T) {
	for _, tc := range []struct {
		name     string
		storeRef string
	}{
		{name: "bare city", storeRef: "city"},
		{name: "city scoped by name", storeRef: "city:test-city"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := beads.NewMemStore()
			blocked := true
			target, err := mem.Create(beads.Bead{Title: "parked work", Status: "open", IsBlocked: &blocked})
			if err != nil {
				t.Fatalf("create target: %v", err)
			}
			sess, err := mem.Create(namedSessionBeadWithTrigger(target.ID, tc.storeRef))
			if err != nil {
				t.Fatalf("create session bead: %v", err)
			}
			rec := beadstest.NewRecordingStore(mem)

			info, err := sessionFrontDoor(rec).Get(sess.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if info.TriggerBeadStoreRef != tc.storeRef {
				t.Fatalf("fixture TriggerBeadStoreRef = %q, want %q", info.TriggerBeadStoreRef, tc.storeRef)
			}
			bound, err := bindNamedSessionTriggerBead(rec, info, "test-city")
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			if bound.TriggerBeadID != "" {
				t.Errorf("TriggerBeadID = %q, want cleared for a parked same-store target", bound.TriggerBeadID)
			}
			if bound.TriggerBeadStoreRef != "" {
				t.Errorf("TriggerBeadStoreRef = %q, want cleared alongside the trigger", bound.TriggerBeadStoreRef)
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
			if v := after.Metadata[beadmeta.TriggerBeadStoreRefMetadataKey]; v != "" {
				t.Errorf("durable trigger store ref = %q, want cleared", v)
			}
		})
	}
}

// TestBindNamedSessionTriggerBead_LeavesSameStoreCityRefStampWhenTargetOpen is
// the negative half of the same-store city ref: reaching the status check is
// not the same as clearing. A workable target keeps both keys and writes
// nothing.
func TestBindNamedSessionTriggerBead_LeavesSameStoreCityRefStampWhenTargetOpen(t *testing.T) {
	mem := beads.NewMemStore()
	target, err := mem.Create(beads.Bead{Title: "work", Status: "open"})
	if err != nil {
		t.Fatalf("create target: %v", err)
	}
	sess, err := mem.Create(namedSessionBeadWithTrigger(target.ID, "city:test-city"))
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	rec := beadstest.NewRecordingStore(mem)

	info, err := sessionFrontDoor(rec).Get(sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	bound, err := bindNamedSessionTriggerBead(rec, info, "test-city")
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	if bound.TriggerBeadID != target.ID {
		t.Errorf("TriggerBeadID = %q, want unchanged %q for a still-open target", bound.TriggerBeadID, target.ID)
	}
	if bound.TriggerBeadStoreRef != "city:test-city" {
		t.Errorf("TriggerBeadStoreRef = %q, want unchanged %q", bound.TriggerBeadStoreRef, "city:test-city")
	}
	if n := len(rec.CallsForOp("Update")); n != 0 {
		t.Errorf("Update ops = %d, want 0 (nothing stale to clear)", n)
	}
}

// TestNamedTriggerRefIsSameStore pins the predicate directly, including the
// empty-cityName guard: with no city name to compare against, "city:" is an
// unrecognized ref, not a same-store match on the empty suffix.
func TestNamedTriggerRefIsSameStore(t *testing.T) {
	for _, tc := range []struct {
		storeRef string
		cityName string
		want     bool
	}{
		{storeRef: "", cityName: "test-city", want: true},
		{storeRef: "  ", cityName: "test-city", want: true},
		{storeRef: "city", cityName: "test-city", want: true},
		{storeRef: "city:test-city", cityName: "test-city", want: true},
		{storeRef: "city", cityName: "", want: true},
		{storeRef: "city:", cityName: "", want: false},
		{storeRef: "city:test-city", cityName: "other-city", want: false},
		{storeRef: "city:other-city", cityName: "test-city", want: false},
		{storeRef: "rig:rig-a", cityName: "test-city", want: false},
		{storeRef: "rig-a", cityName: "test-city", want: false},
		{storeRef: "class:worker", cityName: "test-city", want: false},
		{storeRef: "somewhere-else", cityName: "test-city", want: false},
	} {
		if got := namedTriggerRefIsSameStore(tc.storeRef, tc.cityName); got != tc.want {
			t.Errorf("namedTriggerRefIsSameStore(%q, %q) = %v, want %v", tc.storeRef, tc.cityName, got, tc.want)
		}
	}
}
