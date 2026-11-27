package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

type patchCommandCityResolver struct{ state api.State }

func (r patchCommandCityResolver) ListCities() []api.CityInfo {
	return []api.CityInfo{{Name: r.state.CityName(), Path: r.state.CityPath(), Running: true}}
}

func (r patchCommandCityResolver) CityState(name string) api.State {
	if name == r.state.CityName() {
		return r.state
	}
	return nil
}

func TestConfigAgentPatchCommandPublishesWithoutTouchingRuntime(t *testing.T) {
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	writeCityToml(t, dir, `[workspace]
name = "test-city"
[beads]
provider = "file"
[providers.primary]
base = "builtin:claude"
command = "true"
[providers.fallback]
base = "builtin:codex"
command = "true"
resume_command = "true resume {{.SessionKey}}"
[[agent]]
name = "worker"
provider = "primary"
max_active_sessions = 1
[[agent]]
name = "sibling"
provider = "primary"
max_active_sessions = 1
[[named_session]]
template = "worker"
[[patches.agent]]
name = "worker"
env = { KEEP = "yes" }
`)
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "old-command"}); err != nil {
		t.Fatal(err)
	}
	sp.SetAttached("worker", true)
	sp.SetActivity("worker", time.Now())
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", dir)
	srv := httptest.NewServer(api.NewSupervisorMux(patchCommandCityResolver{cs}, nil, false, "test", "", time.Time{}).Handler())
	t.Cleanup(srv.Close)
	client := api.NewCityScopedClient(srv.URL, "test-city")
	prior, err := client.GetAgentPatch("worker")
	if err != nil {
		t.Fatal(err)
	}
	provider := "fallback"
	change := configedit.AgentPatchMerge{Name: "worker", Revision: prior.Revision, Provider: &provider, OptionDefaults: map[string]string{"model": "gpt-5"}}
	var stdout, stderr bytes.Buffer
	calls := len(sp.SnapshotCalls())
	if err := runAgentPatchCommand(client, "worker", change, true, true, &stdout, &stderr); err != nil {
		t.Fatalf("command: %v; stderr=%s", err, stderr.String())
	}
	var result agentPatchCommandOutput
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	validateJSONAgainstResultSchema(t, []string{"config", "agent-patch"}, stdout.Bytes())
	if !sp.IsRunning("worker") {
		t.Fatal("config write stopped the runtime")
	}
	for _, call := range sp.SnapshotCalls()[calls:] {
		if call.Method == "Start" || call.Method == "Stop" || call.Method == "Interrupt" {
			t.Fatalf("config write touched runtime: %v", call)
		}
	}
	if result.After.Env["KEEP"] != "yes" || result.After.OptionDefaults["model"] != "gpt-5" {
		t.Fatalf("result = %+v", result)
	}
	if sibling := cs.Config().Agents[1]; sibling.Provider != "primary" || len(sibling.OptionDefaults) != 0 {
		t.Fatalf("sibling drifted: %+v", sibling)
	}
	a := cs.Config().Agents[0]
	tp, err := resolveTemplate(&agentBuildParams{cityName: "test-city", cityPath: dir, workspace: &cs.Config().Workspace, providers: cs.Config().Providers, fs: fsys.OSFS{}, lookPath: func(name string) (string, error) { return "/bin/" + name, nil }, stderr: io.Discard, beadNames: map[string]string{}}, &a, "worker", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tp.ResolvedProvider.Name != provider || tp.Hints.ProviderName != "codex" || tp.ResolvedProvider.EffectiveDefaults["model"] != "gpt-5" || !strings.Contains(tp.Command, "--model gpt-5") || !reflect.DeepEqual(tp.Hints.ProcessNames, tp.ResolvedProvider.ProcessNames) || tp.Hints.ReadyPromptPrefix != tp.ResolvedProvider.ReadyPromptPrefix {
		t.Fatalf("wrong launch resolution: %+v", tp)
	}
	for _, attached := range []bool{true, false} {
		env := newReconcilerTestEnv(t)
		env.cfg = cs.Config()
		sn := config.NamedSessionRuntimeName("test-city", env.cfg.Workspace, "worker")
		deferred := tp
		deferred.TemplateName, deferred.InstanceName, deferred.Alias, deferred.SessionName = "worker", "worker", "worker", sn
		deferred.ConfiguredNamedIdentity, deferred.ConfiguredNamedMode = "worker", "on_demand"
		env.desiredState[sn] = deferred
		old := runtime.Config{Command: "old-command"}
		if err := env.sp.Start(context.Background(), sn, old); err != nil {
			t.Fatal(err)
		}
		env.sp.SetAttached(sn, attached)
		env.sp.SetActivity(sn, env.clk.Now())
		row := env.createSessionBead(sn, "worker")
		env.markSessionActive(&row)
		env.setSessionMetadata(&row, map[string]string{namedSessionMetadataKey: "true", namedSessionIdentityMetadata: "worker", namedSessionModeMetadata: "on_demand", "session_key": "prior", "provider_kind": "claude", "started_config_hash": runtime.CoreFingerprint(old), "started_live_hash": runtime.LiveFingerprint(old)})
		env.reconcile([]beads.Bead{row})
		if !env.sp.IsRunning(sn) || env.sessionInfo(row.ID).SessionKey != "prior" {
			t.Fatalf("override restarted active/attached runtime (attached=%v)", attached)
		}
	}
	// A provider-family switch after independent stop and after config-drift
	// reset resolves through the same start path, preserving the seat identity.
	var firstCommand string
	for _, drift := range []bool{false, true} {
		store := beads.NewMemStore()
		b, err := store.Create(beads.Bead{Type: "session", Metadata: map[string]string{"session_name": "worker", "template": "worker", "state": "asleep", "provider_kind": "claude", "session_key": "prior", "started_config_hash": "prior-hash", "alias": "worker"}})
		if err != nil {
			t.Fatal(err)
		}
		info := sessiontest.SeedBead(t, b)
		patch := desiredRowIdentity(cs.Config(), tp, info, rowRefreshKeys(b.Metadata)).Meta
		if drift {
			patch = driftRestartPatch(tp, info, time.Time{})
		}
		if err := session.NewStore(beads.SessionStore{Store: store}).ApplyPatch(info.ID, patch); err != nil {
			t.Fatal(err)
		}
		info = info.ApplyPatch(patch)
		prepared, _, err := buildPreparedStart(startCandidate{info: info, tp: tp}, cs.Config(), store)
		if err != nil {
			t.Fatal(err)
		}
		if prepared.resumed || strings.Contains(prepared.cfg.Command, "prior") || prepared.cfg.Env["GC_PROVIDER"] != "codex" {
			t.Fatalf("incompatible continuation: %+v", prepared.cfg)
		}
		if firstCommand == "" {
			firstCommand = prepared.cfg.Command
		} else if prepared.cfg.Command != firstCommand {
			t.Fatalf("drift start %q differs from stopped start %q", prepared.cfg.Command, firstCommand)
		}
		var payload api.SessionWokePayload
		if err := json.Unmarshal(sessionWokeProviderPayload(tp), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Provider != provider || payload.ProviderKind != "codex" || payload.Model != "gpt-5" {
			t.Fatalf("wake payload: %+v", payload)
		}
	}
	// CAS rejects stale writes through the same CLI adapter, then clearing the
	// fields under the new revision restores the previous patch exactly.
	if _, err := client.MergeAgentPatch(change); err == nil {
		t.Fatal("stale revision accepted")
	}
	if _, err := client.MergeAgentPatch(configedit.AgentPatchMerge{Name: "worker", Revision: result.Revision, ClearProvider: true, RemoveOptions: []string{"model"}}); err != nil {
		t.Fatal(err)
	}
	restored, err := client.GetAgentPatch("worker")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.After, prior.After) {
		t.Fatalf("restore = %+v, want %+v", restored.After, prior.After)
	}
}
