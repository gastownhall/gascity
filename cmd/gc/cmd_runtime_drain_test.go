package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// drainOpsWithCountdown wraps fakeDrainOps and returns false for isRestartRequested
// after N calls, simulating the reconciler clearing the flag without concurrent map access.
type drainOpsWithCountdown struct {
	*fakeDrainOps
	remaining int
	cleared   bool
}

func (c *drainOpsWithCountdown) isRestartRequested(sessionName string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return false, c.err
	}
	if !c.restartRequested[sessionName] {
		if c.cleared {
			return false, nil
		}
		return false, errors.New("restart flag was not set before polling")
	}
	if c.remaining <= 0 {
		delete(c.restartRequested, sessionName)
		c.cleared = true
		return false, nil
	}
	c.remaining--
	return true, nil
}

// fakeDrainOps is a test double for drainOps.
type fakeDrainOps struct {
	mu               sync.Mutex
	draining         map[string]bool
	drainTimes       map[string]time.Time // when drain was set
	acked            map[string]bool
	restartRequested map[string]bool
	driftRestart     map[string]bool
	err              error // injected error for all ops
	restartReadErr   error
	setDrainCalls    []string
	clearDrainCalls  []string
}

func newFakeDrainOps() *fakeDrainOps {
	return &fakeDrainOps{
		draining:         make(map[string]bool),
		drainTimes:       make(map[string]time.Time),
		acked:            make(map[string]bool),
		restartRequested: make(map[string]bool),
		driftRestart:     make(map[string]bool),
	}
}

func (f *fakeDrainOps) setDrain(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setDrainCalls = append(f.setDrainCalls, sessionName)
	if f.err != nil {
		return f.err
	}
	f.draining[sessionName] = true
	f.drainTimes[sessionName] = time.Now()
	return nil
}

func (f *fakeDrainOps) clearDrain(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clearDrainCalls = append(f.clearDrainCalls, sessionName)
	if f.err != nil {
		return f.err
	}
	delete(f.draining, sessionName)
	delete(f.drainTimes, sessionName)
	return nil
}

func (f *fakeDrainOps) isDraining(sessionName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.draining[sessionName], nil
}

func (f *fakeDrainOps) drainStartTime(sessionName string) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return time.Time{}, f.err
	}
	t, ok := f.drainTimes[sessionName]
	if !ok {
		return time.Time{}, fmt.Errorf("no drain time for %s", sessionName)
	}
	return t, nil
}

func (f *fakeDrainOps) setDrainAck(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.acked[sessionName] = true
	return nil
}

func (f *fakeDrainOps) isDrainAcked(sessionName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.acked[sessionName], nil
}

func (f *fakeDrainOps) setRestartRequested(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.restartRequested[sessionName] = true
	return nil
}

func (f *fakeDrainOps) isRestartRequested(sessionName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	if f.restartReadErr != nil {
		return false, f.restartReadErr
	}
	return f.restartRequested[sessionName], nil
}

func (f *fakeDrainOps) clearRestartRequested(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.restartRequested, sessionName)
	return nil
}

func (f *fakeDrainOps) setDriftRestart(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.driftRestart[sessionName] = true
	return nil
}

func (f *fakeDrainOps) isDriftRestart(sessionName string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	return f.driftRestart[sessionName], nil
}

func (f *fakeDrainOps) clearDriftRestart(sessionName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	delete(f.driftRestart, sessionName)
	return nil
}

// ---------------------------------------------------------------------------
// doRuntimeDrain tests
// ---------------------------------------------------------------------------

func TestDoRuntimeDrain(t *testing.T) {
	dops := newFakeDrainOps()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	code := doRuntimeDrain(dops, sp, rec, "worker", "worker", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !dops.draining["worker"] {
		t.Error("drain flag not set")
	}
	if got := stdout.String(); got != "Draining session 'worker'\n" {
		t.Errorf("stdout = %q, want %q", got, "Draining session 'worker'\n")
	}
	if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionDraining {
		t.Errorf("events = %v, want one SessionDraining event", rec.Events)
	}
	if rec.Events[0].Subject != "worker" {
		t.Errorf("event subject = %q, want %q", rec.Events[0].Subject, "worker")
	}
}

func TestDoRuntimeDrainNotRunning(t *testing.T) {
	dops := newFakeDrainOps()
	sp := runtime.NewFake() // no sessions started

	var stdout, stderr bytes.Buffer
	code := doRuntimeDrain(dops, sp, events.Discard, "worker", "worker", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got := stderr.String(); got != "gc runtime drain: session \"worker\" is not running\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestDoRuntimeDrainSetError(t *testing.T) {
	dops := newFakeDrainOps()
	dops.err = errors.New("tmux borked")
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRuntimeDrain(dops, sp, events.Discard, "worker", "worker", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got := stderr.String(); got != "gc runtime drain: tmux borked\n" {
		t.Errorf("stderr = %q", got)
	}
}

// ---------------------------------------------------------------------------
// doRuntimeUndrain tests
// ---------------------------------------------------------------------------

func TestDoRuntimeUndrain(t *testing.T) {
	dops := newFakeDrainOps()
	dops.draining["worker"] = true
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "echo"}); err != nil {
		t.Fatal(err)
	}

	rec := events.NewFake()
	var stdout, stderr bytes.Buffer
	code := doRuntimeUndrain(dops, sp, rec, "worker", "worker", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if dops.draining["worker"] {
		t.Error("drain flag still set after undrain")
	}
	if got := stdout.String(); got != "Undrained session 'worker'\n" {
		t.Errorf("stdout = %q, want %q", got, "Undrained session 'worker'\n")
	}
	if len(rec.Events) != 1 || rec.Events[0].Type != events.SessionUndrained {
		t.Errorf("events = %v, want one SessionUndrained event", rec.Events)
	}
}

func TestDoRuntimeUndrainNotRunning(t *testing.T) {
	dops := newFakeDrainOps()
	sp := runtime.NewFake()

	var stdout, stderr bytes.Buffer
	code := doRuntimeUndrain(dops, sp, events.Discard, "worker", "worker", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got := stderr.String(); got != "gc runtime undrain: session \"worker\" is not running\n" {
		t.Errorf("stderr = %q", got)
	}
}

// ---------------------------------------------------------------------------
// doRuntimeDrainCheck tests
// ---------------------------------------------------------------------------

func TestDoRuntimeDrainCheck(t *testing.T) {
	dops := newFakeDrainOps()
	dops.draining["worker"] = true

	code := doRuntimeDrainCheck(dops, "worker")
	if code != 0 {
		t.Errorf("code = %d, want 0 (draining)", code)
	}
}

func TestDoRuntimeDrainCheckNotDraining(t *testing.T) {
	dops := newFakeDrainOps()

	code := doRuntimeDrainCheck(dops, "worker")
	if code != 1 {
		t.Errorf("code = %d, want 1 (not draining)", code)
	}
}

func TestDoRuntimeDrainCheckError(t *testing.T) {
	dops := newFakeDrainOps()
	dops.err = errors.New("tmux gone")

	code := doRuntimeDrainCheck(dops, "worker")
	if code != 1 {
		t.Errorf("code = %d, want 1 (error → not draining)", code)
	}
}

// ---------------------------------------------------------------------------
// doRuntimeDrainAck tests
// ---------------------------------------------------------------------------

func TestDoRuntimeDrainAck(t *testing.T) {
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainAck(dops, "worker", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !dops.acked["worker"] {
		t.Error("drain ack flag not set")
	}
	if got := stdout.String(); got != "Drain acknowledged. Controller will stop this session.\n" {
		t.Errorf("stdout = %q", got)
	}
}

func TestDoRuntimeDrainAckError(t *testing.T) {
	dops := newFakeDrainOps()
	dops.err = errors.New("tmux borked")
	var stdout, stderr bytes.Buffer
	code := doRuntimeDrainAck(dops, "worker", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got := stderr.String(); got != "gc runtime drain-ack: tmux borked\n" {
		t.Errorf("stderr = %q", got)
	}
}

// ---------------------------------------------------------------------------
// newDrainOps factory tests
// ---------------------------------------------------------------------------

func TestNewDrainOpsAlwaysReturnsNonNil(t *testing.T) {
	// newDrainOps works with any Provider — no type assertions.
	fp := runtime.NewFake()
	dops := newDrainOps(fp)
	if dops == nil {
		t.Fatal("newDrainOps(Fake) = nil, want non-nil")
	}
	if _, ok := dops.(*providerDrainOps); !ok {
		t.Errorf("newDrainOps returned %T, want *providerDrainOps", dops)
	}
}

func TestProviderDrainOpsRoundTrip(t *testing.T) {
	// Verify drain ops work through Provider meta interface.
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "worker", runtime.Config{})
	dops := newDrainOps(sp)

	// Not draining initially.
	draining, _ := dops.isDraining("worker")
	if draining {
		t.Error("should not be draining initially")
	}

	// Set drain.
	if err := dops.setDrain("worker"); err != nil {
		t.Fatalf("setDrain: %v", err)
	}
	draining, _ = dops.isDraining("worker")
	if !draining {
		t.Error("should be draining after setDrain")
	}

	// Drain start time should be parseable.
	ts, err := dops.drainStartTime("worker")
	if err != nil {
		t.Fatalf("drainStartTime: %v", err)
	}
	if ts.IsZero() {
		t.Error("drain start time should not be zero")
	}

	// Set and check ack.
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatalf("setDrainAck: %v", err)
	}
	acked, _ := dops.isDrainAcked("worker")
	if !acked {
		t.Error("should be acked after setDrainAck")
	}

	// Clear drain (also clears ack).
	if err := dops.clearDrain("worker"); err != nil {
		t.Fatalf("clearDrain: %v", err)
	}
	draining, _ = dops.isDraining("worker")
	if draining {
		t.Error("should not be draining after clearDrain")
	}
	acked, _ = dops.isDrainAcked("worker")
	if acked {
		t.Error("ack should be cleared after clearDrain")
	}
}

func TestProviderDrainOpsReportsMetadataErrors(t *testing.T) {
	sp := runtime.NewFailFake()
	dops := newDrainOps(sp)

	if _, err := dops.isDraining("worker"); err == nil {
		t.Fatal("isDraining should return an error when metadata lookup fails")
	}
	if _, err := dops.isDrainAcked("worker"); err == nil {
		t.Fatal("isDrainAcked should return an error when metadata lookup fails")
	}
	if _, err := dops.isRestartRequested("worker"); err == nil {
		t.Fatal("isRestartRequested should return an error when metadata lookup fails")
	}
	if _, err := dops.isDriftRestart("worker"); err == nil {
		t.Fatal("isDriftRestart should return an error when metadata lookup fails")
	}
	if err := dops.clearDrain("worker"); err == nil {
		t.Fatal("clearDrain should return an error when metadata removal fails")
	}
	if err := dops.setDrainAck("worker"); err == nil {
		t.Fatal("setDrainAck should return an error when metadata removal fails")
	}
}

type recordingMetaProvider struct {
	*runtime.Fake
	removeErr  error
	removeKeys []string
	setKeys    []string
}

func (p *recordingMetaProvider) RemoveMeta(name, key string) error {
	p.removeKeys = append(p.removeKeys, key)
	if p.removeErr != nil {
		return p.removeErr
	}
	return p.Fake.RemoveMeta(name, key)
}

func (p *recordingMetaProvider) SetMeta(name, key, value string) error {
	p.setKeys = append(p.setKeys, key)
	return p.Fake.SetMeta(name, key, value)
}

func TestProviderDrainOpsClearDrainAttemptsAllMetadataRemovals(t *testing.T) {
	sp := &recordingMetaProvider{
		Fake:      runtime.NewFake(),
		removeErr: errors.New("metadata removal unavailable"),
	}
	dops := newDrainOps(sp)

	if err := dops.clearDrain("worker"); err == nil {
		t.Fatal("clearDrain should report metadata removal errors")
	}
	want := []string{
		"GC_DRAIN_ACK",
		reconcilerDrainAckSourceKey,
		reconcilerDrainAckReasonKey,
		reconcilerDrainAckGenerationKey,
		"GC_DRAIN",
	}
	if !slices.Equal(sp.removeKeys, want) {
		t.Fatalf("removed keys = %v, want %v", sp.removeKeys, want)
	}
}

func TestProviderDrainOpsSetDrainAckAttemptsAckAfterCleanupErrors(t *testing.T) {
	sp := &recordingMetaProvider{
		Fake:      runtime.NewFake(),
		removeErr: errors.New("metadata removal unavailable"),
	}
	dops := newDrainOps(sp)

	if err := dops.setDrainAck("worker"); err == nil {
		t.Fatal("setDrainAck should report metadata cleanup errors")
	}
	wantRemove := []string{
		reconcilerDrainAckReasonKey,
		reconcilerDrainAckGenerationKey,
	}
	if !slices.Equal(sp.removeKeys, wantRemove) {
		t.Fatalf("removed keys = %v, want %v", sp.removeKeys, wantRemove)
	}
	wantSet := []string{
		reconcilerDrainAckSourceKey,
		"GC_DRAIN_ACK",
	}
	if !slices.Equal(sp.setKeys, wantSet) {
		t.Fatalf("set keys = %v, want %v", sp.setKeys, wantSet)
	}
}

// ---------------------------------------------------------------------------
// doRuntimeRequestRestart tests
// ---------------------------------------------------------------------------

func TestDoRuntimeRequestRestartError(t *testing.T) {
	dops := newFakeDrainOps()
	dops.err = errors.New("tmux borked")
	var stdout, stderr bytes.Buffer
	code := doRuntimeRequestRestart(context.Background(), dops, nil, events.Discard, "worker", "worker",
		time.Millisecond, time.Second, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if got := stderr.String(); got != "gc runtime request-restart: tmux borked\n" {
		t.Errorf("stderr = %q", got)
	}
}

func TestDoRuntimeRequestRestartFlagCleared(t *testing.T) {
	dops := &drainOpsWithCountdown{fakeDrainOps: newFakeDrainOps(), remaining: 2}

	var stdout, stderr bytes.Buffer
	code := doRuntimeRequestRestart(context.Background(), dops, nil, events.Discard, "worker", "worker",
		10*time.Millisecond, 5*time.Second, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0 when flag cleared; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "Waiting up to 5s") {
		t.Errorf("stdout = %q, want bounded wait banner", got)
	}
	if dops.restartRequested["worker"] {
		t.Error("restart flag should be cleared by the simulated reconciler")
	}
}

func TestDoRuntimeRequestRestartTimeout(t *testing.T) {
	dops := newFakeDrainOps()

	var stdout, stderr bytes.Buffer
	code := doRuntimeRequestRestart(context.Background(), dops, nil, events.Discard, "worker", "worker",
		10*time.Millisecond, 25*time.Millisecond, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 on timeout", code)
	}
	if got := stderr.String(); !strings.Contains(got, "controller did not act within") {
		t.Errorf("stderr = %q, want timeout diagnostic", got)
	}
	if !strings.Contains(stderr.String(), "gc dashboard") {
		t.Errorf("stderr = %q, want gc dashboard hint", stderr.String())
	}
}

func TestDoRuntimeRequestRestartTimeoutReportsLastPollError(t *testing.T) {
	dops := newFakeDrainOps()
	dops.restartReadErr = errors.New("metadata read failed")

	var stdout, stderr bytes.Buffer
	code := doRuntimeRequestRestart(context.Background(), dops, nil, events.Discard, "worker", "worker",
		10*time.Millisecond, 25*time.Millisecond, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 on timeout", code)
	}
	if got := stderr.String(); !strings.Contains(got, "last poll error: metadata read failed") {
		t.Errorf("stderr = %q, want last poll error", got)
	}
}

func TestDoRuntimeRequestRestartContextCancel(t *testing.T) {
	dops := newFakeDrainOps()

	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer

	done := make(chan int, 1)
	go func() {
		done <- doRuntimeRequestRestart(ctx, dops, nil, events.Discard, "worker", "worker",
			10*time.Millisecond, 30*time.Second, &stdout, &stderr)
	}()

	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, want 0 on context cancel", code)
		}
		// Flag must remain set so the controller can still act on its next tick.
		if !dops.restartRequested["worker"] {
			t.Error("restart flag should remain set after context cancel")
		}
		if got := stderr.String(); !strings.Contains(got, "restart request remains set") {
			t.Errorf("stderr = %q, want pending restart warning", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("doRuntimeRequestRestart did not exit on context cancel")
	}
}

func TestControllerRestartTimeout(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.City
		want time.Duration
	}{
		{name: "nil config uses floor", cfg: nil, want: 5 * time.Minute},
		{name: "empty interval uses floor", cfg: &config.City{}, want: 5 * time.Minute},
		{name: "below floor clamps up", cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: "15s"}}, want: 5 * time.Minute},
		{name: "middle range uses multiplier", cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: "2m"}}, want: 10 * time.Minute},
		{name: "ceiling edge", cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: "6m"}}, want: 30 * time.Minute},
		{name: "above ceiling clamps down", cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: "10m"}}, want: 30 * time.Minute},
		{name: "invalid duration uses default floor", cfg: &config.City{Daemon: config.DaemonConfig{PatrolInterval: "later"}}, want: 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := controllerRestartTimeout(tt.cfg); got != tt.want {
				t.Fatalf("controllerRestartTimeout() = %s, want %s", got, tt.want)
			}
		})
	}
}

type getMetaErrorProvider struct {
	*runtime.Fake
	err error
}

func (p *getMetaErrorProvider) GetMeta(_, _ string) (string, error) {
	return "", p.err
}

func TestProviderDrainOpsIsRestartRequestedTreatsGoneSessionAsCleared(t *testing.T) {
	dops := newDrainOps(&getMetaErrorProvider{
		Fake: runtime.NewFake(),
		err:  runtime.ErrSessionNotFound,
	})
	requested, err := dops.isRestartRequested("worker")
	if err != nil {
		t.Fatalf("isRestartRequested returned gone-session error: %v", err)
	}
	if requested {
		t.Fatal("isRestartRequested = true, want false for gone session")
	}
}

func TestRequestRestartAcceptsNoArgs(t *testing.T) {
	// Verify the cobra command accepts no args.
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeRequestRestartCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_CITY", "")
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("request-restart with no env should return non-zero")
	}
	if !strings.Contains(stderr.String(), "not in session context") {
		t.Errorf("stderr = %q, want 'not in session context' error", stderr.String())
	}
}

func TestRuntimeRequestRestartNamedOnDemandReturnsWithoutBlocking(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_NAME", "mayor")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	b, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{"gc:session"},
	})
	if err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if err := store.SetMetadata(b.ID, "session_name", "mayor"); err != nil {
		t.Fatalf("set session_name: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_session", "true"); err != nil {
		t.Fatalf("set configured_named_session: %v", err)
	}
	if err := store.SetMetadata(b.ID, "configured_named_mode", "on_demand"); err != nil {
		t.Fatalf("set configured_named_mode: %v", err)
	}
	if err := store.SetMetadata(b.ID, "restart_requested", "true"); err != nil {
		t.Fatalf("set restart_requested: %v", err)
	}
	if err := store.SetMetadata(b.ID, "continuation_reset_pending", "true"); err != nil {
		t.Fatalf("set continuation_reset_pending: %v", err)
	}

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cmdRuntimeRequestRestart(&stdout, &stderr)
	}()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cmdRuntimeRequestRestart blocked for named on-demand session")
	}
	if !strings.Contains(stdout.String(), "Restart skipped for named session") {
		t.Fatalf("stdout = %q, want restart skipped confirmation", stdout.String())
	}
	freshStore, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	refreshed, err := freshStore.Get(b.ID)
	if err != nil {
		t.Fatalf("fetching seeded bead: %v", err)
	}
	if refreshed.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared", refreshed.Metadata["restart_requested"])
	}
	if refreshed.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want cleared", refreshed.Metadata["continuation_reset_pending"])
	}
}

func TestProviderDrainOpsRestartRequestedRoundTrip(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "worker", runtime.Config{})
	dops := newDrainOps(sp)

	// Not requested initially.
	requested, _ := dops.isRestartRequested("worker")
	if requested {
		t.Error("should not be restart-requested initially")
	}

	// Set restart requested.
	if err := dops.setRestartRequested("worker"); err != nil {
		t.Fatalf("setRestartRequested: %v", err)
	}
	requested, _ = dops.isRestartRequested("worker")
	if !requested {
		t.Error("should be restart-requested after set")
	}

	// Clear restart requested.
	if err := dops.clearRestartRequested("worker"); err != nil {
		t.Fatalf("clearRestartRequested: %v", err)
	}
	requested, _ = dops.isRestartRequested("worker")
	if requested {
		t.Error("should not be restart-requested after clear")
	}
}

type removeMetaErrorProvider struct {
	*runtime.Fake
	err error
}

func (p *removeMetaErrorProvider) RemoveMeta(_, _ string) error {
	return p.err
}

func TestProviderDrainOpsClearRestartRequestedTreatsSessionGoneAsBenign(t *testing.T) {
	dops := newDrainOps(&removeMetaErrorProvider{
		Fake: runtime.NewFake(),
		err:  errors.New("no tmux server running"),
	})
	if err := dops.clearRestartRequested("worker"); err != nil {
		t.Fatalf("clearRestartRequested returned gone-session error: %v", err)
	}
}

func TestProviderDrainOpsClearRestartRequestedReturnsCleanupErrors(t *testing.T) {
	dops := newDrainOps(&removeMetaErrorProvider{
		Fake: runtime.NewFake(),
		err:  errors.New("permission denied"),
	})
	if err := dops.clearRestartRequested("worker"); err == nil {
		t.Fatal("clearRestartRequested should return non-gone cleanup errors")
	}
}

func TestProviderDrainOpsDriftRestartRoundTrip(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "worker", runtime.Config{})
	dops := newDrainOps(sp)

	// Not drift-restart initially.
	isDrift, _ := dops.isDriftRestart("worker")
	if isDrift {
		t.Error("should not be drift-restart initially")
	}

	// Set drift restart.
	if err := dops.setDriftRestart("worker"); err != nil {
		t.Fatalf("setDriftRestart: %v", err)
	}
	isDrift, _ = dops.isDriftRestart("worker")
	if !isDrift {
		t.Error("should be drift-restart after set")
	}

	// Clear drift restart.
	if err := dops.clearDriftRestart("worker"); err != nil {
		t.Fatalf("clearDriftRestart: %v", err)
	}
	isDrift, _ = dops.isDriftRestart("worker")
	if isDrift {
		t.Error("should not be drift-restart after clear")
	}
}

// ---------------------------------------------------------------------------
// newRuntimeDrainCheckCmd / newRuntimeDrainAckCmd arg acceptance tests
// ---------------------------------------------------------------------------

func TestDrainCheckAcceptsPositionalArg(t *testing.T) {
	// Verify cobra allows 0 or 1 positional arg (no longer NoArgs).
	// The command will fail at runtime (no city), but it should NOT fail
	// with "unknown command" or "accepts 0 arg(s)" errors.
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeDrainCheckCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"polecat"})
	err := cmd.Execute()
	// Expect a runtime error (no city dir), NOT an arg-count error.
	if err != nil {
		// errExit is expected — the command runs but fails to find a city.
		// What we're testing is that cobra didn't reject the arg.
		if stderr.String() != "" && strings.Contains(stderr.String(), "accepts 0 arg") {
			t.Errorf("drain-check should accept positional arg, got: %s", stderr.String())
		}
	}
}

func TestDrainCheckNoArgsStillWorks(t *testing.T) {
	// Without args and without env vars, drain-check returns exit 1 silently.
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeDrainCheckCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	// Ensure env vars are not set (in case test environment has them).
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_CITY", "")
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("drain-check with no args and no env should return non-zero")
	}
	// Should be silent — no error message on stderr.
	if stderr.Len() > 0 {
		t.Errorf("drain-check should be silent without env vars, got stderr: %q", stderr.String())
	}
}

func TestDrainAckAcceptsPositionalArg(t *testing.T) {
	// Same pattern as drain-check: verify cobra allows positional arg.
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeDrainAckCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	cmd.SetArgs([]string{"polecat"})
	err := cmd.Execute()
	if err != nil {
		if stderr.String() != "" && strings.Contains(stderr.String(), "accepts 0 arg") {
			t.Errorf("drain-ack should accept positional arg, got: %s", stderr.String())
		}
	}
}

func TestDrainAckNoArgsErrorMessage(t *testing.T) {
	// Without args and without env vars, drain-ack prints an error message.
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeDrainAckCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_CITY", "")
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("drain-ack with no args and no env should return non-zero")
	}
	if !strings.Contains(stderr.String(), "not in session context") {
		t.Errorf("stderr = %q, want 'not in session context' error", stderr.String())
	}
}

func TestDrainAckNoArgsFallsBackToCityPathEnv(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	cmd := newRuntimeDrainAckCmd(&stdout, &stderr)
	cmd.SilenceErrors = true
	cmd.SilenceUsage = true
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_ALIAS", "mayor")
	t.Setenv("GC_SESSION_ID", "gc-42")
	t.Setenv("GC_SESSION_NAME", "s-gc-42")
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", cityDir)

	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err != nil {
		t.Fatalf("drain-ack should succeed with GC_CITY_PATH fallback: %v; stderr=%q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Drain acknowledged") {
		t.Fatalf("stdout = %q, want drain acknowledgement", stdout.String())
	}
}

// ---------------------------------------------------------------------------
// resolveAgentIdentity / findAgentByQualified unit tests
// ---------------------------------------------------------------------------

func TestResolveAgentIdentity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: "echo 3"},
			{Name: "singleton", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1), ScaleCheck: "echo 1"},
		},
	}

	tests := []struct {
		name      string
		lookup    string
		wantFound bool
		wantName  string
		wantPool  bool
		wantLabel string
	}{
		// Exact match on non-pool agent.
		{"exact match", "mayor", true, "mayor", false, ""},

		// Exact match on pool agent (base name).
		{"pool base name", "worker", true, "worker", true, ""},

		// Pool instance matches.
		{"pool instance worker-1", "worker-1", true, "worker-1", true, "worker"},
		{"pool instance worker-5", "worker-5", true, "worker-5", true, "worker"},

		// Pool instance out of range (too high).
		{"pool instance worker-6", "worker-6", false, "", false, ""},

		// Pool instance out of range (zero).
		{"pool instance worker-0", "worker-0", false, "", false, ""},

		// Pool instance non-numeric suffix.
		{"pool instance worker-abc", "worker-abc", false, "", false, ""},

		// Pool instance negative (parsed as non-numeric due to dash).
		{"pool instance worker--1", "worker--1", false, "", false, ""},

		// Max=1 pool: the guard requires Max > 1, so {name}-1 does NOT match.
		{"singleton-1 no match", "singleton-1", false, "", false, ""},

		// Nonexistent agent.
		{"nonexistent", "nobody", false, "", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := resolveAgentIdentity(cfg, tt.lookup, "")
			if found != tt.wantFound {
				t.Fatalf("resolveAgentIdentity(%q) found = %v, want %v", tt.lookup, found, tt.wantFound)
			}
			if !found {
				return
			}
			if got.Name != tt.wantName {
				t.Errorf("agent.Name = %q, want %q", got.Name, tt.wantName)
			}
			gotIsMulti := got.SupportsInstanceExpansion()
			if gotIsMulti != tt.wantPool {
				t.Errorf("supportsInstanceExpansion = %v, want %v", gotIsMulti, tt.wantPool)
			}
			if got.PoolName != tt.wantLabel {
				t.Errorf("agent.PoolName = %q, want %q", got.PoolName, tt.wantLabel)
			}
		})
	}
}

func TestResolveAgentIdentityQualified(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "frontend"},
			{Name: "polecat", Dir: "backend"},
			{Name: "worker", Dir: "frontend", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 2"},
			{Name: "coder", Dir: "backend", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1)},
		},
	}

	tests := []struct {
		name       string
		input      string
		rigContext string
		wantFound  bool
		wantDir    string
		wantName   string
	}{
		// City-wide literal.
		{"city-wide literal", "mayor", "", true, "", "mayor"},
		// Qualified literal.
		{"qualified frontend/polecat", "frontend/polecat", "", true, "frontend", "polecat"},
		{"qualified backend/polecat", "backend/polecat", "", true, "backend", "polecat"},
		// Bare name with rig context.
		{"bare polecat + frontend context", "polecat", "frontend", true, "frontend", "polecat"},
		{"bare polecat + backend context", "polecat", "backend", true, "backend", "polecat"},
		// Bare name with no matching context — ambiguous (2 polecats), not found.
		{"bare polecat no context", "polecat", "", false, "", ""},
		// Pool instance with qualified name.
		{"qualified pool frontend/worker-2", "frontend/worker-2", "", true, "frontend", "worker-2"},
		// Pool instance with rig context.
		{"bare pool worker-1 + frontend context", "worker-1", "frontend", true, "frontend", "worker-1"},
		// Step 3: unambiguous bare name — worker is unique across all agents.
		{"unambiguous bare worker", "worker", "", true, "frontend", "worker"},
		// Step 3: unambiguous pool instance via bare name.
		{"unambiguous bare worker-2", "worker-2", "", true, "frontend", "worker-2"},
		// Unlimited pool: qualified lookup.
		{"qualified unlimited backend/coder-5", "backend/coder-5", "", true, "backend", "coder-5"},
		// Unlimited pool: unambiguous bare name.
		{"unambiguous bare coder-42", "coder-42", "", true, "backend", "coder-42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := resolveAgentIdentity(cfg, tt.input, tt.rigContext)
			if found != tt.wantFound {
				t.Fatalf("resolveAgentIdentity(%q, %q) found = %v, want %v", tt.input, tt.rigContext, found, tt.wantFound)
			}
			if !found {
				return
			}
			if got.Dir != tt.wantDir {
				t.Errorf("Dir = %q, want %q", got.Dir, tt.wantDir)
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if strings.Contains(tt.wantName, "-") && got.PoolName == "" {
				t.Errorf("PoolName = %q, want template label for pool instance", got.PoolName)
			}
		})
	}
}

func TestResolveAgentIdentityUnambiguous(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "myrig"},
			{Name: "builder", Dir: "frontend", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 2"},
			{Name: "coder", Dir: "backend", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1)},
		},
	}

	tests := []struct {
		name      string
		input     string
		wantFound bool
		wantDir   string
		wantName  string
	}{
		// Unique bare name resolves via step 3.
		{"unambiguous polecat", "polecat", true, "myrig", "polecat"},
		// Unique pool template resolves via step 3.
		{"unambiguous builder", "builder", true, "frontend", "builder"},
		// Pool instance, unambiguous.
		{"unambiguous builder-2", "builder-2", true, "frontend", "builder-2"},
		// Pool instance out of range.
		{"builder-4 out of range", "builder-4", false, "", ""},
		// City-wide agent resolves via step 1, before step 3.
		{"mayor step 1", "mayor", true, "", "mayor"},
		// Unlimited pool: bare name resolves.
		{"unlimited pool bare", "coder", true, "backend", "coder"},
		// Unlimited pool: any instance number resolves.
		{"unlimited pool instance", "coder-99", true, "backend", "coder-99"},
		// Qualified pool instance: "frontend/builder-2" resolves via step 1b.
		{"qualified pool instance", "frontend/builder-2", true, "frontend", "builder-2"},
		// Qualified pool instance out of range.
		{"qualified pool instance out of range", "frontend/builder-4", false, "", ""},
		// Qualified unlimited pool instance.
		{"qualified unlimited pool instance", "backend/coder-42", true, "backend", "coder-42"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := resolveAgentIdentity(cfg, tt.input, "")
			if found != tt.wantFound {
				t.Fatalf("resolveAgentIdentity(%q) found = %v, want %v", tt.input, found, tt.wantFound)
			}
			if !found {
				return
			}
			if got.Dir != tt.wantDir {
				t.Errorf("Dir = %q, want %q", got.Dir, tt.wantDir)
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if strings.Contains(tt.wantName, "-") && got.PoolName == "" {
				t.Errorf("PoolName = %q, want template label for pool instance", got.PoolName)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// findAgentByName unit tests (pool suffix stripping for gc prime)
// ---------------------------------------------------------------------------

func TestFindAgentByNameExact(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "polecat", Dir: "frontend"},
		},
	}
	a, ok := findAgentByName(cfg, "mayor")
	if !ok {
		t.Fatal("expected to find mayor")
	}
	if a.Name != "mayor" {
		t.Errorf("Name = %q, want %q", a.Name, "mayor")
	}
}

func TestFindAgentByNamePoolInstance(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "polecat", Dir: "frontend", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(5), ScaleCheck: "echo 3"},
		},
	}
	// "polecat-3" should strip suffix and match "polecat" pool.
	a, ok := findAgentByName(cfg, "polecat-3")
	if !ok {
		t.Fatal("expected to find polecat via pool suffix stripping")
	}
	if a.Name != "polecat" {
		t.Errorf("Name = %q, want %q", a.Name, "polecat")
	}
}

func TestFindAgentByNamePoolOutOfRange(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "polecat", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3), ScaleCheck: "echo 2"},
		},
	}
	// "polecat-4" is out of range (max=3).
	_, ok := findAgentByName(cfg, "polecat-4")
	if ok {
		t.Error("polecat-4 should not match pool with max=3")
	}
}

func TestFindAgentByNameSingletonPoolNoMatch(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "singleton", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1), ScaleCheck: "echo 1"},
		},
	}
	// Max=1 pools don't get instance suffixes.
	_, ok := findAgentByName(cfg, "singleton-1")
	if ok {
		t.Error("singleton-1 should not match pool with max=1")
	}
}

func TestFindAgentByNameUnlimitedPool(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "polecat", Dir: "myrig", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1)},
		},
	}
	// Any instance number should match an unlimited pool.
	a, ok := findAgentByName(cfg, "polecat-99")
	if !ok {
		t.Fatal("expected to find polecat-99 in unlimited pool")
	}
	if a.Name != "polecat" {
		t.Errorf("Name = %q, want %q", a.Name, "polecat")
	}
}

func TestFindAgentByNameNoMatch(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
		},
	}
	_, ok := findAgentByName(cfg, "nobody")
	if ok {
		t.Error("expected no match for nonexistent agent")
	}
}
