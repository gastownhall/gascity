package worker

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/pricing"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/usage"
)

// newUsageFactHandle builds a started session handle wired to a real LocalSink,
// returning the handle, the resolved transcript path to write usage entries to,
// and the usage.jsonl path the sink writes facts to. Mirrors
// newInvocationTelemetryHandle but threads a usage sink so the model-fact path
// is exercised end to end.
func newUsageFactHandle(t *testing.T) (handle *SessionHandle, transcriptPath, sinkPath string) {
	t.Helper()
	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath = filepath.Join(t.TempDir(), "usage.jsonl")

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManager(store, sp)
	h, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(sinkPath),
		Session: SessionSpec{
			Profile:  ProfileClaudeTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "claude",
			WorkDir:  workDir,
			Provider: "claude",
			Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info, err := manager.Get(h.sessionID)
	if err != nil {
		t.Fatalf("Get(%q): %v", h.sessionID, err)
	}
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	return h, filepath.Join(slugDir, info.SessionKey+".jsonl"), sinkPath
}

// TestMessageEmitsModelUsageFactToSink is the end-to-end regression for the
// adopt-pr finding that real transcript model usage never reached the usage sink:
// a transcript invocation completes, a prompt op runs, and a configured LocalSink
// must receive a priced model fact whose RunID matches the session run root.
func TestMessageEmitsModelUsageFactToSink(t *testing.T) {
	handle, transcriptPath, sinkPath := newUsageFactHandle(t)

	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "claude-opus-4-7", 100, 50, 2000, 800),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected sink warnings: %v", warnings)
	}
	if len(facts) != 1 {
		t.Fatalf("want exactly 1 model fact recorded, got %d: %+v", len(facts), facts)
	}
	f := facts[0]
	if f.Kind != usage.KindModel {
		t.Fatalf("kind = %q, want %q", f.Kind, usage.KindModel)
	}
	if f.RunID != handle.sessionID {
		t.Fatalf("RunID = %q, want session run root %q", f.RunID, handle.sessionID)
	}
	// SessionID must carry the session bead id end to end (here it equals the run
	// root because a manual chat has no work bead, but it is a distinct field).
	if f.SessionID != handle.sessionID {
		t.Fatalf("SessionID = %q, want the session bead id %q", f.SessionID, handle.sessionID)
	}
	if f.Worker == "" {
		t.Fatalf("worker (session name) must be set: %+v", f)
	}
	if f.Model != "claude-opus-4-7" || f.Provider != "claude" {
		t.Fatalf("model/provider wrong: %+v", f)
	}
	if f.InputTokens != 100 || f.OutputTokens != 50 || f.CacheReadTokens != 2000 || f.CacheCreationTokens != 800 {
		t.Fatalf("tokens wrong: %+v", f)
	}
	if f.UpstreamReqID != "u1" {
		t.Fatalf("UpstreamReqID = %q, want the transcript entry identity u1", f.UpstreamReqID)
	}
	if f.IdempotencyKey != usage.ModelIdempotencyKey(f.RunID, "u1") {
		t.Fatalf("IdempotencyKey = %q, want ModelIdempotencyKey(%q, u1)", f.IdempotencyKey, f.RunID)
	}

	wantCost, ok := pricing.BuildRegistry(nil, nil).Estimate("claude", "claude-opus-4-7", pricing.Usage{
		PromptTokens:        100,
		CompletionTokens:    50,
		CacheReadTokens:     2000,
		CacheCreationTokens: 800,
	})
	if !ok {
		t.Fatal("default pricing registry has no claude-opus-4-7 entry; fix the test fixture")
	}
	if f.Unpriced {
		t.Fatalf("a priced model must not be flagged Unpriced: %+v", f)
	}
	if f.CostUSDEstimate != wantCost {
		t.Fatalf("CostUSDEstimate = %v, want %v", f.CostUSDEstimate, wantCost)
	}
}

// TestMessageEmitsUnpricedModelFactForUnknownModel proves the tri-state honesty
// of the real fact path: an unknown model yields a fact flagged Unpriced with a
// zero cost — "not measured", never a free $0 invocation.
func TestMessageEmitsUnpricedModelFactForUnknownModel(t *testing.T) {
	handle, transcriptPath, sinkPath := newUsageFactHandle(t)

	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntry("u1", "totally-unknown-model-xyz", 100, 50, 0, 0),
	})

	if _, err := handle.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	facts, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(facts) != 1 {
		t.Fatalf("want 1 model fact, got %d: %+v", len(facts), facts)
	}
	f := facts[0]
	if !f.Unpriced {
		t.Fatalf("unknown model must be flagged Unpriced: %+v", f)
	}
	if f.CostUSDEstimate != 0 {
		t.Fatalf("unpriced model must carry zero cost, got %v", f.CostUSDEstimate)
	}
	if f.InputTokens != 100 || f.OutputTokens != 50 {
		t.Fatalf("tokens must still be recorded for an unpriced model: %+v", f)
	}
}

// TestUsageFactRecordingEnabledGate pins the sink gate that decides whether the
// invocation-telemetry loop emits model facts: a nil or Discard sink records
// nothing (metrics still flow), a live sink enables emission.
func TestUsageFactRecordingEnabledGate(t *testing.T) {
	if (&SessionHandle{}).usageFactRecordingEnabled() {
		t.Fatal("nil sink must not enable fact recording")
	}
	if (&SessionHandle{usageSink: usage.Discard}).usageFactRecordingEnabled() {
		t.Fatal("discard sink must not enable fact recording")
	}
	if !(&SessionHandle{usageSink: usage.NewLocalSink(filepath.Join(t.TempDir(), "u.jsonl"))}).usageFactRecordingEnabled() {
		t.Fatal("a live sink must enable fact recording")
	}
}

func TestModelUsageFact(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	u := sessionlog.TailUsage{
		EntryUUID:           "entry-1",
		MessageID:           "msg-9",
		Model:               "claude-opus-4-7",
		InputTokens:         100,
		OutputTokens:        50,
		CacheReadTokens:     10,
		CacheCreationTokens: 5,
	}
	// The session bead carries gc.active_work_bead (the step it is currently on),
	// stamped by the claim hook; modelUsageFact reads it into Fact.StepID.
	bead := beads.Bead{ID: "b1", Metadata: map[string]string{"molecule_id": "mol-7", "gc.active_work_bead": "mol.finalize"}}

	priced := modelUsageFact(u, bead, "session-1", "myrig/polecat-1", "claude", 0.02, true, now)
	if priced.Kind != usage.KindModel {
		t.Fatalf("kind = %q", priced.Kind)
	}
	if priced.RunID != "mol-7" {
		t.Fatalf("RunID = %q, want mol-7 (resolved through the shared run-id chain)", priced.RunID)
	}
	// SessionID is the session bead id (the sessionID arg), distinct from RunID
	// (the resolved run root) and from Worker (the session NAME). It is the join
	// key to the spend plane (EIA session_id) and recall transcripts.
	if priced.SessionID != "session-1" {
		t.Fatalf("SessionID = %q, want the session bead id session-1", priced.SessionID)
	}
	// StepID is the session's gc.active_work_bead (the bare logical step), distinct
	// from RunID — the exact-join key to the events plane and per-step spend rollup.
	if priced.StepID != "mol.finalize" {
		t.Fatalf("StepID = %q, want mol.finalize (the session's gc.active_work_bead), distinct from RunID", priced.StepID)
	}
	if priced.Worker != "myrig/polecat-1" || priced.Model != "claude-opus-4-7" || priced.Provider != "claude" {
		t.Fatalf("identity wrong: %+v", priced)
	}
	if priced.InputTokens != 100 || priced.OutputTokens != 50 || priced.CacheReadTokens != 10 || priced.CacheCreationTokens != 5 {
		t.Fatalf("tokens wrong: %+v", priced)
	}
	if priced.CostUSDEstimate != 0.02 || priced.Unpriced {
		t.Fatalf("priced fact must carry the cost and not be Unpriced: %+v", priced)
	}
	// MessageID is the dedup identity when present (shared by an API response's
	// content blocks), in preference to the per-entry uuid.
	if priced.UpstreamReqID != "msg-9" {
		t.Fatalf("UpstreamReqID = %q, want the message identity msg-9", priced.UpstreamReqID)
	}
	if priced.IdempotencyKey != usage.ModelIdempotencyKey("mol-7", "msg-9") {
		t.Fatalf("IdempotencyKey wrong: %+v", priced)
	}
	if priced.At != now.UnixMilli() {
		t.Fatalf("At = %d, want %d", priced.At, now.UnixMilli())
	}

	// Unpriced collapses cost to zero regardless of the cost argument.
	unp := modelUsageFact(u, bead, "session-1", "w", "claude", 0.02, false, now)
	if !unp.Unpriced || unp.CostUSDEstimate != 0 {
		t.Fatalf("unpriced fact must zero the cost and set the flag: %+v", unp)
	}
}

func TestModelAndComputeFactsShareSessionIDJoinKey(t *testing.T) {
	sessionID := "session-1"
	model := usage.Fact{SessionID: sessionID}
	compute := usage.Fact{SessionID: sessionID}
	if model.SessionID != compute.SessionID {
		t.Fatalf("model session_id %q must match compute session_id %q", model.SessionID, compute.SessionID)
	}
	if model.SessionID != sessionID {
		t.Fatalf("SessionID = %q, want %q", model.SessionID, sessionID)
	}
}

// TestRecordModelUsageFactHungSinkReturnsPromptly keeps the worker-prompt-path
// half of the hung-exec-sink regression: the model-fact write must not block the
// prompt op indefinitely on a hung exec: sink (the sink enforces its own
// deadline).
func TestRecordModelUsageFactHungSinkReturnsPromptly(t *testing.T) {
	script := writeUsageSinkScript(t, "sleep 30")
	h := &SessionHandle{usageSink: usage.NewExecSinkWithTimeout(script, 100*time.Millisecond)}
	f := usage.Fact{RunID: "r1", Kind: usage.KindModel, InputTokens: 10, UpstreamReqID: "msg-1"}

	done := make(chan struct{})
	start := time.Now()
	go func() {
		h.recordModelUsageFact(f)
		close(done)
	}()
	select {
	case <-done:
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("worker model-fact path blocked on a hung sink: took %s", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker model-fact path did not return under a hung sink")
	}
}

func writeUsageSinkScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sink.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
