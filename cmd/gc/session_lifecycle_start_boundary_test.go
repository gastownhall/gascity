package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

type blockingStartProvider struct {
	runtime.Provider
	started chan struct{}
	release chan struct{}
}

func (p *blockingStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	close(p.started)
	select {
	case <-p.release:
		return p.Provider.Start(ctx, name, cfg)
	case <-ctx.Done():
		return ctx.Err()
	}
}

type startUnavailableLivenessProvider struct {
	*runtime.Fake
}

func (p *startUnavailableLivenessProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	return runtime.ObserveLiveness(p.Fake, name, processNames), fmt.Errorf("start preflight: %w", runtime.ErrRuntimeUnavailable)
}

type startConfirmedAbsentProvider struct {
	*runtime.Fake
}

func (p *startConfirmedAbsentProvider) ObserveLivenessWithError(string, []string) (runtime.Liveness, error) {
	return runtime.Liveness{}, nil
}

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

func TestStartPreparedStartCandidateRefusesRigSuspendedBeforeSpawn(t *testing.T) {
	cityPath := t.TempDir()
	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, "aoa", &suspended); err != nil {
		t.Fatalf("SetRigSuspended: %v", err)
	}
	sp := runtime.NewFake()
	rig := config.Rig{Name: "aoa", Path: "/rig/aoa"}
	_, err := startPreparedStartCandidate(context.Background(), preparedStart{
		candidate: startCandidate{
			info: sessionpkg.Info{SessionName: "aoa--worker-1", SessionNameMetadata: "aoa--worker-1"},
			tp:   TemplateParams{TemplateName: "worker", RigName: "aoa"},
		},
		cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
	}, cityPath, nil, sp, &config.City{Rigs: []config.Rig{rig}}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), `rig "aoa" is suspended`) {
		t.Fatalf("error = %v, want suspended rig refusal", err)
	}
	if sp.CountCalls("Start", "aoa--worker-1") != 0 {
		t.Fatal("provider Start ran after suspension landed before spawn")
	}
}

func TestStartPreparedStartCandidateDoesNotKillSessionSuspendedDuringSpawn(t *testing.T) {
	cityPath := t.TempDir()
	fake := runtime.NewFake()
	sp := &blockingStartProvider{Provider: fake, started: make(chan struct{}), release: make(chan struct{})}
	rig := config.Rig{Name: "aoa", Path: "/rig/aoa"}
	done := make(chan error, 1)
	go func() {
		_, err := startPreparedStartCandidate(context.Background(), preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "aoa--worker-1", SessionNameMetadata: "aoa--worker-1"},
				tp:   TemplateParams{TemplateName: "worker", RigName: "aoa"},
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		}, cityPath, nil, sp, &config.City{Rigs: []config.Rig{rig}}, nil, nil, nil)
		done <- err
	}()
	<-sp.started
	suspended := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, "aoa", &suspended); err != nil {
		t.Fatalf("SetRigSuspended: %v", err)
	}
	close(sp.release)
	if err := <-done; err != nil {
		t.Fatalf("in-flight start returned %v, want completion", err)
	}
	if !fake.IsRunning("aoa--worker-1") {
		t.Fatal("session crossing the suspension boundary was killed")
	}
	if fake.CountCalls("Stop", "aoa--worker-1") != 0 {
		t.Fatal("suspension killed an in-flight session")
	}
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
		nil,
	)
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("startPreparedStartCandidate error = %v, want runtime unavailable", err)
	}
	if started {
		t.Fatal("started = true, want false while runtime absence is unconfirmed")
	}
	if got := sp.CountCalls("Start", "legacy-runtime-only"); got != 0 {
		t.Fatalf("Start calls = %d, want 0", got)
	}
	if got := sp.CountCalls("Stop", "legacy-runtime-only"); got != 0 {
		t.Fatalf("Stop calls = %d, want 0", got)
	}
}

func TestStartPreparedStartCandidateConvergesFromConfirmedAbsence(t *testing.T) {
	sp := &startConfirmedAbsentProvider{Fake: runtime.NewFake()}
	started, err := startPreparedStartCandidate(
		context.Background(),
		preparedStart{
			candidate: startCandidate{
				info: sessionpkg.Info{SessionName: "worker", SessionNameMetadata: "worker"},
				tp:   TemplateParams{TemplateName: "worker"},
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		},
		"", nil, sp, nil, nil, nil, nil,
	)
	if err != nil || !started {
		t.Fatalf("startPreparedStartCandidate = (%v, %v), want started without error", started, err)
	}
	if got := sp.CountCalls("Start", "worker"); got != 1 {
		t.Fatalf("Start calls = %d, want 1 after confirmed absence", got)
	}
}

func TestExecutePreparedStartWaveDefersUnavailableWithoutRollbackOrWakeFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pendingCreate bool
	}{
		{name: "ordinary wake"},
		{name: "pending create", pendingCreate: true},
	} {
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
				ID: "gc-worker", Title: "worker", Type: sessionBeadType,
				Labels: []string{sessionBeadLabel}, Metadata: metadata,
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
						tp:   TemplateParams{Command: "claude", SessionName: "worker", TemplateName: "worker"},
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
			if result.err != nil || result.outcome != TraceOutcomeDeferred || result.rollbackPending || result.rateLimitScreen {
				t.Fatalf("result = %#v, want deferred without error, rollback, or rate-limit screening", result)
			}
			if commitStartResult(result, sessionFrontDoor(store), clk, events.Discard, 0, ioDiscard{}, ioDiscard{}) {
				t.Fatal("deferred result counted as committed wake")
			}
			got, err := store.Get(bead.ID)
			if err != nil {
				t.Fatalf("Get session: %v", err)
			}
			if got.Status != "open" || got.Metadata["wake_attempts"] != "2" {
				t.Fatalf("session after deferral = %#v, want open state and unchanged failure budget", got)
			}
			if tc.pendingCreate && got.Metadata["pending_create_claim"] != "true" {
				t.Fatalf("pending_create_claim = %q, want true", got.Metadata["pending_create_claim"])
			}
			if got.Metadata["last_woke_at"] != "" {
				t.Fatalf("last_woke_at = %q, want cleared for retry", got.Metadata["last_woke_at"])
			}
			if starts := sp.CountCalls("Start", "worker"); starts != 0 {
				t.Fatalf("Start calls = %d, want 0", starts)
			}
			if stops := sp.CountCalls("Stop", "worker"); stops != 0 {
				t.Fatalf("Stop calls = %d, want 0", stops)
			}
		})
	}
}
