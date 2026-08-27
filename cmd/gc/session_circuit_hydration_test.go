package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// seedPersistedOpenCircuit writes the durable schema-v59 shape a tripped
// breaker leaves behind: an OPEN state string plus the restart history and the
// last-restart stamp the cooldown is measured from. openedAgo places the last
// restart far enough in the past that maybeAutoReset fires on hydration.
func seedPersistedOpenCircuit(t *testing.T, env *reconcilerTestEnv, id string, lastRestartAgo time.Duration) {
	t.Helper()
	lastRestart := env.clk.Now().UTC().Add(-lastRestartAgo)
	restarts, err := json.Marshal([]string{formatCircuitTime(lastRestart)})
	if err != nil {
		t.Fatalf("marshal restart history: %v", err)
	}
	if err := sessionFrontDoor(env.store).ApplyPatch(id, map[string]string{
		sessionCircuitStateMetadata:            circuitOpen.String(),
		sessionCircuitRestartsMetadata:         string(restarts),
		sessionCircuitLastRestartMetadata:      formatCircuitTime(lastRestart),
		sessionCircuitOpenedAtMetadata:         formatCircuitTime(lastRestart),
		sessionCircuitOpenRestartCountMetadata: "6",
	}); err != nil {
		t.Fatalf("seed persisted OPEN circuit: %v", err)
	}
}

func circuitHydrationSweepInput(env *reconcilerTestEnv, rows []sessionpkg.ReconcileSession, now time.Time) detectorSweepInput {
	desired := make(map[string]TemplateParams, len(rows))
	for _, row := range rows {
		name := row.Info.SessionNameMetadata
		desired[name] = TemplateParams{SessionName: name, TemplateName: row.Info.Template}
	}
	return detectorSweepInput{
		CityPath: "test-city",
		CityName: "test-city",
		Cfg:      env.cfg,
		Provider: env.sp,
		Rows:     rows,
		Desired:  desired,
		Clock:    &clock.Fake{Time: now},
		Trigger:  "patrol",
	}
}

func circuitRowFor(t *testing.T, env *reconcilerTestEnv, id string) sessionpkg.ReconcileSession {
	t.Helper()
	bead, err := env.store.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	return sessionpkg.ReconcileSession{
		Info:    env.sessionInfo(id),
		Circuit: sessionpkg.CircuitStateFromMetadata(bead.Metadata),
	}
}

// TestDetectorSweepHydratesCircuitBreakerWithZeroWrites is WD.11's sweep-side
// RED: the sweep is the hydration point for the respawn breaker. It restores
// the singleton from the snapshot rows the tick already loaded — no store Get,
// no store write — so the keyed gates downstream read a MODEL rather than a raw
// persisted string, and it does so exactly once per sweep rather than once per
// key.
//
// The zero-write half is also the negative DETECTOR.md §3's acceptance asks
// for: a hydrating sweep that wrote would put a second writer beside legacy's
// own Phase-0.5 persistence on the very same tick.
func TestDetectorSweepHydratesCircuitBreakerWithZeroWrites(t *testing.T) {
	env := newReconcilerTestEnv()
	configureAlwaysNamedSession(env)
	bead := createCircuitTestNamedSession(t, env, "asleep")
	seedPersistedOpenCircuit(t, env, bead.ID, time.Minute)

	before, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session) before sweep: %v", err)
	}
	row := circuitRowFor(t, env, bead.ID)

	in := circuitHydrationSweepInput(env, []sessionpkg.ReconcileSession{row}, env.clk.Now())
	result := detectSessionConditions(context.Background(), in)

	if !result.CircuitHydrated {
		t.Fatal("sweep did not report circuit hydration; the sweep is the hydration point (DETECTOR.md §3, circuit/health)")
	}
	if result.CircuitResetsOwed != 0 {
		t.Fatalf("resets owed = %d for a breaker inside its cooldown, want 0", result.CircuitResetsOwed)
	}
	after, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session) after sweep: %v", err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("sweep bumped the row revision %d -> %d; breaker hydration must be zero-write", before.Revision, after.Revision)
	}
	for _, key := range sessionCircuitMetadataKeys {
		if after.Metadata[key] != before.Metadata[key] {
			t.Fatalf("sweep wrote %s: %q -> %q; breaker hydration must be zero-write",
				key, before.Metadata[key], after.Metadata[key])
		}
	}
	if !defaultSessionCircuitBreaker().IsOpen("rig-a/session-a", env.clk.Now().UTC()) {
		t.Fatal("hydrated model is CLOSED for a row whose durable state is OPEN inside its cooldown")
	}
}

// TestExactWakeStartsAfterPersistedOpenBreakerCooldownAndRestart is the
// breaker-reset RED (design-council round 1, change 4). A persisted-OPEN
// breaker whose cooldown has expired, plus a controller restart that empties
// the in-memory model, must EVENTUALLY start the session through the keyed
// wake — because the handler derives circuitOpen from the HYDRATED MODEL (which
// applies maybeAutoReset) rather than from the raw persisted string, and
// persists the reset BEFORE evaluating the gate.
//
// Without that, a zero-write sweep strands a durable "open" string that the
// refusing handler never clears: auto-recovery is lost the moment legacy's
// Phase-0.5 persistence goes away at WE.
func TestExactWakeStartsAfterPersistedOpenBreakerCooldownAndRestart(t *testing.T) {
	env := newReconcilerTestEnv()
	configureAlwaysNamedSession(env)
	bead := createCircuitTestNamedSession(t, env, "asleep")
	// Cooldown is 2*window = 60m by default; place the last restart well past it.
	seedPersistedOpenCircuit(t, env, bead.ID, 3*time.Hour)

	// Controller restart: the process-wide singleton is empty, exactly as it is
	// on the first tick after a restart.
	sessionCircuitBreakerMu.Lock()
	sessionCircuitBreakerSingleton = newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
	sessionCircuitBreakerMu.Unlock()

	params := exactSessionStartParams{
		Generation:  1,
		CityPath:    "test-city",
		CityName:    "test-city",
		Config:      env.cfg,
		Provider:    env.sp,
		Store:       env.store,
		Recorder:    events.Discard,
		Stdout:      &env.stdout,
		Stderr:      &env.stderr,
		Clock:       env.clk,
		RolloutMode: rollout.Require,
	}
	now := env.clk.Now().UTC()

	// Before the sweep hydrates, the model is cold and the durable string is the
	// only signal: refuse, fail-closed, exactly as legacy does.
	if !exactSessionCircuitOpen(params, env.sessionInfo(bead.ID), now) {
		t.Fatal("cold model + durable OPEN string did not refuse; the pre-hydration gate must fail closed")
	}

	// One sweep hydrates the singleton and auto-resets the expired entry.
	in := circuitHydrationSweepInput(env, []sessionpkg.ReconcileSession{circuitRowFor(t, env, bead.ID)}, env.clk.Now())
	detectSessionConditions(context.Background(), in)

	// The gate now derives from the model, and persists the reset BEFORE
	// answering: the durable cluster is cleared and the wake is allowed.
	if exactSessionCircuitOpen(params, env.sessionInfo(bead.ID), now) {
		t.Fatal("hydrated + cooled-down breaker still gated the keyed wake; the gate is reading the raw persisted string")
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := stored.Metadata[sessionCircuitStateMetadata]; got == circuitOpen.String() {
		t.Fatalf("durable circuit state = %q after the reset; the handler must persist the reset before the gate", got)
	}
	if got := stored.Metadata[sessionCircuitRestartsMetadata]; got != "" {
		t.Fatalf("durable restart history = %q after the reset, want cleared", got)
	}
}

// TestExactStartGatesConsumeSweepHydratedProviderHealth pins the second half of
// the hydration architecture: the provider-health snapshot is loaded ONCE per
// sweep and consumed by the keyed gates, instead of each key re-reading
// provider-health.json off disk.
//
// The fixture makes the two answers disagree on purpose — the published
// snapshot says RED while the on-disk file says GREEN — so a gate that re-read
// the file per key fails here rather than passing by coincidence.
func TestExactStartGatesConsumeSweepHydratedProviderHealth(t *testing.T) {
	cityPath := t.TempDir()
	writeProviderHealthFile(t, cityPath, "healthy")

	red := &providerHealthSnapshot{present: true, entries: map[string]bool{providerHealthTestProvider: false}}
	params := exactSessionStartParams{
		CityPath:       cityPath,
		ProviderHealth: func() *providerHealthSnapshot { return red },
	}
	healthy, present := exactSessionProviderHealth(params).check(providerHealthTestProvider)
	if !present || healthy {
		t.Fatalf("gate read healthy=%v present=%v, want the sweep's RED snapshot; a per-key file read would report GREEN", healthy, present)
	}

	// With nothing published the gate falls back to the file read, so a city
	// whose tick has not run yet keeps today's behavior.
	fallback := exactSessionProviderHealth(exactSessionStartParams{CityPath: cityPath})
	if healthy, present := fallback.check(providerHealthTestProvider); !present || !healthy {
		t.Fatalf("unpublished fallback read healthy=%v present=%v, want the on-disk GREEN entry", healthy, present)
	}
}

// TestLegacyCircuitRestorePersistsResetAfterSweepHydration is the ownership
// negative (f). The sweep hydrates the SAME process-wide singleton legacy's
// Phase-0.5 restore uses, and restoreFromMetadata is a consume-once edge: it
// returns reset=false for an identity that already has an entry. A sweep that
// hydrated first would therefore have SILENTLY disabled legacy's auto-reset
// persistence on every city for the whole WD wave.
//
// The ownership decision is that the two arms are READ-SHARED and convergent,
// not effect-competing: both converge the durable row onto the hydrated model
// through one idempotent, provider-free write, so neither depends on which one
// hydrated first and no fence is needed. This test pins that legacy still
// persists after the sweep has consumed the edge.
func TestLegacyCircuitRestorePersistsResetAfterSweepHydration(t *testing.T) {
	env := newReconcilerTestEnv()
	configureAlwaysNamedSession(env)
	env.addDesired("session-a", "template-a", false)
	bead := createCircuitTestNamedSession(t, env, "asleep")
	seedPersistedOpenCircuit(t, env, bead.ID, 3*time.Hour)
	// Quarantined, so the wake phase `continue`s above every other writer of the
	// circuit cluster (the open-breaker persist at the respawn gate and the
	// restart accrual at the start-failure boundary). The restore phase still
	// runs for the row, which leaves its reset persistence as the ONLY thing
	// that can clear the durable OPEN string — otherwise this test would pass on
	// a bystander write.
	env.setSessionMetadata(&bead, map[string]string{
		"quarantined_until": env.clk.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	})

	sessionCircuitBreakerMu.Lock()
	sessionCircuitBreakerSingleton = newSessionCircuitBreaker(sessionCircuitBreakerConfig{})
	sessionCircuitBreakerMu.Unlock()

	// The sweep runs FIRST on every tick (city_runtime.go), so it takes the
	// hydration edge legacy used to take.
	in := circuitHydrationSweepInput(env, []sessionpkg.ReconcileSession{circuitRowFor(t, env, bead.ID)}, env.clk.Now())
	detectSessionConditions(context.Background(), in)

	current, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := current.Metadata[sessionCircuitStateMetadata]; got != circuitOpen.String() {
		t.Fatalf("durable state = %q after the zero-write sweep, want it still %q", got, circuitOpen.String())
	}
	_ = env.reconcile([]beads.Bead{current})

	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("re-read session: %v", err)
	}
	if got := stored.Metadata[sessionCircuitStateMetadata]; got == circuitOpen.String() {
		t.Fatalf("legacy left the durable circuit state %q after the sweep consumed the hydration edge; "+
			"auto-recovery would be lost fleet-wide", got)
	}
}

// TestExactNamedResetHandoffClearsTheCircuitBreaker closes WD.12 delta 9. The
// keyed recycle reuses .103's reset machinery, and that machinery did not carry
// legacy's named circuit-breaker clear — legacy's restart block calls
// resetSessionCircuitBreakerState between the kill and the handoff commit,
// .103's ownership lattice excludes named rows so its arm never needed it, and
// WD.12 recorded the gap as owed to this slice.
//
// It is not cosmetic. A deliberate recycle is exactly the intervention whose
// restart must not count against a breaker that is one restart from tripping;
// leaving the count in place makes the fleet stop respawning the session the
// recycle just asked it to restart.
func TestExactNamedResetHandoffClearsTheCircuitBreaker(t *testing.T) {
	env := newReconcilerTestEnv()
	configureAlwaysNamedSession(env)
	bead := createCircuitTestNamedSession(t, env, "active")
	env.setSessionMetadata(&bead, map[string]string{
		"instance_token":             "reset-token",
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
	})
	seedPersistedOpenCircuit(t, env, bead.ID, time.Minute)

	const identity = "rig-a/session-a"
	cb := defaultSessionCircuitBreaker()
	cb.configure(sessionCircuitBreakerConfig{Window: 30 * time.Minute, MaxRestarts: 5}.withDefaults())
	if _, err := cb.restoreFromMetadata(identity, circuitRowFor(t, env, bead.ID).Circuit, env.clk.Now().UTC()); err != nil {
		t.Fatalf("hydrate breaker: %v", err)
	}
	if !cb.IsOpen(identity, env.clk.Now().UTC()) {
		t.Fatal("precondition: the seeded breaker should be OPEN inside its cooldown")
	}

	provider := &unattendedStopProvider{Fake: env.sp}
	params := exactSessionStartParams{
		Generation: 1, CityPath: "test-city", CityName: "test-city",
		Config: env.cfg, Provider: provider, Store: env.store,
		Recorder: events.Discard, Stderr: &env.stderr, Clock: env.clk,
		RolloutMode: rollout.Require,
	}
	info, response := strandedAuthoritative(t, env, bead.ID)
	if _, _, err := commitExactSessionResetHandoff(
		params, info, response, TemplateParams{Command: "true", SessionName: "session-a", TemplateName: "template-a"},
		env.clk, &env.stderr, func(sessionpkg.Info) bool { return true },
	); err != nil {
		t.Fatalf("keyed reset handoff: %v", err)
	}

	if defaultSessionCircuitBreaker().IsOpen(identity, env.clk.Now().UTC()) {
		t.Fatal("the keyed recycle left the named breaker OPEN; the clear did not travel with the reset")
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := stored.Metadata[sessionCircuitStateMetadata]; got == circuitOpen.String() {
		t.Fatalf("durable circuit state = %q after the keyed recycle, want cleared", got)
	}
	if got := stored.Metadata[sessionCircuitRestartsMetadata]; got != "" {
		t.Fatalf("durable restart history = %q after the keyed recycle, want cleared", got)
	}
}

// TestDetectorSweepDoesNotAccrueProviderHealthEpisodes is the other half of the
// ownership decision (f), and it goes the OPPOSITE way from the breaker.
//
// The breaker's hydration is a pure in-memory restore, so the sweep owns it and
// legacy's arms stay read-shared beside it. providerHealthGate's accrual is not
// analogous: recordRedSkip mints an episode, counts parked sessions and emits
// the ADR-0013 escalation alert — an event on the bus and a line on stdout. That
// is an EFFECT, effects are handler-side by the campaign's own rule, and a sweep
// accruing beside legacy on the same tick would double `sessions_parked` in the
// alert operators read.
//
// So the sweep owns the SNAPSHOT (one file read per tick, consumed by every
// keyed gate) and never touches the gate. This pins that: the sweep has no
// route to a providerHealthGate at all, and a red registry leaves the gate's
// episode state untouched.
func TestDetectorSweepDoesNotAccrueProviderHealthEpisodes(t *testing.T) {
	env := newReconcilerTestEnv()
	configureAlwaysNamedSession(env)
	bead := createCircuitTestNamedSession(t, env, "asleep")

	gate := newProviderHealthGate()
	in := circuitHydrationSweepInput(env, []sessionpkg.ReconcileSession{circuitRowFor(t, env, bead.ID)}, env.clk.Now())
	in.ProviderHealth = &providerHealthSnapshot{present: true, entries: map[string]bool{providerHealthTestProvider: false}}
	detectSessionConditions(context.Background(), in)

	if len(gate.episodes) != 0 {
		t.Fatalf("sweep accrued %d provider-health episodes; episode accrual emits an alert and is handler-side", len(gate.episodes))
	}
}

// providerHealthTestProvider is the one provider name every ADR-0013 fixture in
// this package pins, so the registry entry, the resolved template and the gate
// assertion cannot drift apart.
const providerHealthTestProvider = "claude"

// writeProviderHealthFile seeds the ADR-0013 registry the reconciler reads.
func writeProviderHealthFile(t *testing.T, cityPath, status string) {
	t.Helper()
	const provider = providerHealthTestProvider
	path := filepath.Join(cityPath, providerHealthCacheRelPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir provider-health cache: %v", err)
	}
	payload, err := json.Marshal(providerHealthFileFormat{Providers: []providerHealthRecord{{
		Provider: provider,
		Status:   status,
		ProbedAt: float64(time.Now().UnixNano()) / 1e9,
	}}})
	if err != nil {
		t.Fatalf("marshal provider health: %v", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatalf("write provider health: %v", err)
	}
}

var _ = config.City{}
