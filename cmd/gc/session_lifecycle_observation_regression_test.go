package main

import (
	"context"
	"fmt"
	"maps"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// startRecoveryUnavailableProvider makes the post-error observation fail after
// an authoritative preflight. startErr models ErrStateSync after the runtime
// was successfully created but its durable projection was not.
type startRecoveryUnavailableProvider struct {
	*runtime.Fake
	observationCalls atomic.Int64
	unavailableAt    int64
	startErr         error
}

func (p *startRecoveryUnavailableProvider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	if p.observationCalls.Add(1) == p.unavailableAt {
		return runtime.Liveness{}, fmt.Errorf("recovery observation: %w", runtime.ErrRuntimeUnavailable)
	}
	running := p.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}, nil
}

func (p *startRecoveryUnavailableProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"} {
		if value := cfg.Env[key]; value != "" {
			if err := p.SetMeta(name, key, value); err != nil {
				return err
			}
		}
	}
	return p.startErr
}

func createStartRecoveryFixture(t *testing.T, store beads.Store, clk clock.Clock, workDir string) (beads.Bead, beads.StringMap) {
	t.Helper()
	bead, err := store.Create(beads.Bead{
		ID:     "gc-worker",
		Title:  "worker",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":              "worker",
			"template":                  "worker",
			"state":                     "creating",
			"pending_create_claim":      "true",
			"pending_create_started_at": clk.Now().Format(time.RFC3339),
			"provider":                  "claude",
			"transport":                 "tmux",
			"command":                   "claude",
			"work_dir":                  workDir,
			"generation":                "1",
			"continuation_epoch":        "7",
			"instance_token":            "tok-worker",
			"session_key":               "resume-key",
			"resume_flag":               "--resume",
			"resume_style":              "flag",
			"started_config_hash":       "keep-hash",
			"wake_attempts":             "2",
			"last_woke_at":              clk.Now().Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	return bead, maps.Clone(bead.Metadata)
}

func executeStartRecovery(t *testing.T, store beads.Store, sp runtime.Provider, bead beads.Bead, workDir string) startResult {
	t.Helper()
	results := executePreparedStartWave(
		context.Background(),
		[]preparedStart{{
			candidate: startCandidate{
				info: sessiontest.SeedBead(t, bead),
				tp: TemplateParams{
					Command:      "claude --resume resume-key",
					SessionName:  "worker",
					TemplateName: "worker",
				},
			},
			cfg: runtime.Config{Command: "claude --resume resume-key", WorkDir: workDir},
		}},
		sp,
		store,
		10*time.Second,
	)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	return results[0]
}

func TestExecutePreparedStartWaveDefersErrStateSyncRecoveryWhenObservationUnavailable(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	workDir := t.TempDir()
	bead, before := createStartRecoveryFixture(t, store, clk, workDir)
	sp := &startRecoveryUnavailableProvider{
		Fake:          runtime.NewFake(),
		unavailableAt: 2,
		startErr:      fmt.Errorf("persisting post-start state: %w", sessionpkg.ErrStateSync),
	}

	result := executeStartRecovery(t, store, sp, bead, workDir)
	assertUnavailableStartRecoveryDeferred(t, store, clk, bead.ID, before, result)
	if got := sp.CountCalls("Start", "worker"); got != 1 {
		t.Fatalf("Start calls = %d, want the one start that reached the runtime before ErrStateSync", got)
	}
	if got := sp.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("Stop calls = %d, want 0 while recovery liveness is unavailable", got)
	}
}

func TestExecutePreparedStartWaveDefersErrSessionExistsRecoveryWhenObservationUnavailable(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 30, 0, 0, time.UTC)}
	workDir := t.TempDir()
	bead, before := createStartRecoveryFixture(t, store, clk, workDir)
	sp := &startRecoveryUnavailableProvider{Fake: runtime.NewFake(), unavailableAt: 2}
	if err := sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start existing runtime: %v", err)
	}
	if err := sp.SetMeta("worker", "GC_SESSION_ID", "gc-previous-incarnation"); err != nil {
		t.Fatalf("SetMeta existing session ID: %v", err)
	}
	if err := sp.SetMeta("worker", "GC_INSTANCE_TOKEN", "tok-previous"); err != nil {
		t.Fatalf("SetMeta existing instance token: %v", err)
	}
	startsBefore := sp.CountCalls("Start", "worker")

	result := executeStartRecovery(t, store, sp, bead, workDir)
	assertUnavailableStartRecoveryDeferred(t, store, clk, bead.ID, before, result)
	if got := sp.CountCalls("Start", "worker"); got != startsBefore {
		t.Fatalf("Start calls = %d, want unchanged at %d while collision recovery liveness is unavailable", got, startsBefore)
	}
	if got := sp.CountCalls("Stop", "worker"); got != 0 {
		t.Fatalf("Stop calls = %d, want 0 while collision recovery liveness is unavailable", got)
	}
}

func assertUnavailableStartRecoveryDeferred(
	t *testing.T,
	store beads.Store,
	clk clock.Clock,
	beadID string,
	before beads.StringMap,
	result startResult,
) {
	t.Helper()
	if result.err != nil {
		t.Fatalf("start result err = %v, want nil deferred result", result.err)
	}
	if result.outcome != TraceOutcomeDeferred {
		t.Fatalf("start result outcome = %q, want %q", result.outcome, TraceOutcomeDeferred)
	}
	if result.rollbackPending || result.rateLimitScreen {
		t.Fatalf("deferred recovery result has destructive flags: rollback=%v rate-limit=%v", result.rollbackPending, result.rateLimitScreen)
	}
	rec := events.NewFake()
	if commitStartResult(result, sessionFrontDoor(store), clk, rec, 0, ioDiscard{}, ioDiscard{}) {
		t.Fatal("deferred recovery result should not count as a committed wake")
	}
	if len(rec.Events) != 0 {
		t.Fatalf("lifecycle events = %#v, want none for deferred recovery", rec.Events)
	}

	got, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("Get(%s): %v", beadID, err)
	}
	wantMetadata := maps.Clone(before)
	wantMetadata["last_woke_at"] = ""
	if got.Status != "open" || !maps.Equal(got.Metadata, wantMetadata) {
		t.Fatalf("session after deferred recovery: status=%q metadata=%#v, want open %#v", got.Status, got.Metadata, wantMetadata)
	}
}
