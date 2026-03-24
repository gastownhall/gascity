package main

import (
	"testing"
	"time"

	sessions "github.com/gastownhall/gascity/internal/session"
)

func TestWakeReason_Constants(t *testing.T) {
	reasons := []WakeReason{WakeConfig, WakeAttached, WakeWait, WakeWork}
	seen := map[WakeReason]bool{}
	for _, r := range reasons {
		if seen[r] {
			t.Fatalf("duplicate WakeReason: %s", r)
		}
		seen[r] = true
	}
}

func TestSessionNamePattern(t *testing.T) {
	valid := []string{
		"mayor",
		"test-city-mayor",
		"worker-3",
		"agent_1",
		"A1",
		"x",
	}
	invalid := []string{
		"",
		"-starts-with-dash",
		"_starts-with-underscore",
		"has spaces",
		"has.dots",
		"has/slash",
		"has;semicolon",
		"has$dollar",
		"../traversal",
	}

	for _, name := range valid {
		if !sessions.IsSessionNameSyntaxValid(name) {
			t.Errorf("expected valid session name: %q", name)
		}
	}
	for _, name := range invalid {
		if sessions.IsSessionNameSyntaxValid(name) {
			t.Errorf("expected invalid session name: %q", name)
		}
	}
}

func TestDrainTracker(t *testing.T) {
	dt := newDrainTracker()

	// Initially empty.
	if dt.get("bead-1") != nil {
		t.Fatal("expected nil for unknown bead")
	}

	// Set and get.
	now := time.Now()
	ds := &drainState{
		startedAt:  now,
		deadline:   now.Add(5 * time.Minute),
		reason:     "idle",
		generation: 3,
	}
	dt.set("bead-1", ds)

	got := dt.get("bead-1")
	if got == nil {
		t.Fatal("expected drain state")
	}
	if got.reason != "idle" {
		t.Errorf("reason = %q, want %q", got.reason, "idle")
	}
	if got.generation != 3 {
		t.Errorf("generation = %d, want %d", got.generation, 3)
	}

	// All returns a copy.
	all := dt.all()
	if len(all) != 1 {
		t.Fatalf("all() returned %d entries, want 1", len(all))
	}

	// Remove.
	dt.remove("bead-1")
	if dt.get("bead-1") != nil {
		t.Fatal("expected nil after remove")
	}
	if len(dt.all()) != 0 {
		t.Fatal("expected empty after remove")
	}
}

func TestDrainTracker_WaitIdleProbes(t *testing.T) {
	dt := newDrainTracker()
	release := make(chan struct{})
	done := make(chan struct{})

	dt.beginIdleProbe()
	go func() {
		defer dt.doneIdleProbe()
		<-release
	}()
	go func() {
		dt.waitIdleProbes()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("waitIdleProbes returned before probe completion")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("waitIdleProbes did not unblock after probe completion")
	}
}

func TestExecSpec_ZeroValue(t *testing.T) {
	var spec ExecSpec
	if spec.Path != "" || spec.WorkDir != "" {
		t.Error("zero-value ExecSpec should have empty fields")
	}
	if spec.Args != nil {
		t.Error("zero-value Args should be nil")
	}
	if spec.Env != nil {
		t.Error("zero-value Env should be nil")
	}
}

func TestReconcilerDefaults(t *testing.T) {
	if stabilityThreshold != 30*time.Second {
		t.Errorf("stabilityThreshold = %v, want 30s", stabilityThreshold)
	}
	if defaultMaxWakesPerTick != 5 {
		t.Errorf("defaultMaxWakesPerTick = %d, want 5", defaultMaxWakesPerTick)
	}
	if defaultTickBudget != 5*time.Second {
		t.Errorf("defaultTickBudget = %v, want 5s", defaultTickBudget)
	}
	if orphanGraceTicks != 3 {
		t.Errorf("orphanGraceTicks = %d, want 3", orphanGraceTicks)
	}
	if defaultDrainTimeout != 5*time.Minute {
		t.Errorf("defaultDrainTimeout = %v, want 5m", defaultDrainTimeout)
	}
	if defaultQuarantineDuration != 5*time.Minute {
		t.Errorf("defaultQuarantineDuration = %v, want 5m", defaultQuarantineDuration)
	}
	if defaultMaxWakeAttempts != 5 {
		t.Errorf("defaultMaxWakeAttempts = %d, want 5", defaultMaxWakeAttempts)
	}
}
