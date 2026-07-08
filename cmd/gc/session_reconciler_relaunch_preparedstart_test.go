package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// setupLaunchDriftResumeEnv builds a reconciler env whose alive "worker" session
// carries a session_key and a resume-capable provider, with a stored baseline
// that differs from the desired config in the launch half only (Command). This
// is the launch-only-drift shape that must relaunch the agent in the warm box
// rather than fully restart it. Returns the env, the desired TemplateParams, and
// the created session bead.
func setupLaunchDriftResumeEnv(t *testing.T) (*reconcilerTestEnv, TemplateParams, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	tp := TemplateParams{
		Command:          "claude",
		SessionName:      "worker",
		TemplateName:     "worker",
		InstanceName:     "worker",
		Alias:            "worker",
		Prompt:           "do the work",
		ResolvedProvider: forkClaude(),
	}
	env.desiredState["worker"] = tp
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "claude"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)

	// Desired (current) config and an old baseline that differs only in the
	// launch half (Command) → provision hash matches, launch hash differs.
	agentCfg := sessionCoreConfigForHash(tp, session)
	oldCfg := agentCfg
	oldCfg.Command = "stale-" + agentCfg.Command
	env.setSessionMetadata(&session, map[string]string{
		"session_key":            "warm-conversation",
		"started_config_hash":    runtime.CoreFingerprint(oldCfg),
		"started_provision_hash": runtime.ProvisionFingerprint(oldCfg),
		"started_launch_hash":    runtime.LaunchFingerprint(oldCfg),
		"started_live_hash":      runtime.LiveFingerprint(agentCfg),
	})
	return env, tp, session
}

// TestReconcileSessionBeads_LaunchDriftRelaunchResumesTrackedConversation is the
// #3872 kill-shot: routing the drift-relaunch through buildPreparedStart means
// the Config handed to Relaunch is the same executable config the fresh-start /
// pending-create-recovery paths use. It resumes the tracked conversation
// (Command carries --resume <session_key>) and does not re-send the full startup
// prompt (PromptSuffix cleared, restart nudge + GC_STARTUP_PROMPT_DELIVERED set).
// The previous hash-form config handed Relaunch a bare command that started an
// untracked conversation and re-sent the prompt.
func TestReconcileSessionBeads_LaunchDriftRelaunchResumesTrackedConversation(t *testing.T) {
	env, tp, session := setupLaunchDriftResumeEnv(t)

	env.reconcile([]beads.Bead{session})

	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Fatalf("Relaunch calls = %d, want 1 (launch-only drift must relaunch); stderr=%s", got, env.stderr.String())
	}
	rc := env.sp.LastRelaunchConfig("worker")
	if rc == nil {
		t.Fatal("no Relaunch config recorded")
	}
	// The relaunch resumes the durable conversation instead of starting an
	// untracked one — this is exactly what a fresh resume-based wake would do.
	const wantResume = "--resume warm-conversation"
	if !strings.Contains(rc.Command, wantResume) {
		t.Errorf("Relaunch Command = %q, want it to contain %q", rc.Command, wantResume)
	}
	// The full startup prompt is NOT re-delivered on relaunch.
	if rc.PromptSuffix != "" {
		t.Errorf("Relaunch PromptSuffix = %q, want empty (no double prompt)", rc.PromptSuffix)
	}
	if got := rc.Env[startupPromptDeliveredEnv]; got != "1" {
		t.Errorf("Relaunch Env[%s] = %q, want %q", startupPromptDeliveredEnv, got, "1")
	}
	if want := restartPromptNudge(tp.Prompt, tp.Hints.Nudge); rc.Nudge != want {
		t.Errorf("Relaunch Nudge = %q, want restart nudge %q", rc.Nudge, want)
	}
	// The runtime env the durable hash-form config lacked is present.
	if got := rc.Env["GC_SESSION_ID"]; got == "" {
		t.Errorf("Relaunch Env[GC_SESSION_ID] empty, want session-context env merged")
	}
}

// TestReconcileSessionBeads_LaunchDriftRebaselineNoReDrift proves the rebaseline
// uses buildPreparedStart's pre-rewrite fingerprints, so the very next tick's
// drift comparison (which uses the hash-form sessionCoreConfigForHash) sees no
// Core drift and does NOT relaunch again — no drift loop. Guards against the
// class of bug where the executed config (carrying the --resume rewrite / env)
// leaks into the persisted baseline and never matches the next comparison.
func TestReconcileSessionBeads_LaunchDriftRebaselineNoReDrift(t *testing.T) {
	env, tp, session := setupLaunchDriftResumeEnv(t)
	preLive := session.Metadata["started_live_hash"]

	// Tick 1: launch-only drift → relaunch + rebaseline.
	env.reconcile([]beads.Bead{session})
	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Fatalf("tick 1 Relaunch calls = %d, want 1; stderr=%s", got, env.stderr.String())
	}
	b, _ := env.store.Get(session.ID)

	// The rebaselined started_config_hash equals what the next tick's drift
	// comparison recomputes for the unchanged config (invariant 1).
	wantCore := runtime.CoreFingerprint(sessionCoreConfigForHash(tp, b))
	if got := b.Metadata["started_config_hash"]; got != wantCore {
		t.Errorf("started_config_hash = %q, want next-tick comparison hash %q", got, wantCore)
	}
	wantLaunch := runtime.LaunchFingerprint(sessionCoreConfigForHash(tp, b))
	if got := b.Metadata["started_launch_hash"]; got != wantLaunch {
		t.Errorf("started_launch_hash = %q, want %q", got, wantLaunch)
	}
	// The relaunch does not re-run SessionLive, so started_live_hash is untouched.
	if got := b.Metadata["started_live_hash"]; got != preLive {
		t.Errorf("started_live_hash = %q, want left unchanged %q", got, preLive)
	}

	// Tick 2: config is unchanged → no second relaunch, no drain, no re-drift.
	env.reconcile([]beads.Bead{b})
	if got := env.sp.CountCalls("Relaunch", "worker"); got != 1 {
		t.Errorf("tick 2 Relaunch calls = %d, want still 1 (no re-drift loop); stderr=%s", got, env.stderr.String())
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Errorf("tick 2 expected no drain, got reason=%q", ds.reason)
	}
	b2, _ := env.store.Get(session.ID)
	if got := b2.Metadata["started_config_hash"]; got != wantCore {
		t.Errorf("tick 2 started_config_hash = %q, want stable %q", got, wantCore)
	}
}

// TestRelaunchAgentForLaunchDrift_AbortClearsSpeculativeResumeKey is the #4038
// correctness guard for the launch-drift relaunch fallback. When a launch-only
// drift routes through buildPreparedStart on a bead that has a stored baseline
// (started_config_hash) but no session_key, buildPreparedStart mints a fresh
// session_key so it can build the relaunch command. Because started_config_hash
// is set, firstStart is false and resolveSessionCommand builds
// `--resume <minted-key>` for a conversation the relaunch has never created.
//
// Two invariants protect against that phantom key:
//   - It must never be EXECUTED: the minted-speculative-key guard refuses the
//     relaunch before Relaunch is called (even when the provider would report
//     success), so a rebaseline can never persist the minted key.
//   - It must never SURVIVE an abort: every fallback path (no-prior-key,
//     anti-skew, prepare error, relaunch failure) clears the speculative key so
//     resetConfiguredNamedSessionForConfigDrift's preserve-resume gate cannot see
//     a non-empty session_key plus the stale baseline and --resume a conversation
//     that never existed. A REAL prior key is left intact for the fallback to
//     resume.
//
// This exercises relaunchAgentForLaunchDrift directly rather than a full
// reconcile because the reconcile's start-pending fall-through restarts the
// session in the same tick, which re-stamps the metadata and masks the transient
// reset state the bug lives in. The fix is observable only at the fallback
// boundary, before that restart.
func TestRelaunchAgentForLaunchDrift_AbortClearsSpeculativeResumeKey(t *testing.T) {
	// newDriftEnv builds a launch-only-drift ("Command" changed, provision half
	// unchanged) worker session on a resume-capable provider. priorSessionKey is
	// the bead's session_key before preparation ("" to model the phantom scenario
	// where buildPreparedStart must mint one). When injectRelaunchErr is set the
	// provider fails Relaunch, driving the relaunch-failure abort/fallback path.
	// Returns the drift hashes the caller passes through.
	newDriftEnv := func(t *testing.T, priorSessionKey string, injectRelaunchErr bool) (*reconcilerTestEnv, TemplateParams, beads.Bead, string, string, string, string) {
		t.Helper()
		env := newReconcilerTestEnv()
		env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
		tp := TemplateParams{
			Command:          "claude",
			SessionName:      "worker",
			TemplateName:     "worker",
			InstanceName:     "worker",
			Alias:            "worker",
			Prompt:           "do the work",
			ResolvedProvider: forkClaude(), // SessionIDFlag set → mints a key when none exists
		}
		env.desiredState["worker"] = tp
		if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "claude"}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		session := env.createSessionBead("worker", "worker")
		env.markSessionActive(&session)
		agentCfg := sessionCoreConfigForHash(tp, session)
		oldCfg := agentCfg
		oldCfg.Command = "stale-" + agentCfg.Command
		md := map[string]string{
			"started_config_hash":    runtime.CoreFingerprint(oldCfg),
			"started_provision_hash": runtime.ProvisionFingerprint(oldCfg),
			"started_launch_hash":    runtime.LaunchFingerprint(oldCfg),
			"started_live_hash":      runtime.LiveFingerprint(agentCfg),
		}
		if priorSessionKey != "" {
			md["session_key"] = priorSessionKey
		}
		env.setSessionMetadata(&session, md)
		if injectRelaunchErr {
			env.sp.RelaunchErrors["worker"] = fmt.Errorf("warm box vanished")
		}
		return env, tp, session,
			runtime.CoreFingerprint(oldCfg), runtime.CoreFingerprint(agentCfg),
			runtime.ProvisionFingerprint(oldCfg), runtime.LaunchFingerprint(oldCfg)
	}

	callRelaunch := func(env *reconcilerTestEnv, tp TemplateParams, session *beads.Bead, storedHash, currentHash, storedProvision, storedLaunch string) bool {
		relaunched, _ := relaunchAgentForLaunchDrift(
			context.Background(), env.sp, sessionFrontDoor(env.store), session, "worker",
			tp, "", env.cfg, env.store, storedHash, currentHash, storedProvision, storedLaunch,
			[]string{"Command"}, env.rec, nil, &env.stdout, &env.stderr,
		)
		return relaunched
	}

	// assertSpeculativeKeyCleared verifies the fallback wiped the minted key and
	// the stale baseline on BOTH the raw bead and the store, so the downstream
	// reset cannot preserve a phantom resume.
	assertSpeculativeKeyCleared := func(t *testing.T, env *reconcilerTestEnv, session *beads.Bead) {
		t.Helper()
		if got := strings.TrimSpace(session.Metadata["session_key"]); got != "" {
			t.Errorf("raw bead session_key = %q, want cleared (no phantom resume)", got)
		}
		if got := strings.TrimSpace(session.Metadata["started_config_hash"]); got != "" {
			t.Errorf("raw bead started_config_hash = %q, want cleared (fresh restart)", got)
		}
		b, _ := env.store.Get(session.ID)
		if got := strings.TrimSpace(b.Metadata["session_key"]); got != "" {
			t.Errorf("stored session_key = %q, want cleared (no phantom resume)", got)
		}
		if got := strings.TrimSpace(b.Metadata["started_config_hash"]); got != "" {
			t.Errorf("stored started_config_hash = %q, want cleared (fresh restart)", got)
		}
	}

	// Major #4038 guard: a no-prior-key launch-drift relaunch must NOT execute
	// `--resume <minted-key>` even when the provider would succeed. A successful
	// relaunch would rebaseline and persist the speculative key, tying future
	// starts to a conversation that was never created. The relaunch is refused
	// before Relaunch is called and the speculative key is cleared for the full
	// restart. No relaunch error is injected: the provider WOULD succeed, so this
	// proves the guard, not a failing relaunch, prevents the phantom.
	t.Run("no prior key is refused before relaunch on the success path", func(t *testing.T) {
		env, tp, session, storedHash, currentHash, storedProvision, storedLaunch := newDriftEnv(t, "", false)
		if got := strings.TrimSpace(session.Metadata["session_key"]); got != "" {
			t.Fatalf("precondition: session_key = %q, want empty", got)
		}
		if relaunched := callRelaunch(env, tp, &session, storedHash, currentHash, storedProvision, storedLaunch); relaunched {
			t.Fatalf("relaunched = true, want false (no prior key → full restart); stderr=%s", env.stderr.String())
		}
		if got := env.sp.CountCalls("Relaunch", "worker"); got != 0 {
			t.Errorf("Relaunch calls = %d, want 0 (must not --resume a speculative key); stderr=%s", got, env.stderr.String())
		}
		assertSpeculativeKeyCleared(t, env, &session)
	})

	// Non-gating coverage for the anti-skew fallback, which shares the
	// speculative-key cleanup fold with the other aborts but had no direct test.
	// Trip the gate by leaving storedLaunch equal to the prepared launch hash
	// (launch-unchanged) so it falls back to a full restart before Relaunch, and
	// assert it clears the speculative key.
	t.Run("anti-skew abort clears speculative key", func(t *testing.T) {
		env, tp, session, storedHash, currentHash, storedProvision, _ := newDriftEnv(t, "", false)
		// prepared.launchHash == storedLaunchHash → launch-unchanged skew → abort.
		preparedLaunch := runtime.LaunchFingerprint(sessionCoreConfigForHash(tp, session))
		if relaunched := callRelaunch(env, tp, &session, storedHash, currentHash, storedProvision, preparedLaunch); relaunched {
			t.Fatalf("relaunched = true, want false (anti-skew → full restart); stderr=%s", env.stderr.String())
		}
		if got := env.sp.CountCalls("Relaunch", "worker"); got != 0 {
			t.Errorf("Relaunch calls = %d, want 0 (anti-skew aborts before Relaunch); stderr=%s", got, env.stderr.String())
		}
		assertSpeculativeKeyCleared(t, env, &session)
	})

	// A real resume key that predated preparation names an actual prior
	// conversation, so a relaunch-failure abort must leave it intact for the
	// fallback to resume (hadResumeKeyBeforePrepare is true → no clear).
	t.Run("real prior resume key is preserved when relaunch fails", func(t *testing.T) {
		const priorKey = "warm-conversation"
		env, tp, session, storedHash, currentHash, storedProvision, storedLaunch := newDriftEnv(t, priorKey, true)
		if relaunched := callRelaunch(env, tp, &session, storedHash, currentHash, storedProvision, storedLaunch); relaunched {
			t.Fatalf("relaunched = true, want false; stderr=%s", env.stderr.String())
		}
		if got := strings.TrimSpace(session.Metadata["session_key"]); got != priorKey {
			t.Errorf("raw bead session_key = %q, want preserved %q", got, priorKey)
		}
	})
}
