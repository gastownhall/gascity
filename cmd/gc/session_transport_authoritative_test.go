package main

import (
	"bytes"
	"context"
	"sort"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

const (
	transportFixtureSession      = "worker-session"
	transportFixtureTemplate     = "example/agents.worker"
	transportFixtureIdentity     = "example/agents.worker"
	transportFixtureMCP          = `[{"name":"fixture","transport":"stdio","command":"/bin/fixture-mcp"}]`
	transportFixtureTmuxProvider = "cli-provider"
	transportFixtureACPProvider  = "acp-provider"
	transportFixtureCLICommand   = "agent-cli --profile test"
	transportFixtureACPCommand   = "agent-acp serve"
)

// staleACPArtifactMetadata models a session whose provider and command have
// moved to a tmux transport while ACP-only metadata remains from an earlier
// configuration.
func staleACPArtifactMetadata(transport string) map[string]string {
	meta := map[string]string{
		"session_name":                        transportFixtureSession,
		"state":                               "asleep",
		"provider":                            transportFixtureTmuxProvider,
		"builtin_ancestor":                    "agent-cli",
		"command":                             transportFixtureCLICommand,
		"template":                            transportFixtureTemplate,
		session.MCPIdentityMetadataKey:        transportFixtureIdentity,
		session.MCPServersSnapshotMetadataKey: transportFixtureMCP,
	}
	if transport != "" {
		meta["transport"] = transport
	}
	return meta
}

// TestACPClassifiersHonorPersistedTmuxTransport is the reproduction, applied to
// the cmd/gc copies of the classifier. A stale mcp_identity must not outvote a
// persisted tmux transport; a legacy bead with no transport key at all must
// still classify as ACP from the same artifact.
func TestACPClassifiersHonorPersistedTmuxTransport(t *testing.T) {
	stale := sessionBeadWithMetadata("session-current", staleACPArtifactMetadata(config.SessionTransportTmux))
	staleInfo := sessiontest.SeedBead(t, stale)
	if beadUsesACPTransport(stale, nil) {
		t.Error("beadUsesACPTransport = true for a bead with transport=tmux; stale mcp_identity must not beat the persisted transport")
	}
	if infoUsesACPTransport(staleInfo, nil) {
		t.Error("infoUsesACPTransport = true for a bead with transport=tmux")
	}
	if got := resolvedWorkerRuntimeTransport(
		staleInfo,
		&config.ResolvedProvider{Name: transportFixtureTmuxProvider, Command: "agent-cli"},
		config.SessionTransportACP,
		stale.Metadata,
	); got != config.SessionTransportTmux {
		t.Errorf("resolvedWorkerRuntimeTransport = %q, want tmux even with configured acp and a stale mcp_identity", got)
	}

	legacy := sessionBeadWithMetadata("session-legacy", staleACPArtifactMetadata(""))
	legacyInfo := sessiontest.SeedBead(t, legacy)
	if !beadUsesACPTransport(legacy, nil) {
		t.Error("beadUsesACPTransport = false for a legacy bead with no transport key; the mcp_identity fallback must keep working")
	}
	if !infoUsesACPTransport(legacyInfo, nil) {
		t.Error("infoUsesACPTransport = false for a legacy bead with no transport key")
	}
}

// TestQueueChangedSessionTransportMetadataRepairsProviderSwitch pins the
// reconciler repair: when the desired state resolves a non-ACP transport, the
// transport is restamped AND the stale ACP artifacts are cleared in the same
// diff-gated batch that rewrites provider/builtin_ancestor/command.
func TestReconcileTransportMetadataPatchRepairsProviderSwitch(t *testing.T) {
	cases := []struct {
		name     string
		existing map[string]string
		tp       TemplateParams
		want     map[string]string
	}{
		{
			name:     "acp-provider-to-tmux-provider",
			existing: staleACPArtifactMetadata(config.SessionTransportACP),
			tp:       TemplateParams{IsACP: false},
			want: map[string]string{
				"transport":                           config.SessionTransportTmux,
				session.MCPIdentityMetadataKey:        "",
				session.MCPServersSnapshotMetadataKey: "",
			},
		},
		{
			name:     "legacy-bead-no-transport-key",
			existing: staleACPArtifactMetadata(""),
			tp:       TemplateParams{IsACP: false},
			want: map[string]string{
				"transport":                           config.SessionTransportTmux,
				session.MCPIdentityMetadataKey:        "",
				session.MCPServersSnapshotMetadataKey: "",
			},
		},
		{
			name:     "still-acp-keeps-artifacts",
			existing: staleACPArtifactMetadata(config.SessionTransportACP),
			tp:       TemplateParams{IsACP: true},
			want:     map[string]string{},
		},
		{
			name:     "tmux-to-acp-restamps-transport",
			existing: map[string]string{"transport": config.SessionTransportTmux, "provider": transportFixtureTmuxProvider},
			tp:       TemplateParams{IsACP: true},
			want:     map[string]string{"transport": config.SessionTransportACP},
		},
		{
			name:     "converged-tmux-writes-nothing",
			existing: map[string]string{"transport": config.SessionTransportTmux, "provider": transportFixtureTmuxProvider},
			tp:       TemplateParams{IsACP: false},
			want:     map[string]string{},
		},
		{
			name: "t3-transport-is-preserved",
			existing: map[string]string{
				"transport": "t3",
				"provider":  transportFixtureTmuxProvider,
			},
			tp: TemplateParams{
				EffectiveSessionProvider: "t3bridge",
			},
			want: map[string]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := session.ReconcileTransportMetadataPatch(tc.existing, desiredSessionTransport(tc.tp))
			if len(got) != len(tc.want) {
				t.Fatalf("queued %v, want %v", sortedKeys(got), sortedKeys(tc.want))
			}
			for k, want := range tc.want {
				v, ok := got[k]
				if !ok {
					t.Fatalf("key %q not queued; queued %v", k, sortedKeys(got))
				}
				if v != want {
					t.Fatalf("queued %q = %q, want %q", k, v, want)
				}
			}
		})
	}
}

// TestDesiredSessionTransportIsNeverEmpty pins the create-time half: the
// reconciler's own create path records every effective transport, so a fresh
// bead is never left in the "unknown transport" shape that makes readers fall
// through to legacy heuristics.
func TestDesiredSessionTransportIsNeverEmpty(t *testing.T) {
	if got := desiredSessionTransport(TemplateParams{IsACP: false}); got != config.SessionTransportTmux {
		t.Errorf("desiredSessionTransport(tmux) = %q, want %q", got, config.SessionTransportTmux)
	}
	if got := desiredSessionTransport(TemplateParams{IsACP: true}); got != config.SessionTransportACP {
		t.Errorf("desiredSessionTransport(acp) = %q, want %q", got, config.SessionTransportACP)
	}
	if got := desiredSessionTransport(TemplateParams{EffectiveSessionProvider: "t3bridge"}); got != "t3" {
		t.Errorf("desiredSessionTransport(t3bridge) = %q, want %q", got, "t3")
	}
	if got := desiredSessionTransport(TemplateParams{EffectiveSessionProvider: "exec:/opt/gc-session-t3"}); got != "t3" {
		t.Errorf("desiredSessionTransport(legacy t3bridge exec alias) = %q, want %q", got, "t3")
	}
}

func sessionBeadWithMetadata(id string, meta map[string]string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Type:     session.BeadType,
		Title:    "worker",
		Status:   "open",
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestSyncSessionBeads_ClearsStaleACPArtifactsOnProviderSwitch exercises the
// provider-switch repair through the reconciler.
func TestSyncSessionBeads_ClearsStaleACPArtifactsOnProviderSwitch(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Unix(1000, 0)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), transportFixtureSession, runtime.Config{Command: transportFixtureACPCommand})

	existing, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":                        transportFixtureSession,
			"state":                               "active",
			"template":                            transportFixtureTemplate,
			"transport":                           config.SessionTransportACP,
			"provider":                            transportFixtureACPProvider,
			"command":                             transportFixtureACPCommand,
			session.MCPIdentityMetadataKey:        transportFixtureIdentity,
			session.MCPServersSnapshotMetadataKey: transportFixtureMCP,
		},
	})
	if err != nil {
		t.Fatalf("creating existing bead: %v", err)
	}

	ds := map[string]TemplateParams{
		transportFixtureSession: {
			TemplateName: transportFixtureTemplate,
			Command:      transportFixtureCLICommand,
			IsACP:        false,
			ResolvedProvider: &config.ResolvedProvider{
				Name:            transportFixtureTmuxProvider,
				Kind:            "agent-cli",
				BuiltinAncestor: "agent-cli",
				Command:         "agent-cli",
			},
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)
	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}

	updated, err := store.Get(existing.ID)
	if err != nil {
		t.Fatalf("getting updated bead: %v", err)
	}
	// The projection that already worked, restated so the test fails loudly if
	// the provider switch itself stops landing.
	for key, want := range map[string]string{
		"provider":         transportFixtureTmuxProvider,
		"builtin_ancestor": "agent-cli",
		"command":          transportFixtureCLICommand,
	} {
		if got := updated.Metadata[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
	// The projection that did not.
	if got := updated.Metadata["transport"]; got != config.SessionTransportTmux {
		t.Errorf("transport = %q, want tmux; it froze at the creation-time transport while provider/command followed the switch", got)
	}
	if got := updated.Metadata[session.MCPIdentityMetadataKey]; got != "" {
		t.Errorf("mcp_identity = %q, want cleared; a tmux session must not carry an ACP identity", got)
	}
	if got := updated.Metadata[session.MCPServersSnapshotMetadataKey]; got != "" {
		t.Errorf("mcp_servers_snapshot = %q, want cleared", got)
	}
	if beadUsesACPTransport(updated, nil) {
		t.Error("beadUsesACPTransport = true after the switch to a tmux transport")
	}
	if infoUsesACPTransport(sessiontest.SeedBead(t, updated), nil) {
		t.Error("infoUsesACPTransport = true after the switch to a tmux-only provider")
	}
}

// TestSyncSessionBeads_KeepsACPArtifactsForACPSession is the guard on the other
// side: a session whose desired state is still ACP keeps its transport and its
// MCP artifacts. Without it the repair above could be written as an
// unconditional clear.
func TestSyncSessionBeads_KeepsACPArtifactsForACPSession(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Unix(1000, 0)}
	sp := runtime.NewFake()
	_ = sp.Start(context.TODO(), transportFixtureSession, runtime.Config{Command: transportFixtureACPCommand})

	snapshot := transportFixtureMCP
	existing, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:worker"},
		Metadata: map[string]string{
			"session_name":                        transportFixtureSession,
			"state":                               "active",
			"template":                            transportFixtureTemplate,
			"provider":                            transportFixtureACPProvider,
			"command":                             transportFixtureACPCommand,
			session.MCPIdentityMetadataKey:        transportFixtureIdentity,
			session.MCPServersSnapshotMetadataKey: snapshot,
		},
	})
	if err != nil {
		t.Fatalf("creating existing bead: %v", err)
	}

	ds := map[string]TemplateParams{
		transportFixtureSession: {
			TemplateName: transportFixtureTemplate,
			Command:      transportFixtureACPCommand,
			IsACP:        true,
			ResolvedProvider: &config.ResolvedProvider{
				Name:            transportFixtureACPProvider,
				Kind:            "agent-acp",
				BuiltinAncestor: "agent-acp",
				Command:         "agent-acp",
				ACPCommand:      "agent-acp",
				ACPArgs:         []string{"serve"},
				SupportsACP:     true,
			},
		},
	}

	var stderr bytes.Buffer
	syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)
	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}

	updated, err := store.Get(existing.ID)
	if err != nil {
		t.Fatalf("getting updated bead: %v", err)
	}
	if got := updated.Metadata["transport"]; got != config.SessionTransportACP {
		t.Errorf("transport = %q, want acp (a legacy ACP bead must be backfilled, not demoted)", got)
	}
	if got := updated.Metadata[session.MCPIdentityMetadataKey]; got != transportFixtureIdentity {
		t.Errorf("mcp_identity = %q, want preserved", got)
	}
	if got := updated.Metadata[session.MCPServersSnapshotMetadataKey]; got != snapshot {
		t.Errorf("mcp_servers_snapshot = %q, want preserved", got)
	}
	if !beadUsesACPTransport(updated, nil) {
		t.Error("beadUsesACPTransport = false for a still-ACP session")
	}
}

// TestSyncSessionBeads_CreatesBeadWithExplicitTransport pins the reconciler's
// own create path: a freshly created session records its effective transport,
// so it never starts life in the "unknown transport" shape that makes readers
// fall through to legacy heuristics.
func TestSyncSessionBeads_CreatesBeadWithExplicitTransport(t *testing.T) {
	for name, tc := range map[string]struct {
		isACP                    bool
		effectiveSessionProvider string
		want                     string
	}{
		"tmux": {false, "", config.SessionTransportTmux},
		"acp":  {true, "", config.SessionTransportACP},
		"t3":   {false, "t3bridge", "t3"},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Unix(1000, 0)}
			sp := runtime.NewFake()
			_ = sp.Start(context.TODO(), transportFixtureSession, runtime.Config{Command: transportFixtureCLICommand})

			ds := map[string]TemplateParams{
				transportFixtureSession: {
					TemplateName:             transportFixtureTemplate,
					Command:                  transportFixtureCLICommand,
					Env:                      map[string]string{},
					IsACP:                    tc.isACP,
					EffectiveSessionProvider: tc.effectiveSessionProvider,
				},
			}
			var stderr bytes.Buffer
			syncSessionBeads("", store, ds, sp, allConfiguredDS(ds), nil, clk, &stderr, false)
			if stderr.Len() > 0 {
				t.Fatalf("unexpected stderr: %s", stderr.String())
			}

			all := allSessionBeads(t, store)
			if len(all) != 1 {
				t.Fatalf("expected 1 bead, got %d", len(all))
			}
			if got := all[0].Metadata["transport"]; got != tc.want {
				t.Fatalf("transport = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCommitStartResult_ClearsStaleMCPIdentityForNonACPStart pins the other
// half of the perpetuation loop. The start-commit path used to re-stamp
// mcp_identity whenever the bead already had one, so the artifact re-armed
// itself on every wake and the reconciler's clear could never converge. It is
// now gated on the transport the session was actually prepared with.
func TestCommitStartResult_ClearsStaleMCPIdentityForNonACPStart(t *testing.T) {
	store := beads.NewMemStore()
	bead, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":                     transportFixtureTemplate,
			"agent_name":                   transportFixtureIdentity,
			"session_name":                 transportFixtureSession,
			"state":                        "creating",
			session.MCPIdentityMetadataKey: transportFixtureIdentity,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := startResult{
		prepared: preparedStart{
			candidate: startCandidate{
				info: sessiontest.SeedBead(t, bead),
				tp: TemplateParams{
					TemplateName: transportFixtureTemplate,
					InstanceName: transportFixtureSession,
					IsACP:        false,
				},
			},
			coreHash: "core-abc",
			liveHash: "live-xyz",
		},
		outcome:  "success",
		started:  time.Unix(100, 0),
		finished: time.Unix(101, 0),
	}
	if !commitStartResult(result, sessionFrontDoor(store), &clock.Fake{Time: time.Unix(102, 0)}, events.NewFake(), 0, ioDiscard{}, ioDiscard{}) {
		t.Fatal("commitStartResult returned false for successful start")
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := got.Metadata[session.MCPIdentityMetadataKey]; v != "" {
		t.Fatalf("mcp_identity = %q, want cleared; a non-ACP start must not re-stamp the ACP identity", v)
	}
}
