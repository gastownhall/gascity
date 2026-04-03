package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// stopErrorProvider wraps runtime.Fake but returns an error on Stop.
type stopErrorProvider struct {
	*runtime.Fake
	stopErr error
}

func (s *stopErrorProvider) Stop(_ string) error {
	return s.stopErr
}

func TestDoRigRestart(t *testing.T) {
	sp := runtime.NewFake()
	// Start 2 sessions for agents in the rig.
	// SessionNameFor replaces "/" with "--".
	if err := sp.Start(context.Background(), "frontend--polecat", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "frontend--worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	agents := []config.Agent{
		{Name: "polecat", Dir: "frontend", MaxActiveSessions: intPtr(1)},
		{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(1)},
	}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, nil, nil, agents, "frontend", "city", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Both sessions should be stopped.
	if sp.IsRunning("frontend--polecat") {
		t.Error("polecat session still running")
	}
	if sp.IsRunning("frontend--worker") {
		t.Error("worker session still running")
	}

	// 2 SessionStopped events recorded.
	if len(rec.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.Events))
	}
	for _, e := range rec.Events {
		if e.Type != events.SessionStopped {
			t.Errorf("event type = %q, want %q", e.Type, events.SessionStopped)
		}
	}

	// stdout message.
	if got := stdout.String(); !strings.Contains(got, "Restarted 2 agent(s)") {
		t.Errorf("stdout = %q, want to contain 'Restarted 2 agent(s)'", got)
	}
}

func TestDoRigRestartNoneRunning(t *testing.T) {
	sp := runtime.NewFake() // no sessions started
	rec := events.NewFake()
	agents := []config.Agent{
		{Name: "polecat", Dir: "frontend", MaxActiveSessions: intPtr(1)},
	}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, nil, nil, agents, "frontend", "city", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	if got := stdout.String(); !strings.Contains(got, "Restarted 0 agent(s)") {
		t.Errorf("stdout = %q, want to contain 'Restarted 0 agent(s)'", got)
	}
	if len(rec.Events) != 0 {
		t.Errorf("got %d events, want 0", len(rec.Events))
	}
}

func TestDoRigRestartWithPool(t *testing.T) {
	sp := runtime.NewFake()
	// Pool agent with Max=3, only 2 running.
	// SessionNameFor replaces "/" with "--".
	if err := sp.Start(context.Background(), "frontend--worker-1", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "frontend--worker-2", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	// worker-3 is NOT running.

	rec := events.NewFake()
	agents := []config.Agent{
		{Name: "worker", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 2"},
	}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, nil, nil, agents, "frontend", "city", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Both running instances should be stopped.
	if sp.IsRunning("frontend--worker-1") {
		t.Error("worker-1 still running")
	}
	if sp.IsRunning("frontend--worker-2") {
		t.Error("worker-2 still running")
	}

	// 2 events.
	if len(rec.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.Events))
	}

	// Correct count in output.
	if got := stdout.String(); !strings.Contains(got, "Restarted 2 agent(s)") {
		t.Errorf("stdout = %q, want to contain 'Restarted 2 agent(s)'", got)
	}
}

func TestDoRigRestart_UsesLogicalAgentSubjectForCustomSessionNames(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "frontend/worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "frontend/worker",
			"session_name": "custom-worker",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "custom-worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	agents := []config.Agent{{Name: "worker", Dir: "frontend", MaxActiveSessions: intPtr(1)}}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, store, nil, agents, "frontend", "city", "{{.Agent}}", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Subject != "frontend/worker" {
		t.Fatalf("event subject = %q, want %q", rec.Events[0].Subject, "frontend/worker")
	}
}

func TestDoRigRestart_UsesPoolSessionBeadsForCustomSessionNames(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "frontend/worker-1",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "frontend/worker",
			"agent_name":   "frontend/worker-1",
			"session_name": "custom-worker-1",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "custom-worker-1", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	agents := []config.Agent{{
		Name:              "worker",
		Dir:               "frontend",
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2), ScaleCheck: "echo 1",
	}}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, store, nil, agents, "frontend", "city", "{{.Agent}}", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if sp.IsRunning("custom-worker-1") {
		t.Fatal("custom pool session still running after rig restart")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Subject != "frontend/worker-1" {
		t.Fatalf("event subject = %q, want %q", rec.Events[0].Subject, "frontend/worker-1")
	}
}

func TestDoRigRestart_UsesUnlimitedPoolSessionBeadsForCustomSessionNames(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "frontend/worker-7",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "frontend/worker",
			"agent_name":   "frontend/worker-7",
			"session_name": "custom-worker-7",
			"pool_slot":    "7",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "custom-worker-7", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	agents := []config.Agent{{
		Name:              "worker",
		Dir:               "frontend",
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(-1), ScaleCheck: "echo 1",
	}}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, store, nil, agents, "frontend", "city", "{{.Agent}}", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if sp.IsRunning("custom-worker-7") {
		t.Fatal("custom unlimited-pool session still running after rig restart")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Subject != "frontend/worker-7" {
		t.Fatalf("event subject = %q, want %q", rec.Events[0].Subject, "frontend/worker-7")
	}
}

func TestDoRigRestart_UsesLegacyPoolAgentLabelForCustomSessionNames(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:frontend/worker-1"},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "custom-worker-1",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "custom-worker-1", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	agents := []config.Agent{{
		Name:              "worker",
		Dir:               "frontend",
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2), ScaleCheck: "echo 1",
	}}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, store, nil, agents, "frontend", "city", "{{.Agent}}", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if sp.IsRunning("custom-worker-1") {
		t.Fatal("legacy custom pool session still running after rig restart")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Subject != "frontend/worker-1" {
		t.Fatalf("event subject = %q, want %q", rec.Events[0].Subject, "frontend/worker-1")
	}
}

func TestDoRigRestart_UsesLegacyUnlimitedPoolAgentLabelForCustomSessionNames(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:frontend/worker-7"},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "custom-worker-7",
			"pool_slot":    "7",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "custom-worker-7", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	agents := []config.Agent{{
		Name:              "worker",
		Dir:               "frontend",
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(-1), ScaleCheck: "echo 1",
	}}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, store, nil, agents, "frontend", "city", "{{.Agent}}", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if sp.IsRunning("custom-worker-7") {
		t.Fatal("legacy custom unlimited-pool session still running after rig restart")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Subject != "frontend/worker-7" {
		t.Fatalf("event subject = %q, want %q", rec.Events[0].Subject, "frontend/worker-7")
	}
}

func TestDoRigRestart_UsesFullCityGraphForStopOrdering(t *testing.T) {
	sp := newGatedStopProvider()
	for _, name := range []string{"frontend--api", "frontend--cache"} {
		if err := sp.Start(context.Background(), name, runtime.Config{Command: "echo"}); err != nil {
			t.Fatal(err)
		}
	}

	fullCfg := &config.City{
		Agents: []config.Agent{
			{Name: "api", Dir: "frontend", MaxActiveSessions: intPtr(1), DependsOn: []string{"backend/db"}},
			{Name: "cache", Dir: "frontend", MaxActiveSessions: intPtr(1)},
			{Name: "db", Dir: "backend", MaxActiveSessions: intPtr(1), DependsOn: []string{"frontend/cache"}},
		},
	}
	rigAgents := []config.Agent{
		{Name: "api", Dir: "frontend", MaxActiveSessions: intPtr(1), DependsOn: []string{"backend/db"}},
		{Name: "cache", Dir: "frontend", MaxActiveSessions: intPtr(1)},
	}

	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- doRigRestart(sp, rec, nil, fullCfg, rigAgents, "frontend", "city", "", &stdout, &stderr)
	}()

	firstWave := sp.waitForStops(t, 1)
	if !containsAll(firstWave, "frontend--api") {
		t.Fatalf("first stop wave = %v, want frontend--api", firstWave)
	}
	sp.ensureNoFurtherStop(t)
	sp.release("frontend--api")

	secondWave := sp.waitForStops(t, 1)
	if !containsAll(secondWave, "frontend--cache") {
		t.Fatalf("second stop wave = %v, want frontend--cache", secondWave)
	}
	sp.release("frontend--cache")

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, want 0", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("doRigRestart did not finish")
	}
}

func TestDoRigRestartStopError(t *testing.T) {
	// When Stop fails, the agent is skipped but the command still succeeds.
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "frontend--polecat", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}
	wrapper := &stopErrorProvider{Fake: sp, stopErr: fmt.Errorf("tmux borked")}

	rec := events.NewFake()
	agents := []config.Agent{
		{Name: "polecat", Dir: "frontend", MaxActiveSessions: intPtr(1)},
	}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(wrapper, rec, nil, nil, agents, "frontend", "city", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0", code)
	}
	// Error logged to stderr.
	if !strings.Contains(stderr.String(), "tmux borked") {
		t.Errorf("stderr = %q, want to contain 'tmux borked'", stderr.String())
	}
	// 0 killed (stop failed).
	if got := stdout.String(); !strings.Contains(got, "Restarted 0 agent(s)") {
		t.Errorf("stdout = %q, want to contain 'Restarted 0 agent(s)'", got)
	}
}
