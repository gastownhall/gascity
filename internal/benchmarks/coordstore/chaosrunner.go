package coordstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// ChaosController is the StoreAdapter plus lifecycle surface ChaosRunner needs.
type ChaosController interface {
	StoreAdapter
	Kill(context.Context) error
	Restart(context.Context) (time.Duration, error)
	LastAckTime() time.Time
	AckedIDs() []string
}

type chaosAckLedgerResetter interface {
	ResetAckLedger(path string) error
}

const chaosDurabilityBatchGetLimit = 1000

// KillEvent records one kill/restart/durability probe.
type KillEvent struct {
	KilledAt        time.Time     `json:"killed_at"`
	LastAckAt       time.Time     `json:"last_ack_at,omitempty"`
	LossWindow      time.Duration `json:"loss_window"`
	LossWindowNanos int64         `json:"loss_window_nanos"`
	Recovery        time.Duration `json:"recovery"`
	RecoveryNanos   int64         `json:"recovery_nanos"`
	AckedIDs        int           `json:"acked_ids"`
	MissingIDs      []string      `json:"missing_ids,omitempty"`
}

// ChaosResult describes Phase B chaos artifacts.
type ChaosResult struct {
	RunID                string
	ResultsDir           string
	KillEventsPath       string
	AckedWritesPath      string
	DurabilityReportPath string
	KillEvents           []KillEvent
	Scorecard            Scorecard
}

// ChaosRunner drives a workload while killing and restarting a chaos child.
type ChaosRunner struct {
	backend    string
	controller ChaosController
	workload   WorkloadConfig
	cfg        SoakConfig
}

// NewChaosRunner creates a Phase B chaos runner.
func NewChaosRunner(backend string, controller ChaosController, workload WorkloadConfig, cfg SoakConfig) *ChaosRunner {
	return &ChaosRunner{backend: backend, controller: controller, workload: workload, cfg: cfg}
}

// Run executes the workload and writes Phase B chaos artifacts.
func (r *ChaosRunner) Run(ctx context.Context, w io.Writer) (ChaosResult, error) {
	if r.controller == nil {
		return ChaosResult{}, fmt.Errorf("chaos runner: controller is required")
	}
	cfg := normalizeSoakConfig(r.cfg)
	cfg.SoakPhase = SoakPhaseB
	if cfg.KillCadence <= 0 {
		cfg.KillCadence = 30 * time.Second
	}
	workload := cfg.ScaledWorkload(r.workload)
	runID := newRunID()
	resultDir := filepath.Join(cfg.ResultsDir, runID, r.backend, string(cfg.SoakPhase))
	if err := os.MkdirAll(resultDir, 0o755); err != nil {
		return ChaosResult{}, fmt.Errorf("creating chaos result dir: %w", err)
	}
	if err := r.controller.Reset(ctx); err != nil {
		return ChaosResult{}, fmt.Errorf("chaos reset: %w", err)
	}
	seed, err := NewSeeder(0x1234abcd).Seed(ctx, r.controller, workload)
	if err != nil {
		return ChaosResult{}, fmt.Errorf("chaos seed: %w", err)
	}
	ackedWritesPath := filepath.Join(resultDir, "acked-writes.jsonl")
	if resetter, ok := r.controller.(chaosAckLedgerResetter); ok {
		if err := resetter.ResetAckLedger(ackedWritesPath); err != nil {
			return ChaosResult{}, err
		}
	}
	runner := NewRunner(r.controller, workload, seed)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	scoreCh := make(chan struct {
		score Scorecard
		err   error
	}, 1)
	go func() {
		score, err := runner.Run(runCtx, w)
		scoreCh <- struct {
			score Scorecard
			err   error
		}{score: score, err: err}
	}()

	killEventsPath := filepath.Join(resultDir, "kill-events.jsonl")
	cadence := cfg.KillCadence
	ticker := time.NewTicker(cadence)
	defer ticker.Stop()
	var events []KillEvent
	var score Scorecard
	for {
		select {
		case res := <-scoreCh:
			if res.err != nil {
				return ChaosResult{}, res.err
			}
			score = res.score
			score.Backend = r.backend
			if err := writeDurabilityReport(filepath.Join(resultDir, "durability-report.txt"), r.backend, workload.Name, events); err != nil {
				return ChaosResult{}, err
			}
			return ChaosResult{
				RunID:                runID,
				ResultsDir:           resultDir,
				KillEventsPath:       killEventsPath,
				AckedWritesPath:      ackedWritesPath,
				DurabilityReportPath: filepath.Join(resultDir, "durability-report.txt"),
				KillEvents:           events,
				Scorecard:            score,
			}, nil
		case <-ticker.C:
			event, err := r.killAndRecover(ctx)
			if err != nil {
				cancel()
				return ChaosResult{}, err
			}
			events = append(events, event)
			if err := appendKillEvent(killEventsPath, event); err != nil {
				cancel()
				return ChaosResult{}, err
			}
			minCadence := event.Recovery * 2
			if minCadence > cadence {
				cadence = minCadence
				ticker.Reset(cadence)
			}
		case <-ctx.Done():
			cancel()
			return ChaosResult{}, ctx.Err()
		}
	}
}

func (r *ChaosRunner) killAndRecover(ctx context.Context) (KillEvent, error) {
	lastAck := r.controller.LastAckTime()
	killedAt := time.Now().UTC()
	if err := r.controller.Kill(ctx); err != nil {
		return KillEvent{}, err
	}
	recoveryStart := time.Now()
	restartDur, err := r.controller.Restart(ctx)
	if err != nil {
		return KillEvent{}, err
	}
	if _, err := r.controller.PrimeScan(ctx); err != nil {
		return KillEvent{}, err
	}
	recovery := time.Since(recoveryStart)
	if restartDur > recovery {
		recovery = restartDur
	}
	ackedIDs := r.controller.AckedIDs()
	records, err := r.batchGetAckedIDs(ctx, ackedIDs)
	if err != nil {
		return KillEvent{}, err
	}
	found := make(map[string]struct{}, len(records))
	for _, record := range records {
		found[record.ID] = struct{}{}
	}
	var missing []string
	for _, id := range ackedIDs {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	lossWindow := time.Duration(0)
	if !lastAck.IsZero() {
		lossWindow = killedAt.Sub(lastAck)
	}
	return KillEvent{
		KilledAt:        killedAt,
		LastAckAt:       lastAck,
		LossWindow:      lossWindow,
		LossWindowNanos: lossWindow.Nanoseconds(),
		Recovery:        recovery,
		RecoveryNanos:   recovery.Nanoseconds(),
		AckedIDs:        len(ackedIDs),
		MissingIDs:      missing,
	}, nil
}

func (r *ChaosRunner) batchGetAckedIDs(ctx context.Context, ids []string) ([]Record, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	records := make([]Record, 0, len(ids))
	for start := 0; start < len(ids); start += chaosDurabilityBatchGetLimit {
		end := start + chaosDurabilityBatchGetLimit
		if end > len(ids) {
			end = len(ids)
		}
		batch, err := r.controller.BatchGet(ctx, ids[start:end])
		if err != nil {
			return nil, fmt.Errorf("chaos durability BatchGet ids[%d:%d]: %w", start, end, err)
		}
		records = append(records, batch...)
	}
	return records, nil
}

func appendKillEvent(path string, event KillEvent) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	data, err := json.Marshal(event)
	if err != nil {
		file.Close() //nolint:errcheck
		return err
	}
	data = append(data, '\n')
	if _, err := file.Write(data); err != nil {
		file.Close() //nolint:errcheck
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close() //nolint:errcheck
		return err
	}
	return file.Close()
}

func writeDurabilityReport(path, backend, workload string, events []KillEvent) error {
	missing := 0
	for _, event := range events {
		missing += len(event.MissingIDs)
	}
	body := fmt.Sprintf("Durability report: %s / %s\nkills=%d\nmissing_acked_ids=%d\n", backend, workload, len(events), missing)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("writing durability report: %w", err)
	}
	return nil
}
