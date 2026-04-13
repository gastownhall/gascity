package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestShouldRouteResolvedSessionProviderDefersOnlyLiveLegacyExecBeads(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "active-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start(active-worker): %v", err)
	}
	bp := &agentBuildParams{
		sp:                     sp,
		defaultSessionProvider: "tmux",
		sessionBeads: newSessionBeadSnapshot([]beads.Bead{
			{
				Type: sessionBeadType,
				Metadata: map[string]string{
					"session_name": "active-worker",
					"state":        "active",
				},
			},
			{
				Type: sessionBeadType,
				Metadata: map[string]string{
					"session_name": "sleeping-worker",
					"state":        "asleep",
				},
			},
		}),
	}

	if shouldRouteResolvedSessionProvider(bp, TemplateParams{SessionName: "active-worker", SessionProvider: "exec:/tmp/remote-worker"}) {
		t.Fatal("active legacy exec bead should not route until it is drained/stopped")
	}
	if !shouldRouteResolvedSessionProvider(bp, TemplateParams{SessionName: "sleeping-worker", SessionProvider: "exec:/tmp/remote-worker"}) {
		t.Fatal("non-running legacy exec bead should route so the next start uses exec")
	}
}

func TestShouldRouteResolvedSessionProviderFindsLiveLegacyExecAcrossDuplicateBeads(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	bp := &agentBuildParams{
		sp:                     sp,
		defaultSessionProvider: "tmux",
		sessionBeads: newSessionBeadSnapshot([]beads.Bead{
			{
				Type: sessionBeadType,
				Metadata: map[string]string{
					"session_name": "worker",
					"state":        "creating",
				},
			},
			{
				Type: sessionBeadType,
				Metadata: map[string]string{
					"session_name": "worker",
					"state":        "active",
				},
			},
		}),
	}

	if shouldRouteResolvedSessionProvider(bp, TemplateParams{SessionName: "worker", SessionProvider: "exec:/tmp/remote-worker"}) {
		t.Fatal("active legacy exec bead should defer routing even when a duplicate creating bead is listed first")
	}
}

func TestShouldRouteResolvedSessionProviderRoutesProfiledDefaultProvider(t *testing.T) {
	bp := &agentBuildParams{
		sp:                     runtime.NewFake(),
		defaultSessionProvider: "exec:/tmp/remote-worker",
	}

	if !shouldRouteResolvedSessionProvider(bp, TemplateParams{
		SessionName:            "worker",
		SessionProvider:        "exec:/tmp/remote-worker",
		SessionProviderProfile: remoteWorkerProfile,
	}) {
		t.Fatal("profiled exec provider should route even when provider path matches the unprofiled default")
	}
}

func TestShouldRouteResolvedSessionProviderDefersLiveOwnedProviderMismatch(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	bp := &agentBuildParams{
		sp:                     sp,
		defaultSessionProvider: "tmux",
		sessionBeads: newSessionBeadSnapshot([]beads.Bead{{
			Type: sessionBeadType,
			Metadata: map[string]string{
				"session_name":                     "worker",
				"state":                            "active",
				sessionpkg.MetadataSessionProvider: "acp",
				sessionpkg.MetadataSessionProviderProfile: "",
			},
		}}),
	}

	if shouldRouteResolvedSessionProvider(bp, TemplateParams{
		SessionName:            "worker",
		SessionProvider:        "exec:/tmp/remote-worker",
		SessionProviderProfile: remoteWorkerProfile,
	}) {
		t.Fatal("live owned provider mismatch should not switch routes before the old backend stops")
	}
}

func TestShouldRouteResolvedSessionProviderDefersLiveNonExecProviderMismatch(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	bp := &agentBuildParams{
		sp:                     sp,
		defaultSessionProvider: "tmux",
		sessionBeads: newSessionBeadSnapshot([]beads.Bead{{
			Type: sessionBeadType,
			Metadata: map[string]string{
				"session_name":                     "worker",
				"state":                            "active",
				sessionpkg.MetadataSessionProvider: "exec:/tmp/remote-worker",
			},
		}}),
	}

	if shouldRouteResolvedSessionProvider(bp, TemplateParams{
		SessionName:     "worker",
		SessionProvider: "acp",
	}) {
		t.Fatal("live provider mismatch should not switch routes for non-exec desired providers")
	}
}

func TestShouldRouteResolvedSessionProviderRoutesStoppedOwnedProviderMismatch(t *testing.T) {
	bp := &agentBuildParams{
		sp:                     runtime.NewFake(),
		defaultSessionProvider: "tmux",
		sessionBeads: newSessionBeadSnapshot([]beads.Bead{{
			Type: sessionBeadType,
			Metadata: map[string]string{
				"session_name":                     "worker",
				"state":                            "asleep",
				sessionpkg.MetadataSessionProvider: "acp",
			},
		}}),
	}

	if !shouldRouteResolvedSessionProvider(bp, TemplateParams{
		SessionName:            "worker",
		SessionProvider:        "exec:/tmp/remote-worker",
		SessionProviderProfile: remoteWorkerProfile,
	}) {
		t.Fatal("stopped owned provider mismatch should switch routes so the next start uses the desired backend")
	}
}

func TestShouldRouteResolvedSessionProviderUnroutesStoppedOwnedDefaultMismatch(t *testing.T) {
	bp := &agentBuildParams{
		sp:                     runtime.NewFake(),
		defaultSessionProvider: "tmux",
		sessionBeads: newSessionBeadSnapshot([]beads.Bead{{
			Type: sessionBeadType,
			Metadata: map[string]string{
				"session_name":                     "worker",
				"state":                            "asleep",
				sessionpkg.MetadataSessionProvider: "exec:/tmp/remote-worker",
			},
		}}),
	}

	if !shouldRouteResolvedSessionProvider(bp, TemplateParams{
		SessionName:     "worker",
		SessionProvider: "tmux",
	}) {
		t.Fatal("stopped owned provider mismatch should unroute back to the default backend")
	}
}

func TestShouldRouteResolvedSessionProviderUnroutesStoppedLegacyACPTransport(t *testing.T) {
	bp := &agentBuildParams{
		sp:                     runtime.NewFake(),
		defaultSessionProvider: "tmux",
		sessionBeads: newSessionBeadSnapshot([]beads.Bead{{
			Type: sessionBeadType,
			Metadata: map[string]string{
				"session_name": "worker",
				"state":        "asleep",
				"transport":    "acp",
			},
		}}),
	}

	if !shouldRouteResolvedSessionProvider(bp, TemplateParams{
		SessionName:     "worker",
		SessionProvider: "tmux",
	}) {
		t.Fatal("stopped legacy ACP transport should unroute back to the default backend")
	}
}

func TestShouldRouteResolvedSessionProviderDefersLiveLegacyACPTransportDefaultMismatch(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	bp := &agentBuildParams{
		sp:                     sp,
		defaultSessionProvider: "tmux",
		sessionBeads: newSessionBeadSnapshot([]beads.Bead{{
			Type: sessionBeadType,
			Metadata: map[string]string{
				"session_name": "worker",
				"state":        "active",
				"transport":    "acp",
			},
		}}),
	}

	if shouldRouteResolvedSessionProvider(bp, TemplateParams{
		SessionName:     "worker",
		SessionProvider: "tmux",
	}) {
		t.Fatal("live legacy ACP transport should not unroute back to default until the old backend stops")
	}
}
