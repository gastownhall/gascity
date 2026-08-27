package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// newStatusHealRaceStore builds a conditional-write-capable session store: the
// legacy status heal only fences where the deployment has conditional writes
// enabled, so an unstamped MemStore would exercise the unfenced fallback.
func newStatusHealRaceStore(t *testing.T) beads.Store {
	t.Helper()
	result, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot:         t.TempDir(),
		Provider:          "file",
		ConditionalWrites: gate.Require,
		OpenFileStore:     func() (beads.Store, error) { return beads.NewMemStore(), nil },
	})
	if err != nil {
		t.Fatalf("open conditional-write store: %v", err)
	}
	writer, _, resolveErr := beads.ResolveConditionalWriter(result.Store)
	if writer == nil || resolveErr != nil {
		t.Fatalf("test premise: store resolved writer=%v err=%v, want a conditional writer", writer, resolveErr)
	}
	return result.Store
}

// seedStatusHealRaceSession creates a live configured-named session row in the
// exact post-start shape the pin path leaves behind: state="active" (what
// ConfirmStartedPatch writes) with a live runtime.
func seedStatusHealRaceSession(t *testing.T, store beads.Store) (sessionpkg.Info, int64) {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		Title:  "reviewer",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":           "reviewer",
			"agent_name":             "reviewer",
			"template":               "reviewer",
			"configured_named":       "true",
			"named_session_identity": "test-city/reviewer",
			"named_session_mode":     "on_demand",
			"generation":             "1",
			"instance_token":         "reviewer-token",
			"state":                  "active",
			"pin_awake":              "true",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	info, err := sessionFrontDoor(store).Get(bead.ID)
	if err != nil {
		t.Fatalf("read seeded session: %v", err)
	}
	if bead.Revision == 0 {
		t.Fatalf("test premise: seeded row revision = 0, want a revisioned row")
	}
	return info, bead.Revision
}

// TestLegacyStatusHealDoesNotRevertSuspendLandingAfterSnapshot is the ga-f7v2ft.125
// regression: `gc session suspend` writes {state:suspended, sleep_intent:user-hold,
// held_until} durably, and a fleet tick whose session snapshot predates that write
// computes a status heal from the stale row (observed-alive ⇒ ReconciledState=awake)
// and applies it. Because the heal touches only the state key, held_until and
// sleep_intent survive and the row settles awake+user-hold+held — the exact shape
// the v59 journey's suspend leg observed. The heal is advisory; it must never
// overwrite a row that changed after the snapshot it was computed from.
func TestLegacyStatusHealDoesNotRevertSuspendLandingAfterSnapshot(t *testing.T) {
	store := newStatusHealRaceStore(t)
	front := sessionFrontDoor(store)
	clk := &clock.Fake{Time: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)}

	staleInfo, loadedRevision := seedStatusHealRaceSession(t, store)
	tick := newReconcileTick([]sessionpkg.Info{staleInfo})

	// The CLI's suspend patch lands after the tick loaded its snapshot.
	heldUntil := clk.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if err := front.ApplyPatch(staleInfo.ID, map[string]string{
		"held_until":   heldUntil,
		"sleep_intent": "user-hold",
		"state":        string(sessionpkg.StateSuspended),
	}); err != nil {
		t.Fatalf("apply concurrent suspend patch: %v", err)
	}

	patch, err := applySessionLifecycleStatusHeal(tick, staleInfo.ID, sessionLifecycleStatusHealContext{
		Site:              sessionLifecycleStatusHealSiteDesired,
		RuntimeObserved:   true,
		RuntimeAlive:      true,
		LoadedRevision:    loadedRevision,
		RollbackAvailable: true,
	}, front, clk, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("apply status heal over stale snapshot: %v", err)
	}

	current, err := front.Get(staleInfo.ID)
	if err != nil {
		t.Fatalf("read durable row after heal: %v", err)
	}
	if current.MetadataState != string(sessionpkg.StateSuspended) {
		t.Fatalf("durable state = %q, want %q: the advisory heal destroyed the user suspend marker (heal patch %#v)",
			current.MetadataState, sessionpkg.StateSuspended, patch)
	}
	if current.SleepIntent != "user-hold" || current.HeldUntil != heldUntil {
		t.Fatalf("durable suspend cluster = {sleep_intent:%q held_until:%q}, want {user-hold %q}",
			current.SleepIntent, current.HeldUntil, heldUntil)
	}
	if patch != nil {
		t.Fatalf("stale-snapshot heal patch = %#v, want nil (the row moved under the heal)", patch)
	}
	if folded := tick.infoByID[staleInfo.ID]; folded.MetadataState == string(sessionpkg.StateAwake) {
		t.Fatalf("tick snapshot folded state = %q, want the refused heal not to be folded", folded.MetadataState)
	}
}

// TestLegacyStatusHealAppliesWhenRowIsUnchanged pins the other half of the fence:
// an uncontended advisory heal still lands and still folds.
func TestLegacyStatusHealAppliesWhenRowIsUnchanged(t *testing.T) {
	store := newStatusHealRaceStore(t)
	front := sessionFrontDoor(store)
	clk := &clock.Fake{Time: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)}

	info, loadedRevision := seedStatusHealRaceSession(t, store)
	tick := newReconcileTick([]sessionpkg.Info{info})

	patch, err := applySessionLifecycleStatusHeal(tick, info.ID, sessionLifecycleStatusHealContext{
		Site:              sessionLifecycleStatusHealSiteDesired,
		RuntimeObserved:   true,
		RuntimeAlive:      true,
		LoadedRevision:    loadedRevision,
		RollbackAvailable: true,
	}, front, clk, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("apply status heal: %v", err)
	}
	if patch["state"] != string(sessionpkg.StateAwake) {
		t.Fatalf("uncontended heal patch = %#v, want state=awake", patch)
	}
	current, err := front.Get(info.ID)
	if err != nil {
		t.Fatalf("read durable row after heal: %v", err)
	}
	if current.MetadataState != string(sessionpkg.StateAwake) {
		t.Fatalf("durable state = %q, want awake", current.MetadataState)
	}
	if folded := tick.infoByID[info.ID]; folded.MetadataState != string(sessionpkg.StateAwake) {
		t.Fatalf("tick snapshot folded state = %q, want awake", folded.MetadataState)
	}
}

// TestLegacyStatusHealFailsClosedWithoutARevision pins the partial-store rule: a
// state-changing heal with no load-time revision cannot be fenced, so on a
// conditional-write deployment it is skipped rather than applied blind.
func TestLegacyStatusHealFailsClosedWithoutARevision(t *testing.T) {
	store := newStatusHealRaceStore(t)
	front := sessionFrontDoor(store)
	clk := &clock.Fake{Time: time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)}

	info, _ := seedStatusHealRaceSession(t, store)
	tick := newReconcileTick([]sessionpkg.Info{info})

	patch, err := applySessionLifecycleStatusHeal(tick, info.ID, sessionLifecycleStatusHealContext{
		Site:              sessionLifecycleStatusHealSiteDesired,
		RuntimeObserved:   true,
		RuntimeAlive:      true,
		LoadedRevision:    0,
		RollbackAvailable: true,
	}, front, clk, 10*time.Second, nil)
	if err != nil {
		t.Fatalf("apply status heal without a revision: %v", err)
	}
	if patch != nil {
		t.Fatalf("unrevisioned heal patch = %#v, want nil", patch)
	}
	current, err := front.Get(info.ID)
	if err != nil {
		t.Fatalf("read durable row after heal: %v", err)
	}
	if current.MetadataState != "active" {
		t.Fatalf("durable state = %q, want the unfenceable heal to be skipped", current.MetadataState)
	}
}
