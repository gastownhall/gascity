package coordstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
	"github.com/gastownhall/gascity/internal/benchmarks/coordstore/adapters/boltdb"
)

func TestSoakRunnerWritesPhaseAArtifacts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	adapter := boltdb.New()
	dataDir := filepath.Join(dir, "store")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir store: %v", err)
	}
	if err := adapter.Open(ctx, coordstore.Config{DataDir: dataDir}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer adapter.Close() //nolint:errcheck

	cfg := coordstore.SoakConfig{
		SoakPhase:      coordstore.SoakPhaseA,
		SoakDuration:   150 * time.Millisecond,
		SampleInterval: 25 * time.Millisecond,
		ResultsDir:     filepath.Join(dir, "results"),
		DataDir:        dataDir,
		ScaleFactor:    1.0,
	}
	workload := coordstore.WorkloadConfig{
		Name:          "phase-a-test",
		MainOpenCount: 20,
		WispOpenCount: 10,
		DepEdgeCount:  2,
		CreateRate:    1,
		PointReadRate: 1,
		BatchGetRate:  1,
		MailPollRate:  1,
		Duration:      time.Second,
		Concurrency:   1,
	}

	var progress bytes.Buffer
	result, err := coordstore.NewSoakRunner("bbolt", adapter, workload, cfg).Run(ctx, &progress)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	wantDir := filepath.Join(cfg.ResultsDir, result.RunID, "bbolt", "phase-a")
	if result.ResultsDir != wantDir {
		t.Fatalf("ResultsDir = %q, want %q", result.ResultsDir, wantDir)
	}
	for _, name := range []string{"timeseries.jsonl", "scorecard.json", "scorecard.txt"} {
		if _, err := os.Stat(filepath.Join(wantDir, name)); err != nil {
			t.Fatalf("missing %s: %v", name, err)
		}
	}
	timeseries, err := os.ReadFile(filepath.Join(wantDir, "timeseries.jsonl"))
	if err != nil {
		t.Fatalf("read timeseries: %v", err)
	}
	if strings.Count(strings.TrimSpace(string(timeseries)), "\n")+1 < 1 {
		t.Fatalf("timeseries has no samples: %q", timeseries)
	}
	var scorecard coordstore.Scorecard
	scorecardData, err := os.ReadFile(filepath.Join(wantDir, "scorecard.json"))
	if err != nil {
		t.Fatalf("read scorecard json: %v", err)
	}
	if err := json.Unmarshal(scorecardData, &scorecard); err != nil {
		t.Fatalf("decode scorecard json: %v", err)
	}
	if scorecard.Backend != "bbolt" || scorecard.Workload != "phase-a-test" || scorecard.TotalOps == 0 {
		t.Fatalf("scorecard = %#v", scorecard)
	}
	textData, err := os.ReadFile(filepath.Join(wantDir, "scorecard.txt"))
	if err != nil {
		t.Fatalf("read scorecard text: %v", err)
	}
	if !bytes.Contains(textData, []byte("Scorecard: bbolt / phase-a-test")) {
		t.Fatalf("scorecard text missing header: %s", textData)
	}
}
