package main

import (
	"context"
	"errors"
	"io"
	"maps"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestNamedProviderSwitchRequiresConfirmedStop(t *testing.T) {
	env := newReconcilerTestEnv(t)
	row := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&row, map[string]string{"provider": "primary", "provider_kind": "claude", "session_key": "prior", "started_config_hash": "prior-hash"})
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "old"}); err != nil {
		t.Fatal(err)
	}
	env.sp.StopErrors["worker"] = errors.New("stop failed")
	info := env.sessionInfo(row.ID)
	tp := TemplateParams{Command: "codex", ResolvedProvider: &config.ResolvedProvider{Name: "codex", BuiltinAncestor: "codex"}}
	patch := resetConfiguredNamedSessionForConfigDriftInfo(env.city, info, tp, env.store, env.sp, "worker", true, string(session.StateStartPending), time.Now(), io.Discard)
	if len(patch) != 0 || !env.sp.IsRunning("worker") {
		t.Fatalf("unconfirmed stop changed runtime state: %v", patch)
	}
	next := env.sessionInfo(row.ID)
	if next.Provider != info.Provider || next.SessionKey != info.SessionKey || next.StartedConfigHash != info.StartedConfigHash {
		t.Fatalf("unconfirmed stop changed launch metadata: %+v", next)
	}
}

func TestNamedDriftProviderSwitchClearsIncompatibleContinuation(t *testing.T) {
	for _, v2 := range []bool{false, true} {
		env := newReconcilerTestEnv(t)
		row := env.createSessionBead("worker", "worker")
		env.setSessionMetadata(&row, map[string]string{"provider": "primary", "provider_kind": "claude", "builtin_ancestor": "claude", "session_key": "prior", "started_config_hash": "prior-hash"})
		info := env.sessionInfo(row.ID)
		tp := TemplateParams{Command: "codex --model next", ResolvedProvider: &config.ResolvedProvider{Name: "codex", BuiltinAncestor: "codex", ResumeStyle: "subcommand", ResumeFlag: "resume"}}
		var patch session.MetadataPatch
		if v2 {
			patch = driftRestartPatch(tp, info, time.Now())
		} else {
			patch = resetConfiguredNamedSessionForConfigDriftInfo("", info, tp, env.store, env.sp, "worker", false, string(session.StateStartPending), time.Now(), io.Discard)
		}
		next := info.ApplyPatch(patch)
		if next.SessionKey != "" || next.StartedConfigHash != "" || session.ProviderFamilyFromInfo(next, "") != "codex" || next.Command != tp.Command {
			t.Fatalf("v2=%v kept incompatible continuation/projection: %+v; patch=%v", v2, next, patch)
		}
	}
}

func TestProviderOverrideRefreshWaitsForStop(t *testing.T) {
	tp := TemplateParams{TemplateName: "solo", InstanceName: "solo", Alias: "solo", Command: "codex --model next", ResolvedProvider: &config.ResolvedProvider{Name: "fallback", BuiltinAncestor: "codex", ResumeFlag: "resume", ResumeStyle: "subcommand"}}
	raw := rowRefreshKeys{"command": "claude --model prior", "provider": "primary", "provider_kind": "claude", "builtin_ancestor": "claude", "session_key": "old-conversation", "started_config_hash": "started", "state": "awake", "continuation_epoch": "1"}
	info := session.Info{ID: "gc-1", Template: "solo", AgentName: "solo", Alias: "solo", SessionOrigin: "ephemeral", MetadataState: "awake", StartedConfigHash: "started", Provider: "primary", ProviderKind: "claude", BuiltinAncestor: "claude", SessionKey: "old-conversation"}
	live := desiredRowIdentity(identityCity(), tp, info, raw)
	for _, key := range []string{"command", "provider", "provider_kind", "builtin_ancestor", "session_key", "started_config_hash"} {
		if value, ok := live.Meta[key]; ok {
			t.Errorf("live override rewrote %s=%q", key, value)
		}
	}
	info.MetadataState, raw["state"] = "asleep", "asleep"
	stopped := desiredRowIdentity(identityCity(), tp, info, raw)
	if stopped.Meta["provider"] != "fallback" || stopped.Meta["provider_kind"] != "codex" || stopped.Meta["command"] != tp.Command {
		t.Fatalf("stopped projection: %v", stopped.Meta)
	}
	for _, key := range []string{"session_key", "started_config_hash"} {
		if v, ok := stopped.Meta[key]; !ok || v != "" {
			t.Errorf("incompatible %s kept: %v", key, stopped.Meta)
		}
	}
	if next := info.ApplyPatch(stopped.Meta); next.ID != info.ID || next.Alias != info.Alias || session.ProviderFamilyFromInfo(next, "") != "codex" {
		t.Fatalf("identity or family changed incorrectly: %+v", next)
	}
	// Applying the projection is idempotent, including the conversation reset.
	nextRaw := maps.Clone(raw)
	maps.Copy(nextRaw, stopped.Meta)
	if again := desiredRowIdentity(identityCity(), tp, info.ApplyPatch(stopped.Meta), nextRaw); len(again.Meta) != 0 {
		t.Fatalf("repeat refresh: %v", again.Meta)
	}
	// A deferred first start may already hold the previous provider's key.
	// A resume-only replacement cannot use that preallocated key either.
	info.StartedConfigHash, raw["started_config_hash"] = "", ""
	unstarted := desiredRowIdentity(identityCity(), tp, info, raw)
	if next := info.ApplyPatch(unstarted.Meta); next.SessionKey != "" {
		t.Fatalf("provider switch kept an unstarted conversation key: %q", next.SessionKey)
	}
}
