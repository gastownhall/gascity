package main

import (
	"context"
	"errors"
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

func TestExecutePreparedStartWaveUsesWorkerBoundaryForKnownSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newSessionManagerWithConfig("", store, sp, nil)
	info, err := mgr.CreateSession(context.Background(), sessionpkg.CreateOptions{BeadOnly: true, Template: "worker", Title: "Worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude", Transport: "", Resume: sessionpkg.ProviderResume{}})
	if err != nil {
		t.Fatalf("CreateBeadOnly: %v", err)
	}
	bead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get bead: %v", err)
	}

	results := executePreparedStartWave(
		context.Background(),
		[]preparedStart{{
			candidate: startCandidate{
				info: sessiontest.SeedBead(t, bead),
				tp:   TemplateParams{TemplateName: "worker"},
			},
			cfg: runtime.Config{
				Command: "claude --resume seeded-session",
				WorkDir: info.WorkDir,
			},
		}},
		sp,
		store,
		10*time.Second,
	)
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].err != nil {
		t.Fatalf("start result err = %v, want nil", results[0].err)
	}

	got, err := mgr.Get(info.ID)
	if err != nil {
		t.Fatalf("Get session: %v", err)
	}
	if got.State != sessionpkg.StateStartPending {
		t.Fatalf("state = %q, want %q before lifecycle commit", got.State, sessionpkg.StateStartPending)
	}
	updatedBead, err := store.Get(info.ID)
	if err != nil {
		t.Fatalf("Get updated bead: %v", err)
	}
	if updatedBead.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want preserved before commit", updatedBead.Metadata["pending_create_claim"])
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("session should be running after prepared start")
	}
}

func TestStartPreparedStartCandidateUsesWorkerBoundaryForRuntimeOnlyTarget(t *testing.T) {
	sp := runtime.NewFake()

	usedWorker, err := startPreparedStartCandidate(
		context.Background(),
		preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "legacy-runtime-only", SessionNameMetadata: "legacy-runtime-only"},
				tp:   TemplateParams{TemplateName: "worker"},
			},
			cfg: runtime.Config{
				Command: "claude --resume seeded",
				WorkDir: t.TempDir(),
			},
		},
		"",
		nil,
		sp,
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("startPreparedStartCandidate: %v", err)
	}
	if !usedWorker {
		t.Fatal("usedWorker = false, want true")
	}
	if !sp.IsRunning("legacy-runtime-only") {
		t.Fatal("legacy-runtime-only should be running after prepared start")
	}
	var start runtime.Call
	foundStart := false
	for _, call := range sp.Calls {
		if call.Method == "Start" {
			start = call
			foundStart = true
			break
		}
	}
	if !foundStart {
		t.Fatalf("runtime calls = %#v, want Start", sp.Calls)
	}
	if start.Name != "legacy-runtime-only" {
		t.Fatalf("start name = %q, want legacy-runtime-only", start.Name)
	}
	if start.Config.Command != "claude --resume seeded" {
		t.Fatalf("start command = %q, want claude --resume seeded", start.Config.Command)
	}
}

type startUnavailableLivenessProvider struct {
	*runtime.Fake
	obs runtime.Liveness
}

func (p *startUnavailableLivenessProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return p.obs, fmt.Errorf("start preflight: %w", runtime.ErrRuntimeUnavailable)
}

// startRecoveryUnavailableProvider gives start recovery two different
// observations: an authoritative first preflight and an unavailable second
// observation after the start path returns an error. Its optional startErr is
// returned only after the fake runtime has reached the requested live state,
// which models ErrStateSync's "runtime changed, durable state did not" contract.
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

func TestStartPreparedStartCandidateDefersWhenLivenessUnavailable(t *testing.T) {
	sp := &startUnavailableLivenessProvider{Fake: runtime.NewFake()}

	started, err := startPreparedStartCandidate(
		context.Background(),
		preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "legacy-runtime-only", SessionNameMetadata: "legacy-runtime-only"},
				tp:   TemplateParams{TemplateName: "worker"},
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		},
		"",
		nil,
		sp,
		nil,
		nil,
		nil,
	)
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("startPreparedStartCandidate error = %v, want ErrRuntimeUnavailable", err)
	}
	if started {
		t.Fatal("started = true, want false while runtime liveness is unavailable")
	}
	if got := sp.CountCalls("Start", "legacy-runtime-only"); got != 0 {
		t.Fatalf("Start calls = %d, want 0 while existing-runtime absence is unconfirmed", got)
	}
	if got := sp.CountCalls("Stop", "legacy-runtime-only"); got != 0 {
		t.Fatalf("Stop calls = %d, want 0 while zombie status is unconfirmed", got)
	}
}

func TestExecutePreparedStartWaveDefersLivenessUnavailableWithoutLifecycleFailure(t *testing.T) {
	tests := []struct {
		name          string
		pendingCreate bool
	}{
		{name: "ordinary wake"},
		{name: "pending create", pendingCreate: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
			metadata := map[string]string{
				"session_name":  "worker",
				"template":      "worker",
				"state":         "asleep",
				"last_woke_at":  clk.Now().Format(time.RFC3339),
				"wake_attempts": "2",
			}
			if tc.pendingCreate {
				metadata["state"] = "creating"
				metadata["pending_create_claim"] = "true"
			}
			bead, err := store.Create(beads.Bead{
				ID:       "gc-worker",
				Title:    "worker",
				Type:     sessionBeadType,
				Labels:   []string{sessionBeadLabel},
				Metadata: metadata,
			})
			if err != nil {
				t.Fatalf("Create session: %v", err)
			}
			sp := &startUnavailableLivenessProvider{Fake: runtime.NewFake()}
			results := executePreparedStartWave(
				context.Background(),
				[]preparedStart{{
					candidate: startCandidate{
						info: sessiontest.SeedBead(t, bead),
						tp: TemplateParams{
							Command:      "claude",
							SessionName:  "worker",
							TemplateName: "worker",
						},
					},
					cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
				}},
				sp,
				store,
				10*time.Second,
			)
			if len(results) != 1 {
				t.Fatalf("len(results) = %d, want 1", len(results))
			}
			result := results[0]
			if result.err != nil {
				t.Fatalf("start result err = %v, want nil deferred result", result.err)
			}
			if result.outcome != TraceOutcomeDeferred {
				t.Fatalf("start result outcome = %q, want %q", result.outcome, TraceOutcomeDeferred)
			}
			if result.rollbackPending {
				t.Fatal("rollbackPending = true, want false while liveness is unavailable")
			}
			if commitStartResult(result, sessionFrontDoor(store), clk, events.Discard, 0, ioDiscard{}, ioDiscard{}) {
				t.Fatal("deferred liveness result should not count as a committed wake")
			}

			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get(%s): %v", bead.ID, err)
			}
			if got.Status != "open" {
				t.Fatalf("status = %q, want open", got.Status)
			}
			if got.Metadata["wake_attempts"] != "2" {
				t.Fatalf("wake_attempts = %q, want 2", got.Metadata["wake_attempts"])
			}
			if got.Metadata["last_woke_at"] != "" {
				t.Fatalf("last_woke_at = %q, want cleared for next-tick retry", got.Metadata["last_woke_at"])
			}
			if tc.pendingCreate && got.Metadata["pending_create_claim"] != "true" {
				t.Fatalf("pending_create_claim = %q, want true", got.Metadata["pending_create_claim"])
			}
			if got := sp.CountCalls("Start", "worker"); got != 0 {
				t.Fatalf("Start calls = %d, want 0", got)
			}
			if got := sp.CountCalls("Stop", "worker"); got != 0 {
				t.Fatalf("Stop calls = %d, want 0", got)
			}
		})
	}
}

func TestExecutePreparedStartWaveDefersErrStateSyncRecoveryWhenObservationUnavailable(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	workDir := t.TempDir()
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
	before := maps.Clone(bead.Metadata)
	sp := &startRecoveryUnavailableProvider{
		Fake:          runtime.NewFake(),
		unavailableAt: 2,
		startErr:      fmt.Errorf("persisting post-start state: %w", sessionpkg.ErrStateSync),
	}

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
	assertUnavailableStartRecoveryDeferred(t, store, clk, bead.ID, before, results[0])
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
	before := maps.Clone(bead.Metadata)
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
	assertUnavailableStartRecoveryDeferred(t, store, clk, bead.ID, before, results[0])
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
	if result.rollbackPending {
		t.Fatal("rollbackPending = true, want false while recovery liveness is unavailable")
	}
	if result.rateLimitScreen {
		t.Fatal("rateLimitScreen = true, want false while recovery liveness is unavailable")
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
	if got.Status != "open" {
		t.Fatalf("status = %q, want open", got.Status)
	}
	wantMetadata := maps.Clone(before)
	wantMetadata["last_woke_at"] = ""
	if !maps.Equal(got.Metadata, wantMetadata) {
		t.Fatalf("metadata after deferred recovery = %#v, want %#v", got.Metadata, wantMetadata)
	}
}
