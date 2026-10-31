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

	city := t.TempDir()
	item, release := leasedStart(t, city, preparedStart{
		candidate: startCandidate{
			info: sessiontest.SeedBead(t, bead),
			tp:   TemplateParams{TemplateName: "worker"},
		},
		cfg: runtime.Config{
			Command: "claude --resume seeded-session",
			WorkDir: info.WorkDir,
		},
	})
	defer release()
	results := executePreparedStartWaveForCity(context.Background(), []preparedStart{item}, city, sp, store, nil, 10*time.Second, 1)
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
	city := t.TempDir()
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
		city,
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
	testCity := t.TempDir()
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
		testCity,
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
	city := t.TempDir()
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
		city,
		nil, sp, nil, nil, nil, nil,
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
			results := executePreparedStartWaveForCity(
				context.Background(),
				[]preparedStart{{
					candidate: startCandidate{
						info: sessiontest.SeedBead(t, bead),
						tp:   TemplateParams{Command: "claude", SessionName: "worker", TemplateName: "worker"},
					},
					cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
				}},
				t.TempDir(),
				sp,
				store,
				nil,
				10*time.Second, 1,
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

// TestStartPreparedStartCandidateRuntimeOnlyBorrowsTheCandidateLease: a
// bead-less legacy start runs its runtime-only handle under the candidate's
// lease (the name's flock the start already holds), instead of taking that
// flock a second time and failing on itself. Without the candidate's lease,
// a flock another holder has defers the controller's start at once.
func TestStartPreparedStartCandidateRuntimeOnlyBorrowsTheCandidateLease(t *testing.T) {
	cityPath := t.TempDir()
	const name = "worker-1"
	item := func(lease *sessionpkg.RuntimeLease) preparedStart {
		return preparedStart{
			candidate: startCandidate{
				info:  sessionpkg.Info{SessionName: name, SessionNameMetadata: name},
				tp:    TemplateParams{TemplateName: "worker"},
				lease: lease,
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		}
	}
	lease, release, err := tryRuntimeLease(nil, cityPath, name, "", 0)
	if err != nil {
		t.Fatalf("taking the candidate's flock: %v", err)
	}
	sp := runtime.NewFake()
	began := time.Now()
	if _, err := startPreparedStartCandidate(context.Background(), item(lease), cityPath, nil, sp, &config.City{}, nil, nil, nil); err != nil {
		t.Fatalf("start under the candidate's lease = %v, want started", err)
	}
	if sp.CountCalls("Start", name) != 1 || time.Since(began) > 2*time.Second {
		t.Fatalf("provider Starts = %d after %v, want one at once", sp.CountCalls("Start", name), time.Since(began))
	}
	if err := sp.Stop(name); err != nil {
		t.Fatal(err)
	}
	began = time.Now()
	_, err = startPreparedStartCandidate(context.Background(), item(nil), cityPath, nil, sp, &config.City{}, nil, nil, nil)
	if !errors.Is(err, sessionpkg.ErrRuntimeLeaseBusy) || errors.Is(err, sessionpkg.ErrSessionStarting) || time.Since(began) > 2*time.Second {
		t.Fatalf("start without the lease under another holder's flock = %v after %v, want ErrRuntimeLeaseBusy at once", err, time.Since(began))
	}
	if sp.CountCalls("Start", name) != 1 {
		t.Fatal("the refused start reached the provider")
	}
	release()
}

// TestStartLeaseNameIsTheStartsName: a start's lease names the runtime the
// start starts: the candidate's own name, or, with none, the row's resolved
// one.
func TestStartLeaseNameIsTheStartsName(t *testing.T) {
	for _, c := range []struct {
		meta, resolved, want string
	}{
		{"worker-1", "worker-1", "worker-1"},
		{"worker-1", "s-gc-1", "worker-1"},
		{"", "s-gc-1", "s-gc-1"},
		{"", "", ""},
	} {
		cand := startCandidate{info: sessionpkg.Info{SessionNameMetadata: c.meta, SessionName: c.resolved}}
		if got := startLeaseName(cand); got != c.want {
			t.Errorf("startLeaseName(meta %q, resolved %q) = %q, want %q", c.meta, c.resolved, got, c.want)
		}
	}
}

// startErrProvider fails Start with the error build returns, after running
// any side effect build has (such as canceling the start's own context).
type startErrProvider struct {
	*runtime.Fake
	build func() error
}

func (p *startErrProvider) Start(context.Context, string, runtime.Config) error { return p.build() }

// A start whose pre-start orphan scan could not finish fails with an error
// wrapping runtime.ErrRuntimeUnavailable plus the reason the wait ended. The
// classifier must read that as a deferral whichever way the wait ended, ahead
// of its start-context arms: the scan's budget expiring while the start's own
// context is live, or the start's context canceled mid-scan. Neither is a
// failed start (no rollback, no wake-failure budget spent, no probes).
func TestExecutePreparedStartWaveDefersUnfinishedOrphanScan(t *testing.T) {
	scanUnavailable := func(cause error) error {
		return fmt.Errorf("starting session: pre-start orphan cleanup: %w: %w", runtime.ErrRuntimeUnavailable, cause)
	}
	for _, tc := range []struct {
		name string
		// run returns the parent context for the wave and the start error.
		run func() (context.Context, func() error)
	}{
		{
			name: "budget expired with a live start context",
			run: func() (context.Context, func() error) {
				return context.Background(), func() error { return scanUnavailable(context.DeadlineExceeded) }
			},
		},
		{
			name: "start context canceled mid-scan",
			run: func() (context.Context, func() error) {
				ctx, cancel := context.WithCancel(context.Background())
				return ctx, func() error { cancel(); return scanUnavailable(context.Canceled) }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
			bead, err := store.Create(beads.Bead{
				ID: "gc-worker", Title: "worker", Type: sessionBeadType,
				Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
					"session_name":  "worker",
					"template":      "worker",
					"state":         "asleep",
					"last_woke_at":  clk.Now().Format(time.RFC3339),
					"wake_attempts": "2",
					// A recorded incarnation: the runtime-only start refuses a row without one
					// before it ever reaches the orphan scan.
					"generation":         "1",
					"continuation_epoch": "1",
					"instance_token":     "tok-1",
				},
			})
			if err != nil {
				t.Fatalf("Create session: %v", err)
			}
			ctx, build := tc.run()
			info := sessiontest.SeedBead(t, bead)
			sp := &startErrProvider{Fake: runtime.NewFake(), build: build}
			// The runtime-only start runs under the candidate's lease.
			cityPath := t.TempDir()
			lease, release, err := tryRuntimeLease(nil, cityPath, "worker", "", 0)
			if err != nil {
				t.Fatalf("taking the candidate's flock: %v", err)
			}
			defer release()

			results := executePreparedStartWaveForCity(
				ctx,
				[]preparedStart{{
					candidate: startCandidate{
						info:  info,
						tp:    TemplateParams{Command: "claude", SessionName: "worker", TemplateName: "worker"},
						lease: lease,
						inc:   sessionpkg.RecordedIncarnationForTest(info),
					},
					cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
				}},
				cityPath,
				sp,
				store,
				nil,
				10*time.Second, 1,
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
			if got.Metadata["last_woke_at"] != "" {
				t.Fatalf("last_woke_at = %q, want cleared for retry", got.Metadata["last_woke_at"])
			}
			if stops := sp.CountCalls("Stop", "worker"); stops != 0 {
				t.Fatalf("Stop calls = %d, want 0: a deferred start rolls nothing back", stops)
			}
		})
	}
}

// ctxStartErrProvider fails Start with whatever build returns for the start's
// own context.
type ctxStartErrProvider struct {
	*runtime.Fake
	build func(context.Context) error
}

func (p *ctxStartErrProvider) Start(ctx context.Context, _ string, _ runtime.Config) error {
	return p.build(ctx)
}

func TestDefersUnfinishedOrphanScanWhenStartDeadlineEndsIt(t *testing.T) {
	store := beads.NewMemStore()
	clk := &clock.Fake{Time: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	bead, err := store.Create(beads.Bead{
		ID: "gc-worker", Title: "worker", Type: sessionBeadType,
		Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name":  "worker",
			"template":      "worker",
			"state":         "asleep",
			"last_woke_at":  clk.Now().Format(time.RFC3339),
			"wake_attempts": "2",
			// A recorded incarnation: the runtime-only start refuses a row without one
			// before it ever reaches the orphan scan.
			"generation":         "1",
			"continuation_epoch": "1",
			"instance_token":     "tok-1",
		},
	})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	info := sessiontest.SeedBead(t, bead)
	// The start's own deadline is SHORTER than the scan budget, so the wait ends
	// on the start context's deadline, not the scan's budget.
	sp := &ctxStartErrProvider{Fake: runtime.NewFake(), build: func(ctx context.Context) error {
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return fmt.Errorf("starting session: pre-start orphan cleanup: %w: %w", runtime.ErrRuntimeUnavailable, context.DeadlineExceeded)
	}}
	cityPath := t.TempDir()
	lease, release, err := tryRuntimeLease(nil, cityPath, "worker", "", 0)
	if err != nil {
		t.Fatalf("taking the candidate's flock: %v", err)
	}
	defer release()

	results := executePreparedStartWaveForCity(
		context.Background(),
		[]preparedStart{{
			candidate: startCandidate{
				info:  info,
				tp:    TemplateParams{Command: "claude", SessionName: "worker", TemplateName: "worker"},
				lease: lease,
				inc:   sessionpkg.RecordedIncarnationForTest(info),
			},
			cfg: runtime.Config{Command: "claude", WorkDir: t.TempDir()},
		}},
		cityPath,
		sp,
		store,
		nil,
		50*time.Millisecond, 1,
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
}
