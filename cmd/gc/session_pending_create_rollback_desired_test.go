package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The pending-create rollback tests in session_lifecycle_chaos_test.go all drive
// setDesired(false), so they only exercise the !desired rollback
// (session_reconciler.go ~1686). The tests below cover the DESIRED branch
// (~2229) — the path that matters for a session that is supposed to be running,
// which is the shape a wedged never-started create would take.
//
// newSessionChaosHarness wires the Manager with the harness's own clock.Fake
// (session_lifecycle_chaos_test.go), so a harness-minted intent's
// pending_create_started_at anchors on the same clock the reconciler reads —
// no manual re-anchoring needed.

// desiredPendingCreateMaxTicks bounds runDesiredPendingCreateTicks: every
// caller wants the same generous one-minute-step ceiling, so it is a
// constant rather than a parameter no caller varies.
const desiredPendingCreateMaxTicks = 30

// runDesiredPendingCreateTicks reconciles up to desiredPendingCreateMaxTicks
// one-minute steps and returns the tick at which the pending-create claim was
// released, or -1.
func runDesiredPendingCreateTicks(t *testing.T, h *sessionChaosHarness) int {
	t.Helper()
	for i := 1; i <= desiredPendingCreateMaxTicks; i++ {
		h.reconcileTick()
		h.env.clk.Advance(time.Minute)
		got, err := h.env.store.Get(h.sessionID)
		if err != nil {
			t.Fatalf("store.Get(%s): %v", h.sessionID, err)
		}
		if got.Status == "closed" || strings.TrimSpace(got.Metadata["pending_create_claim"]) == "" {
			t.Logf("claim released at tick %d (%s): status=%q state=%q",
				i, time.Duration(i)*time.Minute, got.Status,
				strings.TrimSpace(got.Metadata["state"]))
			return i
		}
	}
	return -1
}

// TestDesiredPendingCreateRollsBackWhenStartKeepsFailing pins that a desired
// never-started create whose provider Start never succeeds does not retain its
// pending_create_claim. Without this, the bead holds its alias and a capacity
// slot (BaseStateStartPending counts against cap) with no live runtime. The
// observed mechanism is the failed-create rollback at the first tick
// (status=closed, state=failed-create), not the never-started lease — this test
// pins that the claim does not persist on the desired branch, not lease-floor
// timing.
func TestDesiredPendingCreateRollsBackWhenStartKeepsFailing(t *testing.T) {
	h := newSessionChaosHarness(t, 20260729)
	h.createSessionIntent()
	h.assertCreatingIntent()

	h.env.sp.StartErrors[h.sessionName] = errors.New("provider start failure")

	if at := runDesiredPendingCreateTicks(t, h); at < 0 {
		got, _ := h.env.store.Get(h.sessionID)
		t.Fatalf("desired pending-create still claimed after 30m: status=%q state=%q claim=%q running=%v",
			got.Status,
			strings.TrimSpace(got.Metadata["state"]),
			strings.TrimSpace(got.Metadata["pending_create_claim"]),
			h.env.sp.IsRunning(h.sessionName))
	}
}

// TestDesiredQuarantinedPendingCreateRollsBackAfterLeaseExpiry pins the
// interaction between the two independent timers. An active quarantine
// suppresses the wake indefinitely (crash-loop protection,
// session_reconciler.go:3514), but that must NOT also suppress the
// never-started pending-create rollback: the lease has its own 10-minute floor
// (pendingCreateNeverStartedTimeout) and must still release the claim while the
// quarantine is in force. Otherwise a quarantined never-started create holds its
// alias and capacity slot for the whole quarantine window.
func TestDesiredQuarantinedPendingCreateRollsBackAfterLeaseExpiry(t *testing.T) {
	h := newSessionChaosHarness(t, 20260734)
	h.createSessionIntent()
	h.assertCreatingIntent()

	if err := h.env.store.SetMetadataBatch(h.sessionID, map[string]string{
		// Quarantine outlives the never-started lease timeout by a wide margin.
		"quarantined_until": h.env.clk.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seed quarantine: %v", err)
	}
	// Healing must come from the rollback, never from a successful start.
	h.env.sp.StartErrors[h.sessionName] = errors.New("provider start failure")

	at := runDesiredPendingCreateTicks(t, h)
	if at < 0 {
		got, _ := h.env.store.Get(h.sessionID)
		t.Fatalf("quarantined never-started pending-create survived 30m (lease expired at %s): status=%q state=%q claim=%q",
			pendingCreateNeverStartedTimeout, got.Status,
			strings.TrimSpace(got.Metadata["state"]),
			strings.TrimSpace(got.Metadata["pending_create_claim"]))
	}
	// The rollback must be driven by the lease floor, not by the quarantine
	// lifting at 60m — catching a regression that defers it to quarantine expiry.
	if maxTicks := int(pendingCreateNeverStartedTimeout/time.Minute) + 5; at > maxTicks {
		t.Errorf("claim released at tick %d, want <= %d (lease floor %s, not quarantine expiry)",
			at, maxTicks, pendingCreateNeverStartedTimeout)
	}
}

// TestDesiredCreatingPendingCreateReleasesClaim covers the exact input the
// claim-gated projection branch keys on (lifecycle_projection.go:761):
// state=creating + pending_create_claim=true + last_woke_at="". That branch
// returns start-requested and the projection places no age bound on this shape,
// so the release must come from the reconciler; in this scenario the
// failed-create rollback gets there first (tick 1), ahead of the 10m lease.
func TestDesiredCreatingPendingCreateReleasesClaim(t *testing.T) {
	h := newSessionChaosHarness(t, 20260730)
	h.createSessionIntent()

	if err := h.env.store.SetMetadataBatch(h.sessionID, map[string]string{
		"state":                string(sessionpkg.StateCreating),
		"pending_create_claim": "true",
		"last_woke_at":         "",
	}); err != nil {
		t.Fatalf("seed creating shape: %v", err)
	}
	h.env.sp.StartErrors[h.sessionName] = errors.New("provider start failure")

	if at := runDesiredPendingCreateTicks(t, h); at < 0 {
		got, _ := h.env.store.Get(h.sessionID)
		t.Fatalf("creating+claim+never-started survived 30m: status=%q state=%q claim=%q",
			got.Status,
			strings.TrimSpace(got.Metadata["state"]),
			strings.TrimSpace(got.Metadata["pending_create_claim"]))
	}
}
