package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func intPtrRestartBoundary(n int) *int { return &n }

func TestDoRigRestartUsesWorkerBoundaryForKnownSession(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	mgr := newSessionManagerWithConfig("", store, sp, nil)
	info, err := mgr.Create(context.Background(), "frontend/worker", "Worker", "claude", t.TempDir(), "claude", nil, sessionpkg.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	rec := events.NewFake()
	agents := []config.Agent{{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtrRestartBoundary(1)}}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, store, nil, agents, "frontend", "city", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if sp.IsRunning(info.SessionName) {
		t.Fatal("worker session still running after rig restart")
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != sessionpkg.StateSuspended {
		t.Fatalf("state = %q, want %q", got.State, sessionpkg.StateSuspended)
	}
}
