package coordstore

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"
)

func TestRealWorldWorkloadIncludesTerminalRetention(t *testing.T) {
	if RealWorldWorkload.PurgeTerminalRate <= 0 {
		t.Fatalf("RealWorldWorkload.PurgeTerminalRate = %v, want enabled", RealWorldWorkload.PurgeTerminalRate)
	}
	if RealWorldWorkload.PurgeTerminalOlderThan != 10*time.Minute {
		t.Fatalf("RealWorldWorkload.PurgeTerminalOlderThan = %s, want 10m", RealWorldWorkload.PurgeTerminalOlderThan)
	}
}

func TestRunnerSchedulesAndExecutesPurgeTerminal(t *testing.T) {
	ctx := context.Background()
	adapter := &purgeTrackingAdapter{}
	runner := NewRunner(adapter, WorkloadConfig{
		Name:                   "terminal-retention",
		PurgeTerminalRate:      0.033,
		PurgeTerminalOlderThan: 10 * time.Minute,
	}, SeedResult{MainOpenIDs: []string{"main-1"}})

	found := false
	for _, op := range runner.buildSchedule() {
		if op == opPurgeTerminal {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("buildSchedule did not include opPurgeTerminal")
	}
	if got := opName(opPurgeTerminal); got != "PurgeTerminal" {
		t.Fatalf("opName(opPurgeTerminal) = %q, want PurgeTerminal", got)
	}

	rng := rand.New(rand.NewPCG(1, 2))
	if err := runner.execOp(ctx, opPurgeTerminal, rng); err != nil {
		t.Fatalf("execOp(opPurgeTerminal): %v", err)
	}
	if adapter.purgeCalls != 1 {
		t.Fatalf("PurgeTerminal calls = %d, want 1", adapter.purgeCalls)
	}
	if adapter.purgeOlderThan != 10*time.Minute {
		t.Fatalf("PurgeTerminal olderThan = %s, want 10m", adapter.purgeOlderThan)
	}
}

func TestRunnerThrottlesPurgeTerminalRate(t *testing.T) {
	ctx := context.Background()
	adapter := &purgeTrackingAdapter{}
	runner := NewRunner(adapter, WorkloadConfig{
		Name:                   "terminal-retention",
		PurgeTerminalRate:      0.033,
		PurgeTerminalOlderThan: 10 * time.Minute,
	}, SeedResult{MainOpenIDs: []string{"main-1"}})
	runner.lastTerminalPurge = time.Now()

	rng := rand.New(rand.NewPCG(3, 4))
	if err := runner.execOp(ctx, opPurgeTerminal, rng); err != nil {
		t.Fatalf("execOp(opPurgeTerminal): %v", err)
	}
	if adapter.purgeCalls != 0 {
		t.Fatalf("PurgeTerminal calls = %d, want throttled", adapter.purgeCalls)
	}
}

func TestRunnerPrimesTerminalRetentionUntilBatchDrained(t *testing.T) {
	ctx := context.Background()
	adapter := &purgeTrackingAdapter{purgeResults: []int{
		TerminalPurgeBatchSize,
		TerminalPurgeBatchSize,
		7,
	}}
	runner := NewRunner(adapter, WorkloadConfig{
		Name:                   "terminal-retention",
		PurgeTerminalRate:      0.033,
		PurgeTerminalOlderThan: 10 * time.Minute,
	}, SeedResult{MainOpenIDs: []string{"main-1"}})

	if err := runner.primeTerminalRetention(ctx); err != nil {
		t.Fatalf("primeTerminalRetention: %v", err)
	}
	if adapter.purgeCalls != 3 {
		t.Fatalf("PurgeTerminal calls = %d, want 3", adapter.purgeCalls)
	}
}

type purgeTrackingAdapter struct {
	purgeCalls     int
	purgeOlderThan time.Duration
	purgeResults   []int
}

func (a *purgeTrackingAdapter) Open(context.Context, Config) error { return nil }
func (a *purgeTrackingAdapter) Close() error                       { return nil }
func (a *purgeTrackingAdapter) Reset(context.Context) error        { return nil }
func (a *purgeTrackingAdapter) Create(context.Context, Record) (Record, error) {
	return Record{}, nil
}

func (a *purgeTrackingAdapter) Get(context.Context, string) (Record, error) {
	return Record{}, ErrNotFound
}
func (a *purgeTrackingAdapter) Update(context.Context, string, Update) error { return nil }
func (a *purgeTrackingAdapter) Delete(context.Context, string) error         { return nil }
func (a *purgeTrackingAdapter) FilterScan(context.Context, Query) ([]Record, error) {
	return nil, nil
}

func (a *purgeTrackingAdapter) BatchGet(context.Context, []string) ([]Record, error) {
	return nil, nil
}

func (a *purgeTrackingAdapter) SetMetadataBatch(context.Context, string, map[string]string) error {
	return nil
}

func (a *purgeTrackingAdapter) Ready(context.Context, ReadyQuery) ([]Record, error) {
	return nil, nil
}

func (a *purgeTrackingAdapter) DepAdd(context.Context, string, string, string) error {
	return nil
}

func (a *purgeTrackingAdapter) DepRemove(context.Context, string, string) error {
	return nil
}

func (a *purgeTrackingAdapter) DepList(context.Context, string, string) ([]Dep, error) {
	return nil, nil
}
func (a *purgeTrackingAdapter) PurgeExpired(context.Context) (int, error) { return 0, nil }
func (a *purgeTrackingAdapter) PurgeTerminal(_ context.Context, olderThan time.Duration) (int, error) {
	a.purgeCalls++
	a.purgeOlderThan = olderThan
	if len(a.purgeResults) == 0 {
		return 0, nil
	}
	result := a.purgeResults[0]
	a.purgeResults = a.purgeResults[1:]
	return result, nil
}
func (a *purgeTrackingAdapter) PrimeScan(context.Context) (int, error) { return 0, nil }
func (a *purgeTrackingAdapter) RecentScan(context.Context, int) ([]Record, error) {
	return nil, nil
}
func (a *purgeTrackingAdapter) Stats(context.Context) map[string]int64 { return nil }
