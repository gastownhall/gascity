package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeAdoptionProvider implements runtime.Provider for adoption barrier tests.
type fakeAdoptionProvider struct {
	runtime.Provider
	running []string
}

func (f *fakeAdoptionProvider) ListRunning(_ string) ([]string, error) {
	return f.running, nil
}

func TestAdoptionBarrier_NoRunning(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{running: nil}
	cfg := &config.City{}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier(store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("barrier should pass with no running sessions")
	}
	if result.Total != 0 {
		t.Errorf("Total = %d, want 0", result.Total)
	}
}

func TestAdoptionBarrier_AdoptsRunning(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor", "test-city-worker"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "worker"},
		},
	}
	var stderr bytes.Buffer
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	result, passed := runAdoptionBarrier(store, sp, cfg, "test-city", clk, &stderr, false)
	if !passed {
		t.Errorf("barrier should pass, stderr: %s", stderr.String())
	}
	if result.Adopted != 2 {
		t.Errorf("Adopted = %d, want 2", result.Adopted)
	}
	if result.Total != 2 {
		t.Errorf("Total = %d, want 2", result.Total)
	}

	// Verify beads were created with correct labels.
	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(beadList) != 2 {
		t.Errorf("beads count = %d, want 2", len(beadList))
	}
	// Verify agent: label is present on adopted beads.
	for _, b := range beadList {
		hasAgentLabel := false
		for _, l := range b.Labels {
			if len(l) > len("agent:") && l[:len("agent:")] == "agent:" {
				hasAgentLabel = true
				break
			}
		}
		if !hasAgentLabel {
			t.Errorf("bead %q missing agent: label, labels = %v", b.Title, b.Labels)
		}
		if b.Metadata["continuation_epoch"] != "1" {
			t.Errorf("bead %q continuation_epoch = %q, want 1", b.Title, b.Metadata["continuation_epoch"])
		}
		if b.Metadata["instance_token"] == "" {
			t.Errorf("bead %q missing instance_token", b.Title)
		}
	}
}

func TestAdoptionBarrier_SkipsExistingBead(t *testing.T) {
	store := beads.NewMemStore()
	// Pre-create a bead for mayor.
	_, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "test-city-mayor",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor", "test-city-worker"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "worker"},
		},
	}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier(store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("barrier should pass")
	}
	if result.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", result.Adopted)
	}
	if result.AlreadyHadBead != 1 {
		t.Errorf("AlreadyHadBead = %d, want 1", result.AlreadyHadBead)
	}
}

func TestAdoptionBarrier_ClosedBeadDoesNotBlock(t *testing.T) {
	store := beads.NewMemStore()
	// Pre-create and close a bead for mayor.
	b, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "test-city-mayor",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}

	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor"}}
	cfg := &config.City{Agents: []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}}}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier(store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("barrier should pass")
	}
	if result.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1 (closed bead should not prevent adoption)", result.Adopted)
	}
}

func TestAdoptionBarrier_Rerunnable(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor"}}
	cfg := &config.City{Agents: []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}}}
	var stderr bytes.Buffer

	// First run: adopts.
	r1, _ := runAdoptionBarrier(store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if r1.Adopted != 1 {
		t.Fatalf("first run Adopted = %d, want 1", r1.Adopted)
	}

	// Second run: dedup prevents duplicates.
	r2, passed := runAdoptionBarrier(store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("second run: barrier should pass")
	}
	if r2.Adopted != 0 {
		t.Errorf("second run Adopted = %d, want 0", r2.Adopted)
	}
	if r2.AlreadyHadBead != 1 {
		t.Errorf("second run AlreadyHadBead = %d, want 1", r2.AlreadyHadBead)
	}
}

func TestAdoptionBarrier_DryRun(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor", "test-city-worker"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "worker"},
		},
	}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier(store, sp, cfg, "test-city", clock.Real{}, &stderr, true)
	if !passed {
		t.Error("dry run barrier should pass")
	}
	if result.Adopted != 2 {
		t.Errorf("Adopted = %d, want 2", result.Adopted)
	}

	// Verify no beads were actually created.
	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(beadList) != 0 {
		t.Errorf("dry run created %d beads, want 0", len(beadList))
	}
}

func TestAdoptionBarrier_NilStore(t *testing.T) {
	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor"}}
	cfg := &config.City{}
	var stderr bytes.Buffer

	_, passed := runAdoptionBarrier(nil, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if passed {
		t.Error("nil store: barrier should not pass")
	}
}

func TestAdoptionBarrier_PoolSlotDetection(t *testing.T) {
	store := beads.NewMemStore()
	// Pool instance session name: base "worker" produces session "worker",
	// so instance "worker-3" has session name "worker-3".
	sp := &fakeAdoptionProvider{running: []string{"worker-3"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		},
	}
	var stderr bytes.Buffer

	result, _ := runAdoptionBarrier(store, sp, cfg, "test-city", clock.Real{}, &stderr, true)
	// Pool instance "worker-3" should resolve to config agent "worker"
	// via resolvePoolBase, with pool slot 3. AgentName should be the
	// expanded instance name "worker-3" (matching syncSessionBeads).
	found := false
	for _, d := range result.Details {
		if d.SessionName == "worker-3" && d.PoolSlot == 3 && d.AgentName == "worker-3" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected detail with PoolSlot=3, AgentName=worker-3 for worker-3, got %+v", result.Details)
	}
}

func TestAdoptionBarrier_PoolOutOfBounds(t *testing.T) {
	store := beads.NewMemStore()
	// Pool instance exceeding max (5).
	sp := &fakeAdoptionProvider{running: []string{"worker-7"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)},
		},
	}
	var stderr bytes.Buffer

	result, _ := runAdoptionBarrier(store, sp, cfg, "test-city", clock.Real{}, &stderr, true)
	found := false
	for _, d := range result.Details {
		if d.SessionName == "worker-7" && d.PoolSlot == 7 && d.OutOfBounds {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected out-of-bounds detail for worker-7, got %+v", result.Details)
	}
}

func TestParsePoolSlot(t *testing.T) {
	tests := []struct {
		name string
		want int
	}{
		{"s-worker-3", 3},
		{"s-worker-10", 10},
		{"s-mayor", 0},
		{"worker", 0},
	}
	for _, tt := range tests {
		got := parsePoolSlot(tt.name)
		if got != tt.want {
			t.Errorf("parsePoolSlot(%q) = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestAdoptionBarrier_SingletonWithNumericSuffix(t *testing.T) {
	store := beads.NewMemStore()
	// Singleton agent named "db-node-1" — should NOT get pool_slot metadata.
	sp := &fakeAdoptionProvider{running: []string{"db-node-1"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "db-node-1", MaxActiveSessions: intPtr(1)}, // singleton agent
		},
	}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier(store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Errorf("barrier should pass, stderr: %s", stderr.String())
	}
	if result.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", result.Adopted)
	}
	// Verify no pool_slot on the bead.
	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	for _, b := range beadList {
		if b.Metadata["pool_slot"] != "" {
			t.Errorf("singleton agent should not have pool_slot, got %q", b.Metadata["pool_slot"])
		}
	}
}

func TestAdoptionBarrier_UnknownSession(t *testing.T) {
	store := beads.NewMemStore()
	// Running session that doesn't match any config agent.
	sp := &fakeAdoptionProvider{running: []string{"unknown-session"}}
	cfg := &config.City{} // no agents configured
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier(store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("barrier should pass (adopt permissively)")
	}
	if result.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", result.Adopted)
	}
}
