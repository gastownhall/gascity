package worker

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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
	limits := fx.factory.codexUsageLimits
	factory, err := NewFactory(FactoryConfig{
		Store:       fx.store,
		Provider:    runtime.NewFake(),
		SearchPaths: []string{fx.root},
		UsageSink:   usage.NewLocalSink(fx.sinkPath),
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
