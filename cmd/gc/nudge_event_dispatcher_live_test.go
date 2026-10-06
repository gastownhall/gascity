package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/herdr"
	"github.com/gastownhall/gascity/internal/runtime/herdr/herdrtest"
)

func TestNudgeEventDispatcherLiveHerdr(t *testing.T) {
	herdrtest.RequireLive(t)
	t.Setenv("GC_BEADS", "file")

	herdrSession := fmt.Sprintf("gctest-nudge-dispatch-%d", time.Now().UnixNano())
	cityPath := t.TempDir()
	p := herdr.New(herdrSession, t.TempDir(), cityPath, 0, 0)
	_ = p.TeardownServer()
	t.Cleanup(func() { _ = p.TeardownServer() })
	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}

	const agentName = "nudge-live-a"
	startCtx, startCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer startCancel()
	if err := p.Start(startCtx, agentName, runtime.Config{WorkDir: cityPath, Command: "/bin/sh"}); err != nil {
		t.Fatalf("provider Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(agentName) })

	store := openNudgeBeadStore(cityPath)
	if store.Store == nil {
		t.Fatal("opening city bead store")
	}
	if _, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": agentName,
			"alias":        "worker",
			"state":        "active",
		},
	}); err != nil {
		t.Fatalf("creating session bead: %v", err)
	}

	herdrtest.Poll(t, fmt.Sprintf("agent %q registering with herdr", agentName), 15*time.Second, func() (bool, string) {
		return p.IsRunning(agentName), "not running yet"
	})

	ctx, cancel := context.WithCancel(context.Background())
	d := newNudgeEventDispatcher(ctx, cityPath, testWriter(t), "live", testNudgeDispatchStores(cityPath))
	seen := newPasses()
	d.observePasses(seen.record)
	d.update(p, &config.City{}, true)
	defer func() {
		cancel()
		select {
		case <-d.workerDone:
		case <-time.After(5 * time.Second):
			t.Log("dispatcher worker did not stop within 5s")
		}
	}()
	if !d.streaming() {
		t.Fatal("dispatcher not streaming against live herdr")
	}
	seen.next(t, "the subscription's leading resync pass")

	pane := func() string { return herdrLivePaneID(p, agentName) }
	herdrtest.ReportAgent(t, herdrSession, agentName, "working", pane)
	const nudgeText = "wait satisfied: live-dispatch proceed"
	if err := enqueueQueuedNudge(cityPath, newQueuedNudge("worker", nudgeText, time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	herdrtest.ReportAgent(t, herdrSession, agentName, "idle", pane)
	for {
		filter := seen.next(t, "a delivery pass caused by the idle report")
		if filter == agentName {
			break
		}
		if filter != "" {
			t.Fatalf("pass ran for session %q, want %q", filter, agentName)
		}
	}

	state, err := nudgequeue.LoadState(cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending)+len(state.InFlight) == 0 {
		t.Fatalf("queue is empty after refused delivery: state=%+v", state)
	}

	pollersDir := filepath.Join(cityPath, ".gc", "nudges", "pollers")
	if entries, err := os.ReadDir(pollersDir); err == nil && len(entries) > 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("sidecar poller artifacts present: %v", names)
	}
}
