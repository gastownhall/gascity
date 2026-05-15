package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
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
	env := newReconcilerTestEnv()
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

	resetConfiguredNamedSessionForConfigDrift(&session, env.store, env.sp, "mayor", false, "creating", time.Now().UTC(), &env.stderr)

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

	tp := TemplateParams{
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
	cfg := &config.City{Agents: []config.Agent{{Name: "mayor"}}}
	clk := &clock.Fake{Time: time.Date(2026, 5, 13, 16, 23, 30, 0, time.UTC)}

	prepared, err := prepareStartCandidateForCity(
		startCandidate{session: &got, tp: tp, order: 0},
		"", "", cfg, env.sp, env.store, clk, io.Discard,
	)
	if err != nil {
		t.Fatalf("prepareStartCandidateForCity: %v", err)
	}

	if _, err := startPreparedStartCandidate(context.Background(), *prepared, "", env.store, env.sp, cfg); err != nil {
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
	cmd := resolveSessionCommand("claude --dangerously-skip-permissions", got.Metadata["session_key"], rp, firstStart, false)
	if !strings.Contains(cmd, wantArg) {
		t.Fatalf("resolveSessionCommand = %q, want substring %q", cmd, wantArg)
	}
}

func TestReconcileSessionBeads_PreservesSessionKeyWhenNamedRestartDeferred(t *testing.T) {
	env := newReconcilerTestEnv()
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
	if got := deferred.Metadata["state"]; got != "creating" {
		t.Fatalf("state after deferred restart = %q, want creating", got)
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
// is the slow integration cousin of the fast preserve test. It runs the
// full executePlannedStarts pipeline (which adds the post-Start
// staleKeyDetectDelay sleep), so the assertion is on the actually-Started
// runtime exec — not just the prepared command. Skipped in the default
// fast suite; opt in with GC_FAST_UNIT=0 or make test-cmd-gc-process.
func TestResetConfiguredNamedSessionForConfigDrift_PreservesSessionKeyEndToEnd(t *testing.T) {
	skipSlowCmdGCTest(t, "executePlannedStarts waits through stale session-key detection; run make test-cmd-gc-process for full coverage")

	env := newReconcilerTestEnv()
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

	resetConfiguredNamedSessionForConfigDrift(&session, env.store, env.sp, "mayor", false, "creating", time.Now().UTC(), &env.stderr)

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("get after reset: %v", err)
	}

	tp := TemplateParams{
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
	cfg := &config.City{Agents: []config.Agent{{Name: "mayor"}}}
	clk := &clock.Fake{Time: time.Date(2026, 5, 13, 16, 23, 30, 0, time.UTC)}

	woken := executePlannedStarts(
		context.Background(),
		[]startCandidate{{session: &got, tp: tp, order: 0}},
		cfg,
		map[string]TemplateParams{"mayor": tp},
		env.sp,
		env.store,
		"test-city",
		clk,
		events.Discard,
		10*time.Second,
		&env.stdout,
		&env.stderr,
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
func TestResetConfiguredNamedSessionForConfigDrift_AsleepResetClearsHashAndKey(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("mayor", "mayor")
	const (
		priorSessionKey        = "old-provider-conversation"
		priorStartedConfigHash = "old-runtime-hash"
	)
	env.setSessionMetadata(&session, map[string]string{
		"session_key":         priorSessionKey,
		"started_config_hash": priorStartedConfigHash,
	})

	resetConfiguredNamedSessionForConfigDrift(&session, env.store, env.sp, "mayor", false, "asleep", time.Now().UTC(), &env.stderr)

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
	env := newReconcilerTestEnv()
	session := env.createSessionBead("mayor", "mayor")
	// No session_key, no started_config_hash — the session never started.

	resetConfiguredNamedSessionForConfigDrift(&session, env.store, env.sp, "mayor", false, "creating", time.Now().UTC(), &env.stderr)

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
