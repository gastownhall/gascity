package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// wakeFamilyCityConfig is the WD.10a fixture city: one always-on singleton
// dependency, one dependency-bearing singleton target (the canonical singleton
// shape sync actually produces for it), and one agent-capped pool for the Q1
// eligibility arm.
func wakeFamilyCityConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "database", StartCommand: "true", MaxActiveSessions: intPtr(1)},
			{Name: "dependent", StartCommand: "true", MaxActiveSessions: intPtr(1), DependsOn: []string{"database"}},
			{Name: "capped", StartCommand: "true", MaxActiveSessions: intPtr(3)},
		},
	}
}

// stampCanonicalSingleton applies exactly the markers syncSessionBeads stamps on
// a configured single-session agent's row (session_beads.go:1834-1839): pool
// managed plus ephemeral origin, and NO pool slot. This is the shape ga-ij8mh
// proved production always reaches within one tick.
func stampCanonicalSingleton(t *testing.T, env *reconcilerTestEnv, id string) {
	t.Helper()
	if err := env.store.SetMetadataBatch(id, map[string]string{
		"session_origin":       "ephemeral",
		poolManagedMetadataKey: "true",
	}); err != nil {
		t.Fatalf("stamp canonical singleton markers: %v", err)
	}
}

// slotizedPoolMemberEnv is a canonical strict-default pool MEMBER: pool-managed,
// slotized, trigger-bound, named by PoolSessionName. It is the shape the
// strict-default wake family owns, and the only thing the Q1 cases vary is the
// agent's max_active_sessions.
type slotizedPoolMemberEnv struct {
	*reconcilerTestEnv
	poolMemberID string
}

func newSlotizedPoolMemberEnv(t *testing.T, maximum *int) slotizedPoolMemberEnv {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: maximum}},
	}
	bead := env.createSessionBead("worker-1", "worker")
	env.setSessionMetadata(&bead, map[string]string{
		"session_name":                          PoolSessionName("worker", bead.ID),
		"agent_name":                            "worker-1",
		"session_origin":                        "ephemeral",
		poolManagedMetadataKey:                  "true",
		"pool_slot":                             "1",
		beadmeta.TriggerBeadIDMetadataKey:       "ga-work-1",
		beadmeta.TriggerBeadStoreRefMetadataKey: "city:test-city",
	})
	requestExplicitWake(t, env, bead.ID)
	return slotizedPoolMemberEnv{reconcilerTestEnv: env, poolMemberID: bead.ID}
}

func requestExplicitWake(t *testing.T, env *reconcilerTestEnv, id string) {
	t.Helper()
	if err := env.store.SetMetadataBatch(id, sessionpkg.RequestExplicitWakePatch(string(sessionpkg.WakeCauseExplicit), env.clk.Now())); err != nil {
		t.Fatalf("request explicit wake: %v", err)
	}
}

// TestConfiguredDependencyWakeOwnsSyncProducedCanonicalSingleton is the WD.10a
// target-shape RED (ga-ij8mh ruling 4, amendment 1). The family must own the
// canonical singleton row sync produces for a dependency-bearing single-session
// agent, and the two wake families must partition on SLOT markers rather than on
// pool_managed.
func TestConfiguredDependencyWakeOwnsSyncProducedCanonicalSingleton(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = wakeFamilyCityConfig()
	dependency := env.createSessionBead("database", "database")
	env.markSessionActive(&dependency)
	env.addDesired("database", "database", true)
	dependencyBefore := env.sessionInfo(dependency.ID)

	target := env.createSessionBead("dependent", "dependent")
	stampCanonicalSingleton(t, env, target.ID)
	requestExplicitWake(t, env, target.ID)

	info := env.sessionInfo(target.ID)
	if !isPoolManagedSessionInfo(info) {
		t.Fatal("fixture target is not pool-managed; the sync-produced shape is the whole point of this test")
	}
	if !isCanonicalPoolManagedSessionInfoForTemplate(info, "dependent") {
		t.Fatalf("fixture target is not a canonical singleton: %+v", info)
	}

	lease, certified := certifyConfiguredDependencyStartLease(info, env.cfg, env.sp, "test-city", env.store, 1, env.clk.Now().UTC())
	if !certified {
		t.Fatal("certifyConfiguredDependencyStartLease refused the sync-produced canonical singleton row")
	}

	params := exactSessionStartTestParams(t, env)
	params.Generation = 1
	params.RolloutMode = rollout.Auto
	params.ValidateConfiguredDependencyStart = func(current sessionpkg.Info, retained configuredDependencyStartLease) bool {
		return configuredDependencyStartTargetMatches(current, env.cfg, retained) &&
			allDependenciesAliveForTemplateWithClock(retained.TargetTemplate, env.cfg, nil, env.sp, "test-city", env.store, env.clk)
	}
	params.EnterConfiguredDependencyStart = func(configuredDependencyStartLease) bool { return true }

	owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
		SessionID:            target.ID,
		Source:               sessionStartAdmissionSocket,
		ConfiguredDependency: &lease,
	}, params)
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("reconcile result = (owner=%v, err=%v), want keyed success", owner, err)
	}
	if got := env.sp.CountCalls("Start", "dependent"); got != 1 {
		t.Fatalf("target provider Start calls = %d, want exactly one keyed start", got)
	}
	started := env.sessionInfo(target.ID)
	if started.ID != target.ID || started.MetadataState != string(sessionpkg.StateActive) || started.WakeRequest != "" {
		t.Fatalf("started target = %+v, want the same canonical bead active with wake consumed", started)
	}
	if !isCanonicalPoolManagedSessionInfoForTemplate(started, "dependent") {
		t.Fatalf("keyed start dropped the canonical singleton shape: %+v", started)
	}
	after := env.sessionInfo(dependency.ID)
	if after.ID != dependencyBefore.ID || after.InstanceToken != dependencyBefore.InstanceToken {
		t.Fatalf("dependency identity changed: before=%+v after=%+v", dependencyBefore, after)
	}
}

// TestConfiguredDependencyWakeRefusesSlotizedPoolRow is the partition negative:
// the families split on slot markers, so a slotized row stays strict-pool's even
// though it is pool-managed exactly like the canonical singleton.
func TestConfiguredDependencyWakeRefusesSlotizedPoolRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = wakeFamilyCityConfig()
	dependency := env.createSessionBead("database", "database")
	env.markSessionActive(&dependency)
	env.addDesired("database", "database", true)

	target := env.createSessionBead("dependent", "dependent")
	stampCanonicalSingleton(t, env, target.ID)
	if err := env.store.SetMetadataBatch(target.ID, map[string]string{"pool_slot": "1"}); err != nil {
		t.Fatalf("stamp pool slot: %v", err)
	}
	requestExplicitWake(t, env, target.ID)

	if _, certified := certifyConfiguredDependencyStartLease(
		env.sessionInfo(target.ID), env.cfg, env.sp, "test-city", env.store, 1, env.clk.Now().UTC(),
	); certified {
		t.Fatal("configured-dependency family certified a slotized pool row; the families partition on slot markers")
	}
}

// TestStrictDefaultPoolWakeEligibilityIsSupported is the Q1 RED. The uniform
// predicate contract makes eligibility supported() at every pool-family site, so
// an agent-capped pool becomes wake-eligible where the reason-narrowed predicate
// silently refused it.
func TestStrictDefaultPoolWakeEligibilityIsSupported(t *testing.T) {
	for _, tc := range []struct {
		name    string
		maximum *int
		want    bool
	}{
		{name: "unlimited", maximum: intPtr(-1), want: true},
		{name: "agent-capped", maximum: intPtr(3), want: true},
		{name: "canonical-singleton", maximum: intPtr(1), want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newSlotizedPoolMemberEnv(t, tc.maximum)
			info, revision, err := getAuthoritativeSessionStartRecord(env.store, env.poolMemberID)
			if err != nil {
				t.Fatalf("read strict-default pool member: %v", err)
			}
			_, certified := certifyStrictDefaultPoolWakeStartLease(info, revision, env.cfg, 1, env.clk.Now().UTC())
			if certified != tc.want {
				t.Fatalf("certifyStrictDefaultPoolWakeStartLease certified = %t, want %t (uniform predicate contract: eligibility is supported(), singleton excluded by identity)", certified, tc.want)
			}
		})
	}
}

// TestWaitDependencyPoolPredicatesFollowUniformContract pins the F13 fold of the
// remaining two pool-predicate spellings — and pins that it is a RE-SPELLING, not
// a widening. Unlike the two sites Q1 indicted, these fused eligibility with the
// CAPACITY clause: under supported() the reason is EligibleAgentCap exactly when
// a cap exists, so `reason == EligibleAgentCap && max > 1` was already "there is
// a cap above the singleton". The two answers therefore differ, and must: an
// unlimited pool is ELIGIBLE to resume but owes no membership witness, because
// there is no cap for membership to witness against.
func TestWaitDependencyPoolPredicatesFollowUniformContract(t *testing.T) {
	for _, tc := range []struct {
		name        string
		maximum     *int
		eligible    bool
		witnessOwed bool
	}{
		{name: "unlimited", maximum: intPtr(-1), eligible: true, witnessOwed: false},
		{name: "agent-capped", maximum: intPtr(3), eligible: true, witnessOwed: true},
		{name: "canonical-singleton", maximum: intPtr(1), eligible: false, witnessOwed: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newSlotizedPoolMemberEnv(t, tc.maximum)
			info := env.sessionInfo(env.poolMemberID)
			target, bounded := waitDependencyBoundedPoolTarget(info, env.cfg)
			if bounded != tc.witnessOwed {
				t.Fatalf("waitDependencyBoundedPoolTarget bounded = %t (target=%q), want %t", bounded, target, tc.witnessOwed)
			}
			if got := waitDependencyConfiguredTemplateEligible(info, env.cfg, env.sp, "test-city", env.store, env.clk.Now().UTC()); got != tc.eligible {
				t.Fatalf("waitDependencyConfiguredTemplateEligible = %t, want %t", got, tc.eligible)
			}
		})
	}
}

// TestWakeCurrentSingletonSurvivesEveryReaper is the sweep-rule RED (amendment
// 4) at its full width. A canonical singleton row with a CURRENT explicit wake
// must survive every path that reaps undesired rows — the undesired-pool sweep
// the amendment named, plus the two that reach the same row first post-batch-3:
// the acting D-ORPHAN close family (detection AND handler re-derivation) and
// legacy's own arms. A consumed or absent wake must still reap, or the guard
// would strand rows.
func TestWakeCurrentSingletonSurvivesEveryReaper(t *testing.T) {
	for _, tc := range []struct {
		name         string
		wake         bool
		wantPreserve bool
	}{
		{name: "current-wake-preserved", wake: true, wantPreserve: true},
		{name: "no-wake-reaped", wake: false, wantPreserve: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = wakeFamilyCityConfig()
			target := env.createSessionBead("dependent", "dependent")
			stampCanonicalSingleton(t, env, target.ID)
			if tc.wake {
				requestExplicitWake(t, env, target.ID)
			}
			info := env.sessionInfo(target.ID)
			now := env.clk.Now().UTC()

			// The single predicate every reaper answers from.
			if got := wakeCurrentSingletonPreservesUndesiredRow(info, env.cfg, now); got != tc.wantPreserve {
				t.Fatalf("wakeCurrentSingletonPreservesUndesiredRow = %t, want %t", got, tc.wantPreserve)
			}
			row, err := env.store.Get(target.ID)
			if err != nil {
				t.Fatalf("reload row: %v", err)
			}
			if got := wakeCurrentSingletonPreservesUndesiredBead(row, env.cfg, now); got != tc.wantPreserve {
				t.Fatalf("wakeCurrentSingletonPreservesUndesiredBead = %t, want %t (bead mirror disagrees with the Info spelling)", got, tc.wantPreserve)
			}

			// D-ORPHAN, detection side: an undesired row is normally claimed and
			// enqueued for close; a wake-current singleton must not be.
			in := detectorSweepInput{
				CityPath: "test-city",
				CityName: "test-city",
				Cfg:      env.cfg,
				Provider: env.sp,
				Rows:     []sessionpkg.ReconcileSession{{Info: info}},
				Desired:  map[string]TemplateParams{},
				Clock:    env.clk,
				Trigger:  "patrol",
			}
			result := detectSessionConditions(t.Context(), in)
			claimedByOrphan := false
			for _, cond := range result.Conditions {
				if cond.Family == detectorFamilyOrphan {
					claimedByOrphan = true
				}
			}
			if claimedByOrphan == tc.wantPreserve {
				t.Fatalf("D-ORPHAN claimed the row = %t, want %t: %#v", claimedByOrphan, !tc.wantPreserve, result.Conditions)
			}

			// D-ORPHAN, handler re-derivation: the same rule, answered from the
			// same predicate, so the two sides cannot disagree about the row.
			params := exactSessionStartTestParams(t, env)
			params.DesiredSessionNames = func() map[string]bool { return map[string]bool{"other": true} }
			_, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, target.ID)
			if err != nil {
				t.Fatalf("read persisted response: %v", err)
			}
			reason := exactSessionOrphanCloseCandidate(params, info, response, env.clk)
			if (reason == "") != tc.wantPreserve {
				t.Fatalf("exactSessionOrphanCloseCandidate = %q, want preserved=%t", reason, tc.wantPreserve)
			}
		})
	}
}

// TestPoolSweepPreservesCanonicalSingletonWithCurrentWake is the sweep-rule RED
// (amendment 4). The undesired-pool sweep must not reap the very row an explicit
// wake is currently asking the wake family to start.
func TestPoolSweepPreservesCanonicalSingletonWithCurrentWake(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wake      bool
		wantSwept int
	}{
		{name: "current-wake-preserved", wake: true, wantSwept: 0},
		{name: "no-wake-swept", wake: false, wantSwept: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = wakeFamilyCityConfig()
			target := env.createSessionBead("dependent", "dependent")
			stampCanonicalSingleton(t, env, target.ID)
			if tc.wake {
				requestExplicitWake(t, env, target.ID)
			}
			row, err := env.store.Get(target.ID)
			if err != nil {
				t.Fatalf("reload sweep candidate: %v", err)
			}
			snapshot := newSessionBeadSnapshot([]beads.Bead{row})
			swept := sweepUndesiredPoolSessionBeads(
				"", beads.SessionStore{Store: env.store}, nil, snapshot, map[string]TemplateParams{}, env.cfg, env.sp, false,
			)
			if swept != tc.wantSwept {
				t.Fatalf("sweepUndesiredPoolSessionBeads swept %d rows, want %d", swept, tc.wantSwept)
			}
			after := env.sessionInfo(target.ID)
			if tc.wake && (after.Closed || after.MetadataState == "gc_swept") {
				t.Fatalf("wake-current canonical singleton was reaped: %+v", after)
			}
		})
	}
}

// TestDetectorWakeRoutesNamedAndDependencyArmsOnly pins the D-WAKE act frontier.
// The named and configured-dependency arms crossed at WD.10a; the slotized
// pool-member wake arm crossed at WD.10b behind the SAME certified-lease entry
// (AdmitStrictDefaultPoolWake, already covered by the existing start-family
// yield). All three route under one admission source, because all three are one
// arm behind one lease surface and one yield.
func TestDetectorWakeRoutesNamedAndDependencyArmsOnly(t *testing.T) {
	if !detectorActWakeNamedDependency {
		t.Fatal("detectorActWakeNamedDependency must be true from WD.10a: the certified named/dependency wake admissions and the legacy wake yield have both landed")
	}
	if !detectorActWakePoolFill {
		t.Fatal("detectorActWakePoolFill must be true from WD.10b: pool-under-min fill landed with the allocation-ownership seam as its legacy yield")
	}
	for _, tc := range []struct {
		name       string
		reason     TraceReasonCode
		outcome    TraceOutcomeCode
		wantSource sessionStartAdmissionSource
		wantRoute  bool
	}{
		{name: "named", reason: detectorReasonWakeTargetNamed, outcome: TraceOutcomeStartCandidate, wantSource: sessionStartAdmissionWakeFill, wantRoute: true},
		{name: "dependency", reason: detectorReasonWakeTargetDependency, outcome: TraceOutcomeStartCandidate, wantSource: sessionStartAdmissionWakeFill, wantRoute: true},
		{name: "slotized-pool-member", reason: detectorReasonWakeTarget, outcome: TraceOutcomeStartCandidate, wantSource: sessionStartAdmissionWakeFill, wantRoute: true},
		{name: "pool-under-min-fill", reason: detectorReasonWakePoolFill, outcome: TraceOutcomeStartCandidate, wantRoute: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, routable := detectorAdmissionSourceFor(detectorCondition{
				Family:  detectorFamilyWake,
				Reason:  tc.reason,
				Outcome: tc.outcome,
			})
			if routable != tc.wantRoute {
				t.Fatalf("detectorAdmissionSourceFor routable = %t, want %t", routable, tc.wantRoute)
			}
			if routable && source != tc.wantSource {
				t.Fatalf("detectorAdmissionSourceFor source = %q, want %q", source, tc.wantSource)
			}
		})
	}
}

// TestDetectWakeSplitsTargetArmsByShape proves detection separates the arms the
// act frontier routes on: a named row and a canonical singleton dependency row
// carry the routing reasons, a slotized pool row keeps the shadow-only one.
func TestDetectWakeSplitsTargetArmsByShape(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = wakeFamilyCityConfig()
	env.cfg.NamedSessions = []config.NamedSession{{Template: "database", Mode: "always"}}

	named := env.createSessionBead("database", "database")
	if err := env.store.SetMetadataBatch(named.ID, map[string]string{
		"session_origin":            "named",
		"configured_named_identity": "database",
		"configured_named_session":  "true",
	}); err != nil {
		t.Fatalf("stamp named row: %v", err)
	}
	dependent := env.createSessionBead("dependent", "dependent")
	stampCanonicalSingleton(t, env, dependent.ID)
	slotized := env.createSessionBead("pool-1", "capped")
	if err := env.store.SetMetadataBatch(slotized.ID, map[string]string{
		"session_origin":       "ephemeral",
		poolManagedMetadataKey: "true",
		"pool_slot":            "1",
	}); err != nil {
		t.Fatalf("stamp slotized row: %v", err)
	}

	want := map[string]TraceReasonCode{
		named.ID:     detectorReasonWakeTargetNamed,
		dependent.ID: detectorReasonWakeTargetDependency,
		slotized.ID:  detectorReasonWakeTarget,
	}
	for id, wantReason := range want {
		info := env.sessionInfo(id)
		if got := detectorWakeTargetReason(info, env.cfg); got != wantReason {
			t.Errorf("detectorWakeTargetReason(%s) = %q, want %q", id, got, wantReason)
		}
	}
}

// TestKeyedWakeSeamClosesPreLeaseOwnershipWindow is the SECOND-entry-gate RED
// (amendment 2). The losing interleave is the pre-lease window: an in-process
// admission classifies the canonical singleton row legacy-owned, yields, and the
// resulting fallback poke lets legacy's PreWakePatch consume wake_request before
// any certified lease exists. With the seam closed the handler certifies from the
// row it already read and never reports a legacy fallback.
func TestKeyedWakeSeamClosesPreLeaseOwnershipWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		presync    bool
		certifies  bool
		wantOwner  exactSessionStartOwner
		wantCalled bool
	}{
		{name: "seam-closed", certifies: true, wantOwner: exactSessionStartKeyedOwner, wantCalled: true},
		{name: "losing-interleave", certifies: false, wantOwner: exactSessionStartLegacyOwner, wantCalled: true},
		// The wake write lands BEFORE sync stamps the pool markers, so the seam
		// has to fire on the pre-stamp row too. Screening on the stamped shape
		// alone surrenders the row at exactly the moment the race is decided.
		{name: "seam-closed-before-sync-stamp", presync: true, certifies: true, wantOwner: exactSessionStartKeyedOwner, wantCalled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			env.cfg = wakeFamilyCityConfig()
			dependency := env.createSessionBead("database", "database")
			env.markSessionActive(&dependency)
			env.addDesired("database", "database", true)

			target := env.createSessionBead("dependent", "dependent")
			if !tc.presync {
				stampCanonicalSingleton(t, env, target.ID)
			}
			requestExplicitWake(t, env, target.ID)
			before := env.sessionInfo(target.ID)

			called := false
			params := exactSessionStartTestParams(t, env)
			params.Generation = 1
			params.RolloutMode = rollout.Auto
			params.CertifyWakeFamilyStart = func(info sessionpkg.Info, revision int64) bool {
				called = true
				if info.ID != target.ID {
					t.Fatalf("pre-lease certification saw %q, want %q", info.ID, target.ID)
				}
				if revision == 0 {
					t.Fatal("pre-lease certification received a zero row revision")
				}
				return tc.certifies
			}

			owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
				SessionID: target.ID,
				Source:    sessionStartAdmissionInProcess,
			}, params)
			if err != nil {
				t.Fatalf("reconcile err = %v, want nil", err)
			}
			if called != tc.wantCalled {
				t.Fatalf("pre-lease certification called = %t, want %t", called, tc.wantCalled)
			}
			if owner != tc.wantOwner {
				t.Fatalf("owner = %v, want %v", owner, tc.wantOwner)
			}
			after := env.sessionInfo(target.ID)
			if after.WakeRequest != before.WakeRequest || after.MetadataState != before.MetadataState {
				t.Fatalf("pre-lease seam mutated the row: before=%+v after=%+v", before, after)
			}
		})
	}

	// The other half of amendment 2 (ga-ij8mh, sixth round). The seam above
	// narrows the window from the keyed side, but run 13 proved it does not
	// close it: legacy still owns the classification, and a tick already past
	// its loop-top exclusion when the lease lands is not fenced by it. Both
	// writers entered and the row was orphan-closed. The legacy side must stand
	// down on the certified lease at its own effect boundary.
	t.Run("run-13-legacy-stands-down-after-the-lease-lands", func(t *testing.T) {
		f := newWakeStandDownFixture(t)
		snapshot := f.env.sessionInfo(f.targetID)

		params := exactSessionStartTestParams(t, f.env)
		params.Generation = 1
		params.RolloutMode = rollout.Auto
		params.CertifyWakeFamilyStart = func(sessionpkg.Info, int64) bool {
			f.admitCertifiedWake(t)
			return true
		}
		owner, err := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
			SessionID: f.targetID,
			Source:    sessionStartAdmissionInProcess,
		}, params)
		if err != nil || owner != exactSessionStartKeyedOwner {
			t.Fatalf("pre-lease seam = (%v, %v), want the keyed owner", owner, err)
		}

		prepared, prepareErr := f.prepareLegacyStart(snapshot, f.legacyExclusion(t))
		if !errors.Is(prepareErr, errPreWakeSuperseded) || prepared != nil {
			t.Fatalf("legacy prepare after the lease landed = %v (prepared=%t), want errPreWakeSuperseded", prepareErr, prepared != nil)
		}
		if got := f.env.sp.CountCalls("Start", "dependent"); got != 0 {
			t.Fatalf("second provider start after the certified lease = %d, want 0", got)
		}
	})
}

// TestStrictDefaultPoolWakeWitnessExcludesOwnOccupancy is the capacity half of
// the Q1 contract (clause 2): a capped pool at capacity refuses the wake, one
// below capacity takes it, and the woken member's OWN occupancy is never counted
// against the cap it is re-entering.
func TestStrictDefaultPoolWakeWitnessExcludesOwnOccupancy(t *testing.T) {
	for _, tc := range []struct {
		name         string
		maximum      int
		occupied     int
		selfOccupied bool
		want         bool
	}{
		{name: "below-capacity", maximum: 3, occupied: 1, want: true},
		{name: "at-capacity", maximum: 3, occupied: 3, want: false},
		{name: "at-capacity-self-excluded", maximum: 3, occupied: 3, selfOccupied: true, want: true},
		{name: "unlimited", maximum: -1, occupied: 9, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := strictDefaultPoolWakeCapacityAvailable(tc.maximum, tc.occupied, tc.selfOccupied)
			if got != tc.want {
				t.Fatalf("strictDefaultPoolWakeCapacityAvailable(max=%d, occupied=%d, self=%t) = %t, want %t",
					tc.maximum, tc.occupied, tc.selfOccupied, got, tc.want)
			}
		})
	}
}

// recordingWakeAdmitter records D-WAKE's certified-lease admissions. The wake
// family cannot ride the bare Admit entry, so the two admitters are separate and
// a key arriving on the wrong one is itself a failure.
type recordingWakeAdmitter struct {
	wakeKeys []string
	bareKeys []string
}

func (r *recordingWakeAdmitter) admitWake(id string) (sessionStartAdmissionOutcome, error) {
	r.wakeKeys = append(r.wakeKeys, id)
	return sessionStartAdmissionAccepted, nil
}

func (r *recordingWakeAdmitter) admit(id string, _ sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error) {
	r.bareKeys = append(r.bareKeys, id)
	return sessionStartAdmissionAccepted, nil
}

// TestDetectorWakeRoutesDemandThroughCertifiedLeases is WD.10a's primary sweep
// RED: a configured-named row and a canonical singleton dependency row that the
// awake set wants and the liveness probe found dead are handed to the certified
// wake admission by exact key, while a slotized pool member is detected and left
// for WD.10b. No wake key may reach the bare Admit entry.
func TestDetectorWakeRoutesDemandThroughCertifiedLeases(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = wakeFamilyCityConfig()
	env.cfg.NamedSessions = []config.NamedSession{{Name: "database", Template: "database", Mode: "always"}}

	named := env.createSessionBead("database", "database")
	env.setSessionMetadata(&named, map[string]string{
		"session_origin":            "named",
		"configured_named_identity": "database",
		"configured_named_session":  "true",
	})
	requestExplicitWake(t, env, named.ID)

	dependent := env.createSessionBead("dependent", "dependent")
	stampCanonicalSingleton(t, env, dependent.ID)
	requestExplicitWake(t, env, dependent.ID)

	slotized := env.createSessionBead("capped-1", "capped")
	env.setSessionMetadata(&slotized, map[string]string{
		"session_origin":       "ephemeral",
		poolManagedMetadataKey: "true",
		"pool_slot":            "1",
	})
	requestExplicitWake(t, env, slotized.ID)

	rows := []sessionpkg.ReconcileSession{}
	desired := map[string]TemplateParams{}
	for _, id := range []string{named.ID, dependent.ID, slotized.ID} {
		info := env.sessionInfo(id)
		rows = append(rows, sessionpkg.ReconcileSession{Info: info})
		desired[info.SessionNameMetadata] = TemplateParams{SessionName: info.SessionNameMetadata, TemplateName: info.Template}
	}

	admitter := &recordingWakeAdmitter{}
	in := detectorSweepInput{
		CityPath:  "test-city",
		CityName:  "test-city",
		Cfg:       env.cfg,
		Provider:  env.sp,
		Rows:      rows,
		Desired:   desired,
		Clock:     env.clk,
		Trigger:   "patrol",
		Admit:     admitter.admit,
		AdmitWake: admitter.admitWake,
	}
	result := detectSessionConditions(t.Context(), in)
	routeDetectorConditions(in, &result)

	routed := map[string]TraceReasonCode{}
	for _, cond := range result.Conditions {
		if cond.Family != detectorFamilyWake {
			continue
		}
		routed[cond.SessionID] = cond.Reason
		if cond.Reason == detectorReasonWakeTarget && cond.AdmissionSource != sessionStartAdmissionWakeFill {
			t.Fatalf("slotized pool-member wake arm did not route under the certified wake lease: %#v", cond)
		}
	}
	for id, want := range map[string]TraceReasonCode{
		named.ID:     detectorReasonWakeTargetNamed,
		dependent.ID: detectorReasonWakeTargetDependency,
		slotized.ID:  detectorReasonWakeTarget,
	} {
		if got, ok := routed[id]; !ok || got != want {
			t.Errorf("wake condition for %s = %q (present=%t), want %q", id, got, ok, want)
		}
	}
	if len(admitter.bareKeys) != 0 {
		t.Fatalf("wake keys reached the bare Admit entry without a certificate: %v", admitter.bareKeys)
	}
	wantWake := map[string]bool{named.ID: true, dependent.ID: true, slotized.ID: true}
	if len(admitter.wakeKeys) != len(wantWake) {
		t.Fatalf("certified wake admissions = %v, want exactly %v", admitter.wakeKeys, wantWake)
	}
	for _, id := range admitter.wakeKeys {
		if !wantWake[id] {
			t.Fatalf("certified wake admissions = %v, want exactly %v", admitter.wakeKeys, wantWake)
		}
	}
}

// TestDetectorWakeRefusesRoutingWithoutCertifiedAdmission is the fail-closed
// negative for the same seam: a sweep whose call site cannot mint a certificate
// records a traced refusal instead of admitting the key bare.
func TestDetectorWakeRefusesRoutingWithoutCertifiedAdmission(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = wakeFamilyCityConfig()
	dependent := env.createSessionBead("dependent", "dependent")
	stampCanonicalSingleton(t, env, dependent.ID)
	requestExplicitWake(t, env, dependent.ID)
	info := env.sessionInfo(dependent.ID)

	admitter := &recordingWakeAdmitter{}
	in := detectorSweepInput{
		CityPath: "test-city",
		CityName: "test-city",
		Cfg:      env.cfg,
		Provider: env.sp,
		Rows:     []sessionpkg.ReconcileSession{{Info: info}},
		Desired: map[string]TemplateParams{
			info.SessionNameMetadata: {SessionName: info.SessionNameMetadata, TemplateName: info.Template},
		},
		Clock:   env.clk,
		Trigger: "patrol",
		Admit:   admitter.admit,
	}
	result := detectSessionConditions(t.Context(), in)
	routeDetectorConditions(in, &result)

	refused := false
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyWake && cond.AdmissionOutcome == detectorAdmissionRefusedUncertifiable {
			refused = true
		}
	}
	if !refused {
		t.Fatalf("wake condition without a certified admission entry was not refused: %#v", result.Conditions)
	}
	if len(admitter.bareKeys) != 0 {
		t.Fatalf("wake key fell back to the bare Admit entry: %v", admitter.bareKeys)
	}
}
