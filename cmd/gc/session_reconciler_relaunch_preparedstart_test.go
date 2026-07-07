package main

import (
	"context"
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
