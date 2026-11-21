package main

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestResetConfiguredNamedSessionForConfigDrift_PreservesSessionKeyOnContinuationReset
// is the fast regression discriminator for the alive-restart resume-continuity
// contract. When the existing session_key and started_config_hash are both
// non-empty AND the reset transitions back into creating, the reset MUST
// preserve session_key (no fresh UUID) and MUST NOT clear started_config_hash.
// The fast assertion exercises the real prep + Start call via
// prepareStartCandidateForCity + startPreparedStartCandidate, so
// runtime.Fake.Calls records the actual `--resume <prior-session-key>`
// command without paying the staleKeyDetectDelay sleep that
// executePlannedStarts adds for the full runPreparedStartCandidate path.
// Without the fix, this test produces `--session-id <new-uuid>` and goes RED.
//
// Forensic source: pipex-city pc-5pp mayor session, 2026-05-13T16:23:25Z, where
// continuation_reset_pending fired with a 37-hour-old session_key=00a33290…
// and the reset rotated it to 3445a81d… while clearing started_config_hash —
// forcing the wake exec to use --session-id instead of --resume.
func TestResetConfiguredNamedSessionForConfigDrift_PreservesSessionKeyOnContinuationReset(t *testing.T) {
	env := newReconcilerTestEnv(t)
	session := env.createSessionBead("mayor", "mayor")

	const (
		priorSessionKey        = "00a33290-c714-48e4-8a06-12aad53d3ffd"
		priorStartedConfigHash = "5f9030d4d45048d150487fedef0be003d58f374e2d70f1cb4986f4a50f6e69e4"
	)
	env.setSessionMetadata(&session, map[string]string{
		"session_key":         priorSessionKey,
		"started_config_hash": priorStartedConfigHash,
		"resume_flag":         "--resume",
		"resume_style":        "flag",
	})

	tp := driftResumeSessionIDCapableTemplateParams()
	resetConfiguredNamedSessionForConfigDriftInfo(env.city, env.sessionInfo(session.ID), tp, env.store, env.sp, "mayor", false, "creating", time.Now().UTC(), &env.stderr)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}
	if got.Metadata["session_key"] != priorSessionKey {
		t.Errorf("session_key = %q, want preserved %q", got.Metadata["session_key"], priorSessionKey)
	}
	if got.Metadata["started_config_hash"] != priorStartedConfigHash {
		t.Errorf("started_config_hash = %q, want preserved %q (forces --resume not --session-id)",
			got.Metadata["started_config_hash"], priorStartedConfigHash)
	}

	cfg := &config.City{Agents: []config.Agent{{Name: "mayor"}}}
	clk := &clock.Fake{Time: time.Date(2026, 5, 13, 16, 23, 30, 0, time.UTC)}

	prepared, err := prepareStartCandidateForCity(
		startCandidate{info: env.sessionInfo(got.ID), tp: tp, order: 0},
		"", "", cfg, env.sp, env.store, clk, io.Discard, nil, dispatchOptionSources{},
	)
	if err != nil {
		t.Fatalf("prepareStartCandidateForCity: %v", err)
	}

	leased, release := leasedStart(t, env.city, *prepared)
	defer release()
	if _, err := startPreparedStartCandidate(
		context.Background(),
		leased,
		env.city,
		env.store,
		env.sp,
		cfg,
		nil,
		immediateSessionStaleKeyDetectionWaiter,
		nil,
	); err != nil {
		t.Fatalf("startPreparedStartCandidate: %v", err)
	}

	var startCfg *runtime.Config
	for _, call := range env.sp.Calls {
		if call.Method == "Start" && call.Name == "mayor" {
			cfgCopy := call.Config
			startCfg = &cfgCopy
			break
		}
	}
	if startCfg == nil {
		t.Fatalf("expected Start call for mayor, calls=%#v", env.sp.Calls)
	}

	wantArg := "--resume " + priorSessionKey
	if !strings.Contains(startCfg.Command, wantArg) {
		t.Fatalf("Start command = %q, want substring %q", startCfg.Command, wantArg)
	}
	if strings.Contains(startCfg.Command, "--session-id") {
		t.Fatalf("Start command = %q, must NOT contain --session-id when resuming a healthy session_key", startCfg.Command)
	}

	// Cross-check the same decision via the resolveSessionCommand helper
	// to catch future drift between the prep path and the resolve helper.
	firstStart := got.Metadata["started_config_hash"] == ""
	rp := &config.ResolvedProvider{ResumeFlag: "--resume", ResumeStyle: "flag", SessionIDFlag: "--session-id"}
	cmd := resolveSessionCommand("claude --dangerously-skip-permissions", got.Metadata["session_key"], "", rp, firstStart, false)
	if !strings.Contains(cmd, wantArg) {
		t.Fatalf("resolveSessionCommand = %q, want substring %q", cmd, wantArg)
	}
}

func TestReconcileSessionBeads_PreservesSessionKeyWhenNamedRestartDeferred(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Providers: map[string]config.ProviderSpec{
			"resume-provider": {
				Command:       "true",
				PromptMode:    "none",
				ResumeFlag:    "--resume",
				ResumeStyle:   "flag",
				SessionIDFlag: "--session-id",
			},
		},
		Agents: []config.Agent{
			{Name: "worker", Provider: "resume-provider", DependsOn: []string{"db"}},
			{Name: "db"},
		},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	tp := TemplateParams{
		Command:                 "true",
		SessionName:             sessionName,
		TemplateName:            "worker",
		InstanceName:            "worker",
		Alias:                   "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
		ResolvedProvider: &config.ResolvedProvider{
			Name:          "resume-provider",
			Command:       "true",
			PromptMode:    "none",
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
	}
	env.desiredState[sessionName] = tp

	const priorSessionKey = "00a33290-c714-48e4-8a06-12aad53d3ffd"
	oldRuntime := runtime.Config{Command: "old-cmd"}
	priorStartedConfigHash := runtime.CoreFingerprint(oldRuntime)
	if currentHash := runtime.CoreFingerprint(templateParamsToConfig(tp)); priorStartedConfigHash == currentHash {
		t.Fatalf("test setup error: stored hash %q should differ from current %q", priorStartedConfigHash, currentHash)
	}
	if err := env.sp.Start(context.Background(), sessionName, oldRuntime); err != nil {
		t.Fatalf("Start(old runtime): %v", err)
	}
	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"session_key":                priorSessionKey,
		"started_config_hash":        priorStartedConfigHash,
		"started_live_hash":          runtime.LiveFingerprint(oldRuntime),
	})

	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Fatalf("first reconcile woken = %d, want 0 while dependency is down", woken)
	}
	deferred, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get deferred restart: %v", err)
	}
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("%s should have been stopped for in-place config-drift restart", sessionName)
	}
	if got := deferred.Metadata["state"]; got != string(sessionpkg.StateStartPending) {
		t.Fatalf("state after deferred restart = %q, want start-pending", got)
	}
	if got := deferred.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim after deferred restart = %q, want true", got)
	}
	if _, ok := parseRFC3339Metadata(deferred.Metadata["pending_create_started_at"]); !ok {
		t.Fatalf("pending_create_started_at after deferred restart = %q, want valid timestamp",
			deferred.Metadata["pending_create_started_at"])
	}
	if got := deferred.Metadata["session_key"]; got != priorSessionKey {
		t.Fatalf("session_key after deferred restart = %q, want preserved %q", got, priorSessionKey)
	}
	if got := deferred.Metadata["started_config_hash"]; got != priorStartedConfigHash {
		t.Fatalf("started_config_hash after deferred restart = %q, want preserved %q", got, priorStartedConfigHash)
	}

	if err := env.sp.Start(context.Background(), "db", runtime.Config{Command: "db"}); err != nil {
		t.Fatalf("Start(db): %v", err)
	}
	woken = env.reconcile([]beads.Bead{deferred})
	if woken != 1 {
		t.Fatalf("second reconcile woken = %d, want 1 after dependency recovers; stderr=%s", woken, env.stderr.String())
	}

	var startCfg *runtime.Config
	for i := range env.sp.Calls {
		call := env.sp.Calls[i]
		if call.Method == "Start" && call.Name == sessionName {
			cfgCopy := call.Config
			startCfg = &cfgCopy
		}
	}
	if startCfg == nil {
		t.Fatalf("expected resumed Start call for %s, calls=%#v", sessionName, env.sp.Calls)
	}
	wantArg := "--resume " + priorSessionKey
	if !strings.Contains(startCfg.Command, wantArg) {
		t.Fatalf("Start command = %q, want substring %q", startCfg.Command, wantArg)
	}
	if strings.Contains(startCfg.Command, "--session-id") {
		t.Fatalf("Start command = %q, must NOT contain --session-id after deferred restart", startCfg.Command)
	}
}

// TestResetConfiguredNamedSessionForConfigDrift_PreservesSessionKeyEndToEnd
// runs the full executePlannedStarts pipeline, so the assertion is on the
// actually-started runtime command rather than only the prepared command.
// Deterministic inner and outer lifecycle signals keep the fake-runtime path
// in the fast suite without weakening either liveness probe.
func TestResetConfiguredNamedSessionForConfigDrift_PreservesSessionKeyEndToEnd(t *testing.T) {
	env := newReconcilerTestEnv(t)
	session := env.createSessionBead("mayor", "mayor")

	const (
		priorSessionKey        = "00a33290-c714-48e4-8a06-12aad53d3ffd"
		priorStartedConfigHash = "5f9030d4d45048d150487fedef0be003d58f374e2d70f1cb4986f4a50f6e69e4"
	)
	env.setSessionMetadata(&session, map[string]string{
		"session_key":         priorSessionKey,
		"started_config_hash": priorStartedConfigHash,
		"resume_flag":         "--resume",
		"resume_style":        "flag",
	})

	tp := driftResumeSessionIDCapableTemplateParams()
	resetConfiguredNamedSessionForConfigDriftInfo(env.city, env.sessionInfo(session.ID), tp, env.store, env.sp, "mayor", false, "creating", time.Now().UTC(), &env.stderr)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}

	cfg := &config.City{Agents: []config.Agent{{Name: "mayor"}}}
	clk := &clock.Fake{Time: time.Date(2026, 5, 13, 16, 23, 30, 0, time.UTC)}

	woken := executePlannedStartsTraced(
		context.Background(),
		[]startCandidate{{info: env.sessionInfo(got.ID), tp: tp, order: 0}},
		cfg,
		map[string]TemplateParams{"mayor": tp},
		env.sp,
		env.store,
		"test-city",
		env.city,
		clk,
		events.Discard,
		10*time.Second,
		&env.stdout,
		&env.stderr,
		nil,
		withStartStabilityWaiter(immediateStartStabilityWaiter),
		withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
	)
	if woken != 1 {
		t.Fatalf("woken = %d, want 1", woken)
	}

	var startCfg *runtime.Config
	for _, call := range env.sp.Calls {
		if call.Method == "Start" && call.Name == "mayor" {
			cfgCopy := call.Config
			startCfg = &cfgCopy
			break
		}
	}
	if startCfg == nil {
		t.Fatalf("expected Start call for mayor, calls=%#v", env.sp.Calls)
	}
	wantArg := "--resume " + priorSessionKey
	if !strings.Contains(startCfg.Command, wantArg) {
		t.Fatalf("Start command = %q, want substring %q", startCfg.Command, wantArg)
	}
	if strings.Contains(startCfg.Command, "--session-id") {
		t.Fatalf("Start command = %q, must NOT contain --session-id when resuming a healthy session_key", startCfg.Command)
	}
}

// TestResetConfiguredNamedSessionForConfigDrift_AsleepResetClearsHashAndKey
// guards the complementary case: when the reset transitions to asleep (the
// asleep-named-session repair path), the helper must clear started_config_hash
// and mint a fresh session_key. Preservation is gated on StateCreating so the
// asleep-repair contract — drift cleared, next wake uses fresh config — is
// untouched by the resume-continuity change.
//
// The provider is `--session-id`-capable because minting is only meaningful for
// that shape: a caller-supplied key reaches the next start as
// `--session-id <uuid>`, which creates the conversation. Resume-only providers
// are covered by
// TestResetConfiguredNamedSessionForConfigDrift_ResumeOnlyProviderClearsKey.
func TestResetConfiguredNamedSessionForConfigDrift_AsleepResetClearsHashAndKey(t *testing.T) {
	env := newReconcilerTestEnv(t)
	session := env.createSessionBead("mayor", "mayor")
	const (
		priorSessionKey        = "old-provider-conversation"
		priorStartedConfigHash = "old-runtime-hash"
	)
	env.setSessionMetadata(&session, map[string]string{
		"session_key":         priorSessionKey,
		"started_config_hash": priorStartedConfigHash,
	})

	resetConfiguredNamedSessionForConfigDriftInfo(env.city, env.sessionInfo(session.ID), driftResumeSessionIDCapableTemplateParams(), env.store, env.sp, "mayor", false, "asleep", time.Now().UTC(), &env.stderr)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Errorf("started_config_hash = %q, want cleared on asleep reset", got.Metadata["started_config_hash"])
	}
	if got.Metadata["session_key"] == priorSessionKey {
		t.Errorf("session_key still points at prior conversation after asleep reset")
	}
	if got.Metadata["session_key"] == "" {
		t.Error("session_key was not regenerated on asleep reset")
	}
}

// TestResetConfiguredNamedSessionForConfigDrift_GeneratesKeyWhenNoneToPreserve
// guards the never-started case: a session with no session_key and no
// started_config_hash must still receive a fresh session_key on reset so the
// next wake has a valid --session-id argument. Preserving resume metadata is
// conditional on having a prior conversation to resume.
func TestResetConfiguredNamedSessionForConfigDrift_GeneratesKeyWhenNoneToPreserve(t *testing.T) {
	env := newReconcilerTestEnv(t)
	session := env.createSessionBead("mayor", "mayor")
	// No session_key, no started_config_hash — the session never started.

	resetConfiguredNamedSessionForConfigDriftInfo(env.city, env.sessionInfo(session.ID), driftResumeSessionIDCapableTemplateParams(), env.store, env.sp, "mayor", false, "creating", time.Now().UTC(), &env.stderr)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}
	if got.Metadata["session_key"] == "" {
		t.Fatal("session_key must be regenerated when no prior key exists; got empty")
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Errorf("started_config_hash must remain cleared on the no-prior-conversation path; got %q",
			got.Metadata["started_config_hash"])
	}
}

// driftResumeSessionIDCapableTemplateParams is the `--session-id`-capable
// provider shape (claude) used by the config-drift reset tests that assert a
// fresh session_key is minted. Minting is only correct for this shape: the
// caller-supplied key reaches the next start as `--session-id <uuid>`, which
// creates the conversation rather than resuming one.
func driftResumeSessionIDCapableTemplateParams() TemplateParams {
	return TemplateParams{
		Command:      "claude --dangerously-skip-permissions",
		SessionName:  "mayor",
		TemplateName: "mayor",
		ResolvedProvider: &config.ResolvedProvider{
			Name:          "claude",
			Command:       "claude",
			ResumeFlag:    "--resume",
			ResumeStyle:   "flag",
			SessionIDFlag: "--session-id",
		},
	}
}

// driftResumeResumeOnlyTemplateParams is the resume-only provider shape
// (opencode: `--session <key>` to resume, no flag for creating a conversation
// with a caller-supplied ID). codex and gemini have the same shape.
func driftResumeResumeOnlyTemplateParams() TemplateParams {
	return TemplateParams{
		Command:      "opencode",
		SessionName:  "mayor",
		TemplateName: "mayor",
		ResolvedProvider: &config.ResolvedProvider{
			Name:        "opencode",
			Command:     "opencode",
			ResumeFlag:  "--session",
			ResumeStyle: "flag",
		},
	}
}

// TestResetConfiguredNamedSessionForConfigDrift_ResumeOnlyProviderClearsKey is
// the regression guard for the config-drift reset minting a session ID that the
// provider cannot possibly honor.
//
// A provider with no session_id_flag has no way to be told "create a
// conversation with this ID". resolveSessionCommand therefore routes ANY
// non-empty session_key — including one this reset just minted — onto the
// resume path, producing `opencode --session <uuid>` for a conversation that
// was never created. OpenCode rejects it with "Invalid session ID", the pane
// dies on startup, and the session is rebuilt from scratch with its history
// lost.
//
// The correct rotation for a resume-only provider is to CLEAR session_key so
// the next start launches bare and the real provider-minted key is captured
// after startup. This mirrors freshRestartSessionKeyInfo's contract, which the
// restart-request and fresh-cycle handoffs already honor.
//
// Forensic source: stacked-chips-v2 gascity mayor (bead ga-r2l). Five
// consecutive city restarts each minted a UUID over a healthy `ses_…` key —
// 4b0c5b88, b995ac9e, 355012d9, de9b6c4b, 40f28aed — and every one produced
// `reconciler.start.failed` / `deadline_exceeded` on the following wake.
func TestResetConfiguredNamedSessionForConfigDrift_ResumeOnlyProviderClearsKey(t *testing.T) {
	env := newReconcilerTestEnv(t)
	session := env.createSessionBead("mayor", "mayor")
	// A healthy provider-minted OpenCode key, as captured by `gc prime --hook`.
	const priorSessionKey = "ses_fcde424edffeE5J9OJuN9YG869"
	env.setSessionMetadata(&session, map[string]string{
		"session_key":  priorSessionKey,
		"resume_flag":  "--session",
		"resume_style": "flag",
	})

	resetConfiguredNamedSessionForConfigDriftInfo(env.city, env.sessionInfo(session.ID), driftResumeResumeOnlyTemplateParams(), env.store, env.sp, "mayor", false, "asleep", time.Now().UTC(), &env.stderr)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}
	if key := got.Metadata["session_key"]; key != "" {
		t.Fatalf("session_key = %q, want cleared: a provider without session_id_flag cannot be handed a caller-minted ID, so the next start must launch bare", key)
	}
}

// TestResetConfiguredNamedSessionForConfigDrift_ResumeOnlyProviderStartsBare
// closes the loop from the cleared key to the command actually launched. It
// runs the real prep + Start path so the assertion is on the command the
// runtime received, not on an intermediate value: the launch must be the bare
// provider command, never a `--session <uuid>` resume of a conversation that
// was never created.
func TestResetConfiguredNamedSessionForConfigDrift_ResumeOnlyProviderStartsBare(t *testing.T) {
	env := newReconcilerTestEnv(t)
	session := env.createSessionBead("mayor", "mayor")
	env.setSessionMetadata(&session, map[string]string{
		"session_key":  "ses_fcde424edffeE5J9OJuN9YG869",
		"resume_flag":  "--session",
		"resume_style": "flag",
	})

	tp := driftResumeResumeOnlyTemplateParams()
	resetConfiguredNamedSessionForConfigDriftInfo(env.city, env.sessionInfo(session.ID), tp, env.store, env.sp, "mayor", false, "asleep", time.Now().UTC(), &env.stderr)

	cfg := &config.City{Agents: []config.Agent{{Name: "mayor"}}}
	clk := &clock.Fake{Time: time.Date(2026, 8, 24, 12, 46, 13, 0, time.UTC)}
	prepared, err := prepareStartCandidateForCity(
		startCandidate{info: env.sessionInfo(session.ID), tp: tp, order: 0},
		"", "", cfg, env.sp, env.store, clk, io.Discard, nil, dispatchOptionSources{},
	)
	if err != nil {
		t.Fatalf("prepareStartCandidateForCity: %v", err)
	}
	leased, release := leasedStart(t, env.city, *prepared)
	defer release()
	if _, err := startPreparedStartCandidate(
		context.Background(), leased, env.city, env.store, env.sp, cfg, nil,
		immediateSessionStaleKeyDetectionWaiter, nil,
	); err != nil {
		t.Fatalf("startPreparedStartCandidate: %v", err)
	}

	var startCfg *runtime.Config
	for _, call := range env.sp.Calls {
		if call.Method == "Start" && call.Name == "mayor" {
			cfgCopy := call.Config
			startCfg = &cfgCopy
			break
		}
	}
	if startCfg == nil {
		t.Fatalf("expected Start call for mayor, calls=%#v", env.sp.Calls)
	}
	if strings.Contains(startCfg.Command, "--session") {
		t.Fatalf("Start command = %q, must not carry a --session argument: the provider never created that conversation", startCfg.Command)
	}
}

// driftResumeResumeCommandOnlyTemplateParams is the resume-capable shape that
// carries a resume_command instead of a resume flag and no session_id_flag.
// freshRestartSessionKeyInfo treats it as resume-capable, so the rotation must
// clear rather than mint here too.
func driftResumeResumeCommandOnlyTemplateParams() TemplateParams {
	return TemplateParams{
		Command:      "somecli",
		SessionName:  "mayor",
		TemplateName: "mayor",
		ResolvedProvider: &config.ResolvedProvider{
			Name:          "somecli",
			Command:       "somecli",
			ResumeCommand: "somecli resume {{.SessionKey}}",
		},
	}
}

// TestResetConfiguredNamedSessionForConfigDrift_ResumeCommandOnlyProviderClearsKey
// covers the resume_command arm of freshRestartSessionKeyInfo, which the
// resume_flag cases above leave unexercised. The provider advertises resume
// capability through resume_command alone and has no session_id_flag, so the
// rotation must clear the stored key exactly as it does for the resume_flag
// shape.
func TestResetConfiguredNamedSessionForConfigDrift_ResumeCommandOnlyProviderClearsKey(t *testing.T) {
	env := newReconcilerTestEnv(t)
	session := env.createSessionBead("mayor", "mayor")
	env.setSessionMetadata(&session, map[string]string{
		"session_key": "prior-provider-conversation",
	})

	resetConfiguredNamedSessionForConfigDriftInfo(env.city,
		env.sessionInfo(session.ID), driftResumeResumeCommandOnlyTemplateParams(),
		env.store, env.sp, "mayor", false, "asleep", time.Now().UTC(), &env.stderr)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}
	if key := got.Metadata["session_key"]; key != "" {
		t.Fatalf("session_key = %q, want cleared: a resume_command-only provider cannot be handed a caller-minted ID", key)
	}
}

func TestReconcileSessionBeads_ConfigDriftDefersPinnedNamedSession(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "new-cmd", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	tp := TemplateParams{
		Command:                 "new-cmd",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
		ResolvedProvider:        &config.ResolvedProvider{Name: "fake", Command: "new-cmd"},
	}
	env.desiredState[sessionName] = tp

	oldRuntime := runtime.Config{Command: "old-cmd"}
	priorStartedConfigHash := runtime.CoreFingerprint(oldRuntime)
	if currentHash := runtime.CoreFingerprint(templateParamsToConfig(tp)); priorStartedConfigHash == currentHash {
		t.Fatalf("test setup error: stored hash %q should differ from current %q", priorStartedConfigHash, currentHash)
	}
	if err := env.sp.Start(context.Background(), sessionName, oldRuntime); err != nil {
		t.Fatalf("Start(old runtime): %v", err)
	}
	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"pin_awake":                  "true",
		"session_key":                "prior-key",
		"started_config_hash":        priorStartedConfigHash,
		"started_live_hash":          runtime.LiveFingerprint(oldRuntime),
	})

	woken := env.reconcile([]beads.Bead{session})
	if woken != 0 {
		t.Fatalf("reconcile woken = %d, want 0 while pinned drift is deferred", woken)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("pinned named session %q was stopped for config drift", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["state"] == string(sessionpkg.StateStartPending) || got.Metadata["state"] == string(sessionpkg.StateCreating) {
		t.Fatalf("state = %q, want no config-drift reset while pinned", got.Metadata["state"])
	}
	if got.Metadata["started_config_hash"] != priorStartedConfigHash {
		t.Fatalf("started_config_hash = %q, want preserved", got.Metadata["started_config_hash"])
	}
	if got.Metadata[namedSessionConfigDriftDeferredAtMetadata] == "" {
		t.Fatal("config_drift_deferred_at = empty, want pinned deferral recorded")
	}
	if got.Metadata[namedSessionConfigDriftDeferredKeyMetadata] == "" {
		t.Fatal("config_drift_deferred_key = empty, want pinned deferral recorded")
	}
}

// TestReconcileSessionBeads_PinnedCommandDriftAppliesAfterWindowQuiet proves the
// end-to-end contract of issue #348 A on a PINNED named session:
//
//   - before the deferral window lapses the drift is NOT applied (the session
//     keeps running and the deferral stamps are recorded), matching the existing
//     pinned behavior;
//   - after the window lapses AND the session is quiet, the Command drift is
//     applied as a WARM relaunch: the launch-only path relaunches the agent in
//     the existing box (Relaunch, not Stop+Start), the Core baseline
//     (started_config_hash) is rebaselined toward the new Command, and the box
//     stays alive.
//
// The quiet boundary is what keeps this safe: a pinned conversation is never
// respawned mid-turn. The turn-boundary half is pinned by the unit test
// TestShouldDeferPinnedNamedSessionConfigDriftBoundsCommandOnly.
func TestReconcileSessionBeads_PinnedCommandDriftAppliesAfterWindowQuiet(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "new-cmd", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "always",
		}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	tp := TemplateParams{
		Command:                 "new-cmd",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
		ResolvedProvider:        &config.ResolvedProvider{Name: "fake", Command: "new-cmd"},
	}
	env.desiredState[sessionName] = tp

	session := env.createSessionBead(sessionName, "worker")
	agentCfg := sessionCoreConfigForHashInfo(tp, env.sessionInfo(session.ID))
	// Stored baseline = the running config with ONLY the launch half (Command)
	// changed, so the provision hash matches and the launch hash differs —
	// launch-only drift, which relaunches the agent in the warm box.
	oldCfg := agentCfg
	oldCfg.Command = "old-cmd"
	if runtime.CoreFingerprint(oldCfg) == runtime.CoreFingerprint(agentCfg) {
		t.Fatalf("test setup error: stored hash %q should differ from current %q", runtime.CoreFingerprint(oldCfg), runtime.CoreFingerprint(agentCfg))
	}
	breakdownJSON, err := json.Marshal(runtime.CoreFingerprintBreakdown(oldCfg))
	if err != nil {
		t.Fatalf("marshal old breakdown: %v", err)
	}
	if err := env.sp.Start(context.Background(), sessionName, oldCfg); err != nil {
		t.Fatalf("Start(old runtime): %v", err)
	}
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"pin_awake":                  "true",
		"session_key":                "prior-key",
		"started_config_hash":        runtime.CoreFingerprint(oldCfg),
		"started_provision_hash":     runtime.ProvisionFingerprint(oldCfg),
		"started_launch_hash":        runtime.LaunchFingerprint(oldCfg),
		"started_live_hash":          runtime.LiveFingerprint(oldCfg),
		"core_hash_breakdown":        string(breakdownJSON),
	})

	// Tick 1: the drift is observed and the bounded pinned-Command timer starts.
	startsBefore := env.sp.CountCalls("Start", sessionName)
	env.reconcile([]beads.Bead{session})
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get after tick 1: %v", err)
	}
	if got.Metadata[namedSessionConfigDriftDeferredKeyMetadata] == "" {
		t.Fatal("tick 1: config_drift_deferred_key = empty, want the pinned-Command timer recorded")
	}
	if state := sessionpkg.State(got.Metadata["state"]); state != sessionpkg.StateActive && state != sessionpkg.StateAwake {
		t.Fatalf("tick 1: state = %q, want active or awake (drift not applied before the window)", got.Metadata["state"])
	}
	if got.Metadata["started_config_hash"] != runtime.CoreFingerprint(oldCfg) {
		t.Fatalf("tick 1: started_config_hash = %q, want preserved %q before the window", got.Metadata["started_config_hash"], runtime.CoreFingerprint(oldCfg))
	}
	if n := env.sp.CountCalls("Relaunch", sessionName); n != 0 {
		t.Fatalf("tick 1: Relaunch calls = %d, want 0 before the window", n)
	}

	// Tick 2: past the window on a quiet session — the warm relaunch must run.
	env.clk.Time = env.clk.Now().Add(namedSessionPinnedCommandDriftDeferralLimit + time.Minute)
	session, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	env.reconcile([]beads.Bead{session})
	got, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get after tick 2: %v", err)
	}
	if n := env.sp.CountCalls("Relaunch", sessionName); n != 1 {
		t.Fatalf("tick 2: Relaunch calls = %d, want 1 (launch-only Command drift must relaunch in the warm box); stderr=%s", n, env.stderr.String())
	}
	if n := env.sp.CountCalls("Stop", sessionName); n != 0 {
		t.Fatalf("tick 2: Stop calls = %d, want 0 (a launch-only drift must not destroy the box); stderr=%s", n, env.stderr.String())
	}
	if n := env.sp.CountCalls("Start", sessionName); n != startsBefore {
		t.Fatalf("tick 2: Start calls = %d, want %d (relaunch must not re-Start)", n, startsBefore)
	}
	if rc := env.sp.LastRelaunchConfig(sessionName); rc == nil {
		t.Fatal("tick 2: no Relaunch config recorded")
	} else if rc.Command != agentCfg.Command {
		t.Fatalf("tick 2: Relaunch config Command = %q, want %q", rc.Command, agentCfg.Command)
	}
	if got.Metadata["started_config_hash"] != runtime.CoreFingerprint(agentCfg) {
		t.Fatalf("tick 2: started_config_hash = %q, want rebaselined %q", got.Metadata["started_config_hash"], runtime.CoreFingerprint(agentCfg))
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("warm box %q is gone; a launch-only drift must relaunch in place, not destroy the box", sessionName)
	}

	// Tick 3: the rebaselined baseline matches, so the drift episode ends: the
	// deferral stamps are cleared and no further relaunch is issued.
	session, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("reload session before tick 3: %v", err)
	}
	env.reconcile([]beads.Bead{session})
	got, err = env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get after tick 3: %v", err)
	}
	if got.Metadata[namedSessionConfigDriftDeferredKeyMetadata] != "" {
		t.Fatalf("tick 3: config_drift_deferred_key = %q, want cleared once the baseline matches", got.Metadata[namedSessionConfigDriftDeferredKeyMetadata])
	}
	if n := env.sp.CountCalls("Relaunch", sessionName); n != 1 {
		t.Fatalf("tick 3: Relaunch calls = %d, want still 1 (no re-relaunch once the baseline matches)", n)
	}
}
