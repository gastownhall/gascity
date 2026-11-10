package worker

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/usage"
)

func TestInvocationUsageFamilyCopilot(t *testing.T) {
	for _, provider := range []string{"copilot", "github-copilot", "builtin:copilot"} {
		family, ok := InvocationUsageFamily(provider)
		if family != "copilot" || !ok {
			t.Errorf("InvocationUsageFamily(%q) = %q, %v; want copilot, true", provider, family, ok)
		}
	}
	if _, ok := invocationUsageSpecs["copilot"]; !ok {
		t.Fatal("copilot spec missing from invocationUsageSpecs")
	}
}

func writeCopilotSessionLog(t *testing.T, root, sessionID, cwd string, started time.Time, records ...[4]int) {
	t.Helper()
	dir := filepath.Join(root, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"type":"session.start","data":{"startTime":%q,"context":{"cwd":%q}},"id":"start-%s","timestamp":%q}`+"\n",
		started.UTC().Format(time.RFC3339Nano), cwd, sessionID, started.UTC().Format(time.RFC3339Nano))
	for i, r := range records {
		ts := started.Add(time.Duration(i+1) * time.Second).UTC().Format(time.RFC3339Nano)
		body += fmt.Sprintf(`{"type":"session.usage_record","data":{"usage":{"model":"claude-opus-5.5","inputTokens":%d,"outputTokens":%d,"cacheReadTokens":%d,"cacheWriteTokens":%d,"reasoningTokens":0,"cost":15.0,"apiCallId":"%s-call-%d"}},"id":"%s-ev-%d","timestamp":%q}`+"\n",
			r[0], r[1], r[2], r[3], sessionID, i, sessionID, i, ts)
	}
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := started.Add(time.Minute)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
}

// TestFactorySweepSessionModelUsageKeylessCopilotPoolSeat sweeps a Copilot pool
// seat that has no session_key. The seat's work dir holds an earlier session's
// log too, so the newest-for-workdir rule would be wrong; the wake window must
// pick only this wake's log, and the facts carry its tokens.
func TestFactorySweepSessionModelUsageKeylessCopilotPoolSeat(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")

	factory, err := NewFactory(FactoryConfig{
		Store:       beads.NewMemStore(),
		Provider:    runtime.NewFake(),
		SearchPaths: []string{root},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	start := time.Date(2026, 11, 10, 21, 21, 54, 0, time.UTC)
	slept := start.Add(3 * time.Minute)
	// The seat's previous session: same cwd, an hour earlier. Its 9999 output
	// would betray a wrong pick.
	writeCopilotSessionLog(t, root, "earlier", workDir, start.Add(-time.Hour), [4]int{100, 9999, 0, 0})
	// This wake: two model calls.
	writeCopilotSessionLog(t, root, "this-wake", workDir, start.Add(-3*time.Second),
		[4]int{33701, 134, 0, 33697}, [4]int{34201, 120, 33697, 502})

	meta := map[string]string{
		"provider":            "copilot",
		"work_dir":            workDir,
		"awake_started_at":    start.Format(time.RFC3339),
		"last_woke_at":        start.Add(-10 * time.Second).Format(time.RFC3339),
		"slept_at":            slept.Format(time.RFC3339),
		"session_name":        "bd__dog-hc-wisp-1",
		"molecule_id":         "run-C",
		"gc.active_work_bead": "run-C.step-1",
	}
	now := slept.Add(time.Minute)
	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), "hc-wisp-1", meta, now)
	if err != nil || !settled || emitted != 2 {
		t.Fatalf("sweep = %d, %v, %v; want 2, true, nil", emitted, settled, err)
	}

	facts, warnings, err := usage.ReadFacts(sinkPath)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("ReadFacts: %v %v", err, warnings)
	}
	if len(facts) != 2 {
		t.Fatalf("want 2 model facts, got %d: %+v", len(facts), facts)
	}
	for _, f := range facts {
		if f.Kind != usage.KindModel || f.Provider != "copilot" || f.RunID != "run-C" || f.Model != "claude-opus-5.5" {
			t.Errorf("fact = %+v, want model/copilot/run-C/claude-opus-5.5", f)
		}
		if f.OutputTokens == 9999 {
			t.Fatalf("fact came from the seat's earlier session log: %+v", f)
		}
	}
	if f := facts[0]; f.UpstreamReqID != "this-wake-call-0" || f.InputTokens != 4 || f.CacheCreationTokens != 33697 || f.OutputTokens != 134 {
		t.Errorf("first fact = %+v, want this-wake-call-0 with input 4, cache write 33697, output 134", f)
	}

	// A re-sweep with the advanced cursor emits nothing new.
	meta["invocation_usage_cursor"] = "this-wake-call-1"
	if emitted, _, err := factory.SweepSessionModelUsage(context.Background(), "hc-wisp-1", meta, now); err != nil || emitted != 0 {
		t.Fatalf("re-sweep = %d, %v; want 0, nil", emitted, err)
	}
}

// TestFactorySweepSessionModelUsageKeyedCopilot resolves the log by session_key
// (the session-state directory name) when the bead carries one.
func TestFactorySweepSessionModelUsageKeyedCopilot(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	sinkPath := filepath.Join(t.TempDir(), "usage.jsonl")
	factory, err := NewFactory(FactoryConfig{
		Store:       beads.NewMemStore(),
		Provider:    runtime.NewFake(),
		SearchPaths: []string{root},
		UsageSink:   usage.NewLocalSink(sinkPath),
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}
	started := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	writeCopilotSessionLog(t, root, "cad63965-22b9-4044-9c93-83aaa29f2873", workDir, started, [4]int{1000, 10, 0, 0})
	writeCopilotSessionLog(t, root, "other", workDir, started.Add(time.Second), [4]int{5, 5, 0, 0})

	meta := map[string]string{
		"provider":    "copilot",
		"work_dir":    workDir,
		"session_key": "cad63965-22b9-4044-9c93-83aaa29f2873",
		"molecule_id": "run-K",
	}
	emitted, settled, err := factory.SweepSessionModelUsage(context.Background(), "hc-keyed", meta, started.Add(time.Hour))
	if err != nil || !settled || emitted != 1 {
		t.Fatalf("sweep = %d, %v, %v; want 1, true, nil", emitted, settled, err)
	}
	facts, _, err := usage.ReadFacts(sinkPath)
	if err != nil || len(facts) != 1 || facts[0].InputTokens != 1000 {
		t.Fatalf("facts = %+v, %v; want the keyed log's one call", facts, err)
	}
}
