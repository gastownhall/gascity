package main

import (
	"context"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// driftParams builds the keyed handler's params against the test env. It is the
// production shape minus the leases no drift arm consults: the family reads the
// row, the config, the provider and the drain tracker, and nothing else.
func driftParams(env *reconcilerTestEnv, cityPath string, provider runtime.Provider) exactSessionStartParams {
	return exactSessionStartParams{
		Generation:   1,
		CityPath:     cityPath,
		CityName:     "test-city",
		Config:       env.cfg,
		Provider:     provider,
		Store:        env.store,
		Clock:        env.clk,
		Recorder:     events.Discard,
		Stdout:       io.Discard,
		Stderr:       io.Discard,
		DrainTracker: env.dt,
		RolloutMode:  rollout.Require,
	}
}

// driftAgentConfig resolves the executable-config-for-hash form the keyed family
// compares against, through the SAME production resolution the handler uses. A
// fixture that seeded hashes from a hand-built config would prove only that the
// test and the handler agree with each other. The template is read off the row
// rather than passed in, for the same reason: the handler resolves against the
// row's own template, and a fixture free to name a different one could seed a
// baseline the handler never compares against.
func driftAgentConfig(t *testing.T, env *reconcilerTestEnv, params exactSessionStartParams, id string) runtime.Config {
	t.Helper()
	info := env.sessionInfo(id)
	template := normalizedSessionTemplateInfo(info, env.cfg)
	if template == "" {
		template = info.Template
	}
	cfgAgent := findAgentByTemplate(env.cfg, template)
	if cfgAgent == nil {
		t.Fatalf("config has no agent for template %q", template)
	}
	tp, _, err := resolveExactSessionStartTemplate(params, info, cfgAgent, env.clk, io.Discard)
	if err != nil {
		t.Fatalf("resolve keyed template for %q: %v", id, err)
	}
	return sessionCoreConfigForHashInfo(tp, info)
}

// driftSweepInput builds the minimum sweep input that reaches D-DRIFT for one
// row, with admit as the routing seam's enqueue hook.
func driftSweepInput(
	env *reconcilerTestEnv,
	cityPath string,
	provider runtime.Provider,
	info sessionpkg.Info,
	admit func(string, sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error),
) detectorSweepInput {
	name := info.SessionNameMetadata
	// The tick's desired view carries the RESOLVED template, so the sweep's hash
	// compare and the handler's answer from one derivation. A fixture that fed
	// the sweep a hand-built stub would manufacture drift the handler cannot see.
	tp := env.desiredState[name]
	if cfgAgent := findAgentByTemplate(env.cfg, info.Template); cfgAgent != nil {
		if resolved, _, err := resolveExactSessionStartTemplate(driftParams(env, cityPath, provider), info, cfgAgent, env.clk, io.Discard); err == nil {
			tp = resolved
		}
	}
	if tp.SessionName == "" {
		tp = TemplateParams{SessionName: name, TemplateName: info.Template}
	}
	return detectorSweepInput{
		CityPath: cityPath,
		CityName: "test-city",
		Cfg:      env.cfg,
		Provider: provider,
		Rows:     []sessionpkg.ReconcileSession{{Info: info}},
		// Desired, because legacy only evaluates drift for a desired row and the
		// sweep mirrors that: an undesired row belongs to D-ORPHAN.
		Desired: map[string]TemplateParams{name: tp},
		Clock:   &clock.Fake{Time: env.clk.Now()},
		Trigger: "patrol",
		Admit:   admit,
	}
}

// driftOrdinaryTemplate is the ordinary (non-named) agent every fixture below
// seeds. One template keeps the resolved-config comparison legible: the shapes
// under test differ by which HALF of the fingerprint moved, never by agent.
const driftOrdinaryTemplate = "worker"

// seedOrdinaryDriftedSession seeds the schema-v59 shape the legacy anchors seed
// for an ALIVE ordinary session: active, running in the provider, with a stored
// baseline produced by mutate() applied to the session's own resolved config.
func seedOrdinaryDriftedSession(
	t *testing.T,
	env *reconcilerTestEnv,
	params exactSessionStartParams,
	mutate func(runtime.Config) runtime.Config,
	extra map[string]string,
) (beads.Bead, runtime.Config) {
	t.Helper()
	name := driftOrdinaryTemplate
	session := env.createSessionBead(name, name)
	env.markSessionActive(&session)
	if err := env.sp.Start(context.Background(), name, runtime.Config{Command: "current-cmd"}); err != nil {
		t.Fatalf("start fake runtime for %q: %v", name, err)
	}
	agentCfg := driftAgentConfig(t, env, params, session.ID)
	baseline := map[string]string{}
	if mutate != nil {
		old := mutate(agentCfg)
		baseline["started_config_hash"] = runtime.CoreFingerprint(old)
		baseline["started_provision_hash"] = runtime.ProvisionFingerprint(old)
		baseline["started_launch_hash"] = runtime.LaunchFingerprint(old)
	}
	for k, v := range extra {
		baseline[k] = v
	}
	env.setSessionMetadata(&session, baseline)
	return session, agentCfg
}

// TestExactConfigDriftRelaunchesLaunchOnlyDriftOnceByKey is WD.8's primary RED
// for the LAUNCH-RELAUNCH shape. A seeded v59 row whose launch half moved while
// its provision half held is routed by the sweep under the D-DRIFT admission
// source and relaunched into its existing warm box exactly once by exact key —
// no Stop, no Start, no drain — with the Core/provision/launch baselines
// rebaselined so a second admission on the same key is a zero-effect no-op. It
// is the keyed re-point of
// TestReconcileSessionBeads_LaunchOnlyDriftRelaunchesOrdinarySession.
func TestExactConfigDriftRelaunchesLaunchOnlyDriftOnceByKey(t *testing.T) {
	// The controller's own snapshot carries this city path, and the path is a
	// fingerprint input: a fixture that resolved its baseline against some other
	// path would compare two different configs and manufacture drift.
	cityPath := "test-city"
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "current-cmd"}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)
	session, agentCfg := seedOrdinaryDriftedSession(t, env, params, func(cfg runtime.Config) runtime.Config {
		// Only the launch half moves, so the provision hash still matches.
		cfg.Command = "stale-" + cfg.Command
		return cfg
	}, nil)

	cr := newExactDeadlineRuntime(t, env, provider, nil, nil, events.NewFake())
	admit := cr.detectorAdmitFunc()
	if admit == nil {
		t.Fatal("detectorAdmitFunc() = nil under keyed ownership; the sweep has no enqueue seam")
	}

	// The sweep is the producer of this key: it must classify the row into
	// D-DRIFT at the legacy ConfigDrift site and route it under the family's own
	// admission source.
	admitter := &recordingDetectorAdmitter{}
	in := driftSweepInput(env, cityPath, provider, env.sessionInfo(session.ID), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	if len(admitter.keys) != 1 || admitter.keys[0] != session.ID {
		t.Fatalf("sweep enqueued %v, want exactly the drifted key %q", admitter.keys, session.ID)
	}
	if admitter.sources[0] != sessionStartAdmissionConfigDrift {
		t.Fatalf("sweep enqueued under source %q, want %q", admitter.sources[0], sessionStartAdmissionConfigDrift)
	}

	if outcome, err := admit(session.ID, sessionStartAdmissionConfigDrift); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("admitting config-drift key: outcome=%q err=%v", outcome, err)
	}
	awaitCond(t, func() bool {
		stored, err := env.store.Get(session.ID)
		return err == nil && stored.Metadata["started_config_hash"] == runtime.CoreFingerprint(agentCfg)
	}, "keyed launch-only relaunch rebaseline")

	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Fatalf("Relaunch calls = %d, want exactly 1 (launch-only drift relaunches in the warm box)", got)
	}
	if got := env.sp.CountCalls("Stop", "worker"); got != 0 {
		t.Errorf("Stop calls = %d, want 0 (a relaunch is not a Stop+Start)", got)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("launch-only drift began a drain (reason=%q); it must relaunch instead", ds.reason)
	}
	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read relaunched row: %v", err)
	}
	if got, want := stored.Metadata["started_launch_hash"], runtime.LaunchFingerprint(agentCfg); got != want {
		t.Errorf("started_launch_hash = %q, want rebaselined %q", got, want)
	}
	if got, want := stored.Metadata["started_provision_hash"], runtime.ProvisionFingerprint(agentCfg); got != want {
		t.Errorf("started_provision_hash = %q, want %q", got, want)
	}

	// Exactly once by key: the level-triggered condition no longer holds, so a
	// second admission on the same key changes nothing.
	if outcome, err := admit(session.ID, sessionStartAdmissionConfigDrift); err != nil || outcome == sessionStartAdmissionOverflow {
		t.Fatalf("re-admitting converged key: outcome=%q err=%v", outcome, err)
	}
	awaitCond(t, func() bool { return !cr.sessionStartController.ownsConfigDriftConverge(session.ID) }, "config-drift admission drain")
	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Fatalf("second admission relaunched again: Relaunch calls = %d, want still 1", got)
	}
}

// TestExactConfigDriftRestartsNamedSessionInPlace is WD.8's RED for the
// RESTART-IN-PLACE shape: a detached configured-named session whose PROVISION
// half moved cannot be relaunched into its warm box, so the ladder takes the
// full restart — kill the runtime, reset the row to start_pending — exactly
// once by key.
func TestExactConfigDriftRestartsNamedSessionInPlace(t *testing.T) {
	cityPath := t.TempDir()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "current-cmd", MaxActiveSessions: intPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)

	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "current-cmd"}); err != nil {
		t.Fatalf("start fake runtime: %v", err)
	}
	agentCfg := driftAgentConfig(t, env, params, session.ID)
	// A provision-half field moves, so the box itself is stale: not launch-only.
	oldCfg := agentCfg
	oldCfg.PreStart = append([]string{"echo stale-prestart"}, agentCfg.PreStart...)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash":    runtime.CoreFingerprint(oldCfg),
		"started_provision_hash": runtime.ProvisionFingerprint(oldCfg),
		"started_launch_hash":    runtime.LaunchFingerprint(oldCfg),
	})

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, session.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	if !exactSessionConfigDriftCandidate(params, info, response, env.clk) {
		t.Fatal("seeded named row is not a D-DRIFT candidate; the fixture no longer reproduces the condition")
	}
	handled, owner, err := reconcileExactSessionDetectorFamily(
		t.Context(),
		sessionStartAdmission{SessionID: session.ID, Source: sessionStartAdmissionConfigDrift},
		params, info, response, env.clk,
	)
	if !handled || err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("seam did not claim the drifted named row: handled=%v owner=%v err=%v", handled, owner, err)
	}

	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read restarted row: %v", err)
	}
	if stored.Metadata["state"] != string(sessionpkg.StateStartPending) {
		t.Fatalf("restart-in-place left state = %q, want %q", stored.Metadata["state"], sessionpkg.StateStartPending)
	}
	if got := env.sp.CountCalls("Relaunch", sessionName); got != 0 {
		t.Errorf("Relaunch calls = %d, want 0 (provision drift must re-provision, not relaunch)", got)
	}
	if env.sp.IsRunning(sessionName) {
		t.Error("restart-in-place left the stale runtime alive")
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("named restart-in-place began a drain (reason=%q); the restart is the effect", ds.reason)
	}
}

// TestExactConfigDriftRestartInPlaceKeepsResumeAcrossTheNextLegacyPass pins the
// one hazard the off-tick handler introduces that legacy does not have. Legacy's
// alive restart-in-place preserves session_key + started_config_hash so the next
// start resumes the conversation, and it protects that staged state for the rest
// of its own tick with driftRestartedInPlace — a TICK-LOCAL flag. A keyed
// restart lands off-tick, so the very next fleet pass sees a not-alive named row
// with a preserved (still-drifted) hash and no create claim, which is exactly
// legacy's asleep-repair condition: without a durable guard it rotates the key
// and clears the hash, and the resumed conversation is gone.
func TestExactConfigDriftRestartInPlaceKeepsResumeAcrossTheNextLegacyPass(t *testing.T) {
	cityPath := "test-city"
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "current-cmd", MaxActiveSessions: intPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)

	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"session_key":                "warm-conversation",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "current-cmd"}); err != nil {
		t.Fatalf("start fake runtime: %v", err)
	}
	agentCfg := driftAgentConfig(t, env, params, session.ID)
	oldCfg := agentCfg
	oldCfg.PreStart = append([]string{"echo stale-prestart"}, agentCfg.PreStart...)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash":    runtime.CoreFingerprint(oldCfg),
		"started_provision_hash": runtime.ProvisionFingerprint(oldCfg),
		"started_launch_hash":    runtime.LaunchFingerprint(oldCfg),
	})

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, session.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	if owner, err := reconcileExactSessionConfigDrift(
		t.Context(),
		sessionStartAdmission{SessionID: session.ID, Source: sessionStartAdmissionConfigDrift},
		params, info, response, env.clk,
	); err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}
	staged, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read staged row: %v", err)
	}
	if staged.Metadata["session_key"] != "warm-conversation" {
		t.Fatalf("restart-in-place rotated the resume key instead of preserving it: %q", staged.Metadata["session_key"])
	}
	if staged.Metadata["started_config_hash"] != runtime.CoreFingerprint(oldCfg) {
		t.Fatalf("restart-in-place did not preserve the resume baseline: %q", staged.Metadata["started_config_hash"])
	}

	// The next fleet pass, with the keyed admission already retired.
	env.desiredState[sessionName] = TemplateParams{
		TemplateName:            "worker",
		InstanceName:            "worker",
		Alias:                   "worker",
		Command:                 "current-cmd",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{staged}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyConfigDriftConvergeExclusion(func(sessionpkg.Info) bool { return false }),
	)

	after, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read row after the legacy pass: %v", err)
	}
	if after.Metadata["session_key"] != "warm-conversation" {
		t.Fatalf("the next legacy pass rotated the staged resume key: %q; the keyed restart must stage state legacy leaves alone",
			after.Metadata["session_key"])
	}
}

// TestExactConfigDriftDrainsOrdinarySession is the keyed re-point of
// TestReconcileSessionBeads_ConfigDriftInitiatesDrain: an ordinary detached
// session whose provision half moved cannot be relaunched, is not named, and so
// converges by beginning the config-drift drain under the configured
// drift-drain window.
func TestExactConfigDriftDrainsOrdinarySession(t *testing.T) {
	cityPath := t.TempDir()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "current-cmd"}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)
	session, _ := seedOrdinaryDriftedSession(t, env, params, func(cfg runtime.Config) runtime.Config {
		cfg.PreStart = append([]string{"echo stale-prestart"}, cfg.PreStart...)
		return cfg
	}, nil)

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, session.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	if owner, err := reconcileExactSessionConfigDrift(
		t.Context(),
		sessionStartAdmission{SessionID: session.ID, Source: sessionStartAdmissionConfigDrift},
		params, info, response, env.clk,
	); err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}

	ds := env.dt.get(session.ID)
	if ds == nil {
		t.Fatalf("no config-drift drain intent recorded for %q", session.ID)
	}
	if ds.reason != "config-drift" {
		t.Errorf("drain reason = %q, want %q", ds.reason, "config-drift")
	}
	if got := env.sp.CountCalls("Relaunch", "worker"); got != 0 {
		t.Errorf("Relaunch calls = %d, want 0 (provision drift is not launch-only)", got)
	}
	// Enqueue-only begin semantics are preserved: the interrupt is the next
	// advance's, so the drain must not have stopped the runtime here.
	if !env.sp.IsRunning("worker") {
		t.Error("the drift drain stopped the runtime at begin; begin is enqueue-only")
	}
}

// TestExactConfigDriftRebaselinesVersionArtifactSilently is WD.8's REBASELINE
// shape and its first negative in one: a stored hash carrying no version prefix
// is a versioning artifact, not real drift, so the row is rebaselined with ZERO
// provider calls — no relaunch, no restart, no drain.
func TestExactConfigDriftRebaselinesVersionArtifactSilently(t *testing.T) {
	cityPath := t.TempDir()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "current-cmd"}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)
	session, agentCfg := seedOrdinaryDriftedSession(t, env, params, nil, map[string]string{
		// An unversioned hash: what a pre-versioning binary stamped.
		"started_config_hash": "deadbeefdeadbeef",
	})
	if !runtime.IsLegacyOrMismatchedVersion("deadbeefdeadbeef") {
		t.Fatal("fixture hash is not a version artifact; the rebaseline rung is unreachable")
	}

	callsBefore := len(env.sp.Calls)
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, session.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	if owner, err := reconcileExactSessionConfigDrift(
		t.Context(),
		sessionStartAdmission{SessionID: session.ID, Source: sessionStartAdmissionConfigDrift},
		params, info, response, env.clk,
	); err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}

	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read rebaselined row: %v", err)
	}
	for key, want := range map[string]string{
		"started_config_hash":    runtime.CoreFingerprint(agentCfg),
		"started_live_hash":      runtime.LiveFingerprint(agentCfg),
		"live_hash":              runtime.LiveFingerprint(agentCfg),
		"started_provision_hash": runtime.ProvisionFingerprint(agentCfg),
		"started_launch_hash":    runtime.LaunchFingerprint(agentCfg),
	} {
		if stored.Metadata[key] != want {
			t.Errorf("%s = %q, want rebaselined %q", key, stored.Metadata[key], want)
		}
	}
	if stored.Metadata["state"] != "active" {
		t.Errorf("silent rebaseline moved the row out of active: state=%q", stored.Metadata["state"])
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("silent rebaseline began a drain (reason=%q)", ds.reason)
	}
	// The rebaseline is a metadata write and nothing else: the only provider
	// traffic the handler may have paid is its own liveness proof.
	for _, call := range env.sp.Calls[callsBefore:] {
		switch call.Method {
		case "Relaunch", "Stop", "Start", "RunLive", "Kill", "Interrupt":
			t.Errorf("silent rebaseline called provider %s(%q); it must disturb nothing", call.Method, call.Name)
		}
	}
}

// TestExactLiveDriftReapplied is the keyed re-point of
// TestReconcileSessionBeads_LiveDriftReapplied, and it also pins that site 9
// merged into this detector: the sweep raises the LIVE half at the legacy
// LiveDrift site under the SAME admission source as the core half, and the
// handler re-applies session_live without draining.
func TestExactLiveDriftReapplied(t *testing.T) {
	cityPath := t.TempDir()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "current-cmd", SessionLive: []string{"echo live-updated"}}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)
	session, agentCfg := seedOrdinaryDriftedSession(t, env, params, nil, nil)
	// Core matches; only the live half is stale.
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(agentCfg),
		"started_live_hash":   runtime.LiveFingerprint(runtime.Config{Command: "current-cmd"}),
	})
	if runtime.LiveFingerprint(agentCfg) == runtime.LiveFingerprint(runtime.Config{Command: "current-cmd"}) {
		t.Fatal("fixture: session_live did not move the live fingerprint")
	}

	admitter := &recordingDetectorAdmitter{}
	in := driftSweepInput(env, cityPath, provider, env.sessionInfo(session.ID), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	liveArm := false
	for _, cond := range result.Conditions {
		if cond.Family != detectorFamilyDrift {
			continue
		}
		if cond.Site == TraceSiteReconcilerLiveDrift && cond.Reason == detectorReasonLiveDrift {
			liveArm = true
		}
	}
	if !liveArm {
		t.Fatalf("sweep raised no LiveDrift arm for a live-only drift; conditions=%#v", result.Conditions)
	}
	if len(admitter.keys) != 1 || admitter.sources[0] != sessionStartAdmissionConfigDrift {
		t.Fatalf("live-drift arm routed keys=%v sources=%v, want the single D-DRIFT source", admitter.keys, admitter.sources)
	}

	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, session.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	if owner, err := reconcileExactSessionConfigDrift(
		t.Context(),
		sessionStartAdmission{SessionID: session.ID, Source: sessionStartAdmissionConfigDrift},
		params, info, response, env.clk,
	); err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}
	if got := env.sp.CountCalls("RunLive", "worker"); got != 1 {
		t.Fatalf("RunLive calls = %d, want 1 (live drift is re-applied, not drained)", got)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("live drift began a drain (reason=%q); the re-apply is the effect", ds.reason)
	}
	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read re-applied row: %v", err)
	}
	if got, want := stored.Metadata["started_live_hash"], runtime.LiveFingerprint(agentCfg); got != want {
		t.Errorf("started_live_hash = %q, want rebaselined %q", got, want)
	}
	if got, want := stored.Metadata["live_hash"], runtime.LiveFingerprint(agentCfg); got != want {
		t.Errorf("live_hash = %q, want rebaselined %q", got, want)
	}
}

// TestExactLaunchAndLiveDriftRelaunchThenLiveNextCycle is the keyed re-point of
// TestReconcileSessionBeads_LaunchAndLiveDriftRelaunchThenLiveNextTick: one key
// converges the CORE half first (relaunch, live baseline deliberately left
// stale, because a relaunch does not re-run session_live), and the NEXT
// admission on the same key converges the live half. Two effects, never one
// merged effect, and never a silent live drop.
func TestExactLaunchAndLiveDriftRelaunchThenLiveNextCycle(t *testing.T) {
	cityPath := t.TempDir()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "current-cmd", SessionLive: []string{"echo live-new"}}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)
	staleLive := runtime.LiveFingerprint(runtime.Config{Command: "current-cmd"})
	session, agentCfg := seedOrdinaryDriftedSession(t, env, params, func(cfg runtime.Config) runtime.Config {
		cfg.Command = "stale-" + cfg.Command
		return cfg
	}, map[string]string{"started_live_hash": staleLive})
	if runtime.LiveFingerprint(agentCfg) == staleLive {
		t.Fatal("fixture: session_live did not move the live fingerprint")
	}

	convergeOnce := func(t *testing.T) {
		t.Helper()
		info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, session.ID)
		if err != nil {
			t.Fatalf("authoritative read: %v", err)
		}
		if owner, err := reconcileExactSessionConfigDrift(
			t.Context(),
			sessionStartAdmission{SessionID: session.ID, Source: sessionStartAdmissionConfigDrift},
			params, info, response, env.clk,
		); err != nil || owner != exactSessionStartKeyedOwner {
			t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
		}
	}

	convergeOnce(t)
	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Fatalf("cycle 1: Relaunch calls = %d, want 1", got)
	}
	if got := env.sp.CountCalls("RunLive", "worker"); got != 0 {
		t.Errorf("cycle 1: RunLive calls = %d, want 0 (the live half is one cycle behind)", got)
	}
	stored, _ := env.store.Get(session.ID)
	if got, want := stored.Metadata["started_config_hash"], runtime.CoreFingerprint(agentCfg); got != want {
		t.Fatalf("cycle 1: started_config_hash = %q, want rebaselined %q", got, want)
	}
	if got := stored.Metadata["started_live_hash"]; got != staleLive {
		t.Fatalf("cycle 1: started_live_hash = %q, want left stale %q", got, staleLive)
	}

	convergeOnce(t)
	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Errorf("cycle 2: Relaunch calls = %d, want still 1 (the core half already converged)", got)
	}
	if got := env.sp.CountCalls("RunLive", "worker"); got != 1 {
		t.Fatalf("cycle 2: RunLive calls = %d, want 1 (the live half converges now)", got)
	}
	stored, _ = env.store.Get(session.ID)
	if got, want := stored.Metadata["started_live_hash"], runtime.LiveFingerprint(agentCfg); got != want {
		t.Errorf("cycle 2: started_live_hash = %q, want rebaselined %q", got, want)
	}
}

// TestDetectorConfigDriftCurrentConfigRowIsNeverEnqueued is the zero-effect
// negative: a session running the config it is declared with raises no D-DRIFT
// condition, is never enqueued, and is refused by the seam guard — so a stable
// fleet produces zero drift work no matter how many sweeps run over it.
func TestDetectorConfigDriftCurrentConfigRowIsNeverEnqueued(t *testing.T) {
	cityPath := t.TempDir()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "current-cmd"}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)
	session, agentCfg := seedOrdinaryDriftedSession(t, env, params, nil, nil)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(agentCfg),
		"started_live_hash":   runtime.LiveFingerprint(agentCfg),
	})

	admitter := &recordingDetectorAdmitter{}
	in := driftSweepInput(env, cityPath, provider, env.sessionInfo(session.ID), admitter.admit)
	result := detectSessionConditions(context.Background(), in)
	routeDetectorConditions(in, &result)
	for _, cond := range result.Conditions {
		if cond.Family == detectorFamilyDrift {
			t.Fatalf("a current-config row raised a D-DRIFT condition: %#v", cond)
		}
	}
	if len(admitter.keys) != 0 {
		t.Fatalf("a current-config row enqueued %v; want zero enqueues", admitter.keys)
	}

	before, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read seeded row: %v", err)
	}
	callsBefore := len(env.sp.Calls)
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, session.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	if exactSessionConfigDriftCandidate(params, info, response, env.clk) {
		t.Fatal("a current-config row satisfied the D-DRIFT seam guard")
	}
	// And the handler itself refuses with zero effect if some other admission
	// carries the key into it anyway.
	if owner, err := reconcileExactSessionConfigDrift(
		t.Context(),
		sessionStartAdmission{SessionID: session.ID, Source: sessionStartAdmissionConfigDrift},
		params, info, response, env.clk,
	); err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}
	after, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("re-read seeded row: %v", err)
	}
	if after.Metadata["started_config_hash"] != before.Metadata["started_config_hash"] ||
		after.Metadata["state"] != before.Metadata["state"] || after.Metadata["session_key"] != before.Metadata["session_key"] {
		t.Fatalf("the refused handler mutated a current-config row: %#v", after.Metadata)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("a current-config row was drained (reason=%q)", ds.reason)
	}
	if len(env.sp.Calls) != callsBefore {
		t.Fatalf("a current-config row cost provider calls: %#v", env.sp.Calls[callsBefore:])
	}
}

// TestExactConfigDriftAttachedRowDefersInsteadOfConverging is the convergence
// half's second negative, now read from the far side of the WD.9 handoff. An
// attached session's drift is detected and enqueued exactly like any other —
// attachment is provider I/O the sweep may not pay — but the handler's ladder
// lands on the deferral rung, so NONE of the convergence effects run (no
// relaunch, no restart, no drain, no hash rebaseline) and the attached window is
// stamped instead. It is the keyed re-point of
// TestReconcileSessionBeads_AttachedSessionNeverRestartedOnConfigDrift.
func TestExactConfigDriftAttachedRowDefersInsteadOfConverging(t *testing.T) {
	cityPath := t.TempDir()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "current-cmd"}},
	}
	provider := &unattendedStopProvider{Fake: env.sp}
	params := driftParams(env, cityPath, provider)
	session, _ := seedOrdinaryDriftedSession(t, env, params, func(cfg runtime.Config) runtime.Config {
		cfg.Command = "stale-" + cfg.Command
		return cfg
	}, nil)
	env.sp.SetAttached("worker", true)

	before, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read seeded row: %v", err)
	}
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, session.ID)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	// Detection is attachment-blind on purpose: the row IS a candidate.
	if !exactSessionConfigDriftCandidate(params, info, response, env.clk) {
		t.Fatal("an attached drifted row must still be a D-DRIFT candidate; the split is handler-side")
	}
	drift, ok := resolveExactSessionConfigDrift(params, info, env.clk)
	if !ok {
		t.Fatal("resolveExactSessionConfigDrift refused an attached drifted row")
	}
	deferral, deferErr := exactSessionConfigDriftDeferralReason(params, info, drift, env.clk)
	if deferErr != nil {
		t.Fatalf("deferral probe: %v", deferErr)
	}
	if deferral.Rung != driftDeferralAttached || deferral.Outcome != TraceOutcomeDeferredAttached {
		t.Fatalf("deferral = %+v, want the attached rung", deferral)
	}

	if owner, err := reconcileExactSessionConfigDrift(
		t.Context(),
		sessionStartAdmission{SessionID: session.ID, Source: sessionStartAdmissionConfigDrift},
		params, info, response, env.clk,
	); err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}

	if got := env.sp.CountCalls("Relaunch", "worker"); got != 0 {
		t.Errorf("an attached row was relaunched (%d calls)", got)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("an attached row was drained (reason=%q)", ds.reason)
	}
	after, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("re-read attached row: %v", err)
	}
	if after.Metadata["started_config_hash"] != before.Metadata["started_config_hash"] ||
		after.Metadata["state"] != before.Metadata["state"] {
		t.Fatalf("the convergence ladder ran on an attached row: %#v", after.Metadata)
	}
	// The row is held by a durable stamp, not by luck: the deferral write is the
	// handler's own from WD.9 onward.
	if after.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] == "" {
		t.Fatalf("the attached rung left no deferral stamp: %#v", after.Metadata)
	}
	if got, want := after.Metadata[sessionAttachedConfigDriftDeferredKeyMetadata], drift.DriftKey; got != want {
		t.Fatalf("attached deferral key = %q, want the drift key %q", got, want)
	}
}

// TestLegacyConfigDriftArmsYieldToKeyedOwnedRow is the coexistence-doctrine RED.
// Legacy's drift block and the keyed handler compare the SAME two fingerprints
// on the same tick, so an acting D-DRIFT beside a non-yielding legacy converges
// the row twice: two relaunches of one agent, or a drain of a session the keyed
// arm just relaunched.
func TestLegacyConfigDriftArmsYieldToKeyedOwnedRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyConfigDriftConvergeExclusion(func(info sessionpkg.Info) bool { return info.ID == session.ID }),
	)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("legacy drained a row the keyed D-DRIFT handler owns (reason=%q)", ds.reason)
	}
	if got := env.sp.CountCalls("Relaunch", "worker"); got != 0 {
		t.Fatalf("legacy relaunched a row the keyed D-DRIFT handler owns (%d calls)", got)
	}
}

// TestLegacyConfigDriftArmsStillConvergeUnownedRows is the other half of the
// doctrine: the exclusion is narrow. A row the keyed controller does NOT own
// still converges through legacy for the whole WD wave, so installing the bridge
// cannot silently disable fleet drift convergence.
func TestLegacyConfigDriftArmsStillConvergeUnownedRows(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyConfigDriftConvergeExclusion(func(sessionpkg.Info) bool { return false }),
	)

	ds := env.dt.get(session.ID)
	if ds == nil || ds.reason != "config-drift" {
		t.Fatalf("legacy left an unowned drifted row unconverged: drain=%+v stderr=%s", ds, env.stderr.String())
	}
}

// TestLegacyConfigDriftDeferralArmsStillRunForKeyedOwnedRow pins the yield's
// PLACEMENT, which is the whole reason this family needed two act constants and
// two bridges. The convergence bridge sits at the CONVERGENCE effects only:
// installed alone, it stands legacy down from relaunching and draining an
// attached row while leaving legacy's deferral arms live to stamp the window.
// That is what let the two halves cross in two slices — a convergence handler
// that lands on a deferral rung and applies nothing must not silently disable
// the only writer that was defending the human.
func TestLegacyConfigDriftDeferralArmsStillRunForKeyedOwnedRow(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})
	env.sp.SetAttached("worker", true)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames,
		env.cfg, env.sp, env.store, nil, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withLegacyConfigDriftConvergeExclusion(func(sessionpkg.Info) bool { return true }),
	)

	stored, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("read attached row: %v", err)
	}
	if stored.Metadata[sessionAttachedConfigDriftDeferredAtMetadata] == "" {
		t.Fatalf("legacy skipped the attached-deferral stamp for a keyed-owned row: %#v", stored.Metadata)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("legacy drained an attached row (reason=%q)", ds.reason)
	}
}

// TestExactConfigDriftYieldsToADurableRestartRequest pins the seam ordering
// D-DRIFT inherited from legacy's forward pass and lost when it landed
// (ga-f7v2ft.138). Legacy runs the restart-requested block
// (session_reconciler.go:2806) ABOVE the config-drift block (:3050) and
// `continue`s the row past it once the kill lands (:2906); the one path that
// falls through has already applied RestartRequestPatch, which clears
// started_config_hash, so legacy's drift compare never sees a drifted row that
// carries the durable marker. Claiming it here inverts that order and swallows
// the restart whole: a public `gc session reset` reaches
// reconcileExactSessionDetectorFamily, D-DRIFT answers first, and the reset arm
// below never runs.
//
// The drain-tracker leg is the sharper half. The reset family refuses under an
// active legacy drain — that is ga-f7v2ft.103's own park fence — and D-DRIFT
// carries no such gate, so a claim here also acts on a row a legacy drain owns.
func TestExactConfigDriftYieldsToADurableRestartRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(runtime.Config) runtime.Config
		extra  map[string]string
	}{
		{
			// The shape a public `gc session reset` persists on a row whose
			// baseline predates fingerprint versioning: the rebaseline rung.
			name: "public_reset_over_a_version_artifact",
			extra: map[string]string{
				"started_config_hash":        "old-core-hash",
				"restart_requested":          "true",
				"continuation_reset_pending": "true",
			},
		},
		{
			// The same reset over REAL provision drift: the drain rung.
			name: "public_reset_over_real_core_drift",
			mutate: func(cfg runtime.Config) runtime.Config {
				cfg.PreStart = append([]string{"echo stale"}, cfg.PreStart...)
				return cfg
			},
			extra: map[string]string{
				"restart_requested":          "true",
				"continuation_reset_pending": "true",
			},
		},
		{
			// A bare restart request with no reset pending — `gc runtime
			// request-restart` and the progress-stall recycle both land here.
			// Legacy `continue`s it past the drift block on the same marker.
			name:   "bare_restart_request_over_real_core_drift",
			mutate: func(cfg runtime.Config) runtime.Config { cfg.Command = "stale-" + cfg.Command; return cfg },
			extra:  map[string]string{"restart_requested": "true"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			env := newReconcilerTestEnv()
			env.cfg = &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Agents:    []config.Agent{{Name: "worker", StartCommand: "current-cmd"}},
			}
			provider := &unattendedStopProvider{Fake: env.sp}
			params := driftParams(env, cityPath, provider)
			session, _ := seedOrdinaryDriftedSession(t, env, params, tc.mutate, tc.extra)

			before, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("read seeded row: %v", err)
			}
			callsBefore := len(env.sp.Calls)
			info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, session.ID)
			if err != nil {
				t.Fatalf("authoritative read: %v", err)
			}
			// Guard the fixture: without the restart marker this row IS drifted,
			// so a passing test cannot be an accident of a stale baseline.
			unmarked := info
			unmarked.RestartRequested = ""
			if _, drifted := resolveExactSessionConfigDrift(params, unmarked, env.clk); !drifted {
				t.Fatal("fixture is not drifted; the yield under test is unreachable")
			}

			// The sweep half. Legacy's early-continue makes this row
			// legacy-ABSENT at the drift site, and an enqueued config_drift
			// admission would overwrite the source a pending public reset was
			// admitted under (admit(), session_start_controller.go:383), so the
			// source-gated reset arm (session_start_reconcile.go:1920) declines
			// and the reset is dropped.
			admitter := &recordingDetectorAdmitter{}
			in := driftSweepInput(env, cityPath, provider, info, admitter.admit)
			result := detectSessionConditions(context.Background(), in)
			routeDetectorConditions(in, &result)
			for _, cond := range result.Conditions {
				if cond.Family == detectorFamilyDrift {
					t.Fatalf("the sweep raised D-DRIFT on a restart-requested row: %#v", cond)
				}
			}
			if len(admitter.keys) != 0 {
				t.Fatalf("a restart-requested row enqueued %v; want zero D-DRIFT enqueues", admitter.keys)
			}

			if exactSessionConfigDriftCandidate(params, info, response, env.clk) {
				t.Fatal("a restart-requested row satisfied the D-DRIFT seam guard; legacy runs the restart block above the drift block")
			}
			handled, owner, err := reconcileExactSessionDetectorFamily(
				t.Context(),
				sessionStartAdmission{SessionID: session.ID, Source: sessionStartAdmissionSocket},
				params, info, response, env.clk,
			)
			if handled {
				t.Fatalf("the detector family claimed a restart-requested row (owner=%v err=%v); the reset arm below never runs", owner, err)
			}
			// And the handler itself refuses with zero effect if another
			// admission carries the key into it anyway.
			if owner, err := reconcileExactSessionConfigDrift(
				t.Context(),
				sessionStartAdmission{SessionID: session.ID, Source: sessionStartAdmissionConfigDrift},
				params, info, response, env.clk,
			); err != nil || owner != exactSessionStartKeyedOwner {
				t.Fatalf("handler returned owner=%v err=%v, want keyed ownership and no error", owner, err)
			}

			after, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("re-read seeded row: %v", err)
			}
			for _, key := range []string{
				"started_config_hash", "started_provision_hash", "started_launch_hash",
				"restart_requested", "continuation_reset_pending", "state", "session_key",
			} {
				if after.Metadata[key] != before.Metadata[key] {
					t.Errorf("D-DRIFT mutated %s on a restart-requested row: %q -> %q", key, before.Metadata[key], after.Metadata[key])
				}
			}
			if ds := env.dt.get(session.ID); ds != nil {
				t.Errorf("D-DRIFT drained a restart-requested row (reason=%q)", ds.reason)
			}
			for _, call := range env.sp.Calls[callsBefore:] {
				switch call.Method {
				case "Relaunch", "Stop", "Start", "RunLive", "Kill", "Interrupt":
					t.Errorf("D-DRIFT called provider %s(%q) on a restart-requested row", call.Method, call.Name)
				}
			}
		})
	}
}
