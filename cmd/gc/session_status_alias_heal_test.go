package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// seedWakeDemandedDrainingSession is the journey's own row, reduced: a live,
// explicitly woken session that still carries legacy's `active` alias. The wake
// marker is what makes it keyed-owned, and keyed ownership is what stands
// legacy's desired-site heal down (withLegacyStatusHealExclusion), so the keyed
// lane is the only heal owner the row has left.
func seedWakeDemandedDrainingSession(t *testing.T, env *reconcilerTestEnv) (*deadRuntimeProvider, string) {
	t.Helper()
	provider, bead := seedDrainingSession(t, env, "orphaned")
	if err := sessionFrontDoor(env.store).ApplyPatch(bead.ID, sessionpkg.MetadataPatch{
		"wake_request": string(sessionpkg.WakeCauseExplicit),
	}); err != nil {
		t.Fatalf("stamp explicit wake marker: %v", err)
	}
	info := readSessionInfoForTest(t, env, bead.ID)
	if info.MetadataState != string(sessionpkg.StateActive) {
		t.Fatalf("seeded state = %q, want the legacy %q alias", info.MetadataState, sessionpkg.StateActive)
	}
	if !resolveExactSessionStartOwnership(info, env.cfg, time.Now().UTC()) {
		t.Fatal("the fixture is not keyed-owned; legacy would still own its heal and the strand cannot occur")
	}
	return provider, bead.ID
}

// wakeDemandedDrainParams is newExactDrainAdvanceParams with the deployment's
// conditional writer supplied explicitly: the fixture store implements it but
// leaves the rollout gate unset, and the ordinary lane's status heal is wired to
// the resolved writer.
func wakeDemandedDrainParams(t *testing.T, env *reconcilerTestEnv, provider *deadRuntimeProvider) exactSessionStartParams {
	t.Helper()
	params := newExactDrainAdvanceParams(env, provider)
	writer, ok := env.store.(beads.ConditionalWriter)
	if !ok {
		t.Fatalf("session store %T does not implement conditional writes", env.store)
	}
	params.StatusWriter = writer
	return params
}

// TestKeyedPassHealsTheActiveAliasWhenADetectorFamilyClaimsTheRow is
// ga-f7v2ft.140's RED, and its two arms are the differential that names the
// cause. The rows are identical except for one in-memory drain intent: without
// it no family claims the key and the ordinary lane's status heal converges the
// alias; with it the detector-family seam claims the key and returns above that
// heal, and the row — already excluded from legacy's — converges nowhere.
func TestKeyedPassHealsTheActiveAliasWhenADetectorFamilyClaimsTheRow(t *testing.T) {
	t.Run("unclaimed_row_heals_on_the_ordinary_lane", func(t *testing.T) {
		env := newReconcilerTestEnv()
		provider, id := seedWakeDemandedDrainingSession(t, env)
		params := wakeDemandedDrainParams(t, env, provider)
		env.dt.remove(id)

		info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, id)
		if err != nil {
			t.Fatalf("authoritative read: %v", err)
		}
		if exactSessionDrainAdvanceCandidate(params, info, response) {
			t.Fatal("the control arm still carries drain intent; the seam would claim it")
		}
		if _, err := reconcileExactSessionStartWithOwner(t.Context(), drainAdvanceAdmission(id), params); err != nil {
			t.Fatalf("keyed pass on an unclaimed row: %v", err)
		}
		if state := readSessionInfoForTest(t, env, id).MetadataState; state != string(sessionpkg.StateAwake) {
			t.Fatalf("state = %q, want %q from the ordinary lane's status heal", state, sessionpkg.StateAwake)
		}
	})

	t.Run("family_claimed_row_still_heals", func(t *testing.T) {
		env := newReconcilerTestEnv()
		provider, id := seedWakeDemandedDrainingSession(t, env)
		params := wakeDemandedDrainParams(t, env, provider)

		before := readSessionInfoForTest(t, env, id)
		info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, id)
		if err != nil {
			t.Fatalf("authoritative read: %v", err)
		}
		if !exactSessionDrainAdvanceCandidate(params, info, response) {
			t.Fatal("the fixture is not a D-DRAIN candidate; the seam would never claim it")
		}

		owner, err := reconcileExactSessionStartWithOwner(t.Context(), drainAdvanceAdmission(id), params)
		if err != nil || owner != exactSessionStartKeyedOwner {
			t.Fatalf("keyed pass owner/err = %d/%v, want keyed ownership and no error", owner, err)
		}

		after := readSessionInfoForTest(t, env, id)
		if after.MetadataState != string(sessionpkg.StateAwake) {
			t.Fatalf("state after a family-claimed keyed pass = %q, want %q", after.MetadataState, sessionpkg.StateAwake)
		}
		if after.InstanceToken != before.InstanceToken || after.SessionNameMetadata != before.SessionNameMetadata ||
			after.Generation != before.Generation || after.WakeRequest != before.WakeRequest {
			t.Fatalf("alias heal moved identity: before=%+v after=%+v", before, after)
		}
		if !provider.IsRunning(exactDrainAdvanceTestSessionName) {
			t.Fatal("alias heal disturbed the live runtime; it is a metadata-only normalization")
		}
	})
}

// TestKeyedPassLeavesADeadRowsAliasToLegacy pins the fence the heal must keep:
// `active` on a row whose runtime is gone projects to asleep, not awake, and
// that transition changes the base state, so it stays with the lane that owns
// the sleep decision rather than being smuggled in as an alias normalization.
func TestKeyedPassLeavesADeadRowsAliasToLegacy(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, id := seedWakeDemandedDrainingSession(t, env)
	params := wakeDemandedDrainParams(t, env, provider)
	if err := provider.Stop(exactDrainAdvanceTestSessionName); err != nil {
		t.Fatalf("stop seeded runtime: %v", err)
	}

	if _, err := reconcileExactSessionStartWithOwner(t.Context(), drainAdvanceAdmission(id), params); err != nil {
		t.Fatalf("keyed pass on a dead row: %v", err)
	}
	if state := readSessionInfoForTest(t, env, id).MetadataState; state == string(sessionpkg.StateAwake) {
		t.Fatalf("state = %q, want a dead row never healed to awake", state)
	}
}

// TestKeyedPassRefusesTheAliasHealWhenLivenessIsUnproven keeps the same
// fail-closed rule D-DRAIN uses: an unreadable probe is not proof of life, so
// the row keeps its stored alias rather than being normalized on a guess.
func TestKeyedPassRefusesTheAliasHealWhenLivenessIsUnproven(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, id := seedWakeDemandedDrainingSession(t, env)
	params := wakeDemandedDrainParams(t, env, provider)
	provider.incomplete = true

	// The family parks on the same unreadable probe, so the pass returns its
	// refusal; what this pins is that no alias was written on the way out.
	_, _ = reconcileExactSessionStartWithOwner(t.Context(), drainAdvanceAdmission(id), params)
	if state := readSessionInfoForTest(t, env, id).MetadataState; state != string(sessionpkg.StateActive) {
		t.Fatalf("state = %q, want the stored alias retained when liveness is unproven", state)
	}
}

// TestKeyedPassHealsTheAliasOnAnAliveIncompleteObservation is ga-bxa8r's last
// arm. The heal's premise is positive liveness — the planner is asked what a
// LIVE row owes — so it never infers absence and scan completeness proved
// nothing for it. Demanding it anyway meant the alias could only ever be
// normalized on a host quiet enough to license an alive target's sweep, which is
// no host running agents: a live pane withholds the very tmux-absence license
// the /proc scan needs.
//
// The heal stays what it was: projection-neutral, revision-fenced, and reachable
// only from a POSITIVE observation. TestKeyedPassLeavesADeadRowsAliasToLegacy
// and TestKeyedPassRefusesTheAliasHealWhenLivenessIsUnproven above are the
// untouched controls that keep every non-positive observation out.
func TestKeyedPassHealsTheAliasOnAnAliveIncompleteObservation(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, id := seedWakeDemandedDrainingSession(t, env)
	params := wakeDemandedDrainParams(t, env, provider)
	provider.unlicensableAlive = true
	if !provider.IsRunning(exactDrainAdvanceTestSessionName) {
		t.Fatal("the fixture's runtime is not alive, so nothing here proves the positive arm was withheld")
	}

	owner, err := reconcileExactSessionStartWithOwner(t.Context(), drainAdvanceAdmission(id), params)
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("keyed pass owner/err = %d/%v, want keyed ownership and no error", owner, err)
	}
	if state := readSessionInfoForTest(t, env, id).MetadataState; state != string(sessionpkg.StateAwake) {
		t.Fatalf("state = %q, want %q: a positive observation licenses the alias heal", state, sessionpkg.StateAwake)
	}
}
