package coordstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/authorcore"
)

func TestChaosRunnerWritesKillArtifacts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	adapter := authorcore.New()
	if err := adapter.Open(ctx, coordstore.Config{DataDir: filepath.Join(dir, "store")}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer adapter.Close() //nolint:errcheck
	controller := &fakeChaosController{StoreAdapter: adapter}
	cfg := coordstore.SoakConfig{
		SoakPhase:      coordstore.SoakPhaseB,
		SoakDuration:   90 * time.Millisecond,
		KillCadence:    20 * time.Millisecond,
		SampleInterval: 10 * time.Millisecond,
		ResultsDir:     filepath.Join(dir, "results"),
		ScaleFactor:    1,
	}
	workload := coordstore.WorkloadConfig{
		Name:          "chaos-test",
		MainOpenCount: 20,
		WispOpenCount: 5,
		DepEdgeCount:  2,
		CreateRate:    2,
		PointReadRate: 2,
		BatchGetRate:  2,
		MailPollRate:  2,
		Duration:      time.Second,
		Concurrency:   1,
	}

	var progress bytes.Buffer
	result, err := coordstore.NewChaosRunner("fake", controller, workload, cfg).Run(ctx, &progress)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if controller.killCount == 0 || controller.restartCount == 0 {
		t.Fatalf("kill/restart counts = %d/%d", controller.killCount, controller.restartCount)
	}
	wantDir := filepath.Join(cfg.ResultsDir, result.RunID, "fake", "phase-b")
	if result.ResultsDir != wantDir {
		t.Fatalf("ResultsDir = %q, want %q", result.ResultsDir, wantDir)
	}
	for _, name := range []string{"kill-events.jsonl", "durability-report.txt"} {
		if _, err := os.Stat(filepath.Join(wantDir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(wantDir, "kill-events.jsonl"))
	if err != nil {
		t.Fatalf("read kill events: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 1 {
		t.Fatalf("no kill events written: %q", data)
	}
	var event coordstore.KillEvent
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("decode kill event: %v", err)
	}
	if event.KilledAt.IsZero() || event.RecoveryNanos <= 0 {
		t.Fatalf("kill event = %#v", event)
	}
	report, err := os.ReadFile(filepath.Join(wantDir, "durability-report.txt"))
	if err != nil {
		t.Fatalf("read durability report: %v", err)
	}
	if !bytes.Contains(report, []byte("Durability report: fake / chaos-test")) {
		t.Fatalf("report missing header: %s", report)
	}
}

func TestChaosRunnerChunksDurabilityBatchGet(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	adapter := authorcore.New()
	if err := adapter.Open(ctx, coordstore.Config{DataDir: filepath.Join(dir, "store")}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer adapter.Close() //nolint:errcheck
	controller := &fakeChaosController{StoreAdapter: adapter, maxBatchGetSize: 1000}
	cfg := coordstore.SoakConfig{
		SoakPhase:      coordstore.SoakPhaseB,
		SoakDuration:   50 * time.Millisecond,
		KillCadence:    5 * time.Millisecond,
		SampleInterval: 10 * time.Millisecond,
		ResultsDir:     filepath.Join(dir, "results"),
		ScaleFactor:    1,
	}
	workload := coordstore.WorkloadConfig{
		Name:          "chaos-batchget-chunk-test",
		MainOpenCount: 1105,
		PointReadRate: 1,
		Duration:      time.Second,
		Concurrency:   1,
	}

	var progress bytes.Buffer
	result, err := coordstore.NewChaosRunner("fake", controller, workload, cfg).Run(ctx, &progress)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.KillEvents) == 0 {
		t.Fatal("expected at least one kill event")
	}
	if result.KillEvents[0].AckedIDs < workload.MainOpenCount {
		t.Fatalf("AckedIDs = %d, want at least %d", result.KillEvents[0].AckedIDs, workload.MainOpenCount)
	}
	sizes := controller.batchGetSizes()
	if len(sizes) < 2 {
		t.Fatalf("BatchGet sizes = %v, want chunked calls", sizes)
	}
	sawFullChunk := false
	sawTailChunk := false
	for _, size := range sizes {
		if size > controller.maxBatchGetSize {
			t.Fatalf("BatchGet size %d exceeds max %d; all sizes=%v", size, controller.maxBatchGetSize, sizes)
		}
		if size == controller.maxBatchGetSize {
			sawFullChunk = true
		}
		if size == workload.MainOpenCount-controller.maxBatchGetSize {
			sawTailChunk = true
		}
	}
	if !sawFullChunk || !sawTailChunk {
		t.Fatalf("BatchGet sizes = %v, want 1000 and 105 chunks", sizes)
	}
}

type fakeChaosController struct {
	coordstore.StoreAdapter
	mu              sync.Mutex
	ackedIDs        []string
	lastAck         time.Time
	killCount       int
	restartCount    int
	maxBatchGetSize int
	batchGetCalls   []int
}

func (f *fakeChaosController) Create(ctx context.Context, r coordstore.Record) (coordstore.Record, error) {
	created, err := f.StoreAdapter.Create(ctx, r)
	if err != nil {
		return coordstore.Record{}, err
	}
	f.mu.Lock()
	f.ackedIDs = append(f.ackedIDs, created.ID)
	f.lastAck = time.Now()
	f.mu.Unlock()
	return created, nil
}

func (f *fakeChaosController) Update(ctx context.Context, id string, u coordstore.Update) error {
	if err := f.StoreAdapter.Update(ctx, id, u); err != nil {
		return err
	}
	f.mu.Lock()
	f.ackedIDs = append(f.ackedIDs, id)
	f.lastAck = time.Now()
	f.mu.Unlock()
	return nil
}

func (f *fakeChaosController) BatchGet(ctx context.Context, ids []string) ([]coordstore.Record, error) {
	f.mu.Lock()
	f.batchGetCalls = append(f.batchGetCalls, len(ids))
	maxSize := f.maxBatchGetSize
	f.mu.Unlock()
	if maxSize > 0 && len(ids) > maxSize {
		return nil, fmt.Errorf("BatchGet got %d IDs, max %d", len(ids), maxSize)
	}
	return f.StoreAdapter.BatchGet(ctx, ids)
}

func (f *fakeChaosController) Kill(context.Context) error {
	f.mu.Lock()
	f.killCount++
	f.mu.Unlock()
	return nil
}

func (f *fakeChaosController) Restart(context.Context) (time.Duration, error) {
	start := time.Now()
	time.Sleep(time.Millisecond)
	f.mu.Lock()
	f.restartCount++
	f.mu.Unlock()
	return time.Since(start), nil
}

func (f *fakeChaosController) LastAckTime() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastAck
}

func (f *fakeChaosController) AckedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ackedIDs...)
}

func (f *fakeChaosController) batchGetSizes() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.batchGetCalls...)
}
