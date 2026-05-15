package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionauto "github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/sessionlog"
)

type startOverrideProvider struct {
	*runtime.Fake
	startErr error
}

type noImmediateProvider struct {
	runtime.Provider
}

type nonRunningStopRecorder struct {
	*runtime.Fake
	stopCalls int
	stopErr   error
}

func (p *nonRunningStopRecorder) IsRunning(string) bool {
	return false
}

func (p *nonRunningStopRecorder) Stop(name string) error {
	p.stopCalls++
	if p.stopErr != nil {
		return p.stopErr
	}
	return p.Fake.Stop(name)
}

func (p *startOverrideProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.startErr != nil {
		return p.startErr
	}
	return p.Fake.Start(ctx, name, cfg)
}

// failOnceStartProvider simulates a stale session key: the first Start
// after arming succeeds but the process immediately dies (IsRunning returns
// false). The second Start (fresh retry) succeeds and stays running.
type failOnceStartProvider struct {
	*runtime.Fake
	armed   bool
	dieOnce bool // set after armed Start to make IsRunning return false once
}

func (p *failOnceStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.armed {
		p.armed = false
		p.dieOnce = true
		// Start "succeeds" but process will appear dead on next IsRunning check.
		return p.Fake.Start(ctx, name, cfg)
	}
	p.dieOnce = false
	return p.Fake.Start(ctx, name, cfg)
}

func (p *failOnceStartProvider) IsRunning(name string) bool {
	if p.dieOnce {
		p.dieOnce = false
		// Simulate: process started but died immediately (stale key).
		_ = p.Stop(name) // actually kill it so state is consistent
		return false
	}
	return p.Fake.IsRunning(name)
}

// dieAndFailProvider: first Start succeeds but process dies immediately,
// second Start fails outright. Simulates stale key + provider unavailable.
type dieAndFailProvider struct {
	*runtime.Fake
	callCount int
}

func (p *dieAndFailProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	p.callCount++
	if p.callCount == 1 {
		// First call: start succeeds but process will appear dead.
		return p.Fake.Start(ctx, name, cfg)
	}
	// Second call (fresh retry): fail outright.
	return errors.New("provider unavailable")
}

func (p *dieAndFailProvider) IsRunning(name string) bool {
	if p.callCount == 1 {
		// After first Start: process died (stale key).
		_ = p.Stop(name)
		return false
	}
	return p.Fake.IsRunning(name) //nolint:staticcheck // intentional: IsRunning is not on Fake, it's on Provider
}

type startupDeathProvider struct {
	*runtime.Fake
	armed     bool
	failRetry bool
}

func (p *startupDeathProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.armed {
		p.armed = false
		return fmt.Errorf("%w: session %q", runtime.ErrSessionDiedDuringStartup, name)
	}
	if p.failRetry {
		return errors.New("provider unavailable")
	}
	return p.Fake.Start(ctx, name, cfg)
}

type lateSuccessStartProvider struct {
	*runtime.Fake
	startErr error
}

func (p *lateSuccessStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	if id := cfg.Env["GC_SESSION_ID"]; id != "" {
		_ = p.SetMeta(name, "GC_SESSION_ID", id)
	}
	if token := cfg.Env["GC_INSTANCE_TOKEN"]; token != "" {
		_ = p.SetMeta(name, "GC_INSTANCE_TOKEN", token)
	}
	if p.startErr != nil {
		return p.startErr
	}
	return nil
}

func createTestWait(t *testing.T, store beads.Store, sessionID string) beads.Bead {
	t.Helper()
	wait, err := store.Create(beads.Bead{
		Type:   WaitBeadType,
		Labels: []string{WaitBeadLabel, "session:" + sessionID},
		Metadata: map[string]string{
			"session_id": sessionID,
			"state":      "pending",
		},
	})
	if err != nil {
		t.Fatalf("create wait: %v", err)
	}
	return wait
}

type waitFailStore struct {
	*beads.MemStore
}

type failMetadataKeyStore struct {
	*beads.MemStore
	key string
}

func (s failMetadataKeyStore) SetMetadata(id, key, value string) error {
	if key == s.key {
		return errors.New("set metadata failed")
	}
	return s.MemStore.SetMetadata(id, key, value)
}

func (s waitFailStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Label == WaitBeadLabel || strings.HasPrefix(query.Label, "session:") {
		return nil, errors.New("wait list failed")
	}
	return s.MemStore.List(query)
}

func (s waitFailStore) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if label == WaitBeadLabel || strings.HasPrefix(label, "session:") {
		return nil, errors.New("wait list failed")
	}
	return s.MemStore.ListByLabel(label, limit, opts...)
}

func TestCreate(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "my chat", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if info.Template != "helper" {
		t.Errorf("Template = %q, want %q", info.Template, "helper")
	}
	if info.Title != "my chat" {
		t.Errorf("Title = %q, want %q", info.Title, "my chat")
	}
	if info.State != StateActive {
		t.Errorf("State = %q, want %q", info.State, StateActive)
	}
	if info.ID == "" {
		t.Error("ID is empty")
	}

	// Verify the tmux session was started.
	if !sp.IsRunning(info.SessionName) {
		t.Error("runtime session not started")
	}

	// Verify bead was created with correct type and labels.
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Type != BeadType {
		t.Errorf("bead Type = %q, want %q", b.Type, BeadType)
	}
	if b.Status != "open" {
		t.Errorf("bead Status = %q, want %q", b.Status, "open")
	}
	hasLabel := false
	for _, l := range b.Labels {
		if l == LabelSession {
			hasLabel = true
		}
	}
	if !hasLabel {
		t.Errorf("bead missing label %q", LabelSession)
	}
	if b.Metadata["generation"] != "1" {
		t.Errorf("generation = %q, want 1", b.Metadata["generation"])
	}
	if b.Metadata["continuation_epoch"] != "1" {
		t.Errorf("continuation_epoch = %q, want 1", b.Metadata["continuation_epoch"])
	}
	if b.Metadata["instance_token"] == "" {
		t.Error("instance_token is empty")
	}
	startCall := sp.Calls[0]
	if startCall.Method != "Start" {
		t.Fatalf("first runtime call = %q, want Start", startCall.Method)
	}
	if got := startCall.Config.Env["GC_SESSION_ID"]; got != info.ID {
		t.Errorf("GC_SESSION_ID = %q, want %q", got, info.ID)
	}
	if got := startCall.Config.Env["GC_CONTINUATION_EPOCH"]; got != "1" {
		t.Errorf("GC_CONTINUATION_EPOCH = %q, want 1", got)
	}
	if got := startCall.Config.Env["GC_RUNTIME_EPOCH"]; got != "1" {
		t.Errorf("GC_RUNTIME_EPOCH = %q, want 1", got)
	}
	if got := startCall.Config.Env["GC_INSTANCE_TOKEN"]; got == "" {
		t.Error("GC_INSTANCE_TOKEN is empty")
	}
	if got := startCall.Config.Env["GC_DIR"]; got != "/tmp" {
		t.Errorf("GC_DIR = %q, want %q", got, "/tmp")
	}
}

func TestCreateConfirmsStartedStateWithoutControllerDriftHash(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(
		context.Background(),
		"helper",
		"my chat",
		"claude",
		"/tmp",
		"claude",
		map[string]string{"BEADS_DIR": "/tmp/beads"},
		ProviderResume{},
		runtime.Config{
			Env:              map[string]string{"GC_CITY": "test-city"},
			FingerprintExtra: map[string]string{"depends_on": "db"},
			SessionLive:      []string{"echo live"},
		},
	)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := b.Metadata["started_config_hash"]; got != "" {
		t.Fatalf("started_config_hash = %q, want empty so controller owns drift hashes", got)
	}
	if got := b.Metadata["started_live_hash"]; got != "" {
		t.Fatalf("started_live_hash = %q, want empty so controller owns drift hashes", got)
	}
	if got := b.Metadata["live_hash"]; got != "" {
		t.Fatalf("live_hash = %q, want empty so controller owns drift hashes", got)
	}
	if got := b.Metadata["state_reason"]; got != "creation_complete" {
		t.Fatalf("state_reason = %q, want creation_complete", got)
	}
	if got := b.Metadata["creation_complete_at"]; got == "" {
		t.Fatal("creation_complete_at is empty")
	}
}

func TestCreateDefaultsTitleToTemplate(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Title != "helper" {
		t.Errorf("Title = %q, want %q", b.Title, "helper")
	}
}

func TestCreateBeadOnlyDefaultsTitleToTemplate(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateBeadOnly("helper", "", "claude", "/tmp", "claude", "", nil, ProviderResume{})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Title != "helper" {
		t.Errorf("Title = %q, want %q", b.Title, "helper")
	}
}

func TestCreateBeadOnly(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateBeadOnly("helper", "my chat", "claude", "/tmp", "claude", "", nil, ProviderResume{})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	if info.Template != "helper" {
		t.Errorf("Template = %q, want %q", info.Template, "helper")
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["generation"] != "1" {
		t.Errorf("generation = %q, want 1", b.Metadata["generation"])
	}
	if b.Metadata["continuation_epoch"] != "1" {
		t.Errorf("continuation_epoch = %q, want 1", b.Metadata["continuation_epoch"])
	}
	if b.Metadata["instance_token"] == "" {
		t.Error("instance_token is empty")
	}
	if info.ID == "" {
		t.Error("ID is empty")
	}

	// Verify the runtime session was NOT started.
	if sp.IsRunning(info.SessionName) {
		t.Error("runtime session should not be started in bead-only mode")
	}

	// Verify bead was created with state "creating" (not "active").
	if b.Metadata["state"] != "creating" {
		t.Errorf("bead state = %q, want %q", b.Metadata["state"], "creating")
	}
	if b.Metadata["session_name"] == "" {
		t.Error("bead missing session_name metadata")
	}
}

func TestGetSurfacesAgentNameMetadata(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateBeadOnly("helper", "my chat", "claude", "/tmp", "claude", "", nil, ProviderResume{})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	if err := store.SetMetadata(info.ID, "agent_name", "myrig/helper-adhoc-123"); err != nil {
		t.Fatalf("SetMetadata(agent_name): %v", err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AgentName != "myrig/helper-adhoc-123" {
		t.Fatalf("AgentName = %q, want %q", got.AgentName, "myrig/helper-adhoc-123")
	}
}

// TestGetSurfacesLastNudgeDeliveredAtMetadata verifies that
// `metadata.last_nudge_delivered_at` (stamped by the nudge dispatcher on
// successful delivery) round-trips through Info.LastNudgeDeliveredAt so
// `gc session list` can render the "LAST NUDGE" column. The stamp lives
// on the session bead so operators can spot warm sessions whose delivery
// loop has stalled (queued items piling up while the timestamp stays old).
func TestGetSurfacesLastNudgeDeliveredAtMetadata(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateBeadOnly("helper", "my chat", "claude", "/tmp", "claude", "", nil, ProviderResume{})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}

	stamp := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	if err := store.SetMetadata(info.ID, MetadataLastNudgeDeliveredAt, stamp.Format(time.RFC3339)); err != nil {
		t.Fatalf("SetMetadata(%s): %v", MetadataLastNudgeDeliveredAt, err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.LastNudgeDeliveredAt.Equal(stamp) {
		t.Fatalf("LastNudgeDeliveredAt = %v, want %v", got.LastNudgeDeliveredAt, stamp)
	}
}

// TestGetIgnoresInvalidLastNudgeDeliveredAtMetadata ensures a malformed
// metadata value leaves Info.LastNudgeDeliveredAt zero rather than
// surfacing a parser error. The stamp is best-effort observability —
// any future schema drift must degrade gracefully instead of breaking
// the read path that powers `gc session list`.
func TestGetIgnoresInvalidLastNudgeDeliveredAtMetadata(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateBeadOnly("helper", "my chat", "claude", "/tmp", "claude", "", nil, ProviderResume{})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	if err := store.SetMetadata(info.ID, MetadataLastNudgeDeliveredAt, "not-a-timestamp"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.LastNudgeDeliveredAt.IsZero() {
		t.Fatalf("LastNudgeDeliveredAt = %v, want zero (unparsable RFC3339 must not surface)", got.LastNudgeDeliveredAt)
	}
}

func TestCreateNamedWithTransport_UsesExplicitSessionName(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "my chat", "claude", "/tmp", "claude", "", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("CreateNamedWithTransport: %v", err)
	}
	if info.SessionName != "sky" {
		t.Fatalf("SessionName = %q, want sky", info.SessionName)
	}
	if !sp.IsRunning("sky") {
		t.Fatal("expected runtime session named sky to be running")
	}
}

func TestCreateNamedWithTransport_RejectsReusedName(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	if _, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "first", "claude", "/tmp", "claude", "", nil, ProviderResume{}, runtime.Config{}); err != nil {
		t.Fatalf("first CreateNamedWithTransport: %v", err)
	}
	if _, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "second", "claude", "/tmp", "claude", "", nil, ProviderResume{}, runtime.Config{}); err == nil {
		t.Fatal("expected session name conflict")
	} else if !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("expected ErrSessionNameExists, got %v", err)
	}
}

func TestCreateNamedWithTransport_ClosedSessionStillReservesName(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "first", "claude", "/tmp", "claude", "", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("first CreateNamedWithTransport: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "second", "claude", "/tmp", "claude", "", nil, ProviderResume{}, runtime.Config{}); err == nil {
		t.Fatal("expected closed session to keep reserving its explicit name")
	} else if !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("expected ErrSessionNameExists, got %v", err)
	}
}

func TestCreateNamedWithTransport_FailedStartDoesNotBurnExplicitName(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sp.StartErrors["sky"] = errors.New("boom")
	mgr := NewManager(store, sp)

	if _, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "first", "claude", "/tmp", "claude", "", nil, ProviderResume{}, runtime.Config{}); err == nil {
		t.Fatal("expected start failure")
	}
	if err := ensureSessionNameAvailable(store, "sky"); err != nil {
		t.Fatalf("ensureSessionNameAvailable after failed start = %v, want nil", err)
	}

	delete(sp.StartErrors, "sky")
	info, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "second", "claude", "/tmp", "claude", "", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("retry CreateNamedWithTransport: %v", err)
	}
	if info.SessionName != "sky" {
		t.Fatalf("SessionName = %q, want sky", info.SessionName)
	}
}

func TestCreateNamedWithTransport_ConvergesLateSuccessStartError(t *testing.T) {
	store := beads.NewMemStore()
	sp := &lateSuccessStartProvider{
		Fake:     runtime.NewFake(),
		startErr: context.DeadlineExceeded,
	}
	mgr := NewManager(store, sp)

	info, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "first", "claude", "/tmp", "claude", "", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("CreateNamedWithTransport: %v", err)
	}
	if info.SessionName != "sky" {
		t.Fatalf("SessionName = %q, want sky", info.SessionName)
	}
	if !sp.IsRunning("sky") {
		t.Fatal("runtime session should remain running after late-success convergence")
	}
	got, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Status == "closed" {
		t.Fatal("session bead should remain open after late-success convergence")
	}
}

func TestCreateNamedWithTransport_ClearsACPRouteAfterDuplicateRuntimeFailure(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	autoSP := sessionauto.New(defaultSP, acpSP)
	mgr := NewManager(store, autoSP)

	if err := acpSP.Start(context.Background(), "sky", runtime.Config{}); err != nil {
		t.Fatalf("seed acp start: %v", err)
	}
	if _, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "first", "claude", "/tmp", "claude", "acp", nil, ProviderResume{}, runtime.Config{}); err == nil {
		t.Fatal("expected duplicate runtime failure")
	} else if !errors.Is(err, ErrSessionNameExists) {
		t.Fatalf("expected ErrSessionNameExists, got %v", err)
	}
	if err := acpSP.Stop("sky"); err != nil {
		t.Fatalf("seed acp stop: %v", err)
	}

	info, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "second", "claude", "/tmp", "claude", "", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("retry CreateNamedWithTransport: %v", err)
	}
	if !defaultSP.IsRunning(info.SessionName) {
		t.Fatalf("default backend should own %q after ACP duplicate cleanup", info.SessionName)
	}
	if acpSP.IsRunning(info.SessionName) {
		t.Fatalf("ACP backend should not own %q after cleanup", info.SessionName)
	}
}

func TestCreateBeadOnlyNamed_UsesExplicitSessionName(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateBeadOnlyNamed("sky", "helper", "queued", "claude", "/tmp", "claude", "", nil, ProviderResume{})
	if err != nil {
		t.Fatalf("CreateBeadOnlyNamed: %v", err)
	}
	if info.SessionName != "sky" {
		t.Fatalf("SessionName = %q, want sky", info.SessionName)
	}
	if sp.IsRunning("sky") {
		t.Fatal("runtime session should not be started in bead-only mode")
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want true", b.Metadata["pending_create_claim"])
	}
}

func TestCreateAliasedBeadOnlyNamed_SetsPendingCreateMetadata(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateAliasedBeadOnlyNamed("worker", "test-city--worker", "worker", "queued", "claude", "/tmp", "claude", "", nil, ProviderResume{})
	if err != nil {
		t.Fatalf("CreateAliasedBeadOnlyNamed: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := b.Metadata["pending_create_claim"]; got != "true" {
		t.Fatalf("pending_create_claim = %q, want true", got)
	}
	startedAt := b.Metadata["pending_create_started_at"]
	if startedAt == "" {
		t.Fatal("pending_create_started_at is empty")
	}
	if _, err := time.Parse(time.RFC3339, startedAt); err != nil {
		t.Fatalf("pending_create_started_at = %q, want RFC3339: %v", startedAt, err)
	}
}

func TestCreateBeadOnly_SetsPendingCreateClaimForWakeSignal(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateBeadOnly("helper", "queued", "claude", "/tmp", "claude", "", nil, ProviderResume{})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want %q", b.Metadata["pending_create_claim"], "true")
	}
}

func TestCreateRoutesACPSessionsThroughAutoProvider(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	mgr := NewManager(store, sessionauto.New(defaultSP, acpSP))

	info, err := mgr.CreateWithTransport(context.Background(), "helper", "acp chat", "claude", "/tmp", "claude", "acp", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if defaultSP.IsRunning(info.SessionName) {
		t.Fatalf("default backend should not own ACP session %q", info.SessionName)
	}
	if !acpSP.IsRunning(info.SessionName) {
		t.Fatalf("ACP backend should own session %q", info.SessionName)
	}
}

func TestSuspendAndResume(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Suspend.
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// Verify runtime session stopped.
	if sp.IsRunning(info.SessionName) {
		t.Error("runtime session should be stopped after suspend")
	}

	// Verify bead state updated.
	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateSuspended {
		t.Errorf("State = %q, want %q", got.State, StateSuspended)
	}

	// Suspend again is idempotent.
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend (idempotent): %v", err)
	}

	// Resume via Attach.
	err = mgr.Attach(context.Background(), info.ID, "claude --resume", runtime.Config{})
	if err != nil {
		t.Fatalf("Attach (resume): %v", err)
	}

	// Verify runtime session restarted.
	if !sp.IsRunning(info.SessionName) {
		t.Error("runtime session should be running after resume")
	}

	// Verify state back to active.
	got, err = mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateActive {
		t.Errorf("State = %q, want %q", got.State, StateActive)
	}
}

func TestClose(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wait := createTestWait(t, store, info.ID)

	// Close active session.
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify runtime stopped.
	if sp.IsRunning(info.SessionName) {
		t.Error("runtime session should be stopped after close")
	}

	// Verify bead closed.
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Status != "closed" {
		t.Errorf("bead Status = %q, want %q", b.Status, "closed")
	}
	wait, err = store.Get(wait.ID)
	if err != nil {
		t.Fatalf("store.Get(wait): %v", err)
	}
	if wait.Metadata["state"] != waitStateCanceled {
		t.Fatalf("wait state = %q, want %q", wait.Metadata["state"], waitStateCanceled)
	}

	// Close again is idempotent.
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close (idempotent): %v", err)
	}
}

func TestCloseRemovesRuntimeMCPSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	cityPath := t.TempDir()
	mgr := NewManagerWithCityPath(store, sp, cityPath)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := PersistRuntimeMCPServersSnapshot(cityPath, info.ID, []runtime.MCPServerConfig{{
		Name:      "identity",
		Transport: runtime.MCPTransportHTTP,
		URL:       "https://example.invalid/mcp",
	}}); err != nil {
		t.Fatalf("PersistRuntimeMCPServersSnapshot: %v", err)
	}
	if _, err := os.Stat(runtimeMCPServersSnapshotPath(cityPath, info.ID)); err != nil {
		t.Fatalf("Stat(runtime snapshot): %v", err)
	}

	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(runtimeMCPServersSnapshotPath(cityPath, info.ID)); !os.IsNotExist(err) {
		t.Fatalf("runtime snapshot still exists after close, stat err = %v", err)
	}
}

func TestClose_ConfiguredNamedSessionRetiresIdentifiers(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateAliasedNamedWithTransportAndMetadata(
		context.Background(),
		"mayor",
		"test-city--mayor",
		"mayor",
		"Mayor",
		"claude",
		"/tmp",
		"claude",
		"",
		nil,
		ProviderResume{},
		runtime.Config{},
		map[string]string{
			"configured_named_session":  "true",
			"configured_named_identity": "mayor",
		},
	)
	if err != nil {
		t.Fatalf("CreateAliasedNamedWithTransportAndMetadata: %v", err)
	}

	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := b.Status; got != "closed" {
		t.Fatalf("Status = %q, want closed", got)
	}
	if got := b.Metadata["alias"]; got != "" {
		t.Fatalf("alias = %q, want empty", got)
	}
	if got := b.Metadata["session_name"]; got != "" {
		t.Fatalf("session_name = %q, want empty", got)
	}
	if got := b.Metadata["session_name_explicit"]; got != "" {
		t.Fatalf("session_name_explicit = %q, want empty", got)
	}
	if got := b.Metadata["alias_history"]; got != "mayor" {
		t.Fatalf("alias_history = %q, want mayor", got)
	}
}

func TestCreateInjectsUnifiedSessionRuntimeEnv(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateAliasedNamedWithTransportAndMetadata(
		context.Background(),
		"mayor",
		"test-city--mayor",
		"reviewer",
		"Mayor",
		"claude",
		"/tmp",
		"claude",
		"",
		map[string]string{"GC_AGENT": "stale"},
		ProviderResume{},
		runtime.Config{},
		map[string]string{
			"configured_named_session":  "true",
			"configured_named_identity": "mayor",
			"session_origin":            "named",
		},
	)
	if err != nil {
		t.Fatalf("CreateAliasedNamedWithTransportAndMetadata: %v", err)
	}

	var start *runtime.Call
	for i := range sp.Calls {
		if sp.Calls[i].Method == "Start" {
			start = &sp.Calls[i]
			break
		}
	}
	if start == nil {
		t.Fatalf("Start call not recorded: %#v", sp.Calls)
	}
	env := start.Config.Env
	for key, want := range map[string]string{
		"GC_SESSION_ID":     info.ID,
		"GC_SESSION_NAME":   "test-city--mayor",
		"GC_ALIAS":          "mayor",
		"GC_TEMPLATE":       "reviewer",
		"GC_SESSION_ORIGIN": "named",
		"GC_AGENT":          "mayor",
	} {
		if got := env[key]; got != want {
			t.Fatalf("Env[%s] = %q, want %q (env=%v)", key, got, want, env)
		}
	}
}

func TestCreateUsesBuiltinAncestorForGCProviderEnv(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateAliasedNamedWithTransportAndMetadata(
		context.Background(),
		"mayor",
		"test-city--mayor",
		"reviewer",
		"Mayor",
		"claude",
		"/tmp",
		"claude-max",
		"",
		nil,
		ProviderResume{},
		runtime.Config{},
		map[string]string{
			"builtin_ancestor": "claude",
			"provider_kind":    "claude-max",
			"session_origin":   "named",
		},
	)
	if err != nil {
		t.Fatalf("CreateAliasedNamedWithTransportAndMetadata: %v", err)
	}

	cfg := sp.LastStartConfig("test-city--mayor")
	if cfg == nil {
		t.Fatalf("Start call not recorded: %#v", sp.Calls)
	}
	if got := cfg.Env["GC_PROVIDER"]; got != "claude" {
		t.Fatalf("GC_PROVIDER = %q, want claude for %s", got, info.ID)
	}
}

func TestAttachUsesBuiltinAncestorForGCProviderEnv(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)
	b, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name":     "test-city--worker",
			"state":            string(StateSuspended),
			"template":         "worker",
			"work_dir":         "/tmp",
			"provider":         "claude-max",
			"provider_kind":    "claude-max",
			"builtin_ancestor": "claude",
		},
	})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}

	if err := mgr.Attach(context.Background(), b.ID, "claude --resume abc", runtime.Config{}); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	cfg := sp.LastStartConfig("test-city--worker")
	if cfg == nil {
		t.Fatalf("Start call not recorded: %#v", sp.Calls)
	}
	if got := cfg.Env["GC_PROVIDER"]; got != "claude" {
		t.Fatalf("GC_PROVIDER = %q, want claude", got)
	}
}

func TestCreateAliaslessMultiSessionUsesConcreteRuntimeIdentity(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateAliasedNamedWithTransportAndMetadata(
		context.Background(),
		"",
		"ant-adhoc-123",
		"demo/ant",
		"Ant",
		"claude",
		"/tmp",
		"claude",
		"",
		nil,
		ProviderResume{},
		runtime.Config{},
		map[string]string{
			"agent_name":     "demo/ant-adhoc-123",
			"session_origin": "manual",
		},
	)
	if err != nil {
		t.Fatalf("CreateAliasedNamedWithTransportAndMetadata: %v", err)
	}

	var start *runtime.Call
	for i := range sp.Calls {
		if sp.Calls[i].Method == "Start" {
			start = &sp.Calls[i]
			break
		}
	}
	if start == nil {
		t.Fatalf("Start call not recorded: %#v", sp.Calls)
	}
	env := start.Config.Env
	for key, want := range map[string]string{
		"GC_SESSION_ID":     info.ID,
		"GC_SESSION_NAME":   "ant-adhoc-123",
		"GC_ALIAS":          "demo/ant-adhoc-123",
		"GC_TEMPLATE":       "demo/ant",
		"GC_SESSION_ORIGIN": "manual",
		"GC_AGENT":          "demo/ant-adhoc-123",
	} {
		if got := env[key]; got != want {
			t.Fatalf("Env[%s] = %q, want %q (env=%v)", key, got, want, env)
		}
	}
}

func TestCloseSuspended(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// Close suspended session.
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Status != "closed" {
		t.Errorf("bead Status = %q, want %q", b.Status, "closed")
	}
}

func TestClose_IgnoresWaitCancellationFailure(t *testing.T) {
	store := waitFailStore{MemStore: beads.NewMemStore()}
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close should succeed despite wait cancellation failure: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Status != "closed" {
		t.Fatalf("bead Status = %q, want closed", b.Status)
	}
}

func TestList(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Create two sessions with different templates.
	_, err := mgr.Create(context.Background(), "helper", "first", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create 1: %v", err)
	}
	info2, err := mgr.Create(context.Background(), "review", "second", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create 2: %v", err)
	}

	// Suspend the second one.
	if err := mgr.Suspend(info2.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// List all (default excludes closed).
	sessions, err := mgr.List("", "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("List returned %d sessions, want 2", len(sessions))
	}
	if sessions[0].ID != info2.ID {
		t.Fatalf("List order first ID = %q, want newest %q", sessions[0].ID, info2.ID)
	}

	// Filter by state.
	active, err := mgr.List("active", "")
	if err != nil {
		t.Fatalf("List active: %v", err)
	}
	if len(active) != 1 {
		t.Errorf("List active returned %d, want 1", len(active))
	}

	suspended, err := mgr.List("suspended", "")
	if err != nil {
		t.Fatalf("List suspended: %v", err)
	}
	if len(suspended) != 1 {
		t.Errorf("List suspended returned %d, want 1", len(suspended))
	}

	// Filter by template.
	helpers, err := mgr.List("", "helper")
	if err != nil {
		t.Fatalf("List template: %v", err)
	}
	if len(helpers) != 1 {
		t.Errorf("List template=helper returned %d, want 1", len(helpers))
	}
}

func TestListNormalizesLegacyDrainedToAsleep(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	bead, err := store.Create(beads.Bead{
		Title:  "legacy drained",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"template":     "helper",
			"state":        "drained",
			"session_name": "legacy-drained",
		},
	})
	if err != nil {
		t.Fatalf("Create legacy drained session: %v", err)
	}

	got, err := mgr.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != State("asleep") {
		t.Fatalf("Get state = %q, want asleep", got.State)
	}

	sessions, err := mgr.List("asleep", "")
	if err != nil {
		t.Fatalf("List asleep: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("List asleep returned %d sessions, want 1", len(sessions))
	}
	if sessions[0].ID != bead.ID {
		t.Fatalf("List asleep returned %q, want %q", sessions[0].ID, bead.ID)
	}
}

func TestGetNormalizesAwakeToActive(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	bead, err := store.Create(beads.Bead{
		Title:  "awake session",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"template":     "helper",
			"state":        "awake",
			"session_name": "live-awake",
		},
	})
	if err != nil {
		t.Fatalf("Create awake session: %v", err)
	}
	if err := sp.Start(context.Background(), "live-awake", runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	got, err := mgr.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateActive {
		t.Fatalf("Get state = %q, want %q", got.State, StateActive)
	}
}

func TestGetDowngradesStaleActiveStateToAsleep(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	bead, err := store.Create(beads.Bead{
		Title:  "stale awake session",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"template":     "helper",
			"state":        "awake",
			"session_name": "stale-awake",
		},
	})
	if err != nil {
		t.Fatalf("Create stale awake session: %v", err)
	}

	got, err := mgr.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateAsleep {
		t.Fatalf("Get state = %q, want %q", got.State, StateAsleep)
	}
}

func TestPeek(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Set canned peek output on the session name.
	sp.SetPeekOutput(info.SessionName, "hello world")

	out, err := mgr.Peek(info.ID, 50)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if out != "hello world" {
		t.Errorf("Peek output = %q, want %q", out, "hello world")
	}
}

func TestPeekSuspended(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	_, err = mgr.Peek(info.ID, 50)
	if err == nil {
		t.Error("Peek on suspended session should error")
	}
}

func TestAttachClosedErrors(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = mgr.Attach(context.Background(), info.ID, "claude --resume", runtime.Config{})
	if err == nil {
		t.Error("Attach to closed session should error")
	}
}

func TestSessionNameFor(t *testing.T) {
	tests := []struct {
		beadID string
		want   string
	}{
		{"gc-1", "s-gc-1"},
		{"gc-42", "s-gc-42"},
	}
	for _, tt := range tests {
		got := sessionNameFor(tt.beadID)
		if got != tt.want {
			t.Errorf("sessionNameFor(%q) = %q, want %q", tt.beadID, got, tt.want)
		}
	}
}

func TestListExcludesClosedFromActiveFilter(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Filtering by "active" should NOT return the closed session.
	active, err := mgr.List("active", "")
	if err != nil {
		t.Fatalf("List active: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("List active returned %d, want 0 (closed session leaked)", len(active))
	}
}

func TestAttachActiveReattach(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Attach to an active session — should reattach without restarting.
	err = mgr.Attach(context.Background(), info.ID, "claude --resume", runtime.Config{})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Verify state is still active.
	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateActive {
		t.Errorf("State = %q, want %q", got.State, StateActive)
	}
}

func TestSuspendCrashedSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate crash by stopping the runtime behind the manager's back.
	_ = sp.Stop(info.SessionName)

	// Suspend should succeed even though runtime is dead.
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend crashed session: %v", err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateSuspended {
		t.Errorf("State = %q, want %q", got.State, StateSuspended)
	}
}

func TestSuspendCleansDeadRuntimeArtifact(t *testing.T) {
	store := beads.NewMemStore()
	sp := &nonRunningStopRecorder{Fake: runtime.NewFake()}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if sp.stopCalls != 1 {
		t.Fatalf("Stop calls = %d, want 1 to clean dead runtime artifact", sp.stopCalls)
	}
}

func TestSuspendKeepsNonRunningCleanupBestEffort(t *testing.T) {
	store := beads.NewMemStore()
	sp := &nonRunningStopRecorder{Fake: runtime.NewFake(), stopErr: errors.New("cleanup unavailable")}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if sp.stopCalls != 1 {
		t.Fatalf("Stop calls = %d, want 1", sp.stopCalls)
	}
	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateSuspended {
		t.Fatalf("State = %q, want %q", got.State, StateSuspended)
	}
}

func TestCreateStoresCommand(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude --dangerously-skip-permissions", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify the command is stored in the bead metadata.
	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["command"] != "claude --dangerously-skip-permissions" {
		t.Errorf("stored command = %q, want %q", b.Metadata["command"], "claude --dangerously-skip-permissions")
	}

	// Verify it's accessible via Info.
	if info.Command != "claude --dangerously-skip-permissions" {
		t.Errorf("Info.Command = %q, want %q", info.Command, "claude --dangerously-skip-permissions")
	}
}

func TestCreateWithSessionID(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	resume := ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}

	info, err := mgr.Create(context.Background(), "helper", "", "claude --dangerously-skip-permissions", "/tmp", "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Session key should be generated.
	if info.SessionKey == "" {
		t.Fatal("SessionKey is empty")
	}
	// Should look like a UUID.
	if len(info.SessionKey) != 36 {
		t.Errorf("SessionKey length = %d, want 36 (UUID)", len(info.SessionKey))
	}

	// Resume metadata should be stored.
	if info.ResumeFlag != "--resume" {
		t.Errorf("ResumeFlag = %q, want %q", info.ResumeFlag, "--resume")
	}
	if info.ResumeStyle != "flag" {
		t.Errorf("ResumeStyle = %q, want %q", info.ResumeStyle, "flag")
	}

	// The start command should include --session-id <uuid>.
	started := sp.LastStartConfig(info.SessionName)
	if started == nil {
		t.Fatal("session was not started")
	}
	if !strings.Contains(started.Command, "--session-id "+info.SessionKey) {
		t.Errorf("start command = %q, should contain --session-id %s", started.Command, info.SessionKey)
	}
}

func TestBuildResumeCommand(t *testing.T) {
	tests := []struct {
		name string
		info Info
		want string
	}{
		{
			name: "provider with resume flag",
			info: Info{
				Command:     "claude --dangerously-skip-permissions",
				Provider:    "claude",
				SessionKey:  "abc-123",
				ResumeFlag:  "--resume",
				ResumeStyle: "flag",
			},
			want: "claude --dangerously-skip-permissions --resume abc-123",
		},
		{
			name: "provider with subcommand style",
			info: Info{
				Command:     "codex",
				Provider:    "codex",
				SessionKey:  "abc-123",
				ResumeFlag:  "resume",
				ResumeStyle: "subcommand",
			},
			want: "codex resume abc-123",
		},
		{
			name: "no resume flag falls back to command",
			info: Info{
				Command:    "claude --dangerously-skip-permissions",
				Provider:   "claude",
				SessionKey: "abc-123",
			},
			want: "claude --dangerously-skip-permissions",
		},
		{
			name: "no session key falls back to command",
			info: Info{
				Command:    "claude --dangerously-skip-permissions",
				Provider:   "claude",
				ResumeFlag: "--resume",
			},
			want: "claude --dangerously-skip-permissions",
		},
		{
			name: "no command falls back to provider",
			info: Info{
				Provider:   "claude",
				SessionKey: "abc-123",
				ResumeFlag: "--resume",
			},
			want: "claude --resume abc-123",
		},
		{
			name: "subcommand with flags in command",
			info: Info{
				Command:     "codex --model o3",
				Provider:    "codex",
				SessionKey:  "abc-123",
				ResumeFlag:  "resume",
				ResumeStyle: "subcommand",
			},
			want: "codex resume abc-123 --model o3",
		},
		{
			name: "explicit resume_command takes precedence",
			info: Info{
				Command:       "claude --dangerously-skip-permissions",
				Provider:      "claude",
				SessionKey:    "abc-123",
				ResumeFlag:    "--resume",
				ResumeCommand: "claude --resume {{.SessionKey}} --dangerously-skip-permissions",
			},
			want: "claude --resume abc-123 --dangerously-skip-permissions",
		},
		{
			name: "resume_command without session key falls back",
			info: Info{
				Command:       "claude --dangerously-skip-permissions",
				Provider:      "claude",
				ResumeFlag:    "--resume",
				ResumeCommand: "claude --resume {{.SessionKey}} --dangerously-skip-permissions",
			},
			want: "claude --dangerously-skip-permissions",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildResumeCommand(tt.info)
			if got != tt.want {
				t.Errorf("BuildResumeCommand() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCreateWithResumeFlagNoSessionIDFlag(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Provider supports resume but NOT Generate & Pass (no SessionIDFlag).
	resume := ProviderResume{
		ResumeFlag:  "resume",
		ResumeStyle: "subcommand",
		// SessionIDFlag deliberately empty.
	}

	info, err := mgr.Create(context.Background(), "helper", "", "codex --model o3", "/tmp", "codex", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// No session key should be generated since SessionIDFlag is empty.
	if info.SessionKey != "" {
		t.Errorf("SessionKey = %q, want empty (no SessionIDFlag)", info.SessionKey)
	}

	// The start command should be the original command (no --session-id injection).
	started := sp.LastStartConfig(info.SessionName)
	if started == nil {
		t.Fatal("session was not started")
	}
	if started.Command != "codex --model o3" {
		t.Errorf("start command = %q, want %q", started.Command, "codex --model o3")
	}

	// BuildResumeCommand should fall back to stored command (no key to resume with).
	resumeCmd := BuildResumeCommand(info)
	if resumeCmd != "codex --model o3" {
		t.Errorf("BuildResumeCommand() = %q, want %q (fallback to stored command)", resumeCmd, "codex --model o3")
	}
}

func TestCreateFailsCleanup(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFailFake() // all operations fail
	mgr := NewManager(store, sp)

	_, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err == nil {
		t.Fatal("Create should fail when provider fails")
	}

	// The bead should be closed (cleaned up).
	all, _ := store.ListOpen()
	for _, b := range all {
		if b.Type == BeadType && b.Status == "open" {
			t.Errorf("orphan session bead %s left open after failed create", b.ID)
		}
	}
}

func TestRename(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "old title", "echo test", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.Rename(info.ID, "new title"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "new title" {
		t.Errorf("Title = %q, want %q", got.Title, "new title")
	}
}

func TestUpdatePresentationSyncsRuntimeAlias(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateAliasedNamedWithTransport(
		context.Background(),
		"old-alias",
		"",
		"helper",
		"old title",
		"echo test",
		"/tmp",
		"test",
		"",
		nil,
		ProviderResume{},
		runtime.Config{},
	)
	if err != nil {
		t.Fatal(err)
	}

	nextAlias := "new-alias"
	if err := mgr.UpdatePresentation(info.ID, nil, &nextAlias); err != nil {
		t.Fatalf("UpdatePresentation(alias): %v", err)
	}

	got, err := sp.GetMeta(info.SessionName, "GC_ALIAS")
	if err != nil {
		t.Fatalf("GetMeta(GC_ALIAS): %v", err)
	}
	if got != nextAlias {
		t.Fatalf("GC_ALIAS = %q, want %q", got, nextAlias)
	}

	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get(bead): %v", err)
	}
	if bead.Metadata["alias"] != nextAlias {
		t.Fatalf("alias metadata = %q, want %q", bead.Metadata["alias"], nextAlias)
	}
	if bead.Metadata["alias_history"] != "old-alias" {
		t.Fatalf("alias_history = %q, want old-alias", bead.Metadata["alias_history"])
	}
}

func TestRenameNonSessionBead(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Create a plain bead (not a session).
	b, err := store.Create(beads.Bead{Title: "not a session", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	err = mgr.Rename(b.ID, "new title")
	if err == nil {
		t.Error("Rename on non-session bead should error")
	}
}

func TestLoadSessionBead_RepairsEmptyType(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Create a bead then corrupt its type to empty (simulates crash/migration).
	b, err := store.Create(beads.Bead{
		Title:  "mayor",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "mayor",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	emptyType := ""
	if err := store.Update(b.ID, beads.UpdateOpts{Type: &emptyType}); err != nil {
		t.Fatal(err)
	}

	// loadSessionBead should repair the type instead of returning ErrNotSession.
	got, _, err := mgr.loadSessionBead(b.ID, false)
	if err != nil {
		t.Fatalf("loadSessionBead should repair empty type, got error: %v", err)
	}
	if got.Type != BeadType {
		t.Errorf("type after repair = %q, want %q", got.Type, BeadType)
	}

	// Verify the store was updated.
	stored, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Type != BeadType {
		t.Errorf("stored type after repair = %q, want %q", stored.Type, BeadType)
	}
}

func TestLoadSessionBead_RepairsEmptyTypeByLabel(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Create a bead with gc:session label but NO session_name metadata,
	// then corrupt its type to empty. The label alone should be enough
	// to trigger repair.
	b, err := store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state": "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	emptyType := ""
	if err := store.Update(b.ID, beads.UpdateOpts{Type: &emptyType}); err != nil {
		t.Fatal(err)
	}

	got, _, err := mgr.loadSessionBead(b.ID, false)
	if err != nil {
		t.Fatalf("loadSessionBead should repair empty type via label, got error: %v", err)
	}
	if got.Type != BeadType {
		t.Errorf("type after repair = %q, want %q", got.Type, BeadType)
	}

	stored, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Type != BeadType {
		t.Errorf("stored type after repair = %q, want %q", stored.Type, BeadType)
	}
}

func TestRenameNotFound(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	if err := mgr.Rename("nonexistent", "title"); err == nil {
		t.Error("Rename should fail for nonexistent session")
	}
}

func TestPrune(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Create and suspend two sessions.
	s1, err := mgr.Create(context.Background(), "default", "S1", "echo s1", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := mgr.Create(context.Background(), "default", "S2", "echo s2", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.Suspend(s1.ID); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Suspend(s2.ID); err != nil {
		t.Fatal(err)
	}
	wait1 := createTestWait(t, store, s1.ID)
	wait2 := createTestWait(t, store, s2.ID)

	// Prune with cutoff in the future — should prune both.
	pruned, err := mgr.Prune(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 2 {
		t.Errorf("pruned = %d, want 2", pruned)
	}

	// Both should be closed.
	sessions, err := mgr.List("all", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sessions {
		if s.ID == s1.ID || s.ID == s2.ID {
			if s.State != "" { // closed beads have empty state
				t.Errorf("session %s state = %q after prune, want empty (closed)", s.ID, s.State)
			}
		}
	}
	wait1, err = store.Get(wait1.ID)
	if err != nil {
		t.Fatalf("store.Get(wait1): %v", err)
	}
	if wait1.Metadata["state"] != waitStateCanceled {
		t.Fatalf("wait1 state = %q, want %q", wait1.Metadata["state"], waitStateCanceled)
	}
	wait2, err = store.Get(wait2.ID)
	if err != nil {
		t.Fatalf("store.Get(wait2): %v", err)
	}
	if wait2.Metadata["state"] != waitStateCanceled {
		t.Fatalf("wait2 state = %q, want %q", wait2.Metadata["state"], waitStateCanceled)
	}
}

func TestPruneDetailedReportsWaitNudges(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "default", "S1", "echo s1", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatal(err)
	}
	wait := createTestWait(t, store, info.ID)
	if err := store.SetMetadata(wait.ID, "nudge_id", "wait-1"); err != nil {
		t.Fatal(err)
	}

	result, err := mgr.PruneDetailed(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 1 {
		t.Fatalf("result.Count = %d, want 1", result.Count)
	}
	if len(result.SessionIDs) != 1 || result.SessionIDs[0] != info.ID {
		t.Fatalf("result.SessionIDs = %#v, want [%q]", result.SessionIDs, info.ID)
	}
	if len(result.WaitNudgeIDs) != 1 || result.WaitNudgeIDs[0] != "wait-1" {
		t.Fatalf("result.WaitNudgeIDs = %#v, want [wait-1]", result.WaitNudgeIDs)
	}
}

func TestPruneUsesSuspendedAt(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	// Create two sessions and suspend them.
	old, err := mgr.Create(context.Background(), "default", "Old", "echo old", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}
	recent, err := mgr.Create(context.Background(), "default", "Recent", "echo recent", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}

	if err := mgr.Suspend(old.ID); err != nil {
		t.Fatal(err)
	}
	if err := mgr.Suspend(recent.ID); err != nil {
		t.Fatal(err)
	}

	// Backdate the "old" session's suspended_at to 10 days ago.
	tenDaysAgo := time.Now().Add(-10 * 24 * time.Hour).UTC().Format(time.RFC3339)
	if err := store.SetMetadata(old.ID, "suspended_at", tenDaysAgo); err != nil {
		t.Fatal(err)
	}

	// Cutoff at 7 days ago should prune only the old one.
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	pruned, err := mgr.Prune(cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 1 {
		t.Errorf("pruned = %d, want 1", pruned)
	}

	// Old should be closed, recent should still be suspended.
	gotOld, err := mgr.Get(old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOld.State != "" {
		t.Errorf("old session state = %q, want empty (closed)", gotOld.State)
	}

	gotRecent, err := mgr.Get(recent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if gotRecent.State != StateSuspended {
		t.Errorf("recent session state = %q, want %q", gotRecent.State, StateSuspended)
	}
}

func TestSuspendSetsSuspendedAt(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}

	before := time.Now().Add(-time.Second)
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatal(err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw := b.Metadata["suspended_at"]
	if raw == "" {
		t.Fatal("suspended_at metadata not set")
	}
	ts, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("suspended_at not valid RFC3339: %v", err)
	}
	if ts.Before(before) {
		t.Errorf("suspended_at = %v, expected after %v", ts, before)
	}
}

func TestPruneSkipsActive(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	s1, err := mgr.Create(context.Background(), "default", "Active", "echo a", "/tmp", "test", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatal(err)
	}

	// Active session should not be pruned.
	pruned, err := mgr.Prune(time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 0 {
		t.Errorf("pruned = %d, want 0 (active session should be skipped)", pruned)
	}

	got, err := mgr.Get(s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateActive {
		t.Errorf("active session state = %q, want %q", got.State, StateActive)
	}
}

func TestSendResumesSuspendedSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if err := mgr.Send(context.Background(), info.ID, "hello", "claude --resume "+info.SessionKey, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateActive {
		t.Errorf("State = %q, want %q", got.State, StateActive)
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after Send resumes it")
	}
	found := false
	for _, call := range sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want Nudge hello", sp.Calls)
	}
}

func TestSendImmediateUsesImmediateNudge(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if err := mgr.SendImmediate(context.Background(), info.ID, "hello", "claude --resume "+info.SessionKey, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("SendImmediate: %v", err)
	}

	found := false
	for _, call := range sp.Calls {
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
		if call.Method == "Nudge" && call.Name == info.SessionName {
			t.Fatalf("calls = %#v, want immediate nudge without fallback", sp.Calls)
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want NudgeNow hello", sp.Calls)
	}
}

func TestSendImmediateFallsBackToDefaultNudge(t *testing.T) {
	store := beads.NewMemStore()
	fake := runtime.NewFake()
	mgr := NewManager(store, &noImmediateProvider{Provider: fake})

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.SendImmediate(context.Background(), info.ID, "hello", "", runtime.Config{}); err != nil {
		t.Fatalf("SendImmediate: %v", err)
	}

	found := false
	for _, call := range fake.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName {
			t.Fatalf("calls = %#v, want fallback Nudge only", fake.Calls)
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want fallback Nudge hello", fake.Calls)
	}
}

func TestSendResumesSuspendedSession_SyncsGCDirFromBeadWorkDir(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp/worktree", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	err = mgr.Send(context.Background(), info.ID, "hello", "claude --resume "+info.SessionKey, runtime.Config{
		Env: map[string]string{"GC_DIR": "/stale/worktree"},
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	starts := 0
	var resumed runtime.Config
	for _, call := range sp.Calls {
		if call.Method == "Start" && call.Name == info.SessionName {
			starts++
			resumed = call.Config
		}
	}
	if starts < 2 {
		t.Fatalf("expected create + resume Start calls, got %d", starts)
	}
	if resumed.WorkDir != "/tmp/worktree" {
		t.Fatalf("WorkDir = %q, want %q", resumed.WorkDir, "/tmp/worktree")
	}
	if got := resumed.Env["GC_DIR"]; got != "/tmp/worktree" {
		t.Fatalf("GC_DIR = %q, want %q", got, "/tmp/worktree")
	}
}

func TestSendResumesSuspendedSession_PersistsBackfilledInstanceToken(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := store.SetMetadata(info.ID, "instance_token", ""); err != nil {
		t.Fatalf("clear instance_token: %v", err)
	}

	if err := mgr.Send(context.Background(), info.ID, "hello", "claude --resume "+info.SessionKey, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Metadata["instance_token"] == "" {
		t.Fatal("instance_token should be persisted after backfill during resume")
	}
}

func TestSendResumesSuspendedACPSessionOnACPBackend(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	mgr := NewManager(store, sessionauto.New(defaultSP, acpSP))

	info, err := mgr.CreateWithTransport(context.Background(), "helper", "", "claude", "/tmp", "claude", "acp", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	if err := mgr.Send(context.Background(), info.ID, "hello", "claude --resume", runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if defaultSP.IsRunning(info.SessionName) {
		t.Fatalf("default backend should not own resumed ACP session %q", info.SessionName)
	}
	if !acpSP.IsRunning(info.SessionName) {
		t.Fatalf("ACP backend should own resumed session %q", info.SessionName)
	}
	found := false
	for _, call := range acpSP.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ACP calls = %#v, want Nudge hello", acpSP.Calls)
	}
}

func TestSendReRoutesActiveACPSessionBeforeNudge(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	autoSP := sessionauto.New(defaultSP, acpSP)
	mgr := NewManager(store, autoSP)

	info, err := mgr.CreateWithTransport(context.Background(), "helper", "", "claude", "/tmp", "claude", "acp", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	autoSP.Unroute(info.SessionName)

	if err := mgr.Send(context.Background(), info.ID, "hello again", "claude --resume", runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if defaultSP.IsRunning(info.SessionName) {
		t.Fatalf("default backend should not own active ACP session %q", info.SessionName)
	}
	found := false
	for _, call := range acpSP.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello again" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ACP calls = %#v, want rerouted Nudge", acpSP.Calls)
	}
}

func TestSendBackfillsTransportForLegacyACPSession(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	autoSP := sessionauto.New(defaultSP, acpSP)

	legacy, err := store.Create(beads.Bead{
		Title: "legacy acp",
		Type:  BeadType,
		Labels: []string{
			LabelSession,
			"template:helper",
		},
		Metadata: map[string]string{
			"template": "helper",
			"state":    string(StateActive),
			"provider": "claude",
			"work_dir": "/tmp",
			"command":  "claude",
		},
	})
	if err != nil {
		t.Fatalf("Create legacy bead: %v", err)
	}
	sessName := sessionNameFor(legacy.ID)
	if err := store.SetMetadata(legacy.ID, "session_name", sessName); err != nil {
		t.Fatalf("SetMetadata(session_name): %v", err)
	}
	if err := acpSP.Start(context.Background(), sessName, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Start ACP session: %v", err)
	}

	mgr := NewManagerWithTransportResolver(store, autoSP, func(template, _ string) string {
		if template == "helper" {
			return "acp"
		}
		return ""
	})
	if err := mgr.Send(context.Background(), legacy.ID, "hello from legacy", "", runtime.Config{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if defaultSP.IsRunning(sessName) {
		t.Fatalf("default backend should not own legacy ACP session %q", sessName)
	}
	found := false
	for _, call := range acpSP.Calls {
		if call.Method == "Nudge" && call.Name == sessName && call.Message == "hello from legacy" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("ACP calls = %#v, want Nudge for legacy session", acpSP.Calls)
	}

	updated, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if updated.Metadata["transport"] != "acp" {
		t.Fatalf("transport metadata = %q, want %q", updated.Metadata["transport"], "acp")
	}
}

func TestGetDoesNotPersistGuessedTransportForLegacySession(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	autoSP := sessionauto.New(defaultSP, acpSP)

	legacy, err := store.Create(beads.Bead{
		Title: "legacy acp",
		Type:  BeadType,
		Labels: []string{
			LabelSession,
			"template:helper",
		},
		Metadata: map[string]string{
			"template": "helper",
			"state":    string(StateActive),
			"provider": "claude",
			"work_dir": "/tmp",
			"command":  "claude",
		},
	})
	if err != nil {
		t.Fatalf("Create legacy bead: %v", err)
	}

	mgr := NewManagerWithTransportResolver(store, autoSP, func(template, _ string) string {
		if template == "helper" {
			return "acp"
		}
		return ""
	})
	if _, err := mgr.Get(legacy.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}

	updated, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if updated.Metadata["transport"] != "" {
		t.Fatalf("transport metadata = %q, want empty on read-only lookup", updated.Metadata["transport"])
	}
}

func TestGetUsesConfiguredTransportForPendingCreateWithoutRuntimeProbe(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()

	deferred, err := store.Create(beads.Bead{
		Title: "deferred acp",
		Type:  BeadType,
		Labels: []string{
			LabelSession,
			"template:helper",
		},
		Metadata: map[string]string{
			"template":             "helper",
			"state":                string(StateCreating),
			"pending_create_claim": "true",
			"provider":             "claude",
			"work_dir":             "/tmp",
			"command":              "claude",
		},
	})
	if err != nil {
		t.Fatalf("Create deferred bead: %v", err)
	}

	mgr := NewManagerWithTransportResolver(store, sp, func(template, _ string) string {
		if template == "helper" {
			return "acp"
		}
		return ""
	})

	info, err := mgr.Get(deferred.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := info.Transport; got != "acp" {
		t.Fatalf("Transport = %q, want acp", got)
	}
	if len(sp.Calls) != 0 {
		t.Fatalf("runtime calls = %#v, want none for pending create", sp.Calls)
	}
}

func TestGetPrefersLiveTransportDetectionOverConfiguredTransportInference(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	autoSP := sessionauto.New(defaultSP, acpSP)

	legacy, err := store.Create(beads.Bead{
		Title: "legacy tmux",
		Type:  BeadType,
		Labels: []string{
			LabelSession,
			"template:helper",
		},
		Metadata: map[string]string{
			"template": "helper",
			"state":    string(StateActive),
			"provider": "claude",
			"work_dir": "/tmp",
			"command":  "claude",
		},
	})
	if err != nil {
		t.Fatalf("Create legacy bead: %v", err)
	}
	sessName := sessionNameFor(legacy.ID)
	if err := store.SetMetadata(legacy.ID, "session_name", sessName); err != nil {
		t.Fatalf("SetMetadata(session_name): %v", err)
	}
	if err := defaultSP.Start(context.Background(), sessName, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Start default session: %v", err)
	}

	mgr := NewManagerWithTransportResolver(store, autoSP, func(template, _ string) string {
		if template == "helper" {
			return "acp"
		}
		return ""
	})

	info, err := mgr.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := info.Transport; got != "" {
		t.Fatalf("Transport = %q, want empty for live tmux session", got)
	}

	updated, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if got := updated.Metadata["transport"]; got != "" {
		t.Fatalf("transport metadata = %q, want empty for live tmux session", got)
	}
}

func TestGetDoesNotInferConfiguredTransportForStoppedLegacySession(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	autoSP := sessionauto.New(defaultSP, acpSP)

	legacy, err := store.Create(beads.Bead{
		Title: "legacy tmux",
		Type:  BeadType,
		Labels: []string{
			LabelSession,
			"template:helper",
		},
		Metadata: map[string]string{
			"template": "helper",
			"state":    string(StateAsleep),
			"provider": "claude",
			"work_dir": "/tmp",
			"command":  "claude",
		},
	})
	if err != nil {
		t.Fatalf("Create legacy bead: %v", err)
	}
	sessName := sessionNameFor(legacy.ID)
	if err := store.SetMetadata(legacy.ID, "session_name", sessName); err != nil {
		t.Fatalf("SetMetadata(session_name): %v", err)
	}

	mgr := NewManagerWithTransportResolver(store, autoSP, func(template, _ string) string {
		if template == "helper" {
			return "acp"
		}
		return ""
	})

	info, err := mgr.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := info.Transport; got != "" {
		t.Fatalf("Transport = %q, want empty for stopped legacy session without stored transport", got)
	}

	updated, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if got := updated.Metadata["transport"]; got != "" {
		t.Fatalf("transport metadata = %q, want empty for read-only lookup", got)
	}
}

func TestGetDoesNotInferConfiguredTransportForStoppedLegacySessionWithPolicyFallback(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	autoSP := sessionauto.New(defaultSP, acpSP)

	legacy, err := store.Create(beads.Bead{
		Title: "legacy acp",
		Type:  BeadType,
		Labels: []string{
			LabelSession,
			"template:helper",
		},
		Metadata: map[string]string{
			"template": "helper",
			"state":    string(StateAsleep),
			"provider": "claude",
			"work_dir": "/tmp",
			"command":  "claude",
		},
	})
	if err != nil {
		t.Fatalf("Create legacy bead: %v", err)
	}
	sessName := sessionNameFor(legacy.ID)
	if err := store.SetMetadata(legacy.ID, "session_name", sessName); err != nil {
		t.Fatalf("SetMetadata(session_name): %v", err)
	}

	mgr := NewManagerWithTransportPolicyResolverAndCityPath(store, autoSP, "", func(template, _ string) (string, bool) {
		if template == "helper" {
			return "acp", true
		}
		return "", false
	})

	info, err := mgr.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := info.Transport; got != "" {
		t.Fatalf("Transport = %q, want empty for stopped legacy session without stored evidence", got)
	}

	updated, err := store.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if got := updated.Metadata["transport"]; got != "" {
		t.Fatalf("transport metadata = %q, want empty for read-only lookup", got)
	}
}

func TestGetInfersACPTransportFromStoredMCPMetadata(t *testing.T) {
	store := beads.NewMemStore()
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	autoSP := sessionauto.New(defaultSP, acpSP)

	legacy, err := store.Create(beads.Bead{
		Title: "legacy acp",
		Type:  BeadType,
		Labels: []string{
			LabelSession,
			"template:helper",
		},
		Metadata: map[string]string{
			"template":                    "helper",
			"state":                       string(StateAsleep),
			"provider":                    "claude",
			"work_dir":                    "/tmp",
			"command":                     "claude",
			MCPServersSnapshotMetadataKey: `[{"name":"filesystem","transport":"stdio","command":"/bin/mcp"}]`,
		},
	})
	if err != nil {
		t.Fatalf("Create legacy bead: %v", err)
	}

	mgr := NewManagerWithTransportResolver(store, autoSP, nil)
	info, err := mgr.Get(legacy.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := info.Transport; got != "acp" {
		t.Fatalf("Transport = %q, want acp from stored MCP metadata", got)
	}
}

func TestSendConvergesWhenSessionAlreadyResumed(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := sp.Start(context.Background(), info.SessionName, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("fake concurrent Start: %v", err)
	}

	if err := mgr.Send(context.Background(), info.ID, "hello", "claude --resume", runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != StateActive {
		t.Errorf("State = %q, want %q", got.State, StateActive)
	}
	found := false
	for _, call := range sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName && call.Message == "hello" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("calls = %#v, want Nudge hello after converged resume", sp.Calls)
	}
}

func TestSendRequiresResumeCommandForSuspendedSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	err = mgr.Send(context.Background(), info.ID, "hello", "", runtime.Config{})
	if !errors.Is(err, ErrResumeRequired) {
		t.Fatalf("Send error = %v, want ErrResumeRequired", err)
	}
}

func TestSendClosedSessionReturnsErrSessionClosed(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err = mgr.Send(context.Background(), info.ID, "hello", "claude --resume", runtime.Config{WorkDir: "/tmp"})
	if !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Send error = %v, want ErrSessionClosed", err)
	}
}

func TestSendDoesNotSuppressNonDuplicateResumeError(t *testing.T) {
	base := runtime.NewFake()
	sp := &startOverrideProvider{Fake: base}
	store := beads.NewMemStore()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := sp.Fake.Start(context.Background(), info.SessionName, runtime.Config{WorkDir: "/tmp"}); err != nil {
		t.Fatalf("fake concurrent Start: %v", err)
	}
	sp.startErr = errors.New("out of memory")

	err = mgr.Send(context.Background(), info.ID, "hello", "claude --resume", runtime.Config{WorkDir: "/tmp"})
	if err == nil || !strings.Contains(err.Error(), "out of memory") {
		t.Fatalf("Send error = %v, want underlying non-duplicate start failure", err)
	}
}

func TestStopTurnInterruptsActiveSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}

	found := false
	for _, call := range sp.Calls {
		if call.Method == "Interrupt" && call.Name == info.SessionName {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected Interrupt call")
	}
}

func TestStopTurnAllowsPoolManagedSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "pool-worker", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Mark the session bead as pool-managed.
	if err := store.Update(info.ID, beads.UpdateOpts{
		Metadata: map[string]string{"pool_managed": "true"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}

	found := false
	for _, call := range sp.Calls {
		if call.Method == "Interrupt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected Interrupt call for pool-managed session")
	}
}

func TestStopTurnAllowsPoolSlotOnlySession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "pool-slot-worker", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Mark the session bead with pool_slot only (no pool_managed).
	if err := store.Update(info.ID, beads.UpdateOpts{
		Metadata: map[string]string{"pool_slot": "1"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn: %v", err)
	}

	found := false
	for _, call := range sp.Calls {
		if call.Method == "Interrupt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected Interrupt call for pool-slot session")
	}
}

func TestPendingAndRespond(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	pending, supported, err := mgr.Pending(info.ID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if !supported {
		t.Fatal("Pending should report supported for runtime.Fake")
	}
	if pending == nil || pending.RequestID != "req-1" {
		t.Fatalf("Pending = %#v, want req-1", pending)
	}

	if err := mgr.Respond(info.ID, runtime.InteractionResponse{Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got, _, err := mgr.Pending(info.ID); err != nil {
		t.Fatalf("Pending after Respond: %v", err)
	} else if got != nil {
		t.Fatalf("pending should be cleared after Respond, got %#v", got)
	}
}

type pendingSessionGoneProvider struct {
	*runtime.Fake
}

func (p *pendingSessionGoneProvider) Pending(_ string) (*runtime.PendingInteraction, error) {
	return nil, fmt.Errorf("capturing pane: %w", runtime.ErrSessionNotFound)
}

type pendingSessionErrorProvider struct {
	*runtime.Fake
	err error
}

func (p *pendingSessionErrorProvider) Pending(_ string) (*runtime.PendingInteraction, error) {
	return nil, p.err
}

type respondSessionGoneProvider struct {
	*runtime.Fake
}

func (p *respondSessionGoneProvider) Pending(_ string) (*runtime.PendingInteraction, error) {
	return &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	}, nil
}

func (p *respondSessionGoneProvider) Respond(_ string, _ runtime.InteractionResponse) error {
	return fmt.Errorf("send-keys failed: %w", runtime.ErrSessionNotFound)
}

func TestPendingAndRespondTreatMissingRuntimeSessionAsNoPending(t *testing.T) {
	store := beads.NewMemStore()
	sp := &pendingSessionGoneProvider{Fake: runtime.NewFake()}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	pending, supported, err := mgr.Pending(info.ID)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if !supported {
		t.Fatal("Pending should report supported when the provider supports interactions")
	}
	if pending != nil {
		t.Fatalf("Pending = %#v, want nil for missing runtime session", pending)
	}

	err = mgr.Respond(info.ID, runtime.InteractionResponse{Action: "approve"})
	if !errors.Is(err, ErrNoPendingInteraction) {
		t.Fatalf("Respond error = %v, want ErrNoPendingInteraction", err)
	}
}

func TestRespondTreatsRuntimeSessionGoneDuringResponseAsNoPending(t *testing.T) {
	store := beads.NewMemStore()
	sp := &respondSessionGoneProvider{Fake: runtime.NewFake()}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = mgr.Respond(info.ID, runtime.InteractionResponse{Action: "approve"})
	if !errors.Is(err, ErrNoPendingInteraction) {
		t.Fatalf("Respond error = %v, want ErrNoPendingInteraction", err)
	}
}

func TestPendingAndRespondDoNotSwallowUnrelatedNotFoundErrors(t *testing.T) {
	store := beads.NewMemStore()
	sp := &pendingSessionErrorProvider{
		Fake: runtime.NewFake(),
		err:  fmt.Errorf("loading config file: not found"),
	}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	pending, supported, err := mgr.Pending(info.ID)
	if err == nil {
		t.Fatalf("Pending err = nil, want unrelated provider error")
	}
	if !supported {
		t.Fatal("Pending should report supported when provider returned a non-session-gone error")
	}
	if pending != nil {
		t.Fatalf("Pending = %#v, want nil on provider error", pending)
	}
	if !strings.Contains(err.Error(), "loading config file: not found") {
		t.Fatalf("Pending err = %v, want original provider error", err)
	}

	err = mgr.Respond(info.ID, runtime.InteractionResponse{Action: "approve"})
	if err == nil {
		t.Fatalf("Respond err = nil, want unrelated provider error")
	}
	if errors.Is(err, ErrNoPendingInteraction) {
		t.Fatalf("Respond err = %v, must not downgrade unrelated provider errors to ErrNoPendingInteraction", err)
	}
	if !strings.Contains(err.Error(), "loading config file: not found") {
		t.Fatalf("Respond err = %v, want original provider error", err)
	}
}

func TestSendRejectsPendingInteraction(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	err = mgr.Send(context.Background(), info.ID, "hello", "", runtime.Config{})
	if !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("Send error = %v, want %v", err, ErrPendingInteraction)
	}
	for _, call := range sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName {
			t.Fatalf("unexpected Nudge while pending interaction is active: %#v", sp.Calls)
		}
	}
}

func TestSendImmediateRejectsPendingInteraction(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "helper", "", "claude", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "approval",
		Prompt:    "approve?",
	})

	err = mgr.SendImmediate(context.Background(), info.ID, "hello", "", runtime.Config{})
	if !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("SendImmediate error = %v, want %v", err, ErrPendingInteraction)
	}
	for _, call := range sp.Calls {
		if (call.Method == "Nudge" || call.Method == "NudgeNow") && call.Name == info.SessionName {
			t.Fatalf("unexpected nudge while pending interaction is active: %#v", sp.Calls)
		}
	}
}

func TestTranscriptPathPrefersSessionKey(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	workDir := t.TempDir()
	resume := ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	info, err := mgr.Create(context.Background(), "helper", "", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	searchBase := t.TempDir()
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	keyPath := filepath.Join(slugDir, info.SessionKey+".jsonl")
	if err := os.WriteFile(keyPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(key): %v", err)
	}
	latestPath := filepath.Join(slugDir, "latest.jsonl")
	if err := os.WriteFile(latestPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(latest): %v", err)
	}

	path, err := mgr.TranscriptPath(info.ID, []string{searchBase})
	if err != nil {
		t.Fatalf("TranscriptPath: %v", err)
	}
	if path != keyPath {
		t.Errorf("TranscriptPath = %q, want %q", path, keyPath)
	}
}

func TestTranscriptPathAllowsClosedSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	workDir := t.TempDir()
	resume := ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}
	info, err := mgr.Create(context.Background(), "helper", "", "claude", workDir, "claude", nil, resume, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Close(info.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	searchBase := t.TempDir()
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	keyPath := filepath.Join(slugDir, info.SessionKey+".jsonl")
	if err := os.WriteFile(keyPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	path, err := mgr.TranscriptPath(info.ID, []string{searchBase})
	if err != nil {
		t.Fatalf("TranscriptPath: %v", err)
	}
	if path != keyPath {
		t.Errorf("TranscriptPath = %q, want %q for closed session", path, keyPath)
	}
}

func TestTranscriptPathSkipsAmbiguousWorkDirFallback(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	workDir := t.TempDir()
	if _, err := mgr.Create(context.Background(), "helper", "one", "claude", workDir, "claude", nil, ProviderResume{}, runtime.Config{}); err != nil {
		t.Fatalf("Create one: %v", err)
	}
	info2, err := mgr.Create(context.Background(), "helper", "two", "claude", workDir, "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create two: %v", err)
	}

	searchBase := t.TempDir()
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fallbackPath := filepath.Join(slugDir, "latest.jsonl")
	if err := os.WriteFile(fallbackPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	path, err := mgr.TranscriptPath(info2.ID, []string{searchBase})
	if err != nil {
		t.Fatalf("TranscriptPath: %v", err)
	}
	if path != "" {
		t.Errorf("TranscriptPath = %q, want empty when workdir fallback is ambiguous", path)
	}
}

func TestTranscriptPathClosedSessionSkipsAmbiguousHistoricalWorkDirFallback(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	workDir := t.TempDir()
	info1, err := mgr.Create(context.Background(), "helper", "one", "codex", workDir, "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create one: %v", err)
	}
	info2, err := mgr.Create(context.Background(), "helper", "two", "codex", workDir, "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create two: %v", err)
	}
	if err := mgr.Close(info1.ID); err != nil {
		t.Fatalf("Close one: %v", err)
	}
	if err := mgr.Close(info2.ID); err != nil {
		t.Fatalf("Close two: %v", err)
	}

	searchBase := t.TempDir()
	dayDir := filepath.Join(searchBase, "2026", "05", "04")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	codexPath := filepath.Join(dayDir, "rollout-current.jsonl")
	meta := `{"type":"session_meta","payload":{"cwd":"` + workDir + `"}}`
	if err := os.WriteFile(codexPath, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	path, err := mgr.TranscriptPath(info1.ID, []string{searchBase})
	if err != nil {
		t.Fatalf("TranscriptPath: %v", err)
	}
	if path != "" {
		t.Errorf("TranscriptPath = %q, want empty for ambiguous historical codex workdir", path)
	}
}

func TestTranscriptPathSameWorkDirDifferentProvidersUsesProviderSpecificFallback(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	workDir := t.TempDir()
	if _, err := mgr.Create(context.Background(), "helper", "claude", "claude", workDir, "claude", nil, ProviderResume{}, runtime.Config{}); err != nil {
		t.Fatalf("Create claude: %v", err)
	}
	info, err := mgr.Create(context.Background(), "helper", "codex", "codex", workDir, "codex", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create codex: %v", err)
	}

	searchBase := t.TempDir()
	dayDir := filepath.Join(searchBase, "2026", "03", "27")
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	codexPath := filepath.Join(dayDir, "rollout-current.jsonl")
	meta := `{"type":"session_meta","payload":{"cwd":"` + workDir + `"}}`
	if err := os.WriteFile(codexPath, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	path, err := mgr.TranscriptPath(info.ID, []string{searchBase})
	if err != nil {
		t.Fatalf("TranscriptPath: %v", err)
	}
	if path != codexPath {
		t.Errorf("TranscriptPath = %q, want %q", path, codexPath)
	}
}

func TestKill_ActiveState(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "test", "claude", "/tmp", "claude", "", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Kill(info.ID); err != nil {
		t.Fatalf("Kill active session: %v", err)
	}
}

func TestKill_AwakeState(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	info, err := mgr.CreateNamedWithTransport(context.Background(), "sky", "helper", "test", "claude", "/tmp", "claude", "", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, "state", string(StateAwake)); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := mgr.Kill(info.ID); err != nil {
		t.Fatalf("Kill awake session: %v", err)
	}
}

func TestKill_StoppedState_NotRunning(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManager(store, sp)

	b, err := store.Create(beads.Bead{
		Title:    "helper",
		Type:     BeadType,
		Labels:   []string{LabelSession},
		Metadata: map[string]string{"session_name": "sky", "state": "stopped"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Kill(b.ID); err == nil {
		t.Fatal("expected Kill to fail for stopped non-running session")
	}
}

func TestKill_UnknownState_ButRunning(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "sky", runtime.Config{Command: "claude"})
	mgr := NewManager(store, sp)

	b, err := store.Create(beads.Bead{
		Title:    "helper",
		Type:     BeadType,
		Labels:   []string{LabelSession},
		Metadata: map[string]string{"session_name": "sky", "state": "some-future-state"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Kill(b.ID); err != nil {
		t.Fatalf("Kill running session with unknown state: %v", err)
	}
}

// PR #203 — When ensureRunning resumes with --resume <key> and the
// process dies immediately (stale session key), it should clear the key and
// retry fresh without the --resume flag.
func TestEnsureRunning_RetriesWithoutStaleSessionKey(t *testing.T) {
	store := beads.NewMemStore()
	base := runtime.NewFake()

	sp := &failOnceStartProvider{Fake: base}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "worker", "", "claude --dangerously", "/tmp", "claude", nil, ProviderResume{
		ResumeFlag:    "--resume",
		SessionIDFlag: "--session-id",
	}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	sessionKey := b.Metadata["session_key"]
	if sessionKey == "" {
		t.Fatal("expected session_key in bead metadata after Create with ResumeFlag")
	}

	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	sp.armed = true

	resumeCmd := "claude --dangerously --resume " + sessionKey
	err = mgr.Send(context.Background(), info.ID, "hello", resumeCmd, runtime.Config{WorkDir: "/tmp"})
	if err != nil {
		t.Fatalf("Send should retry without stale resume flag but failed: %v", err)
	}

	if !base.IsRunning(info.SessionName) {
		t.Fatal("session should be running after fresh retry")
	}

	b, _ = store.Get(info.ID)
	if b.Metadata["session_key"] != "" {
		t.Errorf("session_key should be cleared after stale key retry, got %q", b.Metadata["session_key"])
	}
}

// TestEnsureRunning_StaleKeyRetryAlsoFails verifies that when the stale-key
// resume detects death and the fresh retry also fails, the error propagates.
func TestEnsureRunning_StaleKeyRetryAlsoFails(t *testing.T) {
	store := beads.NewMemStore()
	base := runtime.NewFake()

	sp := &dieAndFailProvider{Fake: base}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "worker", "", "claude --dangerously", "/tmp", "claude", nil, ProviderResume{
		ResumeFlag:    "--resume",
		SessionIDFlag: "--session-id",
	}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	if b.Metadata["session_key"] == "" {
		t.Fatal("expected session_key in bead metadata")
	}

	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	sp.callCount = 0
	resumeCmd := "claude --dangerously --resume " + b.Metadata["session_key"]
	err = mgr.Send(context.Background(), info.ID, "hello", resumeCmd, runtime.Config{WorkDir: "/tmp"})

	if err == nil {
		t.Fatal("Send should fail when both stale-key resume and fresh retry fail")
	}
	b, _ = store.Get(info.ID)
	if b.Metadata["session_key"] != "" {
		t.Errorf("session_key should be cleared even on retry failure, got %q", b.Metadata["session_key"])
	}
}

func TestEnsureRunning_RetriesAfterStartupDeathError(t *testing.T) {
	store := beads.NewMemStore()
	base := runtime.NewFake()

	sp := &startupDeathProvider{Fake: base}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "worker", "", "claude --dangerously", "/tmp", "claude", nil, ProviderResume{
		ResumeFlag:    "--resume",
		SessionIDFlag: "--session-id",
	}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	sessionKey := b.Metadata["session_key"]
	if sessionKey == "" {
		t.Fatal("expected session_key in bead metadata after Create with ResumeFlag")
	}
	if err := store.SetMetadata(info.ID, "started_config_hash", "hash-before"); err != nil {
		t.Fatalf("SetMetadata started_config_hash: %v", err)
	}

	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	sp.armed = true

	resumeCmd := "claude --dangerously --resume " + sessionKey
	err = mgr.Send(context.Background(), info.ID, "hello", resumeCmd, runtime.Config{WorkDir: "/tmp"})
	if err != nil {
		t.Fatalf("Send should retry after startup-death error but failed: %v", err)
	}

	if !base.IsRunning(info.SessionName) {
		t.Fatal("session should be running after fresh retry")
	}

	b, _ = store.Get(info.ID)
	if b.Metadata["session_key"] != "" {
		t.Errorf("session_key should be cleared after startup-death retry, got %q", b.Metadata["session_key"])
	}
	if b.Metadata["started_config_hash"] != "" {
		t.Errorf("started_config_hash should be cleared after startup-death retry, got %q", b.Metadata["started_config_hash"])
	}
	if b.Metadata["continuation_reset_pending"] != "true" {
		t.Errorf("continuation_reset_pending should be set after startup-death retry, got %q", b.Metadata["continuation_reset_pending"])
	}
}

func TestEnsureRunning_StartupDeathWithoutStrippableResumeClearsMetadata(t *testing.T) {
	store := beads.NewMemStore()
	base := runtime.NewFake()

	sp := &startupDeathProvider{Fake: base}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "worker", "", "claude --dangerously", "/tmp", "claude", nil, ProviderResume{
		ResumeFlag:    "--resume",
		SessionIDFlag: "--session-id",
	}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.SetMetadata(info.ID, "started_config_hash", "hash-before"); err != nil {
		t.Fatalf("SetMetadata started_config_hash: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	if b.Metadata["session_key"] == "" {
		t.Fatal("expected session_key in bead metadata after Create with ResumeFlag")
	}

	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	sp.armed = true

	err = mgr.Send(context.Background(), info.ID, "hello", "claude --dangerously", runtime.Config{WorkDir: "/tmp"})
	if err == nil {
		t.Fatal("Send should fail when stale resume metadata cannot be stripped from the resume command")
	}

	b, _ = store.Get(info.ID)
	if b.Metadata["session_key"] != "" {
		t.Errorf("session_key should be cleared after unstrippable startup-death fallback, got %q", b.Metadata["session_key"])
	}
	if b.Metadata["started_config_hash"] != "" {
		t.Errorf("started_config_hash should be cleared after unstrippable startup-death fallback, got %q", b.Metadata["started_config_hash"])
	}
	if b.Metadata["continuation_reset_pending"] != "true" {
		t.Errorf("continuation_reset_pending should be set after unstrippable startup-death fallback, got %q", b.Metadata["continuation_reset_pending"])
	}
	if b.Metadata["state"] != string(StateSuspended) {
		t.Errorf("state should remain suspended after failed unstrippable fallback, got %q", b.Metadata["state"])
	}
}

// Issue #1655 — a session created without resume capability
// (ProviderResume{} on Create → empty resume_flag in bead metadata)
// must still be able to recover from a stale session_key. The
// named-always case in the issue body is one instance of this shape;
// the invariant is general — any session whose start command was
// never resume-capable should clear a stale key and start fresh
// rather than bail. Previously retryFreshStartAfterStaleKey refused
// the retry because stripResumeFlag is a no-op when resume_flag is
// empty, and the function misclassified that as a strip failure.
func TestEnsureRunning_RetriesWhenResumeFlagIsEmpty(t *testing.T) {
	store := beads.NewMemStore()
	base := runtime.NewFake()

	sp := &startupDeathProvider{Fake: base}
	mgr := NewManager(store, sp)

	// Create a session without resume capability — ProviderResume{}
	// yields an empty resume_flag in bead metadata. The same shape
	// arises for any configured-named-always session whose start
	// command lacks a --resume-style flag.
	info, err := mgr.Create(context.Background(), "worker", "", "fakecmd --follow worker", "/tmp", "claude", nil, ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	if b.Metadata["resume_flag"] != "" {
		t.Fatalf("expected empty resume_flag for ProviderResume{}, got %q", b.Metadata["resume_flag"])
	}

	// Simulate the post-run state described in #1655: the session ran
	// once and a session_key landed in bead metadata even though the
	// start command never accepted a resume flag.
	if err := store.SetMetadata(info.ID, "session_key", "stale-key-1"); err != nil {
		t.Fatalf("SetMetadata session_key: %v", err)
	}
	if err := store.SetMetadata(info.ID, "started_config_hash", "hash-before"); err != nil {
		t.Fatalf("SetMetadata started_config_hash: %v", err)
	}

	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	sp.armed = true

	// For a session without resume capability the "resume command"
	// passed to Send is just the original start command — there is no
	// --resume flag to add or strip.
	err = mgr.Send(context.Background(), info.ID, "hello", "fakecmd --follow worker", runtime.Config{WorkDir: "/tmp"})
	if err != nil {
		t.Fatalf("Send should retry fresh when resume_flag is empty but failed: %v", err)
	}

	if !base.IsRunning(info.SessionName) {
		t.Fatal("session should be running after empty-resume_flag fresh retry")
	}

	b, _ = store.Get(info.ID)
	if b.Metadata["session_key"] != "" {
		t.Errorf("session_key should be cleared after empty-resume_flag retry, got %q", b.Metadata["session_key"])
	}
	if b.Metadata["started_config_hash"] != "" {
		t.Errorf("started_config_hash should be cleared after empty-resume_flag retry, got %q", b.Metadata["started_config_hash"])
	}
	if b.Metadata["continuation_reset_pending"] != "true" {
		t.Errorf("continuation_reset_pending should be set after empty-resume_flag retry, got %q", b.Metadata["continuation_reset_pending"])
	}
}

func TestEnsureRunning_StartupDeathClearMetadataFailurePropagates(t *testing.T) {
	store := failMetadataKeyStore{MemStore: beads.NewMemStore(), key: "session_key"}
	base := runtime.NewFake()
	sp := &startupDeathProvider{Fake: base}
	mgr := NewManager(store, sp)

	info, err := mgr.Create(context.Background(), "worker", "", "claude --dangerously", "/tmp", "claude", nil, ProviderResume{
		ResumeFlag:    "--resume",
		SessionIDFlag: "--session-id",
	}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	b, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}
	sessionKey := b.Metadata["session_key"]
	if sessionKey == "" {
		t.Fatal("expected session_key in bead metadata after Create with ResumeFlag")
	}

	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	sp.armed = true
	resumeCmd := "claude --dangerously --resume " + sessionKey
	err = mgr.Send(context.Background(), info.ID, "hello", resumeCmd, runtime.Config{WorkDir: "/tmp"})
	if err == nil {
		t.Fatal("Send should fail when stale resume metadata cannot be cleared")
	}
	if !strings.Contains(err.Error(), "clearing stale resume metadata session_key") {
		t.Fatalf("Send error = %v, want stale metadata clear failure", err)
	}

	b, err = store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead after failure: %v", err)
	}
	if b.Metadata["session_key"] == "" {
		t.Fatal("session_key should remain set after failed metadata clear")
	}
}
