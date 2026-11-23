package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestReconcileSessionBeads_BrokenPromptTemplateKeepsWorkingPoolSession pins
// what one reconcile tick does to a running pool once its prompt_template
// stops parsing (ga-dlano3jn). resolveTemplate now refuses a template that was
// read but fails to parse, so the pool agent drops out of desired state. A
// running session that holds assigned work must not be drained for that, and
// no new session may be started from the broken template. What happens to
// an idle, unclaimed session is pinned below as current behavior.
func TestReconcileSessionBeads_BrokenPromptTemplateKeepsWorkingPoolSession(t *testing.T) {
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)
	cityDir := t.TempDir()
	writeCityTOML(t, cityDir, "trace-town", "worker")
	if err := os.MkdirAll(filepath.Join(cityDir, "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir prompts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "prompts", "worker.template.md"), []byte("Bad: {{ .Unclosed"), 0o644); err != nil {
		t.Fatalf("write broken prompt template: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "trace-town"},
		Session:   config.SessionConfig{Provider: "fake"},
		Agents: []config.Agent{{
			Name:              "worker",
			Dir:               "repo",
			StartCommand:      "true",
			PromptTemplate:    "prompts/worker.template.md",
			MinActiveSessions: intPtr(0),
			MaxActiveSessions: intPtr(3),
		}},
	}
	store := beads.NewMemStore()
	sp := runtime.NewFake()

	// Queued work keeps pool demand up, so a resolvable template would want
	// another session.
	routedWork := func(title string) beads.Bead {
		t.Helper()
		b, err := store.Create(beads.Bead{
			Title: title, Type: "task", Status: "open",
			Metadata: map[string]string{"gc.routed_to": "repo/worker"},
		})
		if err != nil {
			t.Fatalf("create routed work %q: %v", title, err)
		}
		return b
	}
	routedWork("queued work")
	held := routedWork("held work")

	startPoolSession := func(slot int) (beads.Bead, string) {
		t.Helper()
		b := createCanonicalPoolSession(t, store, &cfg.Agents[0], now, slot)
		setPoolSessionActive(t, store, b.ID)
		b, err := store.Get(b.ID)
		if err != nil {
			t.Fatalf("reload pool session: %v", err)
		}
		name := b.Metadata["session_name"]
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatalf("start pool runtime %s: %v", name, err)
		}
		return b, name
	}
	working, workingName := startPoolSession(1)
	idle, idleName := startPoolSession(2)

	inProgress := "in_progress"
	assignee := working.ID
	if err := store.Update(held.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("assign held work: %v", err)
	}

	var stderr bytes.Buffer
	ds := buildDesiredState("trace-town", cityDir, now, cfg, sp, store, &stderr)
	if !strings.Contains(stderr.String(), "prompt_template") {
		t.Fatalf("desired-state build did not report the broken prompt_template; stderr=%s", stderr.String())
	}
	for name, tp := range ds.State {
		if tp.TemplateName == "repo/worker" {
			t.Fatalf("desired state kept %s for the broken template; prompt=%q", name, tp.Prompt)
		}
	}

	dt := newDrainTracker()
	reconcileSessionBeadsAtPath(
		context.Background(), t.TempDir(), []beads.Bead{working, idle}, ds.State, map[string]bool{"repo/worker": true},
		cfg, sp, store, newFakeDrainOps(), nil, nil, nil, dt, ds.PoolDesiredCounts, false, nil, "trace-town",
		nil, &clock.Fake{Time: now}, events.Discard, 0, 0, io.Discard, &stderr,
	)

	// The session holding assigned work keeps running: no drain, no stop.
	if dt.get(working.ID) != nil {
		t.Fatalf("working session %s began draining for a broken prompt_template; stderr=%s", workingName, stderr.String())
	}
	if stops := sp.CountCalls("Stop", workingName); stops != 0 || !sp.IsRunning(workingName) {
		t.Fatalf("working session %s: Stop calls = %d, running = %t; want 0 and true", workingName, stops, sp.IsRunning(workingName))
	}
	gotWorking, err := store.Get(working.ID)
	if err != nil {
		t.Fatalf("reload working session: %v", err)
	}
	if gotWorking.Status == "closed" {
		t.Fatalf("working session bead closed; metadata=%v", gotWorking.Metadata)
	}

	// Nothing new is started from the broken template: the only Start calls are
	// the two this test seeded.
	starts := 0
	for _, c := range sp.SnapshotCalls() {
		if c.Method == "Start" {
			starts++
		}
	}
	if starts != 2 {
		t.Fatalf("Start calls = %d, want 2 (the seeded sessions only); a broken template must not start a session", starts)
	}

	// Pinned on purpose (ga-dlano3jn): an idle, unclaimed pool session of the
	// broken template is no longer desired, so this tick begins its drain. It is
	// still running after one tick; the stop lands after the drain grace. This
	// pins current behavior, it does not endorse it; change it deliberately.
	if dt.get(idle.ID) == nil {
		t.Fatalf("idle session %s: no drain begun; the pinned behavior changed (ga-dlano3jn); stderr=%s", idleName, stderr.String())
	}
	if stops := sp.CountCalls("Stop", idleName); stops != 0 || !sp.IsRunning(idleName) {
		t.Fatalf("idle session %s: Stop calls = %d, running = %t; want 0 and true after one tick", idleName, stops, sp.IsRunning(idleName))
	}
}
