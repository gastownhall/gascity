package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout"
)

// leaseGateRuntime builds a watchdog runtime whose beads.lease_renewal gate is
// resolved to mode. The gate is read off the boot-latched controller-state
// snapshot, and a non-nil controller state also routes store lookup, so the
// stores go on the controller state rather than the standalone field.
func leaseGateRuntime(t *testing.T, stores map[string]beads.Store, mode rollout.Mode) *CityRuntime {
	t.Helper()
	cr, _ := leaseWatchdogRuntime(nil)
	cr.cs = &controllerState{
		rolloutFlags: rollout.ForTest(rollout.WithBeadsLeaseRenewal(mode)),
		beadStores:   stores,
	}
	return cr
}

// TestLeaseRenewalGateOffStopsAllRenewal proves beads.lease_renewal=off is a
// real kill switch: the watchdog performs no renewal at all, so an operator can
// disable the driver without editing the TTL (the review's "no gate and no kill
// switch short of editing the TTL").
func TestLeaseRenewalGateOffStopsAllRenewal(t *testing.T) {
	store := newLeaseRenewingMemStore()
	seedInProgressBead(t, store, "sess-a")
	cr := leaseGateRuntime(t, map[string]beads.Store{"rig": store}, rollout.Off)
	snapshot := runningSnapshot("sess-a")

	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	for tick := 0; tick <= 20; tick++ {
		if got := cr.runLeaseRenewalWatchdog(t0.Add(time.Duration(tick)*30*time.Second), snapshot); got != 0 {
			t.Fatalf("tick %d renewed = %d, want 0 with the gate off", tick, got)
		}
	}
	if len(store.renews) != 0 {
		t.Errorf("renew calls = %v, want none with beads.lease_renewal=off", store.renews)
	}
}

// TestLeaseRenewalGateAutoRenews proves the default mode drives renewal, so the
// gate's ON position is the shipped behavior gas-76r requires.
func TestLeaseRenewalGateAutoRenews(t *testing.T) {
	store := newLeaseRenewingMemStore()
	seedInProgressBead(t, store, "sess-a")
	cr := leaseGateRuntime(t, map[string]beads.Store{"rig": store}, rollout.Auto)

	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	if got := cr.runLeaseRenewalWatchdog(t0, runningSnapshot("sess-a")); got != 1 {
		t.Fatalf("renewed = %d, want 1 with beads.lease_renewal=auto", got)
	}
}

// TestLeaseRenewalDefaultsOnWithoutAControllerState proves an unresolved gate
// renews rather than silently disabling the driver. Every other gate maps
// ModeUnset to its legacy path; for this one the legacy path IS the gas-76r
// defect, so the unwired direction must fail safe toward renewal.
func TestLeaseRenewalDefaultsOnWithoutAControllerState(t *testing.T) {
	store := newLeaseRenewingMemStore()
	seedInProgressBead(t, store, "sess-a")
	cr, _ := leaseWatchdogRuntime(map[string]beads.Store{"rig": store}) // cs is nil

	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	if got := cr.runLeaseRenewalWatchdog(t0, runningSnapshot("sess-a")); got != 1 {
		t.Fatalf("renewed = %d, want 1 when no controller state resolved the gate", got)
	}
}

// TestLeaseRenewalRequireReportsAnIncapableStore proves require turns a store
// that cannot renew from a silent skip into a reported failure, matching the
// house meaning of Require on the sibling beads gates.
func TestLeaseRenewalRequireReportsAnIncapableStore(t *testing.T) {
	incapable := beads.NewMemStore() // no LeaseRenewer
	seedInProgressBead(t, incapable, "sess-a")
	cr := leaseGateRuntime(t, map[string]beads.Store{"rig": incapable}, rollout.Require)
	stderr := cr.stderr.(interface{ String() string })

	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	if got := cr.runLeaseRenewalWatchdog(t0, runningSnapshot("sess-a")); got != 0 {
		t.Fatalf("renewed = %d, want 0 from a store that cannot renew", got)
	}
	if s := stderr.String(); s == "" {
		t.Error("stderr empty; beads.lease_renewal=require must report an incapable store")
	}
}

// TestLeaseRenewalAutoStaysSilentOnAnIncapableStore is the Require counterpart:
// under auto, capability absence is not a failure and must not spam the
// controller log on every pass.
func TestLeaseRenewalAutoStaysSilentOnAnIncapableStore(t *testing.T) {
	incapable := beads.NewMemStore()
	seedInProgressBead(t, incapable, "sess-a")
	cr := leaseGateRuntime(t, map[string]beads.Store{"rig": incapable}, rollout.Auto)
	stderr := cr.stderr.(interface{ String() string })

	t0 := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	cr.runLeaseRenewalWatchdog(t0, runningSnapshot("sess-a"))
	if s := stderr.String(); s != "" {
		t.Errorf("stderr = %q, want empty under auto", s)
	}
}
