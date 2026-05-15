package main

import (
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// fakeAdoptionProvider implements runtime.Provider for adoption barrier tests.
type fakeAdoptionProvider struct {
	runtime.Provider
	running []string
	alive   map[string]bool
	listErr error
}

type adoptionLockProbeStore struct {
	beads.Store

	targetSessionName string
	listed            chan struct{}
	createAttempted   chan struct{}
	allowCreate       <-chan struct{}
}

type adoptionListFailureStore struct {
	beads.Store
}

type adoptionClockAdvanceStore struct {
	beads.Store

	advance  func()
	advanced bool
}

func (s *adoptionLockProbeStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	result, err := s.Store.List(query)
	if query.Label == sessionBeadLabel {
		select {
		case s.listed <- struct{}{}:
		default:
		}
	}
	return result, err
}

func (s *adoptionListFailureStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if strings.TrimSpace(query.Metadata["session_name"]) != "" {
		return nil, errors.New("live list failed")
	}
	return s.Store.List(query)
}

func (s *adoptionClockAdvanceStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	result, err := s.Store.List(query)
	if strings.TrimSpace(query.Metadata["session_name"]) != "" && !s.advanced {
		s.advanced = true
		s.advance()
	}
	return result, err
}

func (s *adoptionLockProbeStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.Type == sessionBeadType && b.Metadata["session_name"] == s.targetSessionName {
		select {
		case s.createAttempted <- struct{}{}:
		default:
		}
		<-s.allowCreate
	}
	return s.Store.Create(b)
}

type adoptionBarrierOutcome struct {
	result adoptionResult
	passed bool
}

func (f *fakeAdoptionProvider) ListRunning(_ string) ([]string, error) {
	return f.running, f.listErr
}

func (f *fakeAdoptionProvider) IsRunning(name string) bool {
	for _, running := range f.running {
		if running == name {
			return true
		}
	}
	return false
}

func (f *fakeAdoptionProvider) ProcessAlive(name string, _ []string) bool {
	if f.alive == nil {
		return true
	}
	return f.alive[name]
}

func (f *fakeAdoptionProvider) IsAttached(string) bool { return false }

func (f *fakeAdoptionProvider) GetMeta(string, string) (string, error) { return "", nil }

func (f *fakeAdoptionProvider) GetLastActivity(string) (time.Time, error) { return time.Time{}, nil }

func TestAdoptionBarrier_NoRunning(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{running: nil}
	cfg := &config.City{}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("barrier should pass with no running sessions")
	}
	if result.Total != 0 {
		t.Errorf("Total = %d, want 0", result.Total)
	}
}

func TestAdoptionBarrier_PartialListUsesVisibleSessionsButFailsBarrier(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{
		running: []string{"test-city-worker"},
		listErr: &runtime.PartialListError{Err: runtime.ErrSessionNotFound},
	}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if passed {
		t.Fatal("barrier should fail closed on partial session listing")
	}
	if result.Adopted != 1 {
		t.Fatalf("Adopted = %d, want 1 visible session adopted", result.Adopted)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("partially failed")) {
		t.Fatalf("stderr = %q, want partial failure warning", stderr.String())
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

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clk, &stderr, false)
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

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
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

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
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
	r1, _ := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if r1.Adopted != 1 {
		t.Fatalf("first run Adopted = %d, want 1", r1.Adopted)
	}

	// Second run: dedup prevents duplicates.
	r2, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
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

func TestAdoptionBarrier_IgnoresNonRepairableSessionBeadsInConfigSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	sessionName := agent.SessionNameFor("test-city", "worker", cfg.Workspace.SessionTemplate)
	if _, err := store.Create(beads.Bead{
		Title:  "stale malformed worker",
		Type:   "task",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"agent_name":   "worker",
			"session_name": "stale-worker",
			"template":     "worker",
			"state":        "active",
		},
	}); err != nil {
		t.Fatal(err)
	}
	sp := &fakeAdoptionProvider{running: []string{sessionName}}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Fatalf("barrier should pass, result=%+v stderr=%q", result, stderr.String())
	}
	beadList, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing session beads: %v", err)
	}
	for _, b := range beadList {
		if b.Type != sessionBeadType {
			continue
		}
		if got := b.Metadata["agent_name"]; got != "worker" {
			t.Fatalf("adopted bead agent_name = %q, want configured agent name", got)
		}
		if got := b.Metadata["session_name"]; got != sessionName {
			t.Fatalf("adopted bead session_name = %q, want %q", got, sessionName)
		}
		return
	}
	t.Fatalf("adopted session bead not found; beads=%+v", beadList)
}

func TestAdoptionBarrier_SerializesCreateWithSessionIdentifierLock(t *testing.T) {
	const agentName = "worker-3"
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(1), MaxActiveSessions: intPtr(5)}}}
	sessionName := agent.SessionNameFor("test-city", "worker", cfg.Workspace.SessionTemplate) + "-3"
	cityPath := t.TempDir()
	baseStore := beads.NewMemStore()
	allowCreate := make(chan struct{})
	var releaseCreate sync.Once
	t.Cleanup(func() {
		releaseCreate.Do(func() {
			close(allowCreate)
		})
	})
	store := &adoptionLockProbeStore{
		Store:             baseStore,
		targetSessionName: sessionName,
		listed:            make(chan struct{}, 1),
		createAttempted:   make(chan struct{}, 1),
		allowCreate:       allowCreate,
	}
	sp := &fakeAdoptionProvider{running: []string{sessionName}}
	var stderr bytes.Buffer
	done := make(chan adoptionBarrierOutcome, 1)

	err := session.WithCitySessionAliasLock(cityPath, agentName, func() error {
		go func() {
			result, passed := runAdoptionBarrier(cityPath, store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
			done <- adoptionBarrierOutcome{result: result, passed: passed}
		}()

		select {
		case <-store.listed:
		case <-time.After(time.Second):
			t.Fatal("adoption barrier did not list existing session beads")
		}

		_, createErr := baseStore.Create(beads.Bead{
			Title:  agentName,
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "agent:" + agentName},
			Metadata: map[string]string{
				"agent_name":   agentName,
				"session_name": sessionName,
				"state":        "active",
			},
		})
		return createErr
	})
	if err != nil {
		t.Fatalf("holding session identifier lock: %v", err)
	}
	releaseCreate.Do(func() {
		close(allowCreate)
	})

	var outcome adoptionBarrierOutcome
	select {
	case outcome = <-done:
	case <-time.After(time.Second):
		t.Fatal("adoption barrier did not finish after session_name lock released")
	}
	if !outcome.passed {
		t.Fatalf("barrier should pass, stderr: %s", stderr.String())
	}
	if outcome.result.Adopted != 0 {
		t.Fatalf("Adopted = %d, want 0 after locked peer created the bead", outcome.result.Adopted)
	}
	if outcome.result.AlreadyHadBead != 1 {
		t.Fatalf("AlreadyHadBead = %d, want 1", outcome.result.AlreadyHadBead)
	}
	select {
	case <-store.createAttempted:
		t.Fatalf("adoption barrier attempted a duplicate create; outcome=%+v stderr=%q", outcome, stderr.String())
	default:
	}

	beadList, err := baseStore.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing session beads: %v", err)
	}
	if len(beadList) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(beadList))
	}
	if got := beadList[0].Metadata["session_name"]; got != sessionName {
		t.Fatalf("session_name = %q, want %q", got, sessionName)
	}
}

func TestAdoptionBarrier_ReportsInLockListFailuresAsChecks(t *testing.T) {
	store := &adoptionListFailureStore{Store: beads.NewMemStore()}
	sp := &fakeAdoptionProvider{running: []string{"test-city-worker"}}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if passed {
		t.Fatal("barrier should fail when the in-lock bead check fails")
	}
	if result.Skipped != 1 {
		t.Fatalf("Skipped = %d, want 1", result.Skipped)
	}
	log := stderr.String()
	if !strings.Contains(log, `listing session beads for "test-city-worker"`) {
		t.Fatalf("stderr %q does not mention the failing session-bead check", log)
	}
	if strings.Contains(log, "creating bead for") {
		t.Fatalf("stderr %q should not report a list failure as a create failure", log)
	}
}

func TestAdoptionBarrier_StampsSyncedAtAtCreateTime(t *testing.T) {
	fakeClock := &clock.Fake{Time: time.Date(2026, 5, 15, 8, 0, 0, 0, time.UTC)}
	store := &adoptionClockAdvanceStore{
		Store: beads.NewMemStore(),
		advance: func() {
			fakeClock.Advance(time.Hour)
		},
	}
	sp := &fakeAdoptionProvider{running: []string{"test-city-worker"}}
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MaxActiveSessions: intPtr(1)}}}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", fakeClock, &stderr, false)
	if !passed {
		t.Fatalf("barrier should pass, result=%+v stderr=%q", result, stderr.String())
	}
	beadList, err := store.ListByLabel(sessionBeadLabel, 0)
	if err != nil {
		t.Fatalf("listing session beads: %v", err)
	}
	if len(beadList) != 1 {
		t.Fatalf("session bead count = %d, want 1", len(beadList))
	}
	if got, want := beadList[0].Metadata["synced_at"], "2026-05-15T09:00:00Z"; got != want {
		t.Fatalf("synced_at = %q, want %q", got, want)
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

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, true)
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

func TestAdoptionBarrier_SkipsDeadSessions(t *testing.T) {
	store := beads.NewMemStore()
	sp := &fakeAdoptionProvider{
		running: []string{"test-city-mayor", "test-city-worker"},
		alive: map[string]bool{
			"test-city-mayor":  true,
			"test-city-worker": false,
		},
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "worker"},
		},
	}
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Fatalf("barrier should pass, stderr: %s", stderr.String())
	}
	if result.Total != 1 {
		t.Fatalf("Total = %d, want 1 live session", result.Total)
	}
	if result.Adopted != 1 {
		t.Fatalf("Adopted = %d, want 1", result.Adopted)
	}
	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	if len(beadList) != 1 {
		t.Fatalf("beads count = %d, want 1", len(beadList))
	}
	if beadList[0].Metadata["session_name"] != "test-city-mayor" {
		t.Fatalf("adopted bead = %q, want live mayor", beadList[0].Metadata["session_name"])
	}
}

func TestAdoptionBarrier_NilStore(t *testing.T) {
	sp := &fakeAdoptionProvider{running: []string{"test-city-mayor"}}
	cfg := &config.City{}
	var stderr bytes.Buffer

	_, passed := runAdoptionBarrier("", nil, sp, cfg, "test-city", clock.Real{}, &stderr, false)
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

	result, _ := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, true)
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

	result, _ := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, true)
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

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
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

// TestAdoptionBarrier_OrphanDashNSessionLogsWarning verifies the ga-fiw
// defensive log: when a running session ends in "-N" but no configured pool
// agent claims it (because the matching base agent has max_active_sessions=1
// and SupportsInstanceExpansion()=false), the barrier still adopts the
// session but emits a stderr warning so the leak is traceable.
func TestAdoptionBarrier_OrphanDashNSessionLogsWarning(t *testing.T) {
	store := beads.NewMemStore()
	// "refinery-1" looks like a pool instance but the base "refinery" agent
	// has max_active_sessions=1, so resolvePoolBase rejects the suffix.
	sp := &fakeAdoptionProvider{running: []string{"refinery-1"}}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "refinery", MaxActiveSessions: intPtr(1)},
		},
	}
	var stderr bytes.Buffer

	result, _ := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if result.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", result.Adopted)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("refinery-1 ends in -1")) {
		t.Errorf("stderr missing orphan -N warning; got: %s", stderr.String())
	}
	// Verify no pool_slot metadata (we explicitly decline to stamp it).
	beadList, _ := store.ListByLabel(sessionBeadLabel, 0)
	for _, b := range beadList {
		if b.Metadata["pool_slot"] != "" {
			t.Errorf("orphan -N session should not have pool_slot metadata, got %q", b.Metadata["pool_slot"])
		}
	}
}

func TestAdoptionBarrier_UnknownSession(t *testing.T) {
	store := beads.NewMemStore()
	// Running session that doesn't match any config agent.
	sp := &fakeAdoptionProvider{running: []string{"unknown-session"}}
	cfg := &config.City{} // no agents configured
	var stderr bytes.Buffer

	result, passed := runAdoptionBarrier("", store, sp, cfg, "test-city", clock.Real{}, &stderr, false)
	if !passed {
		t.Error("barrier should pass (adopt permissively)")
	}
	if result.Adopted != 1 {
		t.Errorf("Adopted = %d, want 1", result.Adopted)
	}
}
