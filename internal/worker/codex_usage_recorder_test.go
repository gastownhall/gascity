package worker

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/usage"
)

// codexRecorderFixture is a factory over a real codex session bead, a usage
// sink and one rollout under a codex search root.
type codexRecorderFixture struct {
	store    beads.Store
	factory  *Factory
	sinkPath string
	root     string
	path     string
	id       string
	meta     map[string]string
	start    time.Time
}

const codexRecorderRolloutID = "019e0000-cccc-7000-8000-000000000003"

func newCodexRecorderFixture(t *testing.T) *codexRecorderFixture {
	t.Helper()
	fx := &codexRecorderFixture{
		store:    beads.NewMemStore(),
		sinkPath: filepath.Join(t.TempDir(), "usage.jsonl"),
		root:     t.TempDir(),
		start:    time.Now().UTC().Add(-time.Hour).Truncate(time.Second),
	}
	factory, err := NewFactory(FactoryConfig{
		Store:       fx.store,
		Provider:    runtime.NewFake(),
		SearchPaths: []string{fx.root},
		UsageSink:   usage.NewLocalSink(fx.sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	fx.factory = factory
	fx.meta = map[string]string{
		"provider":         "codex",
		"session_key":      codexRecorderRolloutID,
		"work_dir":         t.TempDir(),
		"awake_started_at": fx.start.Format(time.RFC3339),
		"session_name":     "codex-recorder-1",
		"molecule_id":      "run-R",
	}
	b, err := fx.store.Create(beads.Bead{Title: "codex", Type: sessionpkg.BeadType, Labels: []string{sessionpkg.LabelSession}, Metadata: fx.meta})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	fx.id = b.ID
	fx.path = codexWorkerRolloutPathWithID(t, fx.root, fx.start, codexRecorderRolloutID)
	return fx
}

// at is the rollout timestamp sec seconds after the interval start.
func (fx *codexRecorderFixture) at(sec int) string {
	return fx.start.Add(time.Duration(sec) * time.Second).Format("2006-01-02T15:04:05.000Z07:00")
}

func (fx *codexRecorderFixture) tokenCount(sec, total int) map[string]any {
	return codexWorkerTokenCount(fx.at(sec), total, total/2, 0, total/4)
}

func (fx *codexRecorderFixture) turnContext(sec int, model string) map[string]any {
	return map[string]any{
		"timestamp": fx.at(sec),
		"type":      "turn_context",
		"payload":   map[string]any{"turn_id": "t-" + model, "model": model},
	}
}

// codexToolOutput is a response_item record of about n bytes carrying no usage.
func codexToolOutput(n int) map[string]any {
	return map[string]any{
		"timestamp": "2026-06-12T10:00:02.000Z",
		"type":      "response_item",
		"payload":   map[string]any{"type": "function_call_output", "output": strings.Repeat("o", n)},
	}
}

func codexToolOutputs(count, n int) []map[string]any {
	out := make([]map[string]any, count)
	for i := range out {
		out[i] = codexToolOutput(n)
	}
	return out
}

func (fx *codexRecorderFixture) write(t *testing.T, lines []map[string]any) {
	t.Helper()
	writeWorkerTestJSONL(t, fx.path, lines)
}

// sweep runs one sweep of the fixture's rollout.
func (fx *codexRecorderFixture) sweep(t *testing.T) (int, bool, error) {
	t.Helper()
	return fx.factory.SweepSessionModelUsageAtPath(context.Background(), fx.id, fx.meta, fx.path, time.Now().UTC())
}

// sweepUntilSettled sweeps until a pass settles, failing after max passes.
func (fx *codexRecorderFixture) sweepUntilSettled(t *testing.T, maxPasses int) int {
	t.Helper()
	for pass := 1; pass <= maxPasses; pass++ {
		_, settled, err := fx.sweep(t)
		if err != nil {
			t.Fatalf("sweep %d: %v", pass, err)
		}
		if settled {
			return pass
		}
	}
	t.Fatalf("rollout did not settle within %d sweeps", maxPasses)
	return 0
}

// logicalFacts returns the deduplicated model facts' upstream ids, sorted, and
// the facts by id.
func (fx *codexRecorderFixture) logicalFacts(t *testing.T) ([]string, map[string]usage.Fact) {
	t.Helper()
	facts, warnings, err := usage.ReadFacts(fx.sinkPath)
	if err != nil {
		t.Fatalf("ReadFacts: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("ReadFacts warnings: %v", warnings)
	}
	byID := map[string]usage.Fact{}
	for _, f := range facts {
		if f.Kind != usage.KindModel {
			continue
		}
		if _, dup := byID[f.UpstreamReqID]; dup {
			t.Fatalf("fact %s read twice after dedup", f.UpstreamReqID)
		}
		byID[f.UpstreamReqID] = f
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, byID
}

// TestSweepCodexUsageRecoversBacklogBeyondTailWindow is the triage repro: two
// invocations followed by tool output far past the 64 KiB tail window, then a
// third. Every invocation is billed, once.
func TestSweepCodexUsageRecoversBacklogBeyondTailWindow(t *testing.T) {
	fx := newCodexRecorderFixture(t)
	lines := []map[string]any{
		codexWorkerSessionMeta(fx.meta["work_dir"]),
		fx.turnContext(0, "gpt-5.5"),
		fx.tokenCount(1, 150),
		fx.tokenCount(2, 300),
	}
	lines = append(lines, codexToolOutputs(32, 4*1024)...)
	lines = append(lines, fx.tokenCount(3, 450))
	fx.write(t, lines)

	fx.sweepUntilSettled(t, 5)
	// A settled rollout stays settled and records nothing new.
	if emitted, settled, err := fx.sweep(t); err != nil || !settled || emitted != 0 {
		t.Fatalf("re-sweep = %d, %v, %v; want 0, settled, nil", emitted, settled, err)
	}

	ids, byID := fx.logicalFacts(t)
	if want := []string{"total:150", "total:300", "total:450"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("logical facts = %v, want %v", ids, want)
	}
	for _, id := range ids {
		if byID[id].Model != "gpt-5.5" {
			t.Fatalf("%s model = %q, want gpt-5.5", id, byID[id].Model)
		}
	}
}

// TestSweepCodexUsageDrainsBacklogOverBoundedPasses drains a backlog larger
// than the per-pass budget: each pass short of the end is unsettled, and the
// passes together bill every invocation once.
func TestSweepCodexUsageDrainsBacklogOverBoundedPasses(t *testing.T) {
	fx := newCodexRecorderFixture(t)
	fx.factory.codexUsageLimits.MaxBytes = 16 * 1024
	lines := []map[string]any{
		codexWorkerSessionMeta(fx.meta["work_dir"]),
		fx.turnContext(0, "gpt-5.5"),
		fx.tokenCount(1, 150),
	}
	lines = append(lines, codexToolOutputs(8, 8*1024)...)
	lines = append(lines, fx.tokenCount(2, 300))
	lines = append(lines, codexToolOutputs(8, 8*1024)...)
	lines = append(lines, fx.tokenCount(3, 450))
	fx.write(t, lines)

	if _, settled, err := fx.sweep(t); err != nil || settled {
		t.Fatalf("first bounded pass settled=%v err=%v; want an unsettled backlog", settled, err)
	}
	passes := fx.sweepUntilSettled(t, 40)
	if passes < 3 {
		t.Fatalf("settled after %d more passes; the 16 KiB budget must need several", passes)
	}

	ids, _ := fx.logicalFacts(t)
	if want := []string{"total:150", "total:300", "total:450"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("logical facts = %v, want %v", ids, want)
	}
	facts, _, err := usage.ReadFacts(fx.sinkPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 3 {
		t.Fatalf("sink holds %d facts, want each invocation appended once", len(facts))
	}

	// The committed checkpoint mirrors the flat cursor.
	progress, err := fx.factory.manager.InvocationUsageProgress(fx.id)
	if err != nil {
		t.Fatal(err)
	}
	if progress.Cursor != "total:450" || !strings.Contains(progress.CodexCheckpoint, `"cursor":"total:450"`) {
		t.Fatalf("progress = %+v, want cursor total:450 mirrored in the checkpoint", progress)
	}
}

// restart replaces the fixture's factory with a fresh one over the same store,
// sink and search root, keeping its codex page limits: a gc restart between
// sweeps, with only the session bead's committed progress carried over.
func (fx *codexRecorderFixture) restart(t *testing.T) {
	t.Helper()
	fx.restartWith(t, usage.NewLocalSink(fx.sinkPath), fx.store)
}

// restartWith is restart with the new factory writing facts to sink and
// session progress through store, each of which may wrap the fixture's own.
func (fx *codexRecorderFixture) restartWith(t *testing.T, sink usage.Sink, store beads.Store) {
	t.Helper()
	limits := fx.factory.codexUsageLimits
	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    runtime.NewFake(),
		SearchPaths: []string{fx.root},
		UsageSink:   sink,
	})
	if err != nil {
		t.Fatalf("NewFactory after restart: %v", err)
	}
	factory.codexUsageLimits = limits
	fx.factory = factory
}

// TestSweepCodexUsageKeepsModelContextAcrossPagesAndRestart puts the model
// context of an invocation more than 2 MiB back, behind tool output, then
// switches model and lets Codex re-emit that invocation's snapshot after the
// new turn_context. Bounded pages split the read and gc restarts in between:
// each invocation is billed once with its own model and time.
func TestSweepCodexUsageKeepsModelContextAcrossPagesAndRestart(t *testing.T) {
	fx := newCodexRecorderFixture(t)
	fx.factory.codexUsageLimits.MaxBytes = 512 * 1024
	lines := []map[string]any{
		codexWorkerSessionMeta(fx.meta["work_dir"]),
		fx.turnContext(0, "gpt-5.5"),
		fx.tokenCount(1, 150),
	}
	lines = append(lines, codexToolOutputs(320, 8*1024)...) // 2.5 MiB
	lines = append(lines,
		fx.tokenCount(2, 300),
		fx.turnContext(3, "gpt-5.6"),
		fx.tokenCount(4, 300), // the re-emitted snapshot of total:300
		fx.tokenCount(5, 450),
	)
	fx.write(t, lines)

	if _, settled, err := fx.sweep(t); err != nil || settled {
		t.Fatalf("first bounded pass settled=%v err=%v; want an unsettled backlog", settled, err)
	}
	fx.restart(t)
	if _, settled, err := fx.sweep(t); err != nil || settled {
		t.Fatalf("pass after restart settled=%v err=%v; want backlog still pending", settled, err)
	}
	fx.restart(t)
	fx.sweepUntilSettled(t, 20)

	ids, byID := fx.logicalFacts(t)
	if want := []string{"total:150", "total:300", "total:450"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("logical facts = %v, want %v", ids, want)
	}
	want := map[string]struct {
		model string
		sec   int
	}{
		"total:150": {"gpt-5.5", 1},
		"total:300": {"gpt-5.5", 2},
		"total:450": {"gpt-5.6", 5},
	}
	for id, w := range want {
		f := byID[id]
		if f.Model != w.model {
			t.Fatalf("%s model = %q, want %q", id, f.Model, w.model)
		}
		if at := fx.start.Add(time.Duration(w.sec) * time.Second).UnixMilli(); f.At != at {
			t.Fatalf("%s at = %d, want its own invocation time %d", id, f.At, at)
		}
	}
}

// failNthModelSink fails the nth model fact written to it, once.
type failNthModelSink struct {
	inner  usage.Sink
	n      int
	writes int
}

func (s *failNthModelSink) Record(ctx context.Context, f usage.Fact) error {
	if f.Kind == usage.KindModel {
		s.writes++
		if s.writes == s.n {
			return errors.New("transient sink failure")
		}
	}
	return s.inner.Record(ctx, f)
}

// failingProgressStore fails the next fails writes of the codex usage
// checkpoint.
type failingProgressStore struct {
	beads.Store
	fails int
}

func (s *failingProgressStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs[sessionpkg.MetadataKeyCodexUsageCheckpoint]; ok && s.fails > 0 {
		s.fails--
		return errors.New("transient store failure")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// TestSweepCodexUsageRetriesSinkAndStoreFailuresAcrossRestarts fails a fact
// write mid-page, restarts, fails the progress commit after the rest were
// written, restarts again and retries: the logical facts equal those of an
// uninterrupted run, with no invocation lost.
func TestSweepCodexUsageRetriesSinkAndStoreFailuresAcrossRestarts(t *testing.T) {
	fx := newCodexRecorderFixture(t)
	lines := []map[string]any{
		codexWorkerSessionMeta(fx.meta["work_dir"]),
		fx.turnContext(0, "gpt-5.5"),
		fx.tokenCount(1, 150),
	}
	lines = append(lines, codexToolOutputs(16, 8*1024)...)
	lines = append(lines, fx.tokenCount(2, 300))
	lines = append(lines, codexToolOutputs(16, 8*1024)...)
	lines = append(lines,
		fx.tokenCount(3, 450),
		fx.turnContext(4, "gpt-5.6"),
		fx.tokenCount(5, 450),
		fx.tokenCount(6, 600),
	)

	// The uninterrupted run, over its own session and sink.
	ref := newCodexRecorderFixture(t)
	ref.write(t, lines)
	ref.sweepUntilSettled(t, 5)
	wantIDs, wantFacts := ref.logicalFacts(t)

	fx.write(t, lines)
	fx.restartWith(t, &failNthModelSink{inner: usage.NewLocalSink(fx.sinkPath), n: 2}, fx.store)
	if emitted, settled, err := fx.sweep(t); err == nil || settled || emitted != 1 {
		t.Fatalf("sweep with a failing second write = %d, %v, %v; want 1 recorded, unsettled, the sink error", emitted, settled, err)
	}
	if progress, err := fx.factory.manager.InvocationUsageProgress(fx.id); err != nil || progress.Cursor != "total:150" {
		t.Fatalf("progress after the sink failure = %+v, %v; want it committed through total:150", progress, err)
	}

	fx.restartWith(t, usage.NewLocalSink(fx.sinkPath), &failingProgressStore{Store: fx.store, fails: 1})
	if _, settled, err := fx.sweep(t); err == nil || settled {
		t.Fatalf("sweep with a failing commit settled=%v err=%v; want unsettled with the store error", settled, err)
	}

	fx.restart(t)
	fx.sweepUntilSettled(t, 5)

	ids, facts := fx.logicalFacts(t)
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("logical facts = %v, want the uninterrupted run's %v", ids, wantIDs)
	}
	for _, id := range ids {
		got, want := facts[id], wantFacts[id]
		got.SessionID, want.SessionID = "", ""
		if got != want {
			t.Fatalf("%s = %+v, want the uninterrupted run's %+v", id, got, want)
		}
	}
	if want := []string{"total:150", "total:300", "total:450", "total:600"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("logical facts = %v, want %v", ids, want)
	}
	progress, err := fx.factory.manager.InvocationUsageProgress(fx.id)
	if err != nil || progress.Cursor != "total:600" {
		t.Fatalf("final progress = %+v, %v; want cursor total:600", progress, err)
	}
}

// TestMessageRecordsCodexUsageOldestFirstThroughFailure runs a prompt op
// before any sweep, on a rollout whose backlog is past the 64 KiB tail, with
// the second fact write failing: the op records the oldest invocation and
// commits only through it, and the next op recovers the rest.
func TestMessageRecordsCodexUsageOldestFirstThroughFailure(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")
	store := beads.NewMemStore()
	manager := sessionpkg.NewManagerWithOptions(beadstest.Sessions(store), runtime.NewFake(), sessionpkg.WithCityPath(t.TempDir()))
	sink := &failNthModelSink{inner: usage.NewLocalSink(sinkPath), n: 2}
	handle, err := NewSessionHandle(SessionHandleConfig{
		Manager:     manager,
		SearchPaths: []string{searchBase},
		UsageSink:   sink,
		Session: SessionSpec{
			Profile:  ProfileCodexTmuxCLI,
			Template: "probe",
			Title:    "Probe",
			Command:  "codex",
			WorkDir:  workDir,
			Provider: "codex",
			Metadata: map[string]string{"agent_name": "myrig/polecat-1"},
		},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	actor := handleActor(t, handle, sessionpkg.ActorAgent)
	if err := handle.Start(context.Background(), actor); err != nil {
		t.Fatalf("Start: %v", err)
	}

	woke := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	at := func(sec int) string {
		return woke.Add(time.Duration(sec) * time.Second).Format("2006-01-02T15:04:05.000Z07:00")
	}
	lines := []map[string]any{
		codexWorkerSessionMeta(workDir),
		codexWorkerTurnContext(),
		// Before the wake: an earlier interval's invocation, not billed now.
		codexWorkerTokenCount(woke.Add(-2*time.Hour).Format(time.RFC3339), 100, 50, 0, 25),
		codexWorkerTokenCount(at(1), 150, 75, 0, 37),
		codexWorkerTokenCount(at(2), 300, 75, 0, 37),
	}
	lines = append(lines, codexToolOutputs(16, 8*1024)...)
	lines = append(lines, codexWorkerTokenCount(at(3), 450, 75, 0, 37))
	writeWorkerTestJSONL(t, codexWorkerRolloutPathWithID(t, searchBase, woke, codexRecorderRolloutID), lines)
	if err := store.SetMetadataBatch(handle.sessionID, map[string]string{
		"session_key":  codexRecorderRolloutID,
		"last_woke_at": woke.Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	factIDs := func() []string {
		t.Helper()
		facts, _, err := usage.ReadFacts(sinkPath)
		if err != nil {
			t.Fatalf("ReadFacts: %v", err)
		}
		var ids []string
		for _, f := range facts {
			if f.Kind == usage.KindModel {
				ids = append(ids, f.UpstreamReqID)
			}
		}
		sort.Strings(ids)
		return ids
	}

	if _, err := handle.Message(context.Background(), actor, MessageRequest{Text: "hello"}); err != nil {
		t.Fatalf("Message: %v", err)
	}
	if got, want := factIDs(), []string{"total:150"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("facts after the failing op = %v, want %v", got, want)
	}
	progress, err := manager.InvocationUsageProgress(handle.sessionID)
	if err != nil {
		t.Fatalf("InvocationUsageProgress: %v", err)
	}
	if progress.Cursor != "total:150" || !strings.Contains(progress.CodexCheckpoint, `"cursor":"total:150"`) {
		t.Fatalf("progress after the failing op = %+v, want it committed through total:150 only", progress)
	}

	if _, err := handle.Message(context.Background(), actor, MessageRequest{Text: "again"}); err != nil {
		t.Fatalf("second Message: %v", err)
	}
	if got, want := factIDs(), []string{"total:150", "total:300", "total:450"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("facts after the next op = %v, want %v", got, want)
	}
}

// TestSweepCodexUsageReplaysLegacyCursorUnderFloor starts from what an older
// gc left: a flat cursor at total:300, the fact it billed for that invocation,
// and no checkpoint. The sweep replays the current interval: the billed fact
// keeps its key (no second copy), the invocations the tail window lost are
// added, and an invocation before the interval's floor is not billed.
func TestSweepCodexUsageReplaysLegacyCursorUnderFloor(t *testing.T) {
	fx := newCodexRecorderFixture(t)
	lines := []map[string]any{
		codexWorkerSessionMeta(fx.meta["work_dir"]),
		fx.turnContext(-7200, "gpt-5.5"),
		fx.tokenCount(-7200, 100), // an earlier interval's invocation
		fx.tokenCount(1, 150),
	}
	lines = append(lines, codexToolOutputs(16, 8*1024)...)
	lines = append(lines, fx.tokenCount(2, 300))
	lines = append(lines, codexToolOutputs(16, 8*1024)...)
	lines = append(lines, fx.tokenCount(3, 450))
	fx.write(t, lines)

	legacy := usage.Fact{
		RunID:          "run-R",
		SessionID:      fx.id,
		Kind:           usage.KindModel,
		Model:          "gpt-5.5",
		Provider:       "codex",
		InputTokens:    1,
		UpstreamReqID:  "total:300",
		At:             fx.start.Add(2 * time.Second).UnixMilli(),
		IdempotencyKey: usage.ModelIdempotencyKey("run-R", "total:300"),
	}
	if err := usage.NewLocalSink(fx.sinkPath).Record(context.Background(), legacy); err != nil {
		t.Fatalf("seed legacy fact: %v", err)
	}
	if err := fx.store.SetMetadata(fx.id, sessionpkg.MetadataKeyInvocationUsageCursor, "total:300"); err != nil {
		t.Fatalf("seed legacy cursor: %v", err)
	}

	fx.sweepUntilSettled(t, 5)

	ids, byID := fx.logicalFacts(t)
	if want := []string{"total:150", "total:300", "total:450"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("logical facts = %v, want %v (total:100 predates the interval)", ids, want)
	}
	if got := byID["total:300"]; got != legacy {
		t.Fatalf("total:300 = %+v, want the legacy fact kept under its key %+v", got, legacy)
	}
	for _, id := range []string{"total:150", "total:450"} {
		if got, want := byID[id].IdempotencyKey, usage.ModelIdempotencyKey("run-R", id); got != want {
			t.Fatalf("%s key = %q, want %q", id, got, want)
		}
	}
}
