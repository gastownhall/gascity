package main

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// runMaterializedStartupFailureCycles drives defaultMaxWakeAttempts
// consecutive provider-start failures for the sole entry in
// env.desiredState, sourcing every candidate bead (the first and every
// replacement after a failed-create rollback) from the real production
// materialization pass, syncSessionBeads — instead of
// sessionChaosHarness.createReplacementPendingCreateBead's shortcut of
// hand-writing a replacement bead directly to the store
// (startup_health_reconcile_test.go). This mirrors production's real per-tick
// order (buildDesiredStateWithSessionBeadsAt ->
// syncSessionBeadsWithSnapshotAndRigStores -> reconcile, see
// cmd/gc/city_runtime.go), so the startup-health episode is proven to accrue
// correctly when driven by the exact code path production uses to
// materialize candidates (ga-em8g4o gap 4), for identities beyond the single
// auto-derived/unnamed shape the chaos-harness tests cover (ga-em8g4o gap 3).
//
// The materialized session name is discovered from syncSessionBeads' own
// returned index rather than assumed from the desired-state map key: for a
// pool-expanded identity (PoolSlot > 0), syncSessionBeadsWithSnapshotAndRigStores
// unconditionally overrides the created bead's real session_name via
// poolRuntimeSessionName, derived from InstanceName/TemplateName instead of
// the map key (session_name_lookup.go) — so a caller-supplied name would be
// wrong for that shape. The discovered name is asserted stable across every
// attempt (a pool identity's replacement beads must all resolve to the same
// runtime name) and returned for the caller's own assertions.
func runMaterializedStartupFailureCycles(t *testing.T, env *reconcilerTestEnv) string {
	t.Helper()
	var name string
	for attempt := 1; attempt <= defaultMaxWakeAttempts; attempt++ {
		var stderr bytes.Buffer
		openIndex := syncSessionBeads("", env.store, env.desiredState, env.sp, allConfiguredDS(env.desiredState), env.cfg, env.clk, &stderr, false)
		if stderr.Len() > 0 {
			t.Fatalf("attempt %d: syncSessionBeads: unexpected stderr: %s", attempt, stderr.String())
		}
		if len(openIndex) != 1 {
			t.Fatalf("attempt %d: syncSessionBeads materialized %d open beads, want exactly 1: %v", attempt, len(openIndex), openIndex)
		}
		var sn, beadID string
		for k, v := range openIndex {
			sn, beadID = k, v
		}
		switch {
		case attempt == 1:
			name = sn
			env.sp.StartErrors[name] = errors.New("provider start failure")
			// reconcileSessionBeads (unlike syncSessionBeads's CREATE path)
			// correlates a loaded bead to a desiredState entry by the bead's
			// own persisted session name, not by whatever map key the entry
			// was seeded under (session_reconciler.go: name :=
			// strings.TrimSpace(info.SessionNameMetadata); tp, desired :=
			// desiredState[name]). For a pool-expanded identity the seeded
			// key is only a placeholder — production's real per-tick
			// desired-state rebuild naturally re-keys by the discovered
			// name on every tick after the first; do the same here so the
			// reconcile loop below can find this entry and actually attempt
			// Provider.Start against it. A no-op re-keying (same key in,
			// same key out) for a configured named session, whose map key
			// already equals its identity.
			for k, tp := range env.desiredState {
				delete(env.desiredState, k)
				tp.SessionName = name
				env.desiredState[name] = tp
				break
			}
		case sn != name:
			t.Fatalf("attempt %d: syncSessionBeads materialized session name %q, want %q (must stay stable across replacement beads)", attempt, sn, name)
		}

		released := false
		for tick := 1; tick <= 30; tick++ {
			sessions, err := loadSessionBeads(env.store)
			if err != nil {
				t.Fatalf("loadSessionBeads: %v", err)
			}
			env.reconcile(sessions)
			env.clk.Advance(time.Minute)
			got, err := env.store.Get(beadID)
			if err != nil {
				t.Fatalf("store.Get(%s): %v", beadID, err)
			}
			if got.Status == "closed" || strings.TrimSpace(got.Metadata["pending_create_claim"]) == "" {
				released = true
				break
			}
		}
		if !released {
			t.Fatalf("attempt %d: pending-create claim for %s never released within 30 ticks", attempt, beadID)
		}
	}
	return name
}

// assertQuarantineBlocksFurtherMaterializedStarts runs one more real
// materialize-then-reconcile cycle while name's episode is quarantined,
// asserting syncSessionBeads still preserves exactly one open candidate bead
// for name and that reconciling it attempts zero further Provider.Start
// calls (the sixth attempt gap #4 exists to guard against), then returns
// that bead for further metadata assertions.
func assertQuarantineBlocksFurtherMaterializedStarts(t *testing.T, env *reconcilerTestEnv, name string) beads.Bead {
	t.Helper()
	var stderr bytes.Buffer
	openIndex := syncSessionBeads("", env.store, env.desiredState, env.sp, allConfiguredDS(env.desiredState), env.cfg, env.clk, &stderr, false)
	if stderr.Len() > 0 {
		t.Fatalf("post-quarantine syncSessionBeads: unexpected stderr: %s", stderr.String())
	}
	beadID, ok := openIndex[name]
	if !ok {
		t.Fatalf("post-quarantine syncSessionBeads did not preserve an open bead for %q (openIndex=%v)", name, openIndex)
	}

	startsBefore := env.sp.CountCalls("Start", name)
	sessions, err := loadSessionBeads(env.store)
	if err != nil {
		t.Fatalf("loadSessionBeads: %v", err)
	}
	env.reconcile(sessions)
	if got := env.sp.CountCalls("Start", name); got != startsBefore {
		t.Fatalf("Start(%s) called %d more time(s) while quarantined; want 0 (quarantine must block the 6th attempt)", name, got-startsBefore)
	}

	bead, err := env.store.Get(beadID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", beadID, err)
	}
	return bead
}

// TestNamedSessionStartupHealthEpisodeAccruesViaRealMaterialization covers a
// configured NAMED session (ga-em8g4o gap 3), driven end-to-end through real
// production materialization rather than a shortcut-written replacement bead
// (gap 4). A minimal cfg with no NamedSessions entries is sufficient: the
// gate that consults cfg.NamedSessions (blockedReconfiguredNamedIdentities,
// session_beads.go) only reconciles a *live* bead against a *different*
// configured identity, and every other named-specific branch in
// syncSessionBeadsWithSnapshotAndRigStores keys off tp.ConfiguredNamedIdentity
// and the bead's own configured_named_session/configured_named_identity
// metadata directly, not off cfg — confirmed by
// TestSyncSessionBeads_SeedsStartupKickoffMetadataForBoundNamedSession
// already exercising the named path with cfg: nil.
func TestNamedSessionStartupHealthEpisodeAccruesViaRealMaterialization(t *testing.T) {
	env := newReconcilerTestEnv()
	const configuredKey = "gs__captain"
	env.desiredState[configuredKey] = TemplateParams{
		Command:                 "chaos-cmd",
		SessionName:             configuredKey,
		TemplateName:            "captain",
		InstanceName:            "captain",
		ConfiguredNamedIdentity: "gs.captain",
		ConfiguredNamedMode:     "always",
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "fake",
			Command:    "chaos-cmd",
			PromptMode: "none",
		},
	}

	name := runMaterializedStartupFailureCycles(t, env)
	if name != configuredKey {
		t.Fatalf("materialized session name = %q, want %q (a configured named session's identity must match its desired-state key, unlike a pool instance's)", name, configuredKey)
	}

	is := sessionpkg.NewStore(beads.SessionStore{Store: env.store})
	episode, err := is.LoadStartupHealthEpisode(name)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if episode.ConsecutiveCount != defaultMaxWakeAttempts {
		t.Errorf("ConsecutiveCount = %d, want %d", episode.ConsecutiveCount, defaultMaxWakeAttempts)
	}
	if episode.QuarantinedUntil.IsZero() {
		t.Fatal("QuarantinedUntil is zero, want set after reaching the failure threshold")
	}

	// The 5th failure's bead already rolled back to closed/failed-create (like
	// every prior one), so no open bead remains for name yet — run one more
	// real materialize-then-reconcile cycle first to get a live candidate to
	// check invariants against, exactly as the existing
	// startup_health_reconcile_test.go tests do via a synthesized replacement
	// bead after their own identical loop.
	bead := assertQuarantineBlocksFurtherMaterializedStarts(t, env, name)

	open, err := env.store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	var forName []beads.Bead
	for _, b := range open {
		if strings.TrimSpace(b.Metadata["session_name"]) == name {
			forName = append(forName, b)
		}
	}
	if len(forName) != 1 {
		t.Fatalf("open session beads for %q = %d, want exactly 1: %+v", name, len(forName), forName)
	}

	wantCount := strconv.Itoa(episode.ConsecutiveCount)
	if got := bead.Metadata[startupHealthActiveCountMetadataKey]; got != wantCount {
		t.Errorf("%s = %q, want %q (mirrored episode ConsecutiveCount)", startupHealthActiveCountMetadataKey, got, wantCount)
	}
	if got := bead.Metadata[startupHealthActiveKindMetadataKey]; got != string(episode.Kind) {
		t.Errorf("%s = %q, want %q (mirrored episode Kind)", startupHealthActiveKindMetadataKey, got, string(episode.Kind))
	}
}

// TestPoolSessionStartupHealthEpisodeAccruesViaRealMaterialization covers a
// POOL-expanded session (ga-em8g4o gap 3) — an instance name distinct from
// its template name, PoolSlot set — driven end-to-end through real
// production materialization rather than a shortcut-written replacement bead
// (gap 4).
//
// placeholderKey below seeds the desired-state map key/SessionName field,
// but it is discarded on create: for a pool-expanded identity
// (PoolSlot > 0), syncSessionBeadsWithSnapshotAndRigStores unconditionally
// overrides the materialized bead's real session_name via
// poolRuntimeSessionName, derived from InstanceName + TemplateName instead
// (session_name_lookup.go) — mirroring poolIdentitySessionName's sanitized
// form of InstanceName (e.g. "pack/worker-1" -> "pack--worker-1"), not the
// map key. The real name is discovered from syncSessionBeads' own returned
// index (via runMaterializedStartupFailureCycles), never assumed, and pinned
// stable across every replacement cycle.
func TestPoolSessionStartupHealthEpisodeAccruesViaRealMaterialization(t *testing.T) {
	env := newReconcilerTestEnv()
	const placeholderKey = "polecat-1"
	const instanceName = "pack/worker-1"
	env.desiredState[placeholderKey] = TemplateParams{
		Command:      "chaos-cmd",
		SessionName:  placeholderKey,
		TemplateName: "polecat",
		InstanceName: instanceName,
		PoolSlot:     1,
		ResolvedProvider: &config.ResolvedProvider{
			Name:       "fake",
			Command:    "chaos-cmd",
			PromptMode: "none",
		},
	}

	name := runMaterializedStartupFailureCycles(t, env)
	if name == placeholderKey {
		t.Fatalf("test precondition: materialized session name unexpectedly equals the placeholder key %q; the pool-identity override this test targets did not fire", placeholderKey)
	}

	is := sessionpkg.NewStore(beads.SessionStore{Store: env.store})
	episode, err := is.LoadStartupHealthEpisode(name)
	if err != nil {
		t.Fatalf("LoadStartupHealthEpisode: %v", err)
	}
	if episode.ConsecutiveCount != defaultMaxWakeAttempts {
		t.Errorf("ConsecutiveCount = %d, want %d", episode.ConsecutiveCount, defaultMaxWakeAttempts)
	}
	if episode.QuarantinedUntil.IsZero() {
		t.Fatal("QuarantinedUntil is zero, want set after reaching the failure threshold")
	}

	// The 5th failure's bead already rolled back to closed/failed-create (like
	// every prior one), so no open bead remains for name yet — run one more
	// real materialize-then-reconcile cycle first to get a live candidate to
	// check invariants against, exactly as the existing
	// startup_health_reconcile_test.go tests do via a synthesized replacement
	// bead after their own identical loop.
	bead := assertQuarantineBlocksFurtherMaterializedStarts(t, env, name)

	open, err := env.store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	var forName []beads.Bead
	for _, b := range open {
		if strings.TrimSpace(b.Metadata["session_name"]) == name {
			forName = append(forName, b)
		}
	}
	if len(forName) != 1 {
		t.Fatalf("open session beads for %q = %d, want exactly 1: %+v", name, len(forName), forName)
	}

	wantCount := strconv.Itoa(episode.ConsecutiveCount)
	if got := bead.Metadata[startupHealthActiveCountMetadataKey]; got != wantCount {
		t.Errorf("%s = %q, want %q (mirrored episode ConsecutiveCount)", startupHealthActiveCountMetadataKey, got, wantCount)
	}
	if got := bead.Metadata[startupHealthActiveKindMetadataKey]; got != string(episode.Kind) {
		t.Errorf("%s = %q, want %q (mirrored episode Kind)", startupHealthActiveKindMetadataKey, got, string(episode.Kind))
	}
}
