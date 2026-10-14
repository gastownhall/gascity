package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// These tests pin the manual-row teardown at every close site that reaches it
// through releaseRowOwnedRuntime (or the drift rollback's own stop), plus the
// teardown's narrower edges. Each site runs on an exec-profiled runtime with
// an unstamped box under the manual row's name.

// rowCloseOrderProvider records, at each Stop, whether the row was still open, so
// a site that closes the row before stopping its box fails. Like
// blindLivenessProvider it implements no optional interface; with blind set,
// every box reads not running (a pack with no is-running).
type rowCloseOrderProvider struct {
	runtime.Provider
	store       beads.Store
	id          string
	blind       bool
	openAtStops []bool
}

func (p *rowCloseOrderProvider) Stop(name string) error {
	b, err := p.store.Get(p.id)
	p.openAtStops = append(p.openAtStops, err == nil && b.Status != "closed")
	return p.Provider.Stop(name)
}

func (p *rowCloseOrderProvider) IsRunning(name string) bool {
	if p.blind {
		return false
	}
	return p.Provider.IsRunning(name)
}

// manualSiteResult is the row and box after a close site ran.
type manualSiteResult struct {
	// closedOrCleared: the site released the row (closed it, or for the
	// drift rollback, reported its runtime cleared for the rollback).
	closedOrCleared bool
}

// manualCloseSite drives one close site once over a manual row whose box is
// unstamped, and reports whether the site released the row.
type manualCloseSite struct {
	name string
	// prepare adjusts the restarted pending create to the shape the site
	// acts on; nil keeps it.
	prepare func(t *testing.T, env *reconcilerTestEnv, session *beads.Bead)
	// blind: the site needs the box to read not running.
	blind bool
	run   func(t *testing.T, env *reconcilerTestEnv, sp *rowCloseOrderProvider, id, name string) manualSiteResult
}

func manualStartResult(env *reconcilerTestEnv, id, name string, lease *sessionpkg.RuntimeLease) startResult {
	now := env.clk.Now()
	return startResult{
		prepared: preparedStart{candidate: startCandidate{
			info:  env.sessionInfo(id),
			tp:    TemplateParams{SessionName: name, TemplateName: "worker", Command: "test-cmd"},
			lease: lease,
		}},
		outcome:  "success",
		started:  now,
		finished: now,
	}
}

func rowClosed(t *testing.T, env *reconcilerTestEnv, id string) bool {
	t.Helper()
	b, err := env.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Status == "closed"
}

func manualCloseSites() []manualCloseSite {
	return []manualCloseSite{
		{
			// Controller shutdown: the start succeeded but the commit's
			// context was canceled, so the create rolls back.
			name: "shutdown arm",
			run: func(t *testing.T, env *reconcilerTestEnv, sp *rowCloseOrderProvider, id, name string) manualSiteResult {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				commitAsyncStartResultWithContext(ctx, manualStartResult(env, id, name, nil), sp, env.store, env.clk, events.Discard, 0, io.Discard, &env.stderr, nil)
				return manualSiteResult{closedOrCleared: rowClosed(t, env, id)}
			},
		},
		{
			name: "terminal provider error arm",
			run: func(t *testing.T, env *reconcilerTestEnv, sp *rowCloseOrderProvider, id, name string) manualSiteResult {
				result := manualStartResult(env, id, name, nil)
				result.err = errors.New("provider error: model_not_found")
				if runtime.ProviderTerminalErrorReason(result.err.Error()) == "" {
					t.Fatal("fixture: the error must hit the terminal-provider-error arm")
				}
				result.outcome = TraceOutcomeProviderError
				result.rollbackPending = true
				result.provider = sp
				commitStartFailure(result, sessionFrontDoor(env.store), env.clk, events.Discard, 0, &env.stderr, nil)
				return manualSiteResult{closedOrCleared: rowClosed(t, env, id)}
			},
		},
		{
			// A failed-create row whose box reads not running is closed by
			// the reconciler's failed-create close.
			name: "failed-create close",
			prepare: func(_ *testing.T, env *reconcilerTestEnv, session *beads.Bead) {
				env.setSessionMetadata(session, map[string]string{"state": string(sessionpkg.StateFailedCreate), "pending_create_claim": ""})
				delete(env.desiredState, "worker")
				// Past the manual create's own in-flight window, which runs
				// from the store's wall-clock CreatedAt.
				env.clk.Time = time.Now().Add(time.Hour)
			},
			blind: true,
			run: func(t *testing.T, env *reconcilerTestEnv, sp *rowCloseOrderProvider, id, _ string) manualSiteResult {
				got := reconcileTeardownTick(t, env, sp, id)
				return manualSiteResult{closedOrCleared: got.Status == "closed"}
			},
		},
		{
			name:  "stuck-creating reaper",
			blind: true,
			run: func(t *testing.T, env *reconcilerTestEnv, sp *rowCloseOrderProvider, id, _ string) manualSiteResult {
				// The store stamps CreatedAt from the wall clock.
				clk := &clock.Fake{Time: time.Now().Add(24 * time.Hour)}
				reapStaleSessionBeads(env.city, env.store, testSeatWork(env.city, nil, env.store, nil), sp, env.dt, nil, clk, &env.stderr)
				return manualSiteResult{closedOrCleared: rowClosed(t, env, id)}
			},
		},
		{
			// The drift rollback's own stop: the start that spawned the box
			// still holds its runtime lease.
			name: "drift rollback, lease held",
			run: func(t *testing.T, env *reconcilerTestEnv, sp *rowCloseOrderProvider, id, name string) manualSiteResult {
				lease, release, err := tryRuntimeLease(nil, env.city, name, "", 0)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				return manualSiteResult{closedOrCleared: pendingCreateRuntimeClearedForRollback(manualStartResult(env, id, name, lease), sp, &env.stderr)}
			},
		},
	}
}

// TestManualRowTeardown_EveryCloseSite: every site that may close a manual
// pending create on a runtime that cannot vouch for identity stops the row's
// unattributed box first, and holds the row while the stop fails.
//
// Kills: no manual teardown at the shutdown arm, the terminal-error arm, the
// failed-create close or the stuck-creating reaper; the drift rollback
// ignoring the stale-start override.
func TestManualRowTeardown_EveryCloseSite(t *testing.T) {
	for _, site := range manualCloseSites() {
		for _, stopFails := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stop_fails=%v", site.name, stopFails), func(t *testing.T) {
				env, session, name := newRestartedPendingCreate(t, rowManual)
				if site.prepare != nil {
					site.prepare(t, env, &session)
				}
				if stopFails {
					env.sp.StopErrors[name] = errors.New("backend unreachable")
				}
				sp := &rowCloseOrderProvider{Provider: execProfile(env.sp), store: env.store, id: session.ID, blind: site.blind}
				got := site.run(t, env, sp, session.ID, name)
				running := env.sp.IsRunning(name)
				if stopFails {
					if got.closedOrCleared || !running || len(sp.openAtStops) == 0 {
						t.Fatalf("released=%v running=%v stops=%d, want a stop tried and the row held while it fails; stderr=%s",
							got.closedOrCleared, running, len(sp.openAtStops), env.stderr.String())
					}
					return
				}
				if !got.closedOrCleared || running || len(sp.openAtStops) == 0 {
					t.Fatalf("released=%v running=%v stops=%d, want the box stopped and the row released; stderr=%s",
						got.closedOrCleared, running, len(sp.openAtStops), env.stderr.String())
				}
				for i, open := range sp.openAtStops {
					if !open {
						t.Fatalf("stop %d ran after the row closed, want every stop before the close; stderr=%s", i+1, env.stderr.String())
					}
				}
			})
		}
	}
}

// listErrProvider is a provider without error-bearing liveness whose listing
// fails: a bare IsRunning cannot tell "gone" from "could not look", so only
// the listing's error can refuse a confirmation.
type listErrProvider struct{ runtime.Provider }

func (listErrProvider) ListRunning(string) ([]string, error) {
	return nil, errors.New("listing unavailable")
}

// TestTearDownManualRowRuntime_Edges pins the teardown's answers to a gone
// identity, a stop answering gone, an unconfirmable stop, and the stale-start
// override's route and name checks.
//
// Kills: no "gone" exclusion; a stop answering gone treated as a failure;
// confirmRuntimeStopped without its ListRunning check; the stale-start
// override ignoring an unknown route; the override without its session-name
// check.
func TestTearDownManualRowRuntime_Edges(t *testing.T) {
	gone := fmt.Errorf("reading meta: %w", runtime.ErrSessionNotFound)
	manual := func(f *runtime.Fake) sessionpkg.Info {
		b := beads.Bead{ID: "gc-1", Type: sessionBeadType, Metadata: map[string]string{
			"session_origin": "manual", "session_name": "worker", "instance_token": "tok-1",
		}}
		if err := f.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		return sessionInfoFromBead(b)
	}

	t.Run("identity reads gone: closes without a stop", func(t *testing.T) {
		f := runtime.NewFake()
		info := manual(f)
		f.GetMetaErrors["worker"] = map[string]error{"GC_SESSION_ID": gone, "GC_INSTANCE_TOKEN": gone}
		var holds []string
		if !tearDownManualRowRuntime(info, execProfile(f), func(_, line string) { holds = append(holds, line) }, nil) {
			t.Fatalf("held, want the row released: %v", holds)
		}
		if n := countCalls(f, "Stop"); n != 0 {
			t.Fatalf("Stop called %d times for a runtime whose identity reads gone", n)
		}
	})

	t.Run("stop answers gone: released", func(t *testing.T) {
		f := runtime.NewFake()
		info := manual(f)
		f.StopErrors["worker"] = fmt.Errorf("stop: %w", runtime.ErrSessionNotFound)
		var holds []string
		if !tearDownManualRowRuntime(info, blindLivenessProvider{execProfile(f)}, func(_, line string) { holds = append(holds, line) }, nil) {
			t.Fatalf("held on a stop that answered gone: %v", holds)
		}
	})

	t.Run("listing fails after a clean stop: held", func(t *testing.T) {
		f := runtime.NewFake()
		info := manual(f)
		var holds []string
		if tearDownManualRowRuntime(info, listErrProvider{execProfile(f)}, func(_, line string) { holds = append(holds, line) }, nil) {
			t.Fatal("released although the stop could not be confirmed")
		}
		if len(holds) != 1 || !strings.Contains(holds[0], "cannot be confirmed stopped") {
			t.Fatalf("holds = %q, want one unconfirmed-stop hold", holds)
		}
	})

	t.Run("stale start on an unseeded auto provider: not ours", func(t *testing.T) {
		f := runtime.NewFake()
		info := manual(f)
		sp := auto.New(execProfile(f), runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: f}))
		if got := staleAsyncStartRuntimeAttribution(info, "worker", sp, true); got == pendingCreateRuntimeOurs {
			t.Fatal("an unseeded composite's unstamped box read as the start's own")
		}
	})

	t.Run("stale start under another name: not ours", func(t *testing.T) {
		f := runtime.NewFake()
		info := manual(f)
		if err := f.Start(context.Background(), "worker-other", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		if got := staleAsyncStartRuntimeAttribution(info, "worker-other", execProfile(f), true); got == pendingCreateRuntimeOurs {
			t.Fatal("an unstamped box under a name other than the row's read as the start's own")
		}
		if got := staleAsyncStartRuntimeAttribution(info, "worker", execProfile(f), true); got != pendingCreateRuntimeOurs {
			t.Fatalf("control: the row's own name reads %v, want ours", got)
		}
	})
}

// TestReleaseRowOwnedRuntimeLeased_ManualRowDecidesAgainUnderTheFlock: a tick
// decides from its snapshot, so a start that commits the row (or a writer that
// closes it) after the snapshot must not have its box stopped. The tick
// re-reads the row under the name's flock and stops only while it still
// matches.
//
// Kills: no fresh re-read under the flock.
func TestReleaseRowOwnedRuntimeLeased_ManualRowDecidesAgainUnderTheFlock(t *testing.T) {
	for _, tc := range []struct {
		name     string
		after    func(t *testing.T, env *reconcilerTestEnv, session *beads.Bead)
		wantStop bool
	}{
		{name: "row unchanged", wantStop: true},
		{
			name: "start committed",
			after: func(_ *testing.T, env *reconcilerTestEnv, session *beads.Bead) {
				env.setSessionMetadata(session, map[string]string{"state": string(sessionpkg.StateActive), "pending_create_claim": ""})
			},
		},
		{
			name: "row closed",
			after: func(t *testing.T, env *reconcilerTestEnv, session *beads.Bead) {
				if err := env.store.Close(session.ID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "row re-woken",
			after: func(_ *testing.T, env *reconcilerTestEnv, session *beads.Bead) {
				env.setSessionMetadata(session, map[string]string{"last_woke_at": env.clk.Now().UTC().Format(time.RFC3339)})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, session, name := newRestartedPendingCreate(t, rowManual)
			snapshot := env.sessionInfo(session.ID)
			if tc.after != nil {
				tc.after(t, env, &session)
			}
			var stderr bytes.Buffer
			released := releaseRowOwnedRuntimeLeased(env.city, env.store, snapshot, execProfile(env.sp), &stderr, nil)
			stopped := countCalls(env.sp, "Stop") > 0
			if stopped != tc.wantStop || released != tc.wantStop {
				t.Fatalf("stopped=%v released=%v, want %v; stderr=%s", stopped, released, tc.wantStop, stderr.String())
			}
			if !tc.wantStop && !env.sp.IsRunning(name) {
				t.Fatal("the box of a row that moved on was stopped")
			}
		})
	}
}
