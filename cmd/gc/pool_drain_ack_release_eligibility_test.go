package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// A drain acknowledgement hands a pool seat BACK. Gating it on supported() —
// the predicate that answers whether the keyed lane may TAKE a seat — is a
// category error, and it is not a theoretical one: every capacity-shaped reason
// returns false from supported(), so a city with `[workspace]
// max_active_sessions` set refused every agent acknowledgement it ever saw. The
// row stayed in draining, kept owning its session name, and the pool slot was
// gone for the life of the city (mc-by7s: 105 rows, 29 slots).

// TestPoolAllocationShadowPolicyReleaseEligibilityPartitionsOnOwnership pins
// the split the two predicates encode: capacity shapes bound what may be taken
// and never disqualify a release, while the identity-model exclusions say the
// keyed lane does not own this pool at all and disqualify both.
func TestPoolAllocationShadowPolicyReleaseEligibilityPartitionsOnOwnership(t *testing.T) {
	base := func() (*config.City, *config.Agent) {
		cfg := &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Rigs:      []config.Rig{{Name: "rig"}},
			Agents:    []config.Agent{{Name: "worker", Dir: "rig"}},
		}
		return cfg, &cfg.Agents[0]
	}

	tests := []struct {
		name             string
		mutate           func(*config.City, *config.Agent) map[string]struct{}
		wantReason       poolAllocationShadowReason
		wantSupported    bool
		wantReleaseAllow bool
	}{
		// Capacity shapes. Only the first two are allocation-eligible, but all
		// of them describe a bound on taking, so all of them may release.
		{name: "default multi-session pool", wantReason: poolAllocationShadowEligible, wantSupported: true, wantReleaseAllow: true},
		{name: "agent cap", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			maximum := 4
			agent.MaxActiveSessions = &maximum
			return nil
		}, wantReason: poolAllocationShadowEligibleAgentCap, wantSupported: true, wantReleaseAllow: true},
		{name: "minimum floor", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			minimum := 1
			agent.MinActiveSessions = &minimum
			return nil
		}, wantReason: poolAllocationShadowMinFloor, wantSupported: false, wantReleaseAllow: true},
		{name: "workspace cap", mutate: func(cfg *config.City, _ *config.Agent) map[string]struct{} {
			maximum := 100
			cfg.Workspace.MaxActiveSessions = &maximum
			return nil
		}, wantReason: poolAllocationShadowWorkspaceCap, wantSupported: false, wantReleaseAllow: true},
		{name: "rig cap", mutate: func(cfg *config.City, _ *config.Agent) map[string]struct{} {
			maximum := 10
			cfg.Rigs[0].MaxActiveSessions = &maximum
			return nil
		}, wantReason: poolAllocationShadowRigCap, wantSupported: false, wantReleaseAllow: true},
		{name: "namepool", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.Namepool = "names.txt"
			return nil
		}, wantReason: poolAllocationShadowNamepool, wantSupported: false, wantReleaseAllow: true},

		// Identity-model exclusions. These say the pool is not the keyed lane's,
		// so neither predicate admits them.
		{name: "custom scale check", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.ScaleCheck = "printf 1"
			return nil
		}, wantReason: poolAllocationShadowCustomScaleCheck, wantSupported: false, wantReleaseAllow: false},
		{name: "dependency-bearing template", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			agent.DependsOn = []string{"database"}
			return nil
		}, wantReason: poolAllocationShadowDependencies, wantSupported: false, wantReleaseAllow: false},
		{name: "named session binding", mutate: func(_ *config.City, agent *config.Agent) map[string]struct{} {
			return map[string]struct{}{agent.QualifiedName(): {}}
		}, wantReason: poolAllocationShadowNamedSession, wantSupported: false, wantReleaseAllow: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, agent := base()
			var namedTemplates map[string]struct{}
			if tc.mutate != nil {
				namedTemplates = tc.mutate(cfg, agent)
			}
			policy := newPoolAllocationShadowPolicy(cfg, agent, namedTemplates)
			if policy.reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", policy.reason, tc.wantReason)
			}
			if got := policy.supported(); got != tc.wantSupported {
				t.Fatalf("supported() = %t, want %t", got, tc.wantSupported)
			}
			if got := policy.releaseEligible(); got != tc.wantReleaseAllow {
				t.Fatalf("releaseEligible() = %t, want %t", got, tc.wantReleaseAllow)
			}
		})
	}
}

// TestAuthorizeRoutedWorkPoolDrainAckReleasesUnderCapacityShapes is the live
// half: the same acknowledgement the fixture proves authorizable must stay
// authorizable once the city grows a capacity bound. Each case asserts the
// control too — that the bound really does flip supported() — so the test
// cannot pass vacuously against a config mutation that never took effect.
func TestAuthorizeRoutedWorkPoolDrainAckReleasesUnderCapacityShapes(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*config.City, *config.Agent)
		wantReason poolAllocationShadowReason
	}{
		{name: "workspace cap", mutate: func(cfg *config.City, _ *config.Agent) {
			maximum := 100
			cfg.Workspace.MaxActiveSessions = &maximum
		}, wantReason: poolAllocationShadowWorkspaceCap},
		{name: "minimum floor", mutate: func(_ *config.City, agent *config.Agent) {
			minimum := 1
			agent.MinActiveSessions = &minimum
		}, wantReason: poolAllocationShadowMinFloor},
		{name: "namepool", mutate: func(_ *config.City, agent *config.Agent) {
			agent.Namepool = "names.txt"
		}, wantReason: poolAllocationShadowNamepool},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
			agent := findAgentByTemplate(fixture.snapshot.Config, fixture.lease.PoolTarget)
			if agent == nil {
				t.Fatalf("no agent for template %q", fixture.lease.PoolTarget)
			}
			tc.mutate(fixture.snapshot.Config, agent)

			// Control: the mutation must actually reach the policy, and it must
			// be one the allocation predicate refuses. Without this, a mutation
			// that silently no-ops would still let the assertion below pass.
			policy := newPoolAllocationShadowPolicy(fixture.snapshot.Config, agent, nil)
			if policy.reason != tc.wantReason {
				t.Fatalf("policy reason = %q, want %q", policy.reason, tc.wantReason)
			}
			if policy.supported() {
				t.Fatalf("control failed: %q is allocation-eligible, so it cannot show the release regression", tc.wantReason)
			}

			authorized, refusal, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, fixture.info, fixture.lease)
			if err != nil || !authorized || refusal != drainAckRefusalNone {
				t.Fatalf("authorization under %q = (%t, %q, %v), want the release to hold", tc.wantReason, authorized, refusal, err)
			}
		})
	}
}

// TestAuthorizeRoutedWorkPoolDrainAckRefusesIdentityModelExclusions is the
// negative that keeps releaseEligible from degenerating into "always true": a
// pool the keyed lane does not own must still be handed back to legacy.
func TestAuthorizeRoutedWorkPoolDrainAckRefusesIdentityModelExclusions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*config.Agent)
	}{
		{name: "custom scale check", mutate: func(agent *config.Agent) { agent.ScaleCheck = "printf 1" }},
		{name: "dependency-bearing template", mutate: func(agent *config.Agent) { agent.DependsOn = []string{"database"} }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
			agent := findAgentByTemplate(fixture.snapshot.Config, fixture.lease.PoolTarget)
			if agent == nil {
				t.Fatalf("no agent for template %q", fixture.lease.PoolTarget)
			}
			tc.mutate(agent)

			authorized, refusal, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, fixture.info, fixture.lease)
			if err != nil || authorized || refusal != drainAckRefusalPolicyUnsupported {
				t.Fatalf("authorization = (%t, %q, %v), want a policy_unsupported refusal", authorized, refusal, err)
			}
		})
	}
}

// TestAuthorizeRoutedWorkPoolDrainAckRefusesAnUnreachableSourceStoreUnderACap
// covers the clause the release gate uncovered. The source-store check used to
// ride the forSourceStore overlay, which early-returns on !supported() and so
// never ran in a capped city — invisible while the gate refused every capped
// acknowledgement anyway, and a silent skip the moment it stopped. Skipping it
// is not merely a missing refusal: the store resolution downstream surfaces an
// unreadable trigger store as an ERROR, so the acknowledgement stops being
// legacy's to handle and starts being an incident.
func TestAuthorizeRoutedWorkPoolDrainAckRefusesAnUnreachableSourceStoreUnderACap(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
	agent := findAgentByTemplate(fixture.snapshot.Config, fixture.lease.PoolTarget)
	if agent == nil {
		t.Fatalf("no agent for template %q", fixture.lease.PoolTarget)
	}
	maximum := 100
	fixture.snapshot.Config.Workspace.MaxActiveSessions = &maximum

	// Control: the cap must be the shape that used to disable the overlay, and
	// the release gate must admit it — otherwise this passes on the old refusal.
	policy := newPoolAllocationShadowPolicy(fixture.snapshot.Config, agent, nil)
	if policy.supported() || !policy.releaseEligible() {
		t.Fatalf("control failed: policy %q = (supported %t, release %t), want a capacity shape the release gate admits",
			policy.reason, policy.supported(), policy.releaseEligible())
	}

	info := stampLegacyBareTriggerStoreRef(t, fixture, "not-a-configured-store")
	lease, _, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, info)
	if err != nil {
		t.Fatalf("build drain acknowledgement lease: %v", err)
	}

	authorized, refusal, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, info, lease)
	if err != nil || authorized || refusal != drainAckRefusalPolicyUnsupported {
		t.Fatalf("authorization = (%t, %q, %v), want a clean policy_unsupported refusal", authorized, refusal, err)
	}
}

// TestAuthorizeRoutedWorkPoolDrainAckKeepsSingletonExclusionUnderACap guards
// the clause that used to ride policy.maxActiveSessions. The constructor fills
// that field only on the agent-cap branch, so under a workspace cap it stayed
// at -1 and the singleton exclusion silently stopped being evaluated — a hole
// that only became reachable once the release gate stopped refusing everything
// under a cap. Reading the agent's own cap keeps the clause honest.
func TestAuthorizeRoutedWorkPoolDrainAckKeepsSingletonExclusionUnderACap(t *testing.T) {
	setup := func(t *testing.T, agentCap int) (routedWorkPoolDrainAckAuthorizationFixture, *config.Agent) {
		t.Helper()
		fixture := newRoutedWorkPoolDrainAckAuthorizationFixture(t)
		agent := findAgentByTemplate(fixture.snapshot.Config, fixture.lease.PoolTarget)
		if agent == nil {
			t.Fatalf("no agent for template %q", fixture.lease.PoolTarget)
		}
		if isCanonicalPoolManagedSessionInfoForTemplate(fixture.info, fixture.lease.PoolTarget) {
			t.Fatal("fixture row is canonical, so it cannot exercise the singleton exclusion")
		}
		workspaceCap := 100
		fixture.snapshot.Config.Workspace.MaxActiveSessions = &workspaceCap
		agent.MaxActiveSessions = &agentCap
		// The workspace cap wins the constructor race, so maxActiveSessions is
		// never filled — which is exactly why the clause cannot read it.
		policy := newPoolAllocationShadowPolicy(fixture.snapshot.Config, agent, nil)
		if policy.reason != poolAllocationShadowWorkspaceCap || policy.maxActiveSessions != -1 {
			t.Fatalf("policy = (%q, %d), want a workspace_cap policy with an unfilled cap", policy.reason, policy.maxActiveSessions)
		}
		return fixture, agent
	}

	t.Run("singleton pool refuses a non-canonical row", func(t *testing.T) {
		fixture, _ := setup(t, 1)
		authorized, refusal, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, fixture.info, fixture.lease)
		if err != nil || authorized || refusal != drainAckRefusalPolicyUnsupported {
			t.Fatalf("authorization = (%t, %q, %v), want a policy_unsupported refusal", authorized, refusal, err)
		}
	})

	// Control: the refusal above is the singleton clause, not the workspace cap
	// leaking back in. Same row, same cap, a pool that simply is not a singleton.
	t.Run("bounded pool releases the same row", func(t *testing.T) {
		fixture, _ := setup(t, 2)
		authorized, refusal, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, fixture.info, fixture.lease)
		if err != nil || !authorized || refusal != drainAckRefusalNone {
			t.Fatalf("authorization = (%t, %q, %v), want the release to hold", authorized, refusal, err)
		}
	})
}

// TestAuthorizeRoutedWorkPoolDrainAckReleasesACityServedTriggerForARigScopedAgent
// pins the release against the fifth spelling of the single-store assumption.
// A city that relocates its work class serves claims at CITY scope, so a
// rig-scoped pool member's acknowledged trigger legitimately carries the city
// ref — maintainer-city stamps gc.trigger_bead_store_ref=city:maintainer-city
// on every class-served claim. The source-store clause asked whether the
// AGENT's own rig equals the ref, and "rig:<name>" never equals "city:<name>",
// so every such acknowledgement refused lease_invalid/policy_unsupported and
// the seat drained forever. The refusable fact is the CITY's: does the ref
// name a store this city serves at all — the same resolution that services
// the acknowledgement below (routedWorkStore).
func TestAuthorizeRoutedWorkPoolDrainAckReleasesACityServedTriggerForARigScopedAgent(t *testing.T) {
	fixture := newRoutedWorkPoolDrainAckAuthorizationFixtureWithOptions(t,
		routedWorkPoolAuthorizationFixtureOptions{rigName: "gascity"})

	// The city-served trigger: a closed work bead resolvable through the city
	// ref, which is what a relocated work class serves.
	cityWork, err := fixture.store.Create(beads.Bead{
		Title:  "city-served routed work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": fixture.template,
		},
	})
	if err != nil {
		t.Fatalf("create city-served work: %v", err)
	}
	if err := fixture.store.Close(cityWork.ID); err != nil {
		t.Fatalf("close city-served work: %v", err)
	}
	const cityRef = "city:test-city"
	if err := fixture.store.SetMetadataBatch(fixture.info.ID, map[string]string{
		beadmeta.TriggerBeadIDMetadataKey:       cityWork.ID,
		beadmeta.TriggerBeadStoreRefMetadataKey: cityRef,
	}); err != nil {
		t.Fatalf("re-point member trigger at the city-served work: %v", err)
	}
	for key, value := range map[string]string{
		reconcilerDrainAckTriggerBeadIDKey:   cityWork.ID,
		reconcilerDrainAckTriggerStoreRefKey: cityRef,
	} {
		if err := fixture.provider.SetMeta(fixture.info.SessionName, key, value); err != nil {
			t.Fatalf("restamp acknowledged trigger %s: %v", key, err)
		}
	}
	info, err := sessionFrontDoor(fixture.store).Get(fixture.info.ID)
	if err != nil {
		t.Fatalf("read re-pointed pool session: %v", err)
	}
	if err := fixture.cr.poolMembershipShadow.replace(fixture.snapshot.Config, info); err != nil {
		t.Fatalf("publish re-pointed pool membership: %v", err)
	}
	lease, _, _, err := fixture.cr.newRoutedWorkPoolDrainAckLease(fixture.snapshot, info)
	if err != nil {
		t.Fatalf("build drain acknowledgement lease: %v", err)
	}
	if lease.SourceStore != cityRef {
		t.Fatalf("lease source store = %q, want the city ref %q — the clause under test never runs otherwise", lease.SourceStore, cityRef)
	}

	authorized, refusal, err := fixture.cr.authorizeRoutedWorkPoolDrainAck(fixture.snapshot, info, lease)
	if err != nil || !authorized || refusal != drainAckRefusalNone {
		t.Fatalf("authorization = (%t, %q, %v), want the city-served release to hold", authorized, refusal, err)
	}
}
