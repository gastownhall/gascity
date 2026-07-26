package session

import (
	"context"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	transportTestSession      = "worker-session"
	transportTestTemplate     = "example/agents.worker"
	transportTestIdentity     = "example/agents.worker"
	transportTestMCP          = `[{"name":"fixture","transport":"stdio","command":"/bin/fixture-mcp"}]`
	transportTestTmuxProvider = "cli-provider"
	transportTestACPProvider  = "acp-provider"
	transportTestCLICommand   = "agent-cli --profile test"
)

// routeRecordingProvider is a runtime.Provider that also satisfies the
// unexported acpRouteRegistrar seam, so a test can observe whether classifying
// a session as ACP actually routes it onto the ACP runtime. The bug pinned here
// is not cosmetic: routeACPIfNeeded hands the session to the ACP backend.
type routeRecordingProvider struct {
	*runtime.Fake
	routed   []string
	unrouted []string
}

func (p *routeRecordingProvider) RouteACP(name string) { p.routed = append(p.routed, name) }
func (p *routeRecordingProvider) Unroute(name string)  { p.unrouted = append(p.unrouted, name) }

func newRouteRecordingProvider() *routeRecordingProvider {
	return &routeRecordingProvider{Fake: runtime.NewFake()}
}

// TestStaleMCPIdentityDoesNotReclassifyTmuxSession exercises the failure
// through the create and routing paths:
//
//	create a session with a tmux-only provider   -> transport recorded
//	attach a stale mcp_identity to the same bead -> must stay tmux
//
// Before the transport was recorded for tmux, the created bead carried no
// "transport" key at all, so transportForBead fell through to the stored ACP
// artifacts and reported "acp", causing routeACPIfNeeded to select the wrong
// runtime.
func TestStaleMCPIdentityDoesNotReclassifyTmuxSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := newRouteRecordingProvider()
	m := NewManagerWithOptions(store, sp)

	info, err := m.CreateSession(context.Background(), CreateOptions{
		Template: transportTestTemplate,
		Title:    "worker",
		Command:  transportTestCLICommand,
		WorkDir:  t.TempDir(),
		Provider: transportTestTmuxProvider,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := store.SetMetadata(info.ID, MCPIdentityMetadataKey, transportTestIdentity); err != nil {
		t.Fatalf("SetMetadata(mcp_identity): %v", err)
	}
	if err := store.SetMetadata(info.ID, MCPServersSnapshotMetadataKey, transportTestMCP); err != nil {
		t.Fatalf("SetMetadata(mcp_servers_snapshot): %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", info.ID, err)
	}

	if got, _ := m.transportForBead(b, info.SessionName); got != "tmux" {
		t.Errorf("transportForBead = %q, want tmux; a stale mcp_identity must not reclassify a tmux session", got)
	}
	if got, _ := m.transportForInfo(infoFromPersistedBead(b)); got != "tmux" {
		t.Errorf("transportForInfo = %q, want tmux (the Info twin must agree with the bead form)", got)
	}
	if got := m.infoFromBead(b).Transport; got != "tmux" {
		t.Errorf("infoFromBead().Transport = %q, want tmux", got)
	}
	if len(sp.routed) != 0 {
		t.Errorf("RouteACP called for %v; a tmux session must never be routed onto the ACP runtime", sp.routed)
	}
}

// TestLegacyBeadMCPIdentityStillClassifiesACP is the backward-compatibility
// half. Beads created before the transport key was recorded for every transport
// carry no "transport" at all, and a genuinely-ACP one is only recognizable by
// its stored MCP artifacts. That fallback must keep working, and must still
// route.
func TestLegacyBeadMCPIdentityStillClassifiesACP(t *testing.T) {
	for name, meta := range map[string]map[string]string{
		"identity-only": {
			"session_name":         "s-legacy",
			"state":                "asleep",
			"provider":             transportTestACPProvider,
			MCPIdentityMetadataKey: transportTestIdentity,
		},
		"snapshot-only": {
			"session_name":                "s-legacy",
			"state":                       "asleep",
			"provider":                    transportTestACPProvider,
			MCPServersSnapshotMetadataKey: transportTestMCP,
		},
		"empty-transport-key": {
			"session_name":         "s-legacy",
			"state":                "asleep",
			"transport":            "",
			"provider":             transportTestACPProvider,
			MCPIdentityMetadataKey: transportTestIdentity,
		},
	} {
		t.Run(name, func(t *testing.T) {
			sp := newRouteRecordingProvider()
			m := NewManagerWithOptions(beads.NewMemStore(), sp)
			b := sessionBeadFixture("s-legacy", "open", meta)

			if got, _ := m.transportForBead(b, "s-legacy"); got != "acp" {
				t.Errorf("transportForBead = %q, want acp (a legacy ACP bead must keep classifying as ACP)", got)
			}
			if got, _ := m.transportForInfo(infoFromPersistedBead(b)); got != "acp" {
				t.Errorf("transportForInfo = %q, want acp", got)
			}
			if got := m.infoFromBead(b).Transport; got != "acp" {
				t.Errorf("infoFromBead().Transport = %q, want acp", got)
			}
			if len(sp.routed) == 0 {
				t.Error("RouteACP was not called; a genuinely-ACP legacy session must still be routed to the ACP runtime")
			}
		})
	}
}

// TestPersistedTransportBeatsStaleMCPIdentity pins the precedence directly on a
// bead in the post-switch shape. The persisted transport must win over stale
// ACP artifacts.
func TestPersistedTransportBeatsStaleMCPIdentity(t *testing.T) {
	sp := newRouteRecordingProvider()
	m := NewManagerWithOptions(beads.NewMemStore(), sp)
	stale := sessionBeadFixture("session-current", "open", map[string]string{
		"session_name":                transportTestSession,
		"state":                       "asleep",
		"transport":                   "tmux",
		"provider":                    transportTestTmuxProvider,
		"builtin_ancestor":            "agent-cli",
		"command":                     transportTestCLICommand,
		MCPIdentityMetadataKey:        transportTestIdentity,
		MCPServersSnapshotMetadataKey: transportTestMCP,
	})

	if got, _ := m.transportForBead(stale, transportTestSession); got != "tmux" {
		t.Errorf("transportForBead = %q, want tmux", got)
	}
	if got, _ := m.transportForInfo(infoFromPersistedBead(stale)); got != "tmux" {
		t.Errorf("transportForInfo = %q, want tmux", got)
	}
	if len(sp.routed) != 0 {
		t.Errorf("RouteACP called for %v; a tmux session must never be routed onto the ACP runtime", sp.routed)
	}
}

// TestCreateSessionPersistsExplicitTransport pins that BOTH create paths record
// the transport explicitly, including tmux. Leaving the key absent for tmux is
// the root cause: transportFromMetadata then reports "" ("unknown"), and every
// reader falls through to the ACP heuristics below it.
func TestCreateSessionPersistsExplicitTransport(t *testing.T) {
	cases := []struct {
		name      string
		beadOnly  bool
		provider  string
		transport string
		want      string
	}{
		{"started-tmux-implicit", false, transportTestTmuxProvider, "", "tmux"},
		{"started-tmux-explicit", false, transportTestTmuxProvider, "tmux", "tmux"},
		{"started-acp", false, transportTestACPProvider, "acp", "acp"},
		{"started-acp-provider-alias", false, "acp", "", "acp"},
		{"bead-only-tmux-implicit", true, transportTestTmuxProvider, "", "tmux"},
		{"bead-only-acp", true, transportTestACPProvider, "acp", "acp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			m := NewManagerWithOptions(store, runtime.NewFake())
			info, err := m.CreateSession(context.Background(), CreateOptions{
				BeadOnly:  tc.beadOnly,
				Template:  "worker",
				Title:     "Probe",
				Command:   "sleep 1",
				WorkDir:   t.TempDir(),
				Provider:  tc.provider,
				Transport: tc.transport,
			})
			if err != nil {
				t.Fatalf("CreateSession: %v", err)
			}
			b, err := store.Get(info.ID)
			if err != nil {
				t.Fatalf("store.Get(%q): %v", info.ID, err)
			}
			if got := b.Metadata["transport"]; got != tc.want {
				t.Fatalf("bead transport metadata = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPersistedTransportHelper pins the split between the classifier
// (normalizeTransport: "" means "this input proves nothing") and the recorder
// (persistedTransport: never records "unknown"). Collapsing the two would take
// the legacy fallback out with it.
func TestPersistedTransportHelper(t *testing.T) {
	cases := []struct{ provider, transport, want string }{
		{transportTestTmuxProvider, "", "tmux"},
		{"", "", "tmux"},
		{transportTestTmuxProvider, "tmux", "tmux"},
		{transportTestACPProvider, "acp", "acp"},
		{"acp", "", "acp"},
		{transportTestTmuxProvider, "t3", "t3"},
	}
	for _, tc := range cases {
		if got := persistedTransport(tc.provider, tc.transport); got != tc.want {
			t.Errorf("persistedTransport(%q, %q) = %q, want %q", tc.provider, tc.transport, got, tc.want)
		}
	}
	if got := normalizeTransport(transportTestTmuxProvider, ""); got != "" {
		t.Errorf("normalizeTransport(%q, \"\") = %q, want \"\" — the classifier must keep reporting \"unknown\" so the legacy fallback below it still runs", transportTestTmuxProvider, got)
	}
}

func TestReconcileTransportMetadataPatchClearsACPArtifactsForT3(t *testing.T) {
	existing := map[string]string{
		TransportMetadataKey:          "acp",
		MCPIdentityMetadataKey:        transportTestIdentity,
		MCPServersSnapshotMetadataKey: transportTestMCP,
	}
	got := ReconcileTransportMetadataPatch(existing, "t3")
	want := MetadataPatch{
		TransportMetadataKey:          "t3",
		MCPIdentityMetadataKey:        "",
		MCPServersSnapshotMetadataKey: "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReconcileTransportMetadataPatch() = %#v, want %#v", got, want)
	}
}

func TestCreateUsesResolvedEffectiveTransportBeforeDefaulting(t *testing.T) {
	store := beads.NewMemStore()
	mgr := NewManagerWithOptions(
		store,
		runtime.NewFake(),
		WithTransportResolver(func(_, _ string) string { return "t3" }),
	)

	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		BeadOnly:  true,
		Template:  transportTestTemplate,
		Title:     "T3 worker",
		Command:   "agent-cli",
		Provider:  transportTestTmuxProvider,
		Transport: "",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get(%q): %v", info.ID, err)
	}
	if got := bead.Metadata["transport"]; got != "t3" {
		t.Fatalf("transport = %q, want %q", got, "t3")
	}
}
