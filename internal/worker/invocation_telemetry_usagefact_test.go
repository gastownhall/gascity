package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/pricing"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/usage"
)

// writeCodexSessionMetaRollout fabricates a minimal codex rollout
// (rollout-<localtime>-<sessionID>.jsonl, session_meta line only) under
// root/<year>/<month>/<day>, enough for the keyed cwd-matched discovery. Returns
// the rollout path.
func writeCodexSessionMetaRollout(t *testing.T, root, year, month, day, workDir, sessionID string) string {
	t.Helper()
	dayDir := filepath.Join(root, year, month, day)
	if err := os.MkdirAll(dayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dayDir, fmt.Sprintf("rollout-%s-%s-%sT10-00-00-%s.jsonl", year, month, day, sessionID))
	meta := fmt.Sprintf(`{"timestamp":"%s-%s-%sT10:00:00.000Z","type":"session_meta","payload":{"id":%q,"cwd":%q}}`,
		year, month, day, sessionID, workDir)
	if err := os.WriteFile(path, []byte(meta+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

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
	manager := sessionpkg.NewManagerWithOptions(store, sp)
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
	// modelUsageFact resolves RunID from the run chain; StepID is intentionally
	// left unset — model usage is attributed at run level, not per formula step.
	bead := beads.Bead{ID: "b1", Metadata: map[string]string{"molecule_id": "mol-7"}}

	priced := modelUsageFact(u, bead.Metadata, bead.ID, "session-1", "myrig/polecat-1", "claude", 0.02, true, now)
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
	// StepID is intentionally unset: model usage is attributed at run level, not per
	// formula step (the gc.active_work_bead session pointer was retired).
	if priced.StepID != "" {
		t.Fatalf("StepID = %q, want empty (run-level attribution)", priced.StepID)
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
	unp := modelUsageFact(u, bead.Metadata, bead.ID, "session-1", "w", "claude", 0.02, false, now)
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

// TestInvocationUsageFamilyFromInfoResolvesWrappedCodex pins the family-ladder
// fix: a wrapped/manifold provider whose raw name carries no "codex" substring
// but whose builtin_ancestor is codex must resolve to the codex family, so the
// prompt-op seam (and the tick sweep) reach the codex spec instead of silently
// missing the gate. Before the ladder change the local provider_kind → provider
// two-step returned "" for exactly this shape.
func TestInvocationUsageFamilyFromInfoResolvesWrappedCodex(t *testing.T) {
	info := sessionpkg.Info{Provider: "mc-wrap", BuiltinAncestor: "codex"}

	// Document the pre-fix miss: the old two-step could only see provider_kind ||
	// provider, and "mc-wrap" carries no codex substring, so the gate returned "".
	if got := invocationUsageFamily(firstNonEmpty(info.ProviderKind, info.Provider)); got != "" {
		t.Fatalf("precondition: old two-step should miss wrapped codex, got %q", got)
	}
	if got := invocationUsageFamilyFromInfo(info); got != "codex" {
		t.Fatalf("invocationUsageFamilyFromInfo = %q, want codex (builtin_ancestor ladder)", got)
	}
	if _, ok := invocationUsageSpecs["codex"]; !ok {
		t.Fatal("codex spec missing from invocationUsageSpecs; the resolved family reaches no extractor")
	}
	// A plain claude-family alias with no ancestor must still normalize to claude.
	if got := invocationUsageFamilyFromInfo(sessionpkg.Info{Provider: "claude-eco"}); got != "claude" {
		t.Fatalf("claude-eco must still resolve to claude, got %q", got)
	}
}

// TestFactorySweepSessionModelUsageClaude covers the claude-family branch of the
// controller-tick model sweep: discovery via Manager.TranscriptPath, extraction,
// and one model fact per invocation after the cursor, with the cursor advanced.
func TestFactorySweepSessionModelUsageClaude(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	h, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := h.sessionID

	info, err := h.manager.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	transcriptPath := filepath.Join(slugDir, info.SessionKey+".jsonl")
	writeWorkerTestJSONL(t, transcriptPath, []map[string]any{
		usageEntryWithMessageID("u1", "msg-1", 100, 50, 0, 0),
		usageEntryWithMessageID("u2", "msg-2", 200, 100, 0, 0),
	})

	// Stamp the run chain so RunID resolves like a real formula step.
	if err := store.SetMetadata(id, "molecule_id", "run-Z"); err != nil {
		t.Fatal(err)
	}
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}

	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), id, b.Metadata, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("SweepSessionModelUsage: %v", err)
	}
	if !settled {
		t.Fatal("a fully-recorded sweep must report settled")
	}
	if emitted != 2 {
		t.Fatalf("emitted = %d, want 2", emitted)
	}

	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if len(facts) != 2 {
		t.Fatalf("want 2 model facts, got %d: %+v", len(facts), facts)
	}
	for _, f := range facts {
		if f.Kind != usage.KindModel {
			t.Fatalf("kind = %q, want model", f.Kind)
		}
		if f.RunID != "run-Z" || f.StepID != "" {
			t.Fatalf("RunID/StepID = %q/%q, want run-Z/\"\" (run-level attribution)", f.RunID, f.StepID)
		}
		if f.Provider != "claude" {
			t.Fatalf("Provider = %q, want claude", f.Provider)
		}
	}

	refreshed, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := refreshed.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]; got != "msg-2" {
		t.Fatalf("invocation_usage_cursor = %q, want msg-2 (advanced past the swept batch)", got)
	}

	// Re-sweep: cursor now blocks re-record, and any replay dedups by IdempotencyKey.
	emitted2, settled2, err := factory.SweepSessionModelUsage(context.Background(), id, refreshed.Metadata, time.Unix(2, 0).UTC())
	if err != nil {
		t.Fatalf("SweepSessionModelUsage (second): %v", err)
	}
	if !settled2 {
		t.Fatal("a re-sweep with nothing new must still report settled")
	}
	if emitted2 != 0 {
		t.Fatalf("second sweep emitted = %d, want 0 (cursor advanced)", emitted2)
	}
	facts2, _, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts (second): %v", err)
	}
	if len(facts2) != 2 {
		t.Fatalf("second sweep changed fact count to %d, want 2", len(facts2))
	}
}

// TestDiscoverSweepTranscriptCodexBoundedToInterval pins P2-1: the codex sweep
// discovery is bounded to the awake interval's day window (plus the UUIDv7 hint),
// NOT the unbounded date-tree walk, so a large codex history cannot stall the
// synchronous reconcile tick. A rollout on a day outside the interval window (and
// outside the session UUID's own creation-day hint) must not be discovered, even
// though the unbounded finder would have walked back to it.
func TestDiscoverSweepTranscriptCodexBoundedToInterval(t *testing.T) {
	codexRoot := t.TempDir()
	workDir := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{codexRoot},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	// UUIDv7 encoding ~mid-May 2026; the interval below is June 2026. 2018 is far
	// outside both the [interval±1d] window and the [UUID±2d] hint.
	sessionKey := "019e3e8e-3591-7532-a1ef-8b9e882bea2f"
	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	slept := start.Add(90 * time.Second)
	meta := map[string]string{
		"session_key":      sessionKey,
		"work_dir":         workDir,
		"awake_started_at": start.Format(time.RFC3339),
		"slept_at":         slept.Format(time.RFC3339),
	}
	now := slept.Add(time.Minute)

	// Rollout well outside the interval window and the UUID hint: must NOT match.
	writeCodexSessionMetaRollout(t, codexRoot, "2018", "01", "01", workDir, sessionKey)
	if got, _, _ := factory.discoverSweepTranscript("codex", "gc-codex-1", meta, now); got != "" {
		t.Fatalf("rollout outside the interval window must not be discovered by the bounded sweep, got %q", got)
	}

	// Control: the same session's rollout inside the interval window IS discovered.
	inside := writeCodexSessionMetaRollout(t, codexRoot, "2026", "06", "15", workDir, sessionKey)
	if got, _, _ := factory.discoverSweepTranscript("codex", "gc-codex-1", meta, now); got != inside {
		t.Fatalf("rollout inside the interval window must be discovered, got %q want %q", got, inside)
	}
}

// TestFactorySweepSessionModelUsageRecordsMetrics pins P2-3: the sweep mirrors
// the prompt-op seam's OTel metrics — every swept invocation records
// gc.agent.tokens.* (and cost when priced) under the family provider label — so
// the autonomous agents this lane recovers facts for are not invisible to live
// token/cost dashboards.
func TestFactorySweepSessionModelUsageRecordsMetrics(t *testing.T) {
	reader := setupInvocationMetricsReader(t)

	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	h, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := h.sessionID

	info, err := h.manager.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	writeWorkerTestJSONL(t, filepath.Join(slugDir, info.SessionKey+".jsonl"), []map[string]any{
		usageEntryWithMessageID("u1", "msg-1", 100, 50, 2000, 800),
		usageEntryWithMessageID("u2", "msg-2", 200, 100, 0, 0),
	})

	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	emitted, _, err := factory.SweepSessionModelUsage(context.Background(), id, b.Metadata, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("SweepSessionModelUsage: %v", err)
	}
	if emitted != 2 {
		t.Fatalf("emitted = %d, want 2", emitted)
	}

	out := collectInvocationMetrics(t, reader)
	gotInput, attrSets := invocationInt64Total(out, "gc.agent.tokens.input")
	if gotInput != 300 {
		t.Fatalf("gc.agent.tokens.input = %d, want 300 (100+200 across both swept invocations)", gotInput)
	}
	if len(attrSets) == 0 {
		t.Fatal("expected at least one gc.agent.tokens.input datapoint from the sweep")
	}
	if provider := attrSets[0]["provider"]; provider != "claude" {
		t.Fatalf("provider label = %q, want claude (the family the fact also carries)", provider)
	}
	if gotOutput, _ := invocationInt64Total(out, "gc.agent.tokens.output"); gotOutput != 150 {
		t.Fatalf("gc.agent.tokens.output = %d, want 150 (50+100)", gotOutput)
	}
}

// flakyModelSink fails the first failModelWrites model-fact Records, then
// delegates to inner. Compute facts always pass through.
type flakyModelSink struct {
	inner           usage.Sink
	failModelWrites int
}

func (s *flakyModelSink) Record(ctx context.Context, f usage.Fact) error {
	if f.Kind == usage.KindModel && s.failModelWrites > 0 {
		s.failModelWrites--
		return fmt.Errorf("transient sink failure")
	}
	return s.inner.Record(ctx, f)
}

// TestFactorySweepSessionModelUsageRetriesAfterTransientSinkError pins P2-2 at
// the sweep boundary: a sink Record failure mid-batch leaves the interval
// unsettled (settled=false) and does NOT advance the cursor past the gap, so the
// next sweep recovers exactly the missed invocations — no loss, no duplicate.
func TestFactorySweepSessionModelUsageRetriesAfterTransientSinkError(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	// Fail the first model-fact write, then recover.
	flaky := &flakyModelSink{inner: usage.NewLocalSink(sinkPath), failModelWrites: 1}
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{searchBase},
		UsageSink:   flaky,
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	h, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := h.sessionID
	info, err := h.manager.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	writeWorkerTestJSONL(t, filepath.Join(slugDir, info.SessionKey+".jsonl"), []map[string]any{
		usageEntryWithMessageID("u1", "msg-1", 100, 50, 0, 0),
		usageEntryWithMessageID("u2", "msg-2", 200, 100, 0, 0),
	})

	// Tick 1: the first model-fact write fails → unsettled, nothing recorded,
	// cursor NOT advanced.
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), id, b.Metadata, time.Unix(1, 0).UTC())
	if err == nil {
		t.Fatal("tick 1 must surface the sink Record error")
	}
	if settled {
		t.Fatal("a sink Record failure must leave the interval unsettled for retry")
	}
	if emitted != 0 {
		t.Fatalf("tick 1 emitted = %d, want 0 (the first write failed before any success)", emitted)
	}
	refreshed, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if cur := refreshed.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]; cur != "" {
		t.Fatalf("cursor must not advance past the failed write, got %q", cur)
	}

	// Tick 2: sink recovered → both invocations recorded, cursor advances, settled.
	emitted2, settled2, err := factory.SweepSessionModelUsage(context.Background(), id, refreshed.Metadata, time.Unix(2, 0).UTC())
	if err != nil {
		t.Fatalf("tick 2 SweepSessionModelUsage: %v", err)
	}
	if !settled2 {
		t.Fatal("tick 2 recovered the batch and must report settled")
	}
	if emitted2 != 2 {
		t.Fatalf("tick 2 emitted = %d, want 2 (both invocations recovered)", emitted2)
	}
	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if len(facts) != 2 {
		t.Fatalf("want exactly 2 model facts recovered (no loss, no dup), got %d: %+v", len(facts), facts)
	}
}

// TestMessageRecordsWrappedCodexModelFactViaLadder is the P3 end-to-end guard for
// the family-ladder fix on the PROMPT-OP path (the sweep test guards the tick
// path): a wrapped/manifold provider whose raw name carries no "codex" substring
// but whose builtin_ancestor is codex must reach the codex spec at Message/Nudge
// finish and emit one model usage.Fact per invocation with Provider="codex".
// Before the ladder fix the provider_kind → provider two-step returned family ""
// for this shape and no fact was emitted.
func TestMessageRecordsWrappedCodexModelFactViaLadder(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	manager := sessionpkg.NewManagerWithOptions(store, sp)
	h, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(sinkPath),
		Session: SessionSpec{
			Profile:  ProfileCodexTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "codex",
			WorkDir:  workDir,
			Provider: "mc-wrap", // wrapped manifold name, no "codex" substring
			Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := h.sessionID

	// Force the wrapped-codex metadata shape: builtin_ancestor codex, provider_kind
	// empty, raw provider name with no "codex" substring — the exact case the old
	// two-step ladder zeroed.
	sessionUUID := "019d9845-aaaa-7000-8000-0000000000ab"
	for k, v := range map[string]string{
		"builtin_ancestor": "codex",
		"provider_kind":    "",
		"provider":         "mc-wrap",
		"session_key":      sessionUUID,
		"last_woke_at":     time.Now().UTC().Format(time.RFC3339),
	} {
		if err := store.SetMetadata(id, k, v); err != nil {
			t.Fatalf("SetMetadata(%s): %v", k, err)
		}
	}

	rollout := codexWorkerRolloutPathWithID(t, searchBase, time.Now(), sessionUUID)
	writeWorkerTestJSONL(t, rollout, []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		codexWorkerTokenCount("2026-06-12T10:00:05.000Z", 15917, 15562, 10624, 355),
	})
	if _, err := h.Message(context.Background(), MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}

	// A second invocation completes; the next prompt op records it.
	writeWorkerTestJSONL(t, rollout, []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		codexWorkerTokenCount("2026-06-12T10:00:05.000Z", 15917, 15562, 10624, 355),
		codexWorkerTokenCount("2026-06-12T10:00:10.000Z", 34114, 17888, 15232, 309),
	})
	if _, err := h.Nudge(context.Background(), NudgeRequest{Text: "still there?"}); err != nil {
		t.Fatalf("Nudge: %v", err)
	}

	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if len(facts) != 2 {
		t.Fatalf("want 2 codex model facts via the wrapped-provider ladder (0 before the fix: family gate returns %q), got %d: %+v", "", len(facts), facts)
	}
	for _, f := range facts {
		if f.Kind != usage.KindModel {
			t.Fatalf("kind = %q, want model", f.Kind)
		}
		if f.Provider != "codex" {
			t.Fatalf("Provider = %q, want codex (wrapped name resolved to builtin family via the ladder)", f.Provider)
		}
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

// writeKeylessCodexRollout fabricates a full codex rollout (session_meta with cwd,
// a turn_context model line, and one token_count per element of tokenCounts —
// {total, lastInput, lastOutput}) at the local-date path the codex CLI would use
// for ts, keyed by uuid. The keyless-codex sweep fallback keys discovery on the
// session_meta cwd plus the filename-time window, NOT a captured session_key, so
// the path is written in LOCAL time (via codexWorkerRolloutPathWithID) to match
// FindCodexSessionFileNear's time.Local filename parsing on any host TZ.
func writeKeylessCodexRollout(t *testing.T, root string, ts time.Time, cwd, uuid string, tokenCounts [][3]int) {
	t.Helper()
	lines := []map[string]any{
		codexWorkerSessionMeta(cwd),
		codexWorkerTurnContext(),
	}
	for i, tc := range tokenCounts {
		lines = append(lines, codexWorkerTokenCount(
			ts.Add(time.Duration(i+1)*time.Second).UTC().Format("2006-01-02T15:04:05.000Z07:00"),
			tc[0], tc[1], 0, tc[2]))
	}
	writeWorkerTestJSONL(t, codexWorkerRolloutPathWithID(t, root, ts, uuid), lines)
}

// TestFactorySweepSessionModelUsageKeylessCodexDiscoversByWorkdir pins Design B:
// a codex session that NEVER captured a session_key — the maintainer-city graph.v2
// wisp case, where the metadata table has had zero session_key rows ever — still
// mints model facts, because the end-of-interval sweep falls back to the SAME
// bounded (cwd, interval-window) discovery the prompt-op seam already runs for
// keyless codex (TestMessageRecordsCodexTokensFreshWakeWithoutSessionKey). Two
// rollouts sit in the interval window with DIFFERENT cwds; the sweep must pick the
// one whose session_meta cwd equals the session's work_dir and mint its usage,
// ignoring the other. Before Design B the sweep settled keyless codex permanently
// and minted nothing, so factory token counts stayed 0.
func TestFactorySweepSessionModelUsageKeylessCodexDiscoversByWorkdir(t *testing.T) {
	codexRoot := t.TempDir()
	workDir := t.TempDir()
	otherDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{codexRoot},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	slept := start.Add(90 * time.Second)
	// This session's rollout (cwd == workDir): distinctive output=50.
	writeKeylessCodexRollout(t, codexRoot, start, workDir,
		"019e0000-aaaa-7000-8000-000000000001", [][3]int{{150, 100, 50}})
	// A DIFFERENT session's rollout in the same window but a different cwd. The cwd
	// filter must reject it (its output=999 would betray a wrong pick).
	writeKeylessCodexRollout(t, codexRoot, start.Add(5*time.Second), otherDir,
		"019e0000-bbbb-7000-8000-000000000002", [][3]int{{9999, 9999, 999}})

	meta := map[string]string{
		"provider":            "codex",
		"work_dir":            workDir,
		"awake_started_at":    start.Format(time.RFC3339),
		"slept_at":            slept.Format(time.RFC3339),
		"session_name":        "codex-wisp-1",
		"molecule_id":         "run-Z",
		"gc.active_work_bead": "run-Z.step-1",
		// NB: NO session_key — the whole point of Design B.
	}
	now := slept.Add(time.Minute)
	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), "gcg-codex-wisp-1", meta, now)
	if err != nil {
		t.Fatalf("SweepSessionModelUsage: %v", err)
	}
	if !settled {
		t.Fatal("a keyless codex sweep that discovered its rollout by workdir must settle")
	}
	if emitted != 1 {
		t.Fatalf("emitted = %d, want 1 (the one in-window rollout whose cwd matches work_dir)", emitted)
	}

	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if len(facts) != 1 {
		t.Fatalf("want 1 model fact, got %d: %+v", len(facts), facts)
	}
	f := facts[0]
	if f.Kind != usage.KindModel {
		t.Fatalf("kind = %q, want model", f.Kind)
	}
	if f.OutputTokens != 50 {
		t.Fatalf("OutputTokens = %d, want 50 (proves the cwd==work_dir rollout was chosen, not the other)", f.OutputTokens)
	}
	if f.Provider != "codex" {
		t.Fatalf("Provider = %q, want codex", f.Provider)
	}
	if f.RunID != "run-Z" || f.StepID != "" {
		t.Fatalf("RunID/StepID = %q/%q, want run-Z/\"\" (run-level attribution)", f.RunID, f.StepID)
	}
}

// TestFactorySweepSessionModelUsageKeylessCodexAmbiguousWorkdirTakesNone pins the
// correctness-over-coverage guard: when a keyless codex session's work_dir maps to
// MORE THAN ONE in-window rollout (workdir reuse), the sweep refuses to guess — it
// mints nothing and SETTLES the interval. The ambiguity is stable on disk, so
// retrying every tick across the whole recently-closed window could never
// disambiguate and would be pure waste.
func TestFactorySweepSessionModelUsageKeylessCodexAmbiguousWorkdirTakesNone(t *testing.T) {
	codexRoot := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{codexRoot},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	slept := start.Add(90 * time.Second)
	// TWO rollouts, SAME cwd, both inside the interval window → ambiguous.
	writeKeylessCodexRollout(t, codexRoot, start, workDir,
		"019e0000-aaaa-7000-8000-000000000001", [][3]int{{150, 100, 50}})
	writeKeylessCodexRollout(t, codexRoot, start.Add(10*time.Second), workDir,
		"019e0000-bbbb-7000-8000-000000000002", [][3]int{{450, 200, 100}})

	meta := map[string]string{
		"provider":         "codex",
		"work_dir":         workDir,
		"awake_started_at": start.Format(time.RFC3339),
		"slept_at":         slept.Format(time.RFC3339),
		"session_name":     "codex-wisp-1",
	}
	now := slept.Add(time.Minute)
	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), "gcg-codex-wisp-1", meta, now)
	if err != nil {
		t.Fatalf("SweepSessionModelUsage: %v", err)
	}
	if emitted != 0 {
		t.Fatalf("emitted = %d, want 0 (ambiguous workdir must mint nothing)", emitted)
	}
	if !settled {
		t.Fatal("ambiguous keyless codex must settle (stable ambiguity — retrying cannot disambiguate)")
	}
}

// TestFactorySweepSessionModelUsageKeylessClaudeAmbiguousSettles pins the
// shared-workdir pool case: without session_key, Claude TranscriptPath refuses
// ambiguous workdir fallback (empty path). That miss is permanent — settle so
// compute can commit instead of retrying forever (JSONL spam).
func TestFactorySweepSessionModelUsageKeylessClaudeAmbiguousSettles(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	// Two open sessions, same workdir, no session_key → ambiguous workdir fallback.
	for _, title := range []string{"one", "two"} {
		h, err := factory.Session(SessionSpec{
			Profile:  ProfileClaudeTmuxCLI,
			Template: "probe",
			Title:    title,
			Command:  "claude",
			WorkDir:  workDir,
			Provider: "claude",
		})
		if err != nil {
			t.Fatalf("Session(%s): %v", title, err)
		}
		if err := h.Start(context.Background()); err != nil {
			t.Fatalf("Start(%s): %v", title, err)
		}
		// Clear any captured session_key so discovery is forced into workdir fallback.
		if err := store.SetMetadata(h.sessionID, "session_key", ""); err != nil {
			t.Fatal(err)
		}
	}

	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeWorkerTestJSONL(t, filepath.Join(slugDir, "latest.jsonl"), []map[string]any{
		usageEntryWithMessageID("u1", "msg-1", 100, 50, 0, 0),
	})

	sessions, err := store.List(beads.ListQuery{Label: sessionpkg.LabelSession})
	if err != nil || len(sessions) < 2 {
		t.Fatalf("List sessions: err=%v len=%d", err, len(sessions))
	}
	meta := sessions[0].Metadata
	meta["session_key"] = ""
	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), sessions[0].ID, meta, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("SweepSessionModelUsage: %v", err)
	}
	if emitted != 0 {
		t.Fatalf("emitted = %d, want 0 (ambiguous keyless claude must mint nothing)", emitted)
	}
	if !settled {
		t.Fatal("keyless claude ambiguous/empty transcript must settle (permanent miss)")
	}
}

// TestDiscoverSweepTranscriptKeylessClaudeAmbiguousSettles pins the discovery
// half of the same rule: the memoizing live lane must classify a keyless Claude
// shared-workdir ambiguity refusal as a settled miss, so it stops rediscovering
// a path that stays ambiguous for as long as the pool shares that directory.
func TestDiscoverSweepTranscriptKeylessClaudeAmbiguousSettles(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(filepath.Join(t.TempDir(), "usage.jsonl")),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	// Two open sessions, same workdir, no session_key -> ambiguous workdir fallback.
	for _, title := range []string{"one", "two"} {
		h, err := factory.Session(SessionSpec{
			Profile:  ProfileClaudeTmuxCLI,
			Template: "probe",
			Title:    title,
			Command:  "claude",
			WorkDir:  workDir,
			Provider: "claude",
		})
		if err != nil {
			t.Fatalf("Session(%s): %v", title, err)
		}
		if err := h.Start(context.Background()); err != nil {
			t.Fatalf("Start(%s): %v", title, err)
		}
		if err := store.SetMetadata(h.sessionID, "session_key", ""); err != nil {
			t.Fatal(err)
		}
	}

	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeWorkerTestJSONL(t, filepath.Join(slugDir, "latest.jsonl"), []map[string]any{
		usageEntryWithMessageID("u1", "msg-1", 100, 50, 0, 0),
	})

	sessions, err := store.List(beads.ListQuery{Label: sessionpkg.LabelSession})
	if err != nil || len(sessions) < 2 {
		t.Fatalf("List sessions: err=%v len=%d", err, len(sessions))
	}
	meta := sessions[0].Metadata
	meta["session_key"] = ""
	path, settled := factory.DiscoverSweepTranscript(sessions[0].ID, meta, time.Unix(1, 0).UTC())
	if path != "" {
		t.Fatalf("path = %q, want \"\" (ambiguous keyless claude must resolve nothing)", path)
	}
	if !settled {
		t.Fatal("keyless claude ambiguous miss must memoize as settled")
	}
}

// listErrStore wraps a Store and fails List once armed, standing in for a
// transient store-read fault. Setup runs with fail unset so the session beads
// can be created and read back.
type listErrStore struct {
	beads.Store
	fail bool
}

func (s *listErrStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.fail {
		return nil, fmt.Errorf("injected list failure")
	}
	return s.Store.List(query)
}

// TestFactorySweepSessionModelUsageKeylessClaudeTranscriptErrorRetries pins the
// other side of the keyless-Claude settle: an empty transcript path produced by
// a store-read FAILURE is non-definitive, not the manager's clean ambiguity
// refusal. Settling it would strand the interval's tokens forever on a transient
// hiccup, so the sweep must record nothing and stay unsettled for a later tick.
func TestFactorySweepSessionModelUsageKeylessClaudeTranscriptErrorRetries(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	store := &listErrStore{Store: beads.NewMemStore()}
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(filepath.Join(t.TempDir(), "usage.jsonl")),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	h, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "one",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := store.SetMetadata(h.sessionID, "session_key", ""); err != nil {
		t.Fatal(err)
	}

	sessions, err := store.List(beads.ListQuery{Label: sessionpkg.LabelSession})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("List sessions: err=%v len=%d", err, len(sessions))
	}
	meta := sessions[0].Metadata
	meta["session_key"] = ""

	// Arm the fault only now: sameWorkDirSessionBeads is the first List the
	// transcript lookup makes, so TranscriptPath returns a non-nil error.
	store.fail = true

	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), sessions[0].ID, meta, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("SweepSessionModelUsage: %v", err)
	}
	if emitted != 0 {
		t.Fatalf("emitted = %d, want 0 (a failed transcript lookup must mint nothing)", emitted)
	}
	if settled {
		t.Fatal("a keyless claude transcript-lookup error must stay unsettled so a later tick retries")
	}

	path, discSettled := factory.DiscoverSweepTranscript(sessions[0].ID, meta, time.Unix(1, 0).UTC())
	if path != "" {
		t.Fatalf("DiscoverSweepTranscript path = %q, want \"\"", path)
	}
	if discSettled {
		t.Fatal("DiscoverSweepTranscript must not memoize a failed lookup as a settled miss")
	}
}

// TestFactorySweepSessionModelUsageKeylessClaudeAbsentTranscriptRetries pins the
// discriminating third branch of the keyless-Claude rule: a SINGLE keyless session
// on a workdir it shares with nobody is not ambiguous — the lookup came back empty
// only because the transcript has not been written yet. Settling that would strand
// the whole awake epoch on the live lane (liveSweepMemo caches settledMiss until a
// new wake or session_key), so it must stay unsettled and resolve once the JSONL
// lands.
func TestFactorySweepSessionModelUsageKeylessClaudeAbsentTranscriptRetries(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(filepath.Join(t.TempDir(), "usage.jsonl")),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	// Exactly one session on this workdir, no session_key: unambiguous, keyless.
	h, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "only",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := store.SetMetadata(h.sessionID, "session_key", ""); err != nil {
		t.Fatal(err)
	}

	sessions, err := store.List(beads.ListQuery{Label: sessionpkg.LabelSession})
	if err != nil || len(sessions) != 1 {
		t.Fatalf("List sessions: err=%v len=%d", err, len(sessions))
	}
	meta := sessions[0].Metadata
	meta["session_key"] = ""

	// No transcript on disk yet.
	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), sessions[0].ID, meta, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("SweepSessionModelUsage: %v", err)
	}
	if emitted != 0 {
		t.Fatalf("emitted = %d, want 0 (no transcript to read yet)", emitted)
	}
	if settled {
		t.Fatal("an unambiguous keyless claude session with no transcript yet must stay unsettled")
	}

	path, discSettled := factory.DiscoverSweepTranscript(sessions[0].ID, meta, time.Unix(1, 0).UTC())
	if path != "" {
		t.Fatalf("DiscoverSweepTranscript path = %q, want \"\" before the transcript is written", path)
	}
	if discSettled {
		t.Fatal("DiscoverSweepTranscript must not memoize an absent-but-unambiguous transcript as settled")
	}

	// The transcript lands: the same lookup now resolves and settles.
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeWorkerTestJSONL(t, filepath.Join(slugDir, "latest.jsonl"), []map[string]any{
		usageEntryWithMessageID("u1", "msg-1", 100, 50, 0, 0),
	})

	path, discSettled = factory.DiscoverSweepTranscript(sessions[0].ID, meta, time.Unix(1, 0).UTC())
	if path == "" {
		t.Fatal("DiscoverSweepTranscript must resolve the transcript once it is written")
	}
	if !discSettled {
		t.Fatal("a found path is always settled")
	}
}

// TestFactorySweepSessionModelUsageKeylessCodexDirtyScanRetries pins the P3 fix
// at the sweep boundary: when the keyless-codex (cwd, wake-window) fallback misses
// because a transient IO fault clouded the scan (here an unreadable day directory
// → EACCES), the interval must NOT settle — it must stay a candidate so a later,
// unclouded tick recovers the model facts. A clean miss settling permanently
// (proven elsewhere) is correct; a fault-clouded miss doing so would silently drop
// the interval's tokens forever.
func TestFactorySweepSessionModelUsageKeylessCodexDirtyScanRetries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-000 unreadable dir is not enforced for root")
	}
	codexRoot := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{codexRoot},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	slept := start.Add(90 * time.Second)
	writeKeylessCodexRollout(t, codexRoot, start, workDir,
		"019e0000-dddd-7000-8000-000000000001", [][3]int{{150, 100, 50}})

	// Seal the rollout's day directory so os.ReadDir(dayDir) fails with EACCES — a
	// non-ENOENT IO fault that clouds the scan (the rollout cannot be enumerated).
	local := start.In(time.Local)
	dayDir := filepath.Join(codexRoot, local.Format("2006"), local.Format("01"), local.Format("02"))
	if err := os.Chmod(dayDir, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dayDir, 0o755) })

	meta := map[string]string{
		"provider":            "codex",
		"work_dir":            workDir,
		"awake_started_at":    start.Format(time.RFC3339),
		"slept_at":            slept.Format(time.RFC3339),
		"session_name":        "codex-wisp-1",
		"molecule_id":         "run-Z",
		"gc.active_work_bead": "run-Z.step-1",
	}
	now := slept.Add(time.Minute)

	// Tick 1: dirty scan → transient miss, NOT settled.
	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), "gcg-codex-wisp-1", meta, now)
	if err != nil {
		t.Fatalf("SweepSessionModelUsage (dirty tick): %v", err)
	}
	if emitted != 0 {
		t.Fatalf("dirty-scan tick emitted = %d, want 0", emitted)
	}
	if settled {
		t.Fatal("a keyless codex miss from a DIRTY scan (transient IO fault) must NOT settle — it must retry")
	}

	// The IO fault clears; the next tick scans cleanly and recovers the fact.
	if err := os.Chmod(dayDir, 0o755); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	emitted2, settled2, err := factory.SweepSessionModelUsage(context.Background(), "gcg-codex-wisp-1", meta, now)
	if err != nil {
		t.Fatalf("SweepSessionModelUsage (clean tick): %v", err)
	}
	if !settled2 {
		t.Fatal("the clean retry tick must settle")
	}
	if emitted2 != 1 {
		t.Fatalf("clean retry emitted = %d, want 1 (the rollout recovered once the fault cleared)", emitted2)
	}
}

// TestFactorySweepSessionModelUsageKeylessCodexDirtySingletonRetries pins the P3
// synthesis fix at the sweep boundary: a keyless-codex (cwd, wake-window) scan that
// finds exactly ONE visible matching rollout but is clouded by a transient IO fault
// must NOT record that singleton and must NOT settle. A concurrent fault can hide a
// second same-cwd, in-window rollout that would make the pick ambiguous, so the
// lone match is non-definitive until a clean scan confirms it. Before the fix the
// visible match was recorded and the interval settled, permanently misattributing
// the tokens on a false singleton. The dirty source here is a per-file cwd-probe
// open fault (a sibling rollout whose os.Open faults with EACCES) — the branch the
// reviewers found exercised by no sweep-level test; when the fault clears the
// sibling turns out to be a different session, so the true singleton records.
func TestFactorySweepSessionModelUsageKeylessCodexDirtySingletonRetries(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod-000 unreadable file is not enforced for root")
	}
	codexRoot := t.TempDir()
	workDir := t.TempDir()
	otherDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{codexRoot},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	start := time.Date(2026, 6, 15, 10, 0, 0, 0, time.UTC)
	slept := start.Add(90 * time.Second)
	// The one visible in-window match (cwd == work_dir): distinctive output=50.
	writeKeylessCodexRollout(t, codexRoot, start, workDir,
		"019e0000-eeee-7000-8000-000000000001", [][3]int{{150, 100, 50}})
	// A sibling rollout in the same day dir whose cwd-probe os.Open faults with
	// EACCES once sealed. While unreadable its cwd is unknowable, so it could be a
	// second same-cwd rollout: it clouds the scan and makes the visible singleton
	// non-definitive.
	sealedTS := start.Add(20 * time.Second)
	sealed := codexWorkerRolloutPathWithID(t, codexRoot, sealedTS, "019e0000-ffff-7000-8000-000000000002")
	writeKeylessCodexRollout(t, codexRoot, sealedTS, otherDir,
		"019e0000-ffff-7000-8000-000000000002", [][3]int{{450, 200, 100}})
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(sealed, 0o644) })

	meta := map[string]string{
		"provider":            "codex",
		"work_dir":            workDir,
		"awake_started_at":    start.Format(time.RFC3339),
		"slept_at":            slept.Format(time.RFC3339),
		"session_name":        "codex-wisp-1",
		"molecule_id":         "run-Z",
		"gc.active_work_bead": "run-Z.step-1",
	}
	now := slept.Add(time.Minute)

	// Tick 1: one visible match, but the sealed sibling clouds the scan → the
	// singleton is non-definitive, so mint nothing and do NOT settle.
	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), "gcg-codex-wisp-1", meta, now)
	if err != nil {
		t.Fatalf("SweepSessionModelUsage (dirty singleton tick): %v", err)
	}
	if emitted != 0 {
		t.Fatalf("dirty-singleton tick emitted = %d, want 0 (a clouded lone match must not be recorded)", emitted)
	}
	if settled {
		t.Fatal("a keyless codex dirty singleton must NOT settle — a hidden second same-cwd rollout could make it ambiguous")
	}
	if facts, _, rerr := usage.ReadFacts(sinkPath); rerr == nil && len(facts) != 0 {
		t.Fatalf("dirty-singleton tick wrote %d facts, want 0 (record nothing until a clean scan confirms the singleton)", len(facts))
	}

	// The IO fault clears; the sibling reads as a DIFFERENT cwd, so the visible
	// rollout is the sole clean match and its usage records.
	if err := os.Chmod(sealed, 0o644); err != nil {
		t.Fatalf("restore chmod: %v", err)
	}
	emitted2, settled2, err := factory.SweepSessionModelUsage(context.Background(), "gcg-codex-wisp-1", meta, now)
	if err != nil {
		t.Fatalf("SweepSessionModelUsage (clean tick): %v", err)
	}
	if !settled2 {
		t.Fatal("the clean retry tick must settle")
	}
	if emitted2 != 1 {
		t.Fatalf("clean retry emitted = %d, want 1 (the true singleton records once the fault cleared)", emitted2)
	}
	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	if len(facts) != 1 {
		t.Fatalf("want 1 model fact after recovery, got %d: %+v", len(facts), facts)
	}
	if facts[0].OutputTokens != 50 {
		t.Fatalf("OutputTokens = %d, want 50 (proves the cwd==work_dir rollout recorded, not the sibling)", facts[0].OutputTokens)
	}
}

// paddedUsageEntry returns a usage-bearing assistant entry inflated to roughly
// padBytes so a modest number of entries exceeds the extractor's fixed tail
// window, reproducing a real transcript's bytes-per-invocation.
func paddedUsageEntry(uuid, messageID string, padBytes int) map[string]any {
	entry := usageEntryWithMessageID(uuid, messageID, 100, 50, 0, 0)
	entry["message"].(map[string]any)["content"] = strings.Repeat("x", padBytes)
	return entry
}

// TestFactorySweepRecoversBacklogBeyondFixedTailWindow is the end-to-end guard
// for the defect: when more than one tail window of transcript accumulates
// between sweeps, every invocation past the window used to be dropped
// permanently, because the persisted cursor is a message identity with no byte
// offset to resume from. The sweep must now recover the whole backlog.
func TestFactorySweepRecoversBacklogBeyondFixedTailWindow(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	h, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := h.sessionID

	info, err := h.manager.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	transcriptPath := filepath.Join(slugDir, info.SessionKey+".jsonl")

	// 40 entries at ~20KB each ≈ 800KB: over a dozen fixed windows.
	const backlog = 40
	rows := make([]map[string]any, 0, backlog+1)
	rows = append(rows, usageEntryWithMessageID("u0", "msg-cursor", 1, 1, 0, 0))
	for i := range backlog {
		rows = append(rows, paddedUsageEntry(fmt.Sprintf("u%d", i+1), fmt.Sprintf("msg-%02d", i), 20*1024))
	}
	writeWorkerTestJSONL(t, transcriptPath, rows)

	// The session was last recorded at the very first entry, so all 40 that
	// follow are owed. Under the fixed window only the last handful are visible.
	if err := store.SetMetadata(id, sessionpkg.MetadataKeyInvocationUsageCursor, "msg-cursor"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(id, "molecule_id", "run-Z"); err != nil {
		t.Fatal(err)
	}
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}

	// Guard: the fixed window genuinely cannot reach the cursor, so this test
	// keeps exercising the defect rather than quietly becoming a tautology.
	fixed, err := sessionlog.ExtractTailUsage(transcriptPath)
	if err != nil {
		t.Fatalf("ExtractTailUsage: %v", err)
	}
	if len(fixed) >= backlog {
		t.Fatalf("fixed tail window saw %d of %d entries; test no longer reproduces the backlog", len(fixed), backlog)
	}

	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), id, b.Metadata, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("SweepSessionModelUsage: %v", err)
	}
	if !settled {
		t.Fatal("a fully-recorded sweep must report settled")
	}
	if emitted != backlog {
		t.Fatalf("emitted = %d, want %d (the whole backlog past the fixed window)", emitted, backlog)
	}

	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected sink warnings: %v", warnings)
	}
	if len(facts) != backlog {
		t.Fatalf("sink holds %d facts, want %d", len(facts), backlog)
	}
}

// TestFactorySweepWithoutCursorMatchesFixedTailWindow pins the other half of
// the per-family coverage ceiling documented on SweepSessionModelUsage: growth
// is cursor-bounded, so before a cursor exists there is no growth at all.
// ExtractTailUsageSince short-circuits to one fixed 64KB read, and the sweep
// therefore emits only what that window holds — the pre-change behavior, with
// no cap log. The cursor the sweep then persists points at the newest entry,
// which is the growth loop's stop condition, so no later sweep reaches back
// across the gap. TestFactorySweepRecoversBacklogBeyondFixedTailWindow
// pre-seeds a cursor and so cannot cover this branch.
func TestFactorySweepWithoutCursorMatchesFixedTailWindow(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")

	store := beads.NewMemStore()
	sp := runtime.NewFake()
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: []string{searchBase},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	h, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  workDir,
		Provider: "claude",
		Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
	})
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := h.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	id := h.sessionID

	info, err := h.manager.Get(id)
	if err != nil {
		t.Fatalf("Get(%q): %v", id, err)
	}
	slugDir := filepath.Join(searchBase, sessionlog.ProjectSlug(workDir))
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", slugDir, err)
	}
	transcriptPath := filepath.Join(slugDir, info.SessionKey+".jsonl")

	// Same ~800KB backlog as the cursor-seeded test, so the only difference
	// between the two is whether a cursor exists.
	const backlog = 40
	rows := make([]map[string]any, 0, backlog)
	for i := range backlog {
		rows = append(rows, paddedUsageEntry(fmt.Sprintf("u%d", i+1), fmt.Sprintf("msg-%02d", i), 20*1024))
	}
	writeWorkerTestJSONL(t, transcriptPath, rows)

	// No MetadataKeyInvocationUsageCursor: this is a session's first sweep.
	if err := store.SetMetadata(id, "molecule_id", "run-Z"); err != nil {
		t.Fatal(err)
	}
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(b.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]); got != "" {
		t.Fatalf("precondition: cursor = %q, want empty", got)
	}

	// The fixed window is what the sweep is expected to be limited to, and it
	// must be genuinely narrower than the backlog or this asserts nothing.
	fixed, err := sessionlog.ExtractTailUsage(transcriptPath)
	if err != nil {
		t.Fatalf("ExtractTailUsage: %v", err)
	}
	if len(fixed) == 0 || len(fixed) >= backlog {
		t.Fatalf("fixed tail window saw %d of %d entries; test no longer isolates the empty-cursor branch", len(fixed), backlog)
	}

	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), id, b.Metadata, time.Unix(1, 0).UTC())
	if err != nil {
		t.Fatalf("SweepSessionModelUsage: %v", err)
	}
	if !settled {
		t.Fatal("a fully-recorded sweep must report settled")
	}
	if emitted != len(fixed) {
		t.Fatalf("emitted = %d, want %d (the fixed window only, with no cursor to grow toward)", emitted, len(fixed))
	}

	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected sink warnings: %v", warnings)
	}
	if len(facts) != len(fixed) {
		t.Fatalf("sink holds %d facts, want %d", len(facts), len(fixed))
	}

	// The persisted cursor is the newest entry, not the oldest one the sweep
	// missed: the gap below it is unreachable from every later sweep.
	after, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	wantCursor := fmt.Sprintf("msg-%02d", backlog-1)
	if got := after.Metadata[sessionpkg.MetadataKeyInvocationUsageCursor]; got != wantCursor {
		t.Fatalf("persisted cursor = %q, want %q (the newest entry)", got, wantCursor)
	}
}
