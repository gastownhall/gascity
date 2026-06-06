package coordstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// SoakResult describes the artifacts produced by a SoakRunner run.
type SoakResult struct {
	RunID             string
	ResultsDir        string
	TimeSeriesPath    string
	ScorecardJSONPath string
	ScorecardTextPath string
	Scorecard         Scorecard
}

// SoakRunner wraps Runner with telemetry and artifact writing.
type SoakRunner struct {
	backend  string
	adapter  StoreAdapter
	workload WorkloadConfig
	cfg      SoakConfig
}

// NewSoakRunner creates an in-process soak runner for one backend.
func NewSoakRunner(backend string, adapter StoreAdapter, workload WorkloadConfig, cfg SoakConfig) *SoakRunner {
	return &SoakRunner{backend: backend, adapter: adapter, workload: workload, cfg: cfg}
}

// Run seeds the adapter, records time-series telemetry during the workload,
// and writes final scorecard artifacts.
func (r *SoakRunner) Run(ctx context.Context, w io.Writer) (SoakResult, error) {
	if r.adapter == nil {
		return SoakResult{}, fmt.Errorf("soak runner: adapter is required")
	}
	cfg := normalizeSoakConfig(r.cfg)
	workload := cfg.ScaledWorkload(r.workload)
	runID := newRunID()
	resultDir := filepath.Join(cfg.ResultsDir, runID, r.backend, string(cfg.SoakPhase))
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		return SoakResult{}, fmt.Errorf("creating soak result dir: %w", err)
	}

	if err := r.adapter.Reset(ctx); err != nil {
		return SoakResult{}, fmt.Errorf("soak reset: %w", err)
	}
	seed, err := NewSeeder(0x1234abcd).Seed(ctx, r.adapter, workload)
	if err != nil {
		return SoakResult{}, fmt.Errorf("soak seed: %w", err)
	}
	runner := NewRunner(r.adapter, workload, seed)
	recorder := NewTimeSeriesRecorder(TimeSeriesRecorderConfig{
		Path:              filepath.Join(resultDir, "timeseries.jsonl"),
		DataDir:           cfg.DataDir,
		SampleInterval:    cfg.SampleInterval,
		Adapter:           r.adapter,
		HistogramSnapshot: runner.HistogramSnapshot,
	})
	if err := recorder.Start(ctx); err != nil {
		return SoakResult{}, err
	}
	scorecard, runErr := runner.Run(ctx, w)
	stopErr := recorder.Stop()
	if runErr != nil {
		return SoakResult{}, runErr
	}
	if stopErr != nil {
		return SoakResult{}, stopErr
	}
	scorecard.Backend = r.backend

	scorecardJSONPath := filepath.Join(resultDir, "scorecard.json")
	if err := writeScorecardJSON(scorecardJSONPath, scorecard); err != nil {
		return SoakResult{}, err
	}
	scorecardTextPath := filepath.Join(resultDir, "scorecard.txt")
	if err := writeScorecardText(scorecardTextPath, scorecard); err != nil {
		return SoakResult{}, err
	}
	return SoakResult{
		RunID:             runID,
		ResultsDir:        resultDir,
		TimeSeriesPath:    filepath.Join(resultDir, "timeseries.jsonl"),
		ScorecardJSONPath: scorecardJSONPath,
		ScorecardTextPath: scorecardTextPath,
		Scorecard:         scorecard,
	}, nil
}

func normalizeSoakConfig(cfg SoakConfig) SoakConfig {
	if cfg.SoakPhase == "" {
		cfg.SoakPhase = SoakPhaseA
	}
	if cfg.SampleInterval <= 0 {
		cfg.SampleInterval = time.Second
	}
	if cfg.ResultsDir == "" {
		cfg.ResultsDir = filepath.Join(os.TempDir(), "coordstore-soak")
	}
	if cfg.ScaleFactor <= 0 {
		cfg.ScaleFactor = 1
	}
	return cfg
}

func newRunID() string {
	return time.Now().UTC().Format("20060102T150405.000000000Z")
}

func writeScorecardJSON(path string, scorecard Scorecard) error {
	data, err := json.MarshalIndent(scorecard, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling scorecard json: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing scorecard json: %w", err)
	}
	return nil
}

func writeScorecardText(path string, scorecard Scorecard) error {
	var buf bytes.Buffer
	scorecard.PrintTable(&buf)
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("writing scorecard text: %w", err)
	}
	return nil
}
