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

func TestDoRigRestart_UsesBoundPoolSlotOnlySessionBeadForCustomSessionName(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "frontend/ops.furiosa",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "frontend/ops.worker",
			"session_name": "custom-ops-furiosa",
			"pool_slot":    "1",
			"state":        "awake",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "custom-ops-furiosa", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	agents := []config.Agent{{
		Name:              "worker",
		Dir:               "frontend",
		BindingName:       "ops",
		NamepoolNames:     []string{"furiosa", "nux"},
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(2), ScaleCheck: "echo 1",
	}}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, store, nil, agents, "frontend", "city", "{{.Agent}}", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if sp.IsRunning("custom-ops-furiosa") {
		t.Fatal("custom bound pool session still running after rig restart")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Subject != "frontend/ops.furiosa" {
		t.Fatalf("event subject = %q, want %q", rec.Events[0].Subject, "frontend/ops.furiosa")
	}
}

func TestDoRigRestart_UsesTemplateIdentityPoolSlotSessionBeadForCustomSessionName(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "frontend/worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "frontend/worker",
			"agent_name":   "frontend/worker",
			"session_name": "custom-worker-7",
			"pool_slot":    "7",
			"state":        "awake",
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
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(10), ScaleCheck: "echo 1",
	}}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, store, nil, agents, "frontend", "city", "{{.Agent}}", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if sp.IsRunning("custom-worker-7") {
		t.Fatal("template-identity pool session still running after rig restart")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Subject != "frontend/worker-7" {
		t.Fatalf("event subject = %q, want %q", rec.Events[0].Subject, "frontend/worker-7")
	}
}

func TestDoRigRestart_DoesNotTargetOutOfBoundsAliasOnlyBoundedPoolIdentity(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:  "stale out-of-bounds pool instance",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "frontend/worker",
			"alias":        "frontend/worker-7",
			"session_name": "custom-worker-7",
			"state":        "awake",
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
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5), ScaleCheck: "echo 1",
	}}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, store, nil, agents, "frontend", "city", "{{.Agent}}", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !sp.IsRunning("custom-worker-7") {
		t.Fatal("out-of-bounds pool session should not have been restarted")
	}
	if len(rec.Events) != 0 {
		t.Fatalf("got %d events, want 0", len(rec.Events))
	}
}

func TestDoRigRestart_PrefersLiveFallbackCandidateOncePerLogicalInstance(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	for _, bead := range []beads.Bead{
		{
			Title:  "stale duplicate",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"template":     "frontend/worker",
				"session_name": "s-stale-worker-7",
				"pool_slot":    "7",
			},
		},
		{
			Title:  "live legacy",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"common_name":  "worker",
				"session_name": "worker-7",
				"alias":        "frontend/worker-7",
			},
		},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatal(err)
		}
	}
	if err := sp.Start(context.Background(), "worker-7", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	agents := []config.Agent{{
		Name:              "worker",
		Dir:               "frontend",
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(10), ScaleCheck: "echo 1",
	}}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, store, nil, agents, "frontend", "city", "{{.Agent}}", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if sp.IsRunning("worker-7") {
		t.Fatal("live fallback session still running after rig restart")
	}
	if len(rec.Events) != 1 {
		t.Fatalf("got %d events, want 1", len(rec.Events))
	}
	if rec.Events[0].Subject != "frontend/worker-7" {
		t.Fatalf("event subject = %q, want %q", rec.Events[0].Subject, "frontend/worker-7")
	}
}

func TestDoRigRestart_StopsAllLiveCandidatesForLogicalInstance(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	for _, bead := range []beads.Bead{
		{
			Title:  "stale duplicate",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"template":     "frontend/worker",
				"session_name": "s-stale-worker-7",
				"pool_slot":    "7",
			},
		},
		{
			Title:  "live legacy",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel},
			Metadata: map[string]string{
				"common_name":  "worker",
				"session_name": "worker-7",
				"alias":        "frontend/worker-7",
			},
		},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatal(err)
		}
	}
	for _, sessionName := range []string{"s-stale-worker-7", "worker-7"} {
		if err := sp.Start(context.Background(), sessionName, runtime.Config{Command: "echo"}); err != nil {
			t.Fatal(err)
		}
	}

	rec := events.NewFake()
	agents := []config.Agent{{
		Name:              "worker",
		Dir:               "frontend",
		MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(10), ScaleCheck: "echo 1",
	}}

	var stdout, stderr bytes.Buffer
	code := doRigRestart(sp, rec, store, nil, agents, "frontend", "city", "{{.Agent}}", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	for _, sessionName := range []string{"s-stale-worker-7", "worker-7"} {
		if sp.IsRunning(sessionName) {
			t.Fatalf("%s still running after rig restart", sessionName)
		}
	}
	if len(rec.Events) != 2 {
		t.Fatalf("got %d events, want 2", len(rec.Events))
	}
	for _, event := range rec.Events {
		if event.Subject != "frontend/worker-7" {
			t.Fatalf("event subject = %q, want %q", event.Subject, "frontend/worker-7")
		}
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

// Kills a rig restart that leaves a min_active_sessions floor unrevivable
// (mc-ewptj): the restart marks the floor pool's live seats city-stop before
// it stops them, as gc stop marks every session, so cityStopPoolBeads
// revives them; a pool with no floor, a seat it does not target, and a seat
// an operator suspended (only an active row is marked) are not marked.
func TestDoRigRestart_MarksFloorPoolSeatsCityStop(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	seat := func(template, agent, name, reason string, running bool) string {
		t.Helper()
		b, err := store.Create(beads.Bead{Title: agent, Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"template": template, "agent_name": agent, "session_name": name, "pool_slot": name[len(name)-1:], "state": "awake", "sleep_reason": reason,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if running {
			if err := sp.Start(context.Background(), name, runtime.Config{Command: "echo"}); err != nil {
				t.Fatal(err)
			}
		}
		return b.ID
	}
	floored := seat("frontend/worker", "frontend/worker-1", "worker-1", "", true)
	suspended := seat("frontend/worker", "frontend/worker-2", "worker-2", "", true)
	if err := store.SetMetadataBatch(suspended, map[string]string{"state": "suspended", "sleep_intent": "user-hold", "suspended_at": time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	idle := seat("frontend/worker", "frontend/worker-3", "worker-3", "", false)
	unfloored := seat("frontend/helper", "frontend/helper-1", "helper-1", "", true)
	agents := []config.Agent{
		{Name: "worker", Dir: "frontend", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)},
		{Name: "helper", Dir: "frontend", MaxActiveSessions: intPtr(2)},
	}
	var stdout, stderr bytes.Buffer
	if code := doRigRestart(sp, events.NewFake(), store, nil, agents, "frontend", "city", "{{.Agent}}", &stdout, &stderr); code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	for id, want := range map[string]string{floored: "city-stop", suspended: "", idle: "", unfloored: ""} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got := b.Metadata["sleep_reason"]; got != want {
			t.Errorf("%s sleep_reason = %q, want %q", b.Metadata["session_name"], got, want)
		}
	}
}

// Kills a min_active_sessions seat a rig restart leaves down under legacy
// (mc-ewptj): the real doRigRestart stops a live floor seat with no work,
// leaving it asleep with its city-stop mark, and legacy's floor revival
// restarts it.
func TestLegacyFloorRevivesARigRestartedSeat(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", StartCommand: "true", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(3)}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&session, map[string]string{"state": "active", "pool_slot": "1"})
	if !env.sp.IsRunning("worker") {
		if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "true"}); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := doRigRestart(env.sp, events.NewFake(), env.store, env.cfg, env.cfg.Agents, "", "city", "", &stdout, &stderr); code != 0 {
		t.Fatalf("doRigRestart = %d: %s", code, stderr.String())
	}
	if after, _ := env.store.Get(session.ID); env.sp.IsRunning("worker") || after.Metadata["sleep_reason"] != "city-stop" {
		t.Fatalf("after the restart: running %v, row %v; want the seat stopped and marked city-stop", env.sp.IsRunning("worker"), after.Metadata)
	}
	for range 3 {
		cur, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		env.reconcileWithPoolDesired([]beads.Bead{cur}, map[string]int{"worker": 1})
		env.clk.Time = env.clk.Time.Add(time.Minute)
	}
	if !env.sp.IsRunning("worker") {
		got, _ := env.store.Get(session.ID)
		t.Fatalf("the floor seat stays down: state=%q sleep_reason=%q", got.Metadata["state"], got.Metadata["sleep_reason"])
	}
}

// Kills a city-stop mark written over a hold or a sleep that lands after the
// mark's listing (the #7787 review; gc stop's mark had the same race): the
// mark is decided again on a fresh read, so an operator's suspend keeps its
// hold, and a quarantine its own reason, and neither gets city-stop, which
// the floor revival would otherwise wake.
func TestCityStopMarkSparesASuspendAfterTheListing(t *testing.T) {
	for _, c := range []struct {
		name  string
		patch map[string]string
	}{
		{"an operator's suspend", map[string]string{"state": "suspended", "sleep_intent": "user-hold"}},
		{"a quarantine", map[string]string{"sleep_reason": "quarantine", "quarantined_until": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			store := &writeAfterListStore{MemStore: beads.NewMemStore(), patch: c.patch}
			b, err := store.Create(beads.Bead{Title: "worker-1", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
				"template": "worker", "session_name": "worker-1", "state": "awake",
			}})
			if err != nil {
				t.Fatal(err)
			}
			store.target = b.ID
			var stderr bytes.Buffer
			markSessionsCityStop(sessionFrontDoor(store), map[string]bool{"worker-1": true}, "gc rig restart", &stderr)
			got, err := store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range c.patch {
				if got.Metadata[k] != v {
					t.Fatalf("written after the listing %v: row %v (stderr %q); want %s=%q kept and no city-stop mark", store.written, got.Metadata, stderr.String(), k, v)
				}
			}
			if !store.written || got.Metadata["sleep_reason"] == "city-stop" {
				t.Fatalf("written after the listing %v: row %v; want no city-stop mark", store.written, got.Metadata)
			}
		})
	}
}

// writeAfterListStore writes patch on its target row, as an operator or the
// controller would, right after the first label listing returns.
type writeAfterListStore struct {
	*beads.MemStore
	target  string
	patch   map[string]string
	written bool
}

func (s *writeAfterListStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	out, err := s.MemStore.ListByLabel(label, limit, opts...)
	if err == nil && !s.written && s.target != "" {
		s.written = true
		err = s.SetMetadataBatch(s.target, s.patch)
	}
	return out, err
}

// Kills a stop that warns on every gc:session-labeled row the session front
// door refuses (a non-session type): the mark skips it quietly.
func TestCityStopMarkSkipsATypeDamagedRowQuietly(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{Title: "damaged", Type: "task", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{"session_name": "damaged", "state": "awake"}})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	markSessionsCityStop(sessionFrontDoor(store), nil, "gc stop", &stderr)
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 || got.Metadata["sleep_reason"] != "" {
		t.Fatalf("stderr %q, row %v; want the damaged row skipped quietly", stderr.String(), got.Metadata)
	}
}
