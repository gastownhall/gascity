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
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

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
	code := deliverSessionNudgeWithProvider(target, fake, "check deploy status", nudgeDeliveryWaitIdle, &stdout, &stderr)
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
	code := deliverSessionNudgeWithProvider(target, fake, "check deploy status", nudgeDeliveryWaitIdle, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("deliverSessionNudgeWithProvider = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
	}
}

func TestSendMailNotifyWithProviderQueuesWhenSessionSleeping(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)},
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: "sess-mayor",
	}

	if err := sendMailNotifyWithProvider(target, runtime.NewFake(), "human"); err != nil {
		t.Fatalf("sendMailNotifyWithProvider: %v", err)
	}

	pending, inFlight, dead, err := listQueuedNudges(dir, "mayor", time.Now())
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
		agent:       config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)},
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

	if err := sendMailNotifyWithProvider(target, fake, "human"); err != nil {
		t.Fatalf("sendMailNotifyWithProvider: %v", err)
	}
	if !called {
		t.Fatal("startNudgePoller was not called")
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

	fake := runtime.NewFake()
	if err := fake.Start(context.Background(), "sess-worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	fake.Activity = map[string]time.Time{"sess-worker": time.Now().Add(-10 * time.Second)}

	target := nudgeTarget{
		cityPath:    dir,
		agent:       config.Agent{Name: "worker"},
		resolved:    &config.ResolvedProvider{Name: "codex"},
		sessionName: "sess-worker",
	}

	delivered, err := tryDeliverQueuedNudgesByPoller(target, fake, 3*time.Second)
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
	if !strings.Contains(nudgeCalls[0].Message, "Deferred reminders:") {
		t.Fatalf("nudge message = %q, want deferred reminder wrapper", nudgeCalls[0].Message)
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
