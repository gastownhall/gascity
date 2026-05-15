package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func intPtrNudge(n int) *int { return &n }

type providerMissNudgeProvider struct {
	*runtime.Fake
}

func (p *providerMissNudgeProvider) Nudge(name string, content []runtime.ContentBlock) error {
	_ = p.Fake.Nudge(name, content)
	return fmt.Errorf("%w: provider does not own %q", runtime.ErrSessionNotFound, name)
}

func (p *providerMissNudgeProvider) NudgeNow(name string, content []runtime.ContentBlock) error {
	_ = p.Fake.NudgeNow(name, content)
	return fmt.Errorf("%w: provider does not own %q", runtime.ErrSessionNotFound, name)
}

type missingNudgeBeadStore struct {
	*beads.MemStore
	missingID string
}

func (s *missingNudgeBeadStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.missingID {
		return fmt.Errorf("setting metadata on %q: exit status 1: Error resolving %s: no issue found matching %q", id, id, id)
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *missingNudgeBeadStore) Close(id string) error {
	if id == s.missingID {
		return fmt.Errorf("closing bead %q: exit status 1: Error resolving %s: no issue found matching %q", id, id, id)
	}
	return s.MemStore.Close(id)
}

type ambiguousNudgeBeadStore struct {
	*beads.MemStore
	ambiguousID string
}

func (s *ambiguousNudgeBeadStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.ambiguousID {
		return fmt.Errorf("setting metadata on %q: exit status 1: Error resolving %s: ambiguous ID %q matches 86 issues: [gc-170 gc-171 gc-172 ...]\nUse more characters to disambiguate", id, id, id)
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *ambiguousNudgeBeadStore) Close(id string) error {
	if id == s.ambiguousID {
		return fmt.Errorf("closing bead %q: exit status 1: Error resolving %s: ambiguous ID %q matches 86 issues: [gc-170 gc-171 gc-172 ...]\nUse more characters to disambiguate", id, id, id)
	}
	return s.MemStore.Close(id)
}

type unrelatedNotFoundNudgeBeadStore struct {
	*beads.MemStore
	errorID string
}

func (s *unrelatedNotFoundNudgeBeadStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.errorID {
		return fmt.Errorf("setting metadata on %q: backend path not found", id)
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

type rollbackCloseFailStore struct {
	*beads.MemStore
	closeErr error
}

func (s *rollbackCloseFailStore) Close(string) error {
	return s.closeErr
}

func TestMarkQueuedNudgeTerminalFallsBackWhenStoredBeadIDEmpty(t *testing.T) {
	store := beads.NewMemStore()
	item := queuedNudge{
		ID:        "nudge-empty-bead",
		Agent:     "wendy.wendy",
		SessionID: "mc-ayq6xi",
		Source:    "session",
		Message:   "follow up",
		CreatedAt: time.Now().Add(-time.Minute).UTC(),
	}
	createdID, created, err := ensureQueuedNudgeBead(store, item)
	if err != nil {
		t.Fatalf("ensureQueuedNudgeBead: %v", err)
	}
	if !created {
		t.Fatal("expected ensureQueuedNudgeBead to create a backing nudge bead")
	}

	now := time.Now().UTC()
	item.LastError = "expired"
	if err := markQueuedNudgeTerminal(store, item, "expired", "expired", "", now); err != nil {
		t.Fatalf("markQueuedNudgeTerminal: %v", err)
	}

	bead, err := store.Get(createdID)
	if err != nil {
		t.Fatalf("Get(%q): %v", createdID, err)
	}
	if bead.Status != "closed" {
		t.Fatalf("bead.Status = %q, want closed", bead.Status)
	}
	if bead.Metadata["state"] != "expired" {
		t.Fatalf("state = %q, want expired", bead.Metadata["state"])
	}
}

func TestMarkQueuedNudgeTerminalFallsBackFromMissingStoredBeadID(t *testing.T) {
	store := &missingNudgeBeadStore{MemStore: beads.NewMemStore(), missingID: "gc-458"}
	item := queuedNudge{
		ID:        "nudge-stale",
		Agent:     "wendy.wendy",
		SessionID: "mc-ayq6xi",
		Source:    "session",
		Message:   "follow up",
		BeadID:    "gc-458",
		CreatedAt: time.Now().Add(-time.Minute).UTC(),
	}
	createdID, created, err := ensureQueuedNudgeBead(store, item)
	if err != nil {
		t.Fatalf("ensureQueuedNudgeBead: %v", err)
	}
	if !created {
		t.Fatal("expected ensureQueuedNudgeBead to create a backing nudge bead")
	}

	now := time.Now().UTC()
	item.LastError = "expired"
	if err := markQueuedNudgeTerminal(store, item, "expired", "expired", "", now); err != nil {
		t.Fatalf("markQueuedNudgeTerminal: %v", err)
	}

	bead, err := store.Get(createdID)
	if err != nil {
		t.Fatalf("Get(%q): %v", createdID, err)
	}
	if bead.Status != "closed" {
		t.Fatalf("bead.Status = %q, want closed", bead.Status)
	}
	if bead.Metadata["state"] != "expired" {
		t.Fatalf("state = %q, want expired", bead.Metadata["state"])
	}
	if bead.Metadata["terminal_reason"] != "expired" {
		t.Fatalf("terminal_reason = %q, want expired", bead.Metadata["terminal_reason"])
	}
}

func TestMarkQueuedNudgeTerminalReturnsUnrelatedNotFoundErrors(t *testing.T) {
	store := &unrelatedNotFoundNudgeBeadStore{MemStore: beads.NewMemStore()}
	item := queuedNudge{
		ID:        "nudge-terminal-error",
		Agent:     "wendy.wendy",
		SessionID: "mc-ayq6xi",
		Source:    "session",
		Message:   "follow up",
		CreatedAt: time.Now().Add(-time.Minute).UTC(),
	}
	createdID, _, err := ensureQueuedNudgeBead(store, item)
	if err != nil {
		t.Fatalf("ensureQueuedNudgeBead: %v", err)
	}
	store.errorID = createdID
	item.BeadID = createdID

	err = markQueuedNudgeTerminal(store, item, "expired", "expired", "", time.Now().UTC())
	if err == nil {
		t.Fatal("markQueuedNudgeTerminal returned nil, want unrelated not found error")
	}
	if !strings.Contains(err.Error(), "backend path not found") {
		t.Fatalf("markQueuedNudgeTerminal error = %v, want backend path not found", err)
	}
}

func TestPruneExpiredQueuedNudgesIgnoresMissingTerminalBead(t *testing.T) {
	store := &missingNudgeBeadStore{MemStore: beads.NewMemStore(), missingID: "gc-458"}
	now := time.Now().UTC()
	state := &nudgeQueueState{
		Pending: []queuedNudge{
			{
				ID:           "nudge-stale",
				BeadID:       "gc-458",
				Agent:        "wendy.wendy",
				SessionID:    "mc-ayq6xi",
				Source:       "session",
				Message:      "follow up",
				CreatedAt:    now.Add(-2 * time.Hour),
				DeliverAfter: now.Add(-2 * time.Hour),
				ExpiresAt:    now.Add(-time.Hour),
			},
		},
	}

	if err := pruneExpiredQueuedNudges(state, store, now); err != nil {
		t.Fatalf("pruneExpiredQueuedNudges: %v", err)
	}
	if len(state.Pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(state.Pending))
	}
	if len(state.Dead) != 1 {
		t.Fatalf("dead = %d, want 1", len(state.Dead))
	}
	if state.Dead[0].LastError != "expired" {
		t.Fatalf("dead[0].LastError = %q, want expired", state.Dead[0].LastError)
	}
}

func TestMarkQueuedNudgeTerminalHandlesAmbiguousBeadID(t *testing.T) {
	store := &ambiguousNudgeBeadStore{MemStore: beads.NewMemStore(), ambiguousID: "gc-17"}
	item := queuedNudge{
		ID:        "nudge-ambiguous",
		Agent:     "wendy.wendy",
		SessionID: "mc-ayq6xi",
		Source:    "session",
		Message:   "follow up",
		BeadID:    "gc-17",
		CreatedAt: time.Now().Add(-time.Minute).UTC(),
	}
	createdID, created, err := ensureQueuedNudgeBead(store, item)
	if err != nil {
		t.Fatalf("ensureQueuedNudgeBead: %v", err)
	}
	if !created {
		t.Fatal("expected ensureQueuedNudgeBead to create a backing nudge bead")
	}

	now := time.Now().UTC()
	item.LastError = "expired"
	if err := markQueuedNudgeTerminal(store, item, "expired", "expired", "", now); err != nil {
		t.Fatalf("markQueuedNudgeTerminal with ambiguous BeadID: %v", err)
	}

	bead, err := store.Get(createdID)
	if err != nil {
		t.Fatalf("Get(%q): %v", createdID, err)
	}
	if bead.Status != "closed" {
		t.Fatalf("bead.Status = %q, want closed", bead.Status)
	}
	if bead.Metadata["state"] != "expired" {
		t.Fatalf("state = %q, want expired", bead.Metadata["state"])
	}
}

func TestPruneExpiredQueuedNudgesWithAmbiguousBeadIDContinues(t *testing.T) {
	// Regression: stale entries with short bead IDs (e.g. "gc-17") that match many
	// beads in a large store used to abort the entire nudge processing loop.
	store := &ambiguousNudgeBeadStore{MemStore: beads.NewMemStore(), ambiguousID: "gc-17"}
	now := time.Now().UTC()
	state := &nudgeQueueState{
		Pending: []queuedNudge{
			{
				ID:           "nudge-ambiguous",
				BeadID:       "gc-17",
				Agent:        "gc-ub35o",
				SessionID:    "gc-ub35o",
				Source:       "session",
				Message:      "Run gc hook",
				CreatedAt:    now.Add(-8 * 24 * time.Hour),
				DeliverAfter: now.Add(-8 * 24 * time.Hour),
				ExpiresAt:    now.Add(-7 * 24 * time.Hour),
			},
		},
	}

	if err := pruneExpiredQueuedNudges(state, store, now); err != nil {
		t.Fatalf("pruneExpiredQueuedNudges: %v", err)
	}
	if len(state.Pending) != 0 {
		t.Fatalf("pending = %d, want 0 (stale entry must be pruned)", len(state.Pending))
	}
	if len(state.Dead) != 1 {
		t.Fatalf("dead = %d, want 1", len(state.Dead))
	}
}

func TestDeliverSessionNudgeWithProviderWaitIdleQueuesForCodex(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: "sess-worker",
	}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Queued nudge for worker") {
		t.Fatalf("stdout = %q, want queued confirmation", stdout.String())
	}
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			t.Fatalf("unexpected direct nudge call: %+v", call)
		}
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if len(inFlight) != 0 {
		t.Fatalf("inFlight = %d, want 0", len(inFlight))
	}
	if len(dead) != 0 {
		t.Fatalf("dead = %d, want 0", len(dead))
	}
	if pending[0].Source != "session" {
		t.Fatalf("source = %q, want session", pending[0].Source)
	}
}

func TestDeliverSessionNudgeWithWorkerImmediateResumesSuspendedSession(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	info, err := mgr.Create(context.Background(), "worker", "Worker", "claude", dir, "claude", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	target := nudgeTarget{
		cityPath:    dir,
		sessionID:   info.ID,
		sessionName: info.SessionName,
	}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithWorker(target, store, fake, "check deploy status", nudgeDeliveryImmediate, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithWorker = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Nudged "+info.ID) {
		t.Fatalf("stdout = %q, want nudge confirmation", stdout.String())
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != session.StateActive {
		t.Fatalf("state = %q, want %q", got.State, session.StateActive)
	}

	var sawStart, sawNudgeNow bool
	for _, call := range fake.Calls {
		if call.Method == "Start" && call.Name == info.SessionName {
			sawStart = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName && call.Message == "check deploy status" {
			sawNudgeNow = true
		}
	}
	if !sawStart || !sawNudgeNow {
		t.Fatalf("calls = %#v, want resumed Start and immediate nudge", fake.Calls)
	}
}

func TestDeliverSessionNudgeWithWorkerWaitIdleResumesClaudeSession(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	info, err := mgr.Create(context.Background(), "worker", "Worker", "claude", dir, "claude", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	fake.WaitForIdleErrors[info.SessionName] = nil

	target := nudgeTarget{
		cityPath:    dir,
		sessionID:   info.ID,
		sessionName: info.SessionName,
	}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithWorker(target, store, fake, "check deploy status", nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithWorker = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Nudged "+info.ID) {
		t.Fatalf("stdout = %q, want nudge confirmation", stdout.String())
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != session.StateActive {
		t.Fatalf("state = %q, want %q", got.State, session.StateActive)
	}

	var sawWait bool
	delivered := ""
	for _, call := range fake.Calls {
		if call.Method == "WaitForIdle" && call.Name == info.SessionName {
			sawWait = true
		}
		if call.Method == "NudgeNow" && call.Name == info.SessionName {
			delivered = call.Message
		}
	}
	if !sawWait {
		t.Fatalf("calls = %#v, want WaitForIdle", fake.Calls)
	}
	if !strings.Contains(delivered, "<system-reminder>") {
		t.Fatalf("delivered message = %q, want system-reminder wrapper", delivered)
	}
}

func TestDeliverSessionNudgeWithWorkerWaitIdleQueuesUnsupportedProviderAfterResume(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	info, err := mgr.Create(context.Background(), "worker", "Worker", "codex", dir, "codex", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	target := nudgeTarget{
		cityPath:    dir,
		sessionID:   info.ID,
		sessionName: info.SessionName,
	}
	called := false
	prev := startNudgePoller
	startNudgePoller = func(cityPath, agentName, sessionName string) error {
		called = true
		if cityPath != dir || agentName != info.ID || sessionName != info.SessionName {
			t.Fatalf("unexpected poller args city=%q agent=%q session=%q", cityPath, agentName, sessionName)
		}
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithWorker(target, store, fake, "check deploy status", nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithWorker = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Queued nudge for "+info.ID) {
		t.Fatalf("stdout = %q, want queued confirmation", stdout.String())
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.State != session.StateActive {
		t.Fatalf("state = %q, want %q", got.State, session.StateActive)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, info.ID, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
}

func TestDeliverSessionNudgeWithProviderWaitIdleStartsCodexPollerWhenQueued(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: "sess-worker",
	}

	called := false
	prev := startNudgePoller
	startNudgePoller = func(cityPath, agentName, sessionName string) error {
		called = true
		if cityPath != dir || agentName != "worker" || sessionName != "sess-worker" {
			t.Fatalf("unexpected poller args city=%q agent=%q session=%q", cityPath, agentName, sessionName)
		}
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
	}
}

func TestDeliverSessionNudgeWithProviderWaitIdleStartsClaudePollerWhenQueued(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors["sess-worker"] = runtime.ErrInteractionUnsupported

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: "sess-worker",
	}

	called := false
	prev := startNudgePoller
	startNudgePoller = func(cityPath, agentName, sessionName string) error {
		called = true
		if cityPath != dir || agentName != "worker" || sessionName != "sess-worker" {
			t.Fatalf("unexpected poller args city=%q agent=%q session=%q", cityPath, agentName, sessionName)
		}
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
	}
	if !strings.Contains(stdout.String(), "Queued nudge for worker") {
		t.Fatalf("stdout = %q, want queued confirmation", stdout.String())
	}
}

func TestPollerSessionIdleEnoughUsesSuppliedLastActivity(t *testing.T) {
	target := nudgeTarget{sessionName: "sess-worker"}
	last := time.Now().Add(-5 * time.Second)
	obs := worker.LiveObservation{LastActivity: &last}

	if !pollerSessionIdleEnough(target, nil, 3*time.Second, obs) {
		t.Fatal("pollerSessionIdleEnough = false, want true when supplied last activity is old enough")
	}

	recent := time.Now().Add(-1 * time.Second)
	obs.LastActivity = &recent
	if pollerSessionIdleEnough(target, nil, 3*time.Second, obs) {
		t.Fatal("pollerSessionIdleEnough = true, want false when supplied last activity is too recent")
	}
}

func TestPollerSessionIdleEnoughFallsBackToIdleWaitWhenActivityUnavailable(t *testing.T) {
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors["sess-worker"] = nil
	target := nudgeTarget{sessionName: "sess-worker"}
	obs := worker.LiveObservation{}

	if !pollerSessionIdleEnough(target, fake, 3*time.Second, obs) {
		t.Fatal("pollerSessionIdleEnough = false, want idle wait fallback to allow delivery")
	}

	var sawWait bool
	for _, call := range fake.Calls {
		if call.Method == "WaitForIdle" && call.Name == "sess-worker" {
			sawWait = true
			break
		}
	}
	if !sawWait {
		t.Fatalf("calls = %#v, want WaitForIdle fallback", fake.Calls)
	}

	fake.WaitForIdleErrors["sess-worker"] = errors.New("timed out waiting for idle")
	if pollerSessionIdleEnough(target, fake, 3*time.Second, obs) {
		t.Fatal("pollerSessionIdleEnough = true, want idle wait error to suppress delivery")
	}
}

func TestShouldKeepNudgePollerAliveDuringStartupGrace(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now()
	item := newQueuedNudgeWithOptions("worker", "queued follow-up", "session", now.Add(-time.Minute), queuedNudgeOptions{
		ID:        "n-grace",
		SessionID: "gc-1",
	})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	target := nudgeTarget{
		cityPath:  dir,
		agent:     config.Agent{Name: "worker"},
		sessionID: "gc-1",
	}

	if !shouldKeepNudgePollerAlive(target, time.Time{}, now) {
		t.Fatal("shouldKeepNudgePollerAlive = false, want true on first missing-session check with queued items")
	}
	if !shouldKeepNudgePollerAlive(target, now.Add(-defaultNudgePollStartGrace/2), now) {
		t.Fatal("shouldKeepNudgePollerAlive = false, want true within startup grace")
	}
	if shouldKeepNudgePollerAlive(target, now.Add(-defaultNudgePollStartGrace-time.Second), now) {
		t.Fatal("shouldKeepNudgePollerAlive = true, want false after startup grace expires")
	}
}

func TestDeliverSessionNudgeWithProviderImmediateUsesImmediateNudge(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: "sess-worker",
	}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryImmediate, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 0; stderr: %s", code, stderr.String())
	}

	immediateCalls := 0
	for _, call := range fake.Calls {
		if call.Method == "NudgeNow" {
			immediateCalls++
		}
		if call.Method == "Nudge" {
			t.Fatalf("unexpected regular nudge call: %+v", call)
		}
	}
	if immediateCalls != 1 {
		t.Fatalf("immediate nudge calls = %d, want 1", immediateCalls)
	}
	if !strings.Contains(stdout.String(), "Nudged worker") {
		t.Fatalf("stdout = %q, want immediate nudge confirmation", stdout.String())
	}
}

func TestDeliverSessionNudgeWithProviderWaitIdleWrapsDirectDeliveryInSystemReminder(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors["sess-worker"] = nil

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: "sess-worker",
	}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 0; stderr: %s", code, stderr.String())
	}

	var waitCalls, nudgeNowCalls int
	var delivered string
	for _, call := range fake.Calls {
		switch call.Method {
		case "WaitForIdle":
			waitCalls++
		case "NudgeNow":
			nudgeNowCalls++
			delivered = call.Message
		}
	}
	if waitCalls != 1 {
		t.Fatalf("wait-idle calls = %d, want 1", waitCalls)
	}
	if nudgeNowCalls != 1 {
		t.Fatalf("immediate nudge calls = %d, want 1", nudgeNowCalls)
	}
	if !strings.Contains(delivered, "<system-reminder>") {
		t.Fatalf("delivered message = %q, want system-reminder wrapper", delivered)
	}
	if !strings.Contains(delivered, "[session] check deploy status") {
		t.Fatalf("delivered message = %q, want session reminder content", delivered)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending=%d inFlight=%d dead=%d, want all zero", len(pending), len(inFlight), len(dead))
	}
}

func TestDeliverSessionNudgeWithProviderWaitIdleLeavesACPDeliveryUnwrapped(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	target := nudgeTarget{
		cityPath:    dir,
		transport:   "acp",
		agent:       config.Agent{Name: "worker", Session: "acp"},
		sessionName: "sess-worker",
	}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 0; stderr: %s", code, stderr.String())
	}

	var delivered string
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			delivered = call.Message
		}
		if call.Method == "WaitForIdle" {
			t.Fatalf("unexpected wait-idle call for acp target: %+v", call)
		}
	}
	if delivered != "check deploy status" {
		t.Fatalf("delivered message = %q, want raw ACP nudge", delivered)
	}
}

func TestDeliverSessionNudgeWithProviderWaitIdleQueuesACPProviderMiss(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := &providerMissNudgeProvider{Fake: runtime.NewFake()}
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	target := nudgeTarget{
		cityPath:    dir,
		transport:   "acp",
		agent:       config.Agent{Name: "worker", Session: "acp"},
		sessionName: "sess-worker",
	}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Queued nudge for worker") {
		t.Fatalf("stdout = %q, want queued confirmation", stdout.String())
	}

	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
}

func TestDeliverSessionNudgeWithProviderImmediateExplainsACPProviderMiss(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := &providerMissNudgeProvider{Fake: runtime.NewFake()}
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	target := nudgeTarget{
		cityPath:    dir,
		transport:   "acp",
		agent:       config.Agent{Name: "worker", Session: "acp"},
		sessionName: "sess-worker",
	}

	var stdout, stderr bytes.Buffer
	code := deliverSessionNudgeWithProvider(target, fake, nudgeDeliveryImmediate, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if !strings.Contains(stderr.String(), "does not own the ACP connection") {
		t.Fatalf("stderr = %q, want ACP ownership guidance", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--delivery=wait-idle") {
		t.Fatalf("stderr = %q, want wait-idle guidance", stderr.String())
	}

	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 0/0/0", len(pending), len(inFlight), len(dead))
	}
}

func TestSendMailNotifyWithProviderQueuesWhenSessionSleeping(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "mayor", MaxActiveSessions: intPtrNudge(1)},
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: "sess-mayor",
	}

	if err := sendMailNotifyWithProvider(target, runtime.NewFake()); err != nil {
		t.Fatalf("sendMailNotifyWithProvider: %v", err)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, target.agentKey(), time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if len(inFlight) != 0 {
		t.Fatalf("inFlight = %d, want 0", len(inFlight))
	}
	if len(dead) != 0 {
		t.Fatalf("dead = %d, want 0", len(dead))
	}
	if pending[0].Source != "mail" {
		t.Fatalf("source = %q, want mail", pending[0].Source)
	}
	if !strings.Contains(pending[0].Message, "You have mail from human") {
		t.Fatalf("message = %q, want mail reminder", pending[0].Message)
	}
}

func TestSendMailNotifyWithProviderStartsCodexPollerWhenQueueingRunningSession(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-mayor", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "mayor", MaxActiveSessions: intPtrNudge(1)},
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: "sess-mayor",
	}

	called := false
	prev := startNudgePoller
	startNudgePoller = func(cityPath, agentName, sessionName string) error {
		called = true
		if cityPath != dir || agentName != "mayor" || sessionName != "sess-mayor" {
			t.Fatalf("unexpected poller args city=%q agent=%q session=%q", cityPath, agentName, sessionName)
		}
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	if err := sendMailNotifyWithProvider(target, fake); err != nil {
		t.Fatalf("sendMailNotifyWithProvider: %v", err)
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
	}
}

func TestSendMailNotifyWithProviderStartsClaudePollerWhenQueueingRunningSession(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-mayor", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors["sess-mayor"] = runtime.ErrInteractionUnsupported
	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "mayor", MaxActiveSessions: intPtrNudge(1)},
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: "sess-mayor",
	}

	called := false
	prev := startNudgePoller
	startNudgePoller = func(cityPath, agentName, sessionName string) error {
		called = true
		if cityPath != dir || agentName != "mayor" || sessionName != "sess-mayor" {
			t.Fatalf("unexpected poller args city=%q agent=%q session=%q", cityPath, agentName, sessionName)
		}
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	if err := sendMailNotifyWithProvider(target, fake); err != nil {
		t.Fatalf("sendMailNotifyWithProvider: %v", err)
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
	}
}

func TestSendMailNotifyWithWorkerStartsPollerBySessionIDForAliasedTarget(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.Create(context.Background(), "mayor", "Mayor", "codex", dir, "codex", nil, session.ProviderResume{}, runtime.Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := store.SetMetadata(info.ID, "alias", "mayor"); err != nil {
		t.Fatalf("SetMetadata(alias): %v", err)
	}
	target := nudgeTarget{
		cityPath:    dir,
		alias:       "mayor",
		agent:       config.Agent{Name: "mayor", MaxActiveSessions: intPtrNudge(1)},
		sessionID:   info.ID,
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: info.SessionName,
	}

	called := false
	prev := startNudgePoller
	startNudgePoller = func(cityPath, agentName, sessionName string) error {
		called = true
		if cityPath != dir || agentName != info.ID || sessionName != info.SessionName {
			t.Fatalf("unexpected poller args city=%q agent=%q session=%q", cityPath, agentName, sessionName)
		}
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })

	if err := sendMailNotifyWithWorker(target, store, fake, "human"); err != nil {
		t.Fatalf("sendMailNotifyWithWorker: %v", err)
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
	}

	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
	if pending[0].Agent != "mayor" || pending[0].SessionID != info.ID {
		t.Fatalf("queued nudge agent/session = %q/%q, want mayor/%s", pending[0].Agent, pending[0].SessionID, info.ID)
	}
}

func TestSendMailNotifyWithProviderWaitIdleWrapsDirectDeliveryInSystemReminder(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-mayor", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors["sess-mayor"] = nil

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "mayor", MaxActiveSessions: intPtrNudge(1)},
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: "sess-mayor",
	}

	if err := sendMailNotifyWithProvider(target, fake); err != nil {
		t.Fatalf("sendMailNotifyWithProvider: %v", err)
	}

	var waitCalls, nudgeNowCalls int
	var delivered string
	for _, call := range fake.Calls {
		switch call.Method {
		case "WaitForIdle":
			waitCalls++
		case "NudgeNow":
			nudgeNowCalls++
			delivered = call.Message
		}
	}
	if waitCalls != 1 {
		t.Fatalf("wait-idle calls = %d, want 1", waitCalls)
	}
	if nudgeNowCalls != 1 {
		t.Fatalf("immediate nudge calls = %d, want 1", nudgeNowCalls)
	}
	if !strings.Contains(delivered, "<system-reminder>") {
		t.Fatalf("delivered message = %q, want system-reminder wrapper", delivered)
	}
	if !strings.Contains(delivered, "[mail] You have mail from human") {
		t.Fatalf("delivered message = %q, want mail reminder content", delivered)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, target.agentKey(), time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending=%d inFlight=%d dead=%d, want all zero", len(pending), len(inFlight), len(dead))
	}
}

func TestSendMailNotifyWithWorkerWaitIdlePreservesMailSource(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	info, err := mgr.Create(context.Background(), "mayor", "Mayor", "claude", dir, "claude", nil, session.ProviderResume{}, runtime.Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors[info.SessionName] = nil

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "mayor"},
		sessionID:   info.ID,
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: info.SessionName,
	}

	if err := sendMailNotifyWithWorker(target, store, fake, "human"); err != nil {
		t.Fatalf("sendMailNotifyWithWorker: %v", err)
	}

	var delivered string
	for _, call := range fake.Calls {
		if call.Method == "NudgeNow" {
			delivered = call.Message
		}
	}
	if !strings.Contains(delivered, "<system-reminder>") {
		t.Fatalf("delivered message = %q, want system-reminder wrapper", delivered)
	}
	if !strings.Contains(delivered, "[mail] You have mail from human") {
		t.Fatalf("delivered message = %q, want mail-tagged reminder content", delivered)
	}
	assertSessionLastNudgeDeliveredAtStamped(t, store, info.ID)
}

func TestSendMailNotifyWithWorkerQueuesWhenRuntimeIsGone(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	info, err := mgr.Create(context.Background(), "mayor", "Mayor", "claude", dir, "claude", nil, session.ProviderResume{}, runtime.Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := fake.Stop(info.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "mayor"},
		sessionID:   info.ID,
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionName: info.SessionName,
	}

	startCalls := len(fake.Calls)
	if err := sendMailNotifyWithWorker(target, store, fake, "human"); err != nil {
		t.Fatalf("sendMailNotifyWithWorker: %v", err)
	}
	for _, call := range fake.Calls[startCalls:] {
		if call.Method == "Start" || call.Method == "WaitForIdle" || call.Method == "Nudge" || call.Method == "NudgeNow" {
			t.Fatalf("calls = %#v, want queue fallback without waking dead runtime", fake.Calls[startCalls:])
		}
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, target.agentKey(), time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
}

func TestSendMailNotifyWithWorkerQueuesWhenDirectProviderMisses(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")

	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := &providerMissNudgeProvider{Fake: runtime.NewFake()}
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)

	info, err := mgr.Create(context.Background(), "worker", "Worker", "codex", dir, "codex", nil, session.ProviderResume{}, runtime.Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := store.SetMetadata(info.ID, "transport", "acp"); err != nil {
		t.Fatalf("SetMetadata(transport): %v", err)
	}

	target := nudgeTarget{
		cityPath:    dir,
		transport:   "acp",
		agent:       config.Agent{Name: "worker", Session: "acp"},
		sessionID:   info.ID,
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: info.SessionName,
	}

	if err := sendMailNotifyWithWorker(target, store, fake, "human"); err != nil {
		t.Fatalf("sendMailNotifyWithWorker: %v", err)
	}

	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
	if pending[0].SessionID != info.ID {
		t.Fatalf("queued nudge session_id = %q, want %q", pending[0].SessionID, info.ID)
	}
}

func TestResolveNudgeTarget_MaterializesNamedSessionFromAlias(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"

[[agent]]
name = "witness"
dir = "myrig"
provider = "codex"
start_command = "echo"

[[named_session]]
template = "witness"
dir = "myrig"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	runtimeName := config.NamedSessionRuntimeName(cfg.Workspace.Name, cfg.Workspace, "myrig/witness")
	t.Setenv("GC_CITY", cityDir)

	target, err := resolveNudgeTarget("myrig/witness")
	if err != nil {
		t.Fatalf("resolveNudgeTarget(alias): %v", err)
	}
	if target.alias != "myrig/witness" {
		t.Fatalf("alias = %q, want myrig/witness", target.alias)
	}
	if target.agent.QualifiedName() != "myrig/witness" {
		t.Fatalf("agent = %q, want myrig/witness", target.agent.QualifiedName())
	}
	if target.sessionName == "" {
		t.Fatal("sessionName should be populated for configured singleton alias")
	}

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionID, err := resolveSessionID(store, target.sessionName)
	if err != nil {
		t.Fatalf("resolveSessionID(created canonical): %v", err)
	}
	if err := store.SetMetadata(sessionID, "continuation_epoch", "epoch-7"); err != nil {
		t.Fatalf("SetMetadata(continuation_epoch): %v", err)
	}

	target, err = resolveNudgeTarget(runtimeName)
	if err != nil {
		t.Fatalf("resolveNudgeTarget(runtime name): %v", err)
	}
	if target.sessionID != sessionID {
		t.Fatalf("sessionID = %q, want %q", target.sessionID, sessionID)
	}
	if target.continuationEpoch != "epoch-7" {
		t.Fatalf("continuationEpoch = %q, want epoch-7", target.continuationEpoch)
	}
}

func TestTryDeliverQueuedNudgesByPollerDeliversAndAcks(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now().Add(-1 * time.Minute)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "review the deploy logs", "session", now)); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.Create(context.Background(), "worker", "Worker", "codex", dir, "codex", nil, session.ProviderResume{}, runtime.Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{info.SessionName: idleSince}

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		sessionID:   info.ID,
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: info.SessionName,
	}
	obs := worker.LiveObservation{Running: true, LastActivity: &idleSince}

	delivered, err := tryDeliverQueuedNudgesByPoller(target, store, fake, 3*time.Second, obs)
	if err != nil {
		t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
	}
	if !delivered {
		t.Fatal("delivered = false, want true")
	}

	var nudgeCalls []runtime.Call
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			nudgeCalls = append(nudgeCalls, call)
		}
	}
	if len(nudgeCalls) != 1 {
		t.Fatalf("nudge calls = %d, want 1", len(nudgeCalls))
	}
	if !strings.Contains(nudgeCalls[0].Message, "<system-reminder>") {
		t.Fatalf("nudge message = %q, want system-reminder wrapper", nudgeCalls[0].Message)
	}
	if !strings.Contains(nudgeCalls[0].Message, "review the deploy logs") {
		t.Fatalf("nudge message = %q, want original reminder", nudgeCalls[0].Message)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(pending))
	}
	if len(inFlight) != 0 {
		t.Fatalf("inFlight = %d, want 0", len(inFlight))
	}
	if len(dead) != 0 {
		t.Fatalf("dead = %d, want 0", len(dead))
	}
}

func TestTryDeliverQueuedNudgesByPollerLeavesACPDeliveryUnwrapped(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now().Add(-1 * time.Minute)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "check hook output", "session", now)); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{"sess-worker": idleSince}

	target := nudgeTarget{
		cityPath:    dir,
		transport:   "acp",
		agent:       config.Agent{Name: "worker", Session: "acp"},
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: "sess-worker",
	}
	obs := worker.LiveObservation{Running: true, LastActivity: &idleSince}

	delivered, err := tryDeliverQueuedNudgesByPoller(target, openNudgeBeadStore(dir), fake, 3*time.Second, obs)
	if err != nil {
		t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
	}
	if !delivered {
		t.Fatal("delivered = false, want true")
	}

	var nudgeCalls []runtime.Call
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			nudgeCalls = append(nudgeCalls, call)
		}
	}
	if len(nudgeCalls) != 1 {
		t.Fatalf("nudge calls = %d, want 1", len(nudgeCalls))
	}
	if strings.Contains(nudgeCalls[0].Message, "<system-reminder>") {
		t.Fatalf("ACP nudge message = %q, want plain text without system-reminder wrapper", nudgeCalls[0].Message)
	}
	if !strings.Contains(nudgeCalls[0].Message, "check hook output") {
		t.Fatalf("nudge message = %q, want original reminder", nudgeCalls[0].Message)
	}
}

func TestTryDeliverQueuedNudgesByPollerKeepsACPProviderMissRecoverable(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now().Add(-1 * time.Minute)
	if err := enqueueQueuedNudge(dir, newQueuedNudge("worker", "check hook output", "session", now)); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	fake := &providerMissNudgeProvider{Fake: runtime.NewFake()}
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{"sess-worker": idleSince}

	target := nudgeTarget{
		cityPath:    dir,
		transport:   "acp",
		agent:       config.Agent{Name: "worker", Session: "acp"},
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: "sess-worker",
	}
	obs := worker.LiveObservation{Running: true, LastActivity: &idleSince}

	for i := 0; i < defaultQueuedNudgeMaxAttempts; i++ {
		delivered, err := tryDeliverQueuedNudgesByPoller(target, openNudgeBeadStore(dir), fake, 3*time.Second, obs)
		if err != nil {
			t.Fatalf("tryDeliverQueuedNudgesByPoller tick %d: %v", i+1, err)
		}
		if delivered {
			t.Fatalf("delivered = true on tick %d, want provider miss to leave item recoverable", i+1)
		}
	}

	var nudgeCalls int
	for _, call := range fake.Calls {
		if call.Method == "Nudge" {
			nudgeCalls++
		}
	}
	if nudgeCalls != defaultQueuedNudgeMaxAttempts {
		t.Fatalf("nudge calls = %d, want %d recoverable attempts", nudgeCalls, defaultQueuedNudgeMaxAttempts)
	}

	pending, inFlight, dead, err := listQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudgesForTarget: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
	if pending[0].Attempts != 0 {
		t.Fatalf("attempts = %d, want 0 for transient ACP provider miss", pending[0].Attempts)
	}
}

func TestCmdNudgeDrainStampsLastNudgeDeliveredAt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inject bool
	}{
		{name: "plain", inject: false},
		{name: "hook_inject", inject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")

			cityDir := t.TempDir()
			writeNamedSessionCityTOML(t, cityDir)
			t.Setenv("GC_CITY", cityDir)

			store, err := openCityStoreAt(cityDir)
			if err != nil {
				t.Fatalf("openCityStoreAt: %v", err)
			}
			created, err := store.Create(beads.Bead{
				Title:  "Session: worker",
				Type:   session.BeadType,
				Status: "open",
				Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"session_name": "worker-session",
					"agent_name":   "worker",
					"template":     "worker",
					"state":        string(session.StateActive),
				},
			})
			if err != nil {
				t.Fatalf("store.Create session: %v", err)
			}

			item := newQueuedNudgeWithOptions("worker", "check hook output", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{
				SessionID: created.ID,
			})
			if err := enqueueQueuedNudgeWithStore(cityDir, store, item); err != nil {
				t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
			}

			var stdout, stderr bytes.Buffer
			code := cmdNudgeDrainWithFormat([]string{created.ID}, tc.inject, "", &stdout, &stderr)
			if code != 0 {
				t.Fatalf("cmdNudgeDrainWithFormat = %d, want 0; stderr=%s", code, stderr.String())
			}
			if !strings.Contains(stdout.String(), "check hook output") {
				t.Fatalf("stdout = %q, want drained nudge text", stdout.String())
			}

			refetched, err := store.Get(created.ID)
			if err != nil {
				t.Fatalf("store.Get session: %v", err)
			}
			raw := strings.TrimSpace(refetched.Metadata[session.MetadataLastNudgeDeliveredAt])
			if raw == "" {
				t.Fatalf("session bead missing %s after successful drain ack", session.MetadataLastNudgeDeliveredAt)
			}
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				t.Fatalf("parse %s=%q: %v", session.MetadataLastNudgeDeliveredAt, raw, err)
			}
			if drift := time.Since(parsed); drift < 0 || drift > time.Minute {
				t.Fatalf("%s timestamp drift %s is outside the 1-minute test window (raw=%q)", session.MetadataLastNudgeDeliveredAt, drift, raw)
			}
		})
	}
}

func TestDeliverSlingNudgeWaitIdleWrapsInSystemReminder(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()

	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.Create(context.Background(), "worker", "Worker", "claude", dir, "claude", nil, session.ProviderResume{}, runtime.Config{WorkDir: dir})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.WaitForIdleErrors[info.SessionName] = nil

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "claude"},
		sessionID:   info.ID,
		sessionName: info.SessionName,
	}

	var stdout, stderr bytes.Buffer
	deliverSlingNudge(target, fake, store, dir, &stdout, &stderr)

	var nudgeNowCalls int
	var delivered string
	for _, call := range fake.Calls {
		if call.Method == "NudgeNow" {
			nudgeNowCalls++
			delivered = call.Message
		}
	}
	if nudgeNowCalls != 1 {
		t.Fatalf("immediate nudge calls = %d, want 1", nudgeNowCalls)
	}
	if !strings.Contains(delivered, "<system-reminder>") {
		t.Fatalf("delivered message = %q, want system-reminder wrapper", delivered)
	}
	if !strings.Contains(delivered, "[sling] Work slung. Check your hook.") {
		t.Fatalf("delivered message = %q, want sling reminder content", delivered)
	}
	assertSessionLastNudgeDeliveredAtStamped(t, store, info.ID)
}

func assertSessionLastNudgeDeliveredAtStamped(t *testing.T, store beads.Store, sessionID string) {
	t.Helper()
	refetched, err := store.Get(sessionID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", sessionID, err)
	}
	raw := strings.TrimSpace(refetched.Metadata[session.MetadataLastNudgeDeliveredAt])
	if raw == "" {
		t.Fatalf("session bead missing %s after successful direct delivery", session.MetadataLastNudgeDeliveredAt)
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatalf("parse %s=%q: %v", session.MetadataLastNudgeDeliveredAt, raw, err)
	}
	if drift := time.Since(parsed); drift < 0 || drift > time.Minute {
		t.Fatalf("%s timestamp drift %s is outside the 1-minute test window (raw=%q)", session.MetadataLastNudgeDeliveredAt, drift, raw)
	}
}

func TestClaimDueQueuedNudgesClaimsOnceUntilAck(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	item := newQueuedNudge("worker", "finish the audit", "session", time.Now().Add(-time.Minute))
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	claimed, err := claimDueQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("claimDueQueuedNudges: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %d, want 1", len(claimed))
	}

	claimedAgain, err := claimDueQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("claimDueQueuedNudges second pass: %v", err)
	}
	if len(claimedAgain) != 0 {
		t.Fatalf("claimedAgain = %d, want 0", len(claimedAgain))
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(pending))
	}
	if len(inFlight) != 1 {
		t.Fatalf("inFlight = %d, want 1", len(inFlight))
	}
	if len(dead) != 0 {
		t.Fatalf("dead = %d, want 0", len(dead))
	}

	if err := ackQueuedNudges(dir, queuedNudgeIDs(claimed)); err != nil {
		t.Fatalf("ackQueuedNudges: %v", err)
	}
	pending, inFlight, dead, err = listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges after ack: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("after ack pending=%d inFlight=%d dead=%d, want all zero", len(pending), len(inFlight), len(dead))
	}
}

func TestClaimDueQueuedNudgesForTargetLeavesSiblingFencePending(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now().Add(-time.Minute)
	items := []queuedNudge{
		newQueuedNudgeWithOptions("worker", "for this session", "session", now, queuedNudgeOptions{
			ID:                "n1",
			SessionID:         "gc-1",
			ContinuationEpoch: "1",
		}),
		newQueuedNudgeWithOptions("worker", "for sibling session", "session", now, queuedNudgeOptions{
			ID:                "n2",
			SessionID:         "gc-2",
			ContinuationEpoch: "1",
		}),
		newQueuedNudgeWithOptions("worker", "unfenced", "session", now, queuedNudgeOptions{
			ID: "n3",
		}),
	}
	for _, item := range items {
		if err := enqueueQueuedNudge(dir, item); err != nil {
			t.Fatalf("enqueueQueuedNudge(%s): %v", item.ID, err)
		}
	}

	target := nudgeTarget{
		agent:             config.Agent{Name: "worker"},
		sessionID:         "gc-1",
		continuationEpoch: "1",
	}
	claimed, err := claimDueQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("claimDueQueuedNudgesForTarget: %v", err)
	}
	if got := queuedNudgeIDs(claimed); len(got) != 2 || got[0] != "n1" || got[1] != "n3" {
		t.Fatalf("claimed IDs = %#v, want [n1 n3]", got)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "n2" {
		t.Fatalf("pending = %#v, want only n2", pending)
	}
	if len(inFlight) != 2 {
		t.Fatalf("inFlight = %d, want 2", len(inFlight))
	}
	if len(dead) != 0 {
		t.Fatalf("dead = %d, want 0", len(dead))
	}
}

func TestClaimDueQueuedNudgesForTargetClaimsHistoricalAlias(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	item := newQueuedNudgeWithOptions("mayor", "renamed session", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{
		ID:        "n-old-alias",
		SessionID: "gc-1",
	})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	target := nudgeTarget{
		alias:        "sky",
		aliasHistory: []string{"mayor"},
		sessionID:    "gc-1",
	}
	claimed, err := claimDueQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("claimDueQueuedNudgesForTarget: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != item.ID {
		t.Fatalf("claimed = %#v, want historical alias item", claimed)
	}
}

func TestClaimDueQueuedNudgesForTargetClaimsSameSessionStaleEpoch(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now().Add(-time.Minute)
	item := newQueuedNudgeWithOptions("worker", "stale epoch", "wait", now, queuedNudgeOptions{
		ID:                "n-stale",
		SessionID:         "gc-1",
		ContinuationEpoch: "1",
	})
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	target := nudgeTarget{
		agent:             config.Agent{Name: "worker"},
		sessionID:         "gc-1",
		continuationEpoch: "2",
	}
	claimed, err := claimDueQueuedNudgesForTarget(dir, target, time.Now())
	if err != nil {
		t.Fatalf("claimDueQueuedNudgesForTarget: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != item.ID {
		t.Fatalf("claimed = %#v, want stale same-session nudge", claimed)
	}

	deliverable, rejected := splitQueuedNudgesForTarget(target, claimed)
	if len(deliverable) != 0 {
		t.Fatalf("deliverable = %#v, want none", deliverable)
	}
	if len(rejected) != 1 || rejected[0].ID != item.ID {
		t.Fatalf("rejected = %#v, want stale same-session nudge rejected", rejected)
	}
}

func TestRecordQueuedNudgeFailureRequeuesClaimedNudge(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	item := newQueuedNudge("worker", "retry me", "session", time.Now().Add(-time.Minute))
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	claimed, err := claimDueQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("claimDueQueuedNudges: %v", err)
	}
	now := time.Now()
	if err := recordQueuedNudgeFailure(dir, queuedNudgeIDs(claimed), context.DeadlineExceeded, now); err != nil {
		t.Fatalf("recordQueuedNudgeFailure: %v", err)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", now)
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if len(inFlight) != 0 {
		t.Fatalf("inFlight = %d, want 0", len(inFlight))
	}
	if len(dead) != 0 {
		t.Fatalf("dead = %d, want 0", len(dead))
	}
	if pending[0].Attempts != 1 {
		t.Fatalf("attempts = %d, want 1", pending[0].Attempts)
	}
	if !pending[0].DeliverAfter.After(now) {
		t.Fatalf("deliverAfter = %s, want after %s", pending[0].DeliverAfter, now)
	}
}

func TestQueuedNudgeFailureMovesToDeadLetter(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	item := newQueuedNudge("worker", "stuck reminder", "session", time.Now().Add(-time.Hour))
	if err := enqueueQueuedNudge(dir, item); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}

	for i := 0; i < defaultQueuedNudgeMaxAttempts; i++ {
		if err := recordQueuedNudgeFailure(dir, []string{item.ID}, context.DeadlineExceeded, time.Now().Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("recordQueuedNudgeFailure(%d): %v", i, err)
		}
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending = %d, want 0", len(pending))
	}
	if len(inFlight) != 0 {
		t.Fatalf("inFlight = %d, want 0", len(inFlight))
	}
	if len(dead) != 1 {
		t.Fatalf("dead = %d, want 1", len(dead))
	}
	if dead[0].Attempts != defaultQueuedNudgeMaxAttempts {
		t.Fatalf("attempts = %d, want %d", dead[0].Attempts, defaultQueuedNudgeMaxAttempts)
	}
}

func TestFailedQueuedNudge_DeadLettersFenceMismatch(t *testing.T) {
	item := newQueuedNudgeWithOptions("worker", "stale epoch", "wait", time.Now(), queuedNudgeOptions{
		ID:                "n-stale",
		SessionID:         "gc-1",
		ContinuationEpoch: "1",
	})

	updated, dead := failedQueuedNudge(item, errNudgeSessionFenceMismatch, time.Now())
	if !dead {
		t.Fatal("dead = false, want true for permanent fence mismatch")
	}
	if updated.DeadAt.IsZero() {
		t.Fatal("DeadAt is zero, want terminal timestamp")
	}
}

func TestAcquireNudgePollerLeaseAllowsBootstrapPID(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	pidPath := nudgePollerPIDPath(dir, "sess-worker")
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	release, err := acquireNudgePollerLease(dir, "sess-worker")
	if err != nil {
		t.Fatalf("acquireNudgePollerLease: %v", err)
	}
	release()

	_, err = os.Stat(pidPath)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pid file still exists after release: %v", err)
	}
}

func TestSplitQueuedNudgesForTarget_RejectsFencedNudgesWithoutResolvedSession(t *testing.T) {
	items := []queuedNudge{
		{ID: "n1", SessionID: "gc-1", ContinuationEpoch: "2"},
		{ID: "n2"},
	}

	deliverable, rejected := splitQueuedNudgesForTarget(nudgeTarget{}, items)

	if len(deliverable) != 1 || deliverable[0].ID != "n2" {
		t.Fatalf("deliverable = %#v, want only unfenced n2", deliverable)
	}
	if len(rejected) != 1 || rejected[0].ID != "n1" {
		t.Fatalf("rejected = %#v, want fenced n1 rejected", rejected)
	}
}

func TestSplitQueuedNudgesForDelivery_BlocksCanceledWaitNudge(t *testing.T) {
	store := beads.NewMemStore()
	wait, err := store.Create(beads.Bead{
		Type:   waitBeadType,
		Labels: []string{waitBeadLabel},
		Metadata: map[string]string{
			"state": waitStateCanceled,
		},
	})
	if err != nil {
		t.Fatalf("create wait bead: %v", err)
	}

	deliverable, blocked, err := splitQueuedNudgesForDelivery(store, []queuedNudge{{
		ID:        "n1",
		Agent:     "worker",
		Source:    "wait",
		Reference: &nudgeReference{Kind: "bead", ID: wait.ID},
	}})
	if err != nil {
		t.Fatalf("splitQueuedNudgesForDelivery: %v", err)
	}
	if len(deliverable) != 0 {
		t.Fatalf("deliverable = %#v, want none", deliverable)
	}
	if got := blocked["wait-canceled"]; len(got) != 1 || got[0].ID != "n1" {
		t.Fatalf("blocked = %#v, want n1 under wait-canceled", blocked)
	}
}

func TestSplitQueuedNudgesForDelivery_AllowsReadyLegacyWaitNudge(t *testing.T) {
	store := beads.NewMemStore()
	wait, err := store.Create(beads.Bead{
		Type:   session.LegacyWaitBeadType,
		Labels: []string{waitBeadLabel},
		Metadata: map[string]string{
			"session_id": "gc-session",
			"state":      waitStateReady,
		},
	})
	if err != nil {
		t.Fatalf("create legacy wait bead: %v", err)
	}

	deliverable, blocked, err := splitQueuedNudgesForDelivery(store, []queuedNudge{{
		ID:        "n1",
		Agent:     "worker",
		Source:    "wait",
		Reference: &nudgeReference{Kind: "bead", ID: wait.ID},
	}})
	if err != nil {
		t.Fatalf("splitQueuedNudgesForDelivery: %v", err)
	}
	if len(deliverable) != 1 || deliverable[0].ID != "n1" {
		t.Fatalf("deliverable = %#v, want n1", deliverable)
	}
	if len(blocked) != 0 {
		t.Fatalf("blocked = %#v, want empty", blocked)
	}
}

func TestWithNudgeTargetFence_FillsSessionMetadata(t *testing.T) {
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":       "sess-worker",
			"continuation_epoch": "7",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	target := withNudgeTargetFence(store, nudgeTarget{sessionName: "sess-worker"})
	if target.sessionID != sessionBead.ID {
		t.Fatalf("sessionID = %q, want %q", target.sessionID, sessionBead.ID)
	}
	if target.continuationEpoch != "7" {
		t.Fatalf("continuationEpoch = %q, want 7", target.continuationEpoch)
	}
}

func TestFindQueuedNudgeBead_IgnoresClosedRollbackBead(t *testing.T) {
	store := beads.NewMemStore()
	open, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Labels: []string{nudgeBeadLabel, "nudge:test"},
		Metadata: map[string]string{
			"nudge_id": "test",
			"state":    "queued",
		},
	})
	if err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	closed, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Labels: []string{nudgeBeadLabel, "nudge:test"},
		Metadata: map[string]string{
			"nudge_id": "test",
			"state":    "failed",
		},
	})
	if err != nil {
		t.Fatalf("create closed nudge bead: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("close nudge bead: %v", err)
	}

	found, ok, err := findQueuedNudgeBead(store, "test")
	if err != nil {
		t.Fatalf("findQueuedNudgeBead: %v", err)
	}
	if !ok {
		t.Fatal("findQueuedNudgeBead returned not found, want open bead")
	}
	if found.ID != open.ID {
		t.Fatalf("findQueuedNudgeBead = %s, want %s", found.ID, open.ID)
	}
}

func TestFindAnyQueuedNudgeBead_PrefersTerminalClosedBeadOverRollbackArtifact(t *testing.T) {
	store := beads.NewMemStore()
	rollback, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Labels: []string{nudgeBeadLabel, "nudge:test"},
		Metadata: map[string]string{
			"nudge_id": "test",
			"state":    "queued",
		},
	})
	if err != nil {
		t.Fatalf("create rollback nudge bead: %v", err)
	}
	if err := store.Close(rollback.ID); err != nil {
		t.Fatalf("close rollback nudge bead: %v", err)
	}
	terminal, err := store.Create(beads.Bead{
		Type:   nudgeBeadType,
		Labels: []string{nudgeBeadLabel, "nudge:test"},
		Metadata: map[string]string{
			"nudge_id": "test",
			"state":    "failed",
		},
	})
	if err != nil {
		t.Fatalf("create terminal nudge bead: %v", err)
	}
	if err := store.Close(terminal.ID); err != nil {
		t.Fatalf("close terminal nudge bead: %v", err)
	}

	found, ok, err := findAnyQueuedNudgeBead(store, "test")
	if err != nil {
		t.Fatalf("findAnyQueuedNudgeBead: %v", err)
	}
	if !ok {
		t.Fatal("findAnyQueuedNudgeBead returned not found")
	}
	if found.ID != terminal.ID {
		t.Fatalf("findAnyQueuedNudgeBead = %s, want %s", found.ID, terminal.ID)
	}
}

func TestCmdSessionNudgeQueueResolvesSessionName(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "myrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[[agent]]
name = "worker"
dir = "myrig"
provider = "codex"
start_command = "echo"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Chdir(cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":       "sess-worker",
			"agent_name":         "myrig/worker",
			"template":           "myrig/worker",
			"provider":           "codex",
			"work_dir":           rigDir,
			"continuation_epoch": "7",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionNudge([]string{"sess-worker", "check", "deploy"}, nudgeDeliveryQueue, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionNudge = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Queued nudge for "+sessionBead.ID) {
		t.Fatalf("stdout = %q, want queue confirmation", stdout.String())
	}

	pending, inFlight, dead, err := listQueuedNudges(cityDir, sessionBead.ID, time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 || len(inFlight) != 0 || len(dead) != 0 {
		t.Fatalf("pending/inFlight/dead = %d/%d/%d, want 1/0/0", len(pending), len(inFlight), len(dead))
	}
	if pending[0].SessionID != sessionBead.ID {
		t.Fatalf("SessionID = %q, want %q", pending[0].SessionID, sessionBead.ID)
	}
	if pending[0].ContinuationEpoch != "7" {
		t.Fatalf("ContinuationEpoch = %q, want 7", pending[0].ContinuationEpoch)
	}
	if pending[0].Agent != sessionBead.ID {
		t.Fatalf("Agent = %q, want %s", pending[0].Agent, sessionBead.ID)
	}
}

func TestPruneDeadQueuedNudges_RemovesOldDeadItems(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now()

	// Enqueue and immediately dead-letter two nudges at different ages.
	old := newQueuedNudgeWithOptions("worker", "ancient", "session", now.Add(-3*time.Hour), queuedNudgeOptions{ID: "n-old"})
	recent := newQueuedNudgeWithOptions("worker", "recent", "session", now.Add(-10*time.Minute), queuedNudgeOptions{ID: "n-recent"})
	for _, item := range []queuedNudge{old, recent} {
		if err := enqueueQueuedNudge(dir, item); err != nil {
			t.Fatalf("enqueueQueuedNudge(%s): %v", item.ID, err)
		}
	}
	// Dead-letter both at different times: old at -2h, recent at -30m.
	for i := 0; i < defaultQueuedNudgeMaxAttempts; i++ {
		if err := recordQueuedNudgeFailure(dir, []string{"n-old"}, context.DeadlineExceeded, now.Add(-2*time.Hour+time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("recordQueuedNudgeFailure(n-old, %d): %v", i, err)
		}
	}
	for i := 0; i < defaultQueuedNudgeMaxAttempts; i++ {
		if err := recordQueuedNudgeFailure(dir, []string{"n-recent"}, context.DeadlineExceeded, now.Add(-30*time.Minute+time.Duration(i)*time.Second)); err != nil {
			t.Fatalf("recordQueuedNudgeFailure(n-recent, %d): %v", i, err)
		}
	}

	// With defaultQueuedNudgeDeadRetention (1h), old should be pruned (has terminal bead), recent kept.
	store := openNudgeBeadStore(dir)
	err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
		return pruneDeadQueuedNudges(state, store, now)
	})
	if err != nil {
		t.Fatalf("pruneDeadQueuedNudges: %v", err)
	}

	_, _, dead, err := listQueuedNudges(dir, "worker", now)
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(dead) != 1 {
		t.Fatalf("dead = %d, want 1 (only recent)", len(dead))
	}
	if dead[0].ID != "n-recent" {
		t.Fatalf("surviving dead ID = %q, want n-recent", dead[0].ID)
	}
}

func TestPruneDeadQueuedNudges_RetainsItemsWithoutBeadID(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	// Directly inject a dead item without a BeadID into the queue state.
	err := withNudgeQueueState(dir, func(state *nudgeQueueState) error {
		state.Dead = append(state.Dead, queuedNudge{
			ID:      "n-orphan",
			Agent:   "worker",
			Source:  "session",
			Message: "no bead record",
			DeadAt:  now.Add(-3 * time.Hour),
		})
		return nil
	})
	if err != nil {
		t.Fatalf("seed dead item: %v", err)
	}

	err = withNudgeQueueState(dir, func(state *nudgeQueueState) error {
		return pruneDeadQueuedNudges(state, nil, now)
	})
	if err != nil {
		t.Fatalf("pruneDeadQueuedNudges: %v", err)
	}

	_, _, dead, err := listQueuedNudges(dir, "worker", now)
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != "n-orphan" {
		t.Fatalf("dead = %v, want [n-orphan] retained (no bead record)", dead)
	}
}

func TestEnqueueSupersedes_SameAgentSourceReference(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now()

	first := newQueuedNudgeWithOptions("worker", "first reminder", "sling", now, queuedNudgeOptions{
		ID:        "n-first",
		Reference: &nudgeReference{Kind: "bead", ID: "bead-123"},
	})
	if err := enqueueQueuedNudge(dir, first); err != nil {
		t.Fatalf("enqueueQueuedNudge(first): %v", err)
	}

	second := newQueuedNudgeWithOptions("worker", "second reminder", "sling", now.Add(time.Second), queuedNudgeOptions{
		ID:        "n-second",
		Reference: &nudgeReference{Kind: "bead", ID: "bead-123"},
	})
	if err := enqueueQueuedNudge(dir, second); err != nil {
		t.Fatalf("enqueueQueuedNudge(second): %v", err)
	}

	pending, _, dead, err := listQueuedNudges(dir, "worker", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %d, want 1", len(pending))
	}
	if pending[0].ID != "n-second" {
		t.Fatalf("pending ID = %q, want n-second", pending[0].ID)
	}
	if len(dead) != 1 {
		t.Fatalf("dead = %d, want 1 (superseded first)", len(dead))
	}
	if dead[0].ID != "n-first" {
		t.Fatalf("dead ID = %q, want n-first", dead[0].ID)
	}

	// Verify the superseded nudge has a terminal bead record with state "superseded".
	store := openNudgeBeadStore(dir)
	if store != nil {
		b, ok, err := findAnyQueuedNudgeBead(store, "n-first")
		if err != nil {
			t.Fatalf("findAnyQueuedNudgeBead(n-first): %v", err)
		}
		if !ok {
			t.Fatal("expected bead record for superseded nudge n-first")
		}
		if got := b.Metadata["state"]; got != "superseded" {
			t.Fatalf("superseded bead state = %q, want \"superseded\"", got)
		}
		if got := b.Metadata["terminal_reason"]; got != "superseded" {
			t.Fatalf("superseded bead terminal_reason = %q, want \"superseded\"", got)
		}
	}
}

func TestEnqueueSupersedes_InFlightNudge(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now()

	// Enqueue a nudge, then claim it so it becomes in-flight.
	first := newQueuedNudgeWithOptions("worker", "first reminder", "sling", now, queuedNudgeOptions{
		ID:        "n-inflight",
		Reference: &nudgeReference{Kind: "bead", ID: "bead-456"},
	})
	if err := enqueueQueuedNudge(dir, first); err != nil {
		t.Fatalf("enqueueQueuedNudge(first): %v", err)
	}
	claimed, err := claimDueQueuedNudgesMatching(dir, now.Add(time.Millisecond), func(item queuedNudge) bool {
		return item.ID == "n-inflight"
	})
	if err != nil {
		t.Fatalf("claimDueQueuedNudgesMatching: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed = %d, want 1", len(claimed))
	}

	// Verify it is in-flight.
	_, inFlight, _, err := listQueuedNudges(dir, "worker", now.Add(time.Second))
	if err != nil {
		t.Fatalf("listQueuedNudges (pre-supersede): %v", err)
	}
	if len(inFlight) != 1 || inFlight[0].ID != "n-inflight" {
		t.Fatalf("in-flight = %v, want [n-inflight]", inFlight)
	}

	// Enqueue a new nudge with the same reference — should supersede the in-flight one.
	second := newQueuedNudgeWithOptions("worker", "second reminder", "sling", now.Add(2*time.Second), queuedNudgeOptions{
		ID:        "n-replacement",
		Reference: &nudgeReference{Kind: "bead", ID: "bead-456"},
	})
	if err := enqueueQueuedNudge(dir, second); err != nil {
		t.Fatalf("enqueueQueuedNudge(second): %v", err)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "worker", now.Add(3*time.Second))
	if err != nil {
		t.Fatalf("listQueuedNudges (post-supersede): %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "n-replacement" {
		t.Fatalf("pending = %v, want [n-replacement]", pending)
	}
	if len(inFlight) != 0 {
		t.Fatalf("in-flight = %d, want 0 (superseded)", len(inFlight))
	}
	if len(dead) != 1 || dead[0].ID != "n-inflight" {
		t.Fatalf("dead = %v, want [n-inflight]", dead)
	}
}

func TestListQueuedNudges_CategorizesPendingAndDead(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	now := time.Now()

	// Create a pending nudge and a dead nudge.
	pending := newQueuedNudgeWithOptions("worker", "do work", "session", now, queuedNudgeOptions{ID: "n-live"})
	if err := enqueueQueuedNudge(dir, pending); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	stale := newQueuedNudgeWithOptions("worker", "old work", "session", now.Add(-2*time.Hour), queuedNudgeOptions{ID: "n-stale"})
	if err := enqueueQueuedNudge(dir, stale); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	for i := 0; i < defaultQueuedNudgeMaxAttempts; i++ {
		if err := recordQueuedNudgeFailure(dir, []string{"n-stale"}, context.DeadlineExceeded, now.Add(-time.Hour)); err != nil {
			t.Fatalf("recordQueuedNudgeFailure: %v", err)
		}
	}

	pendingList, _, deadList, err := listQueuedNudges(dir, "worker", now)
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pendingList) != 1 || pendingList[0].ID != "n-live" {
		t.Fatalf("pending = %v, want [n-live]", pendingList)
	}
	if len(deadList) != 1 || deadList[0].ID != "n-stale" {
		t.Fatalf("dead = %v, want [n-stale]", deadList)
	}
}

// TestMarkQueuedNudgeTerminalStampsCloseReason verifies that
// markQueuedNudgeTerminal stamps a canonical close_reason on the nudge
// bead's metadata before invoking store.Close. BdStore.Close forwards
// metadata.close_reason as `bd close --reason ...`; without this stamp,
// cities running with validation.on-close=error reject the close, the
// withNudgeQueueState transaction rolls back, and the nudge bounces
// between Pending and InFlight forever, generating a bead.updated event
// per claim attempt for every wedged nudge.
//
// This test pins the contract that the close_reason metadata flows
// through every state markQueuedNudgeTerminal handles. The
// nudgeCanonicalCloseReason helper guarantees the >=20 char floor.
func TestMarkQueuedNudgeTerminalStampsCloseReason(t *testing.T) {
	cases := []struct {
		name           string
		state          string
		reason         string
		commitBoundary string
	}{
		{name: "failed_fence_mismatch", state: "failed", reason: "queued nudge session fence mismatch"},
		{name: "expired", state: "expired", reason: "expired"},
		{name: "superseded", state: "superseded", reason: "superseded"},
		{name: "injected", state: "injected", reason: "", commitBoundary: "provider-nudge-return"},
		{name: "accepted_for_injection", state: "accepted_for_injection", reason: "", commitBoundary: "hook-transport-accepted"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			item := queuedNudge{
				ID:        "nudge-" + tc.name,
				Agent:     "agent-terminal",
				SessionID: "pc-qr6",
				Source:    "session",
				Message:   "DOG_DONE: reaper",
				CreatedAt: time.Now().Add(-time.Minute).UTC(),
			}
			createdID, _, err := ensureQueuedNudgeBead(store, item)
			if err != nil {
				t.Fatalf("ensureQueuedNudgeBead: %v", err)
			}

			now := time.Now().UTC()
			item.LastError = tc.reason
			if err := markQueuedNudgeTerminal(store, item, tc.state, tc.reason, tc.commitBoundary, now); err != nil {
				t.Fatalf("markQueuedNudgeTerminal: %v", err)
			}

			bead, err := store.Get(createdID)
			if err != nil {
				t.Fatalf("Get(%q): %v", createdID, err)
			}
			if bead.Status != "closed" {
				t.Fatalf("bead.Status = %q, want closed", bead.Status)
			}
			want := nudgeCanonicalCloseReason(tc.state)
			if got := bead.Metadata["close_reason"]; got != want {
				t.Errorf("close_reason = %q, want %q", got, want)
			}
			// Existing audit metadata must remain stamped alongside close_reason.
			if got := bead.Metadata["state"]; got != tc.state {
				t.Errorf("state = %q, want %q", got, tc.state)
			}
			if got := bead.Metadata["terminal_reason"]; got != tc.reason {
				t.Errorf("terminal_reason = %q, want %q", got, tc.reason)
			}
		})
	}
}

// TestNudgeCanonicalCloseReasonMeetsValidatorThreshold pins the >=20
// char floor for every known queue terminalization state and the
// unknown-code fallback. The validator (bd's validation.on-close=error,
// per gastownhall/beads#2654) rejects close reasons under 20 chars, so
// any helper output that drops below the floor would silently break
// nudge close under strict cities and reintroduce the queue-bounce loop.
func TestNudgeCanonicalCloseReasonMeetsValidatorThreshold(t *testing.T) {
	// All states that markQueuedNudgeTerminal is invoked with across the
	// nudge codepaths (recordQueuedNudgeFailureDetailed,
	// pruneExpiredQueuedNudges, recoverExpiredInFlightNudges,
	// ackQueuedNudgesWithOutcome, supersession in enqueueQueuedNudgeWithStore,
	// terminalizeBlockedQueuedNudges → ackQueuedNudgesWithOutcome).
	knownStates := []string{
		"failed",
		"expired",
		"superseded",
		"injected",
		"accepted_for_injection",
	}
	for _, s := range knownStates {
		got := nudgeCanonicalCloseReason(s)
		if len(got) < 20 {
			t.Errorf("nudgeCanonicalCloseReason(%q) = %q (%d chars), want >=20", s, got, len(got))
		}
	}
	// Unknown short code falls back to a >=20 char canonical phrase.
	if got := nudgeCanonicalCloseReason("x"); len(got) < 20 {
		t.Errorf("unknown-short-code fallback = %q (%d chars), want >=20", got, len(got))
	}
	// Empty input also yields a >=20 char fallback (avoids accidental
	// short close_reason if a caller passes "").
	if got := strings.TrimSpace(nudgeCanonicalCloseReason("")); len(got) < 20 {
		t.Errorf("trimmed empty-code fallback = %q (%d chars), want >=20", got, len(got))
	}
	// Codes already >=20 characters pass through unchanged.
	const long = "a-very-long-state-code-already-sufficient"
	if got := nudgeCanonicalCloseReason(long); got != long {
		t.Errorf("long-code passthrough = %q, want %q", got, long)
	}
}

// TestEnqueueQueuedNudgeWithStore_RollbackStampsCloseReason verifies that
// enqueueQueuedNudgeWithStore's rollback path stamps a canonical
// metadata.close_reason on the partially-created nudge bead before
// invoking store.Close. Without the stamp, BdStore.Close has no
// metadata.close_reason to forward, the validator (under
// validation.on-close=error) rejects the close, and the rollback leaks
// an OPEN nudge bead with metadata.state="queued".
//
// Triggers the rollback by writing corrupt JSON to the queue state
// file before the call, which fails LoadState inside withNudgeQueueState
// and propagates the error up to the rollback site.
func TestEnqueueQueuedNudgeWithStore_RollbackStampsCloseReason(t *testing.T) {
	dir := t.TempDir()

	// Force LoadState to fail by pre-populating state.json with corrupt
	// JSON. WithState propagates the parse error up, which means
	// enqueueQueuedNudgeWithStore enters its rollback branch.
	statePath := nudgequeue.StatePath(dir)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("{not-valid-json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	store := beads.NewMemStore()
	item := newQueuedNudgeWithOptions("worker", "rollback test", "session", time.Now(), queuedNudgeOptions{
		ID: "nudge-rollback-target",
	})

	err := enqueueQueuedNudgeWithStore(dir, store, item)
	if err == nil {
		t.Fatal("enqueueQueuedNudgeWithStore: expected error from corrupt queue state")
	}

	bead, ok, err := findAnyQueuedNudgeBead(store, item.ID)
	if err != nil {
		t.Fatalf("findAnyQueuedNudgeBead: %v", err)
	}
	if !ok {
		t.Fatal("findAnyQueuedNudgeBead: bead not found; rollback should leave a closed bead, not delete it")
	}
	if bead.Status != "closed" {
		t.Fatalf("bead.Status = %q, want closed (rollback should have closed via store.Close)", bead.Status)
	}
	if got := bead.Metadata["close_reason"]; got != nudgeEnqueueRollbackCloseReason {
		t.Errorf("close_reason = %q, want %q", got, nudgeEnqueueRollbackCloseReason)
	}
	// Belt-and-braces: the canonical reason itself meets the validator
	// floor. If someone shortens it without thinking, this guard fires.
	if got := nudgeEnqueueRollbackCloseReason; len(got) < 20 {
		t.Errorf("nudgeEnqueueRollbackCloseReason = %q (%d chars), want >=20 to satisfy validation.on-close=error", got, len(got))
	}
}

func TestEnqueueQueuedNudgeWithStore_RollbackReturnsCloseFailure(t *testing.T) {
	dir := t.TempDir()

	statePath := nudgequeue.StatePath(dir)
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("{not-valid-json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	closeErr := errors.New("rollback close failed")
	store := &rollbackCloseFailStore{
		MemStore: beads.NewMemStore(),
		closeErr: closeErr,
	}
	item := newQueuedNudgeWithOptions("worker", "rollback close failure", "session", time.Now(), queuedNudgeOptions{
		ID: "nudge-rollback-close-failure",
	})

	err := enqueueQueuedNudgeWithStore(dir, store, item)
	if err == nil {
		t.Fatal("enqueueQueuedNudgeWithStore: expected error from corrupt queue state and rollback close failure")
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("error = %v, want joined rollback close failure", err)
	}
	if !strings.Contains(err.Error(), "rollback nudge bead") {
		t.Fatalf("error = %q, want rollback context", err)
	}
}
