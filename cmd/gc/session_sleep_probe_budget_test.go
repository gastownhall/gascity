package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/exec"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// budgetLeaf is a provider leaf reporting a fixed idle-proof budget.
type budgetLeaf struct {
	runtime.Provider
	budget time.Duration
}

func (l budgetLeaf) IdleProbeBudget() time.Duration { return l.budget }

// The idle-proof timeout is the routed leaf's budget clamped to
// [1s, fenceProbeTimeout], and 1s for a leaf without one.
// Kills: a constant 1s (remote exec captures cannot fit two observations, so
// interactive sessions never drain), a budget read off the composite instead
// of the routed leaf, an exec pack without an idle boundary getting the larger
// budget, and an unclamped budget stalling the effect pass past the fence bound.
func TestIdleSleepProbeTimeoutFor(t *testing.T) {
	const idleBoundary = `{"version":0,"capabilities":["proc.exec","report-activity","report-attachment"]}`
	declared := exec.NewSeamBacked(writeHandshakePack(t, idleBoundary))
	undeclared := exec.NewSeamBacked(writeHandshakePack(t, `{"version":0,"capabilities":["report-activity","report-attachment"]}`))
	fake := func() runtime.Provider { return runtime.NewFake() }

	tests := []struct {
		name string
		sp   runtime.Provider
		want time.Duration
	}{
		{"nil provider", nil, idleSleepProbeTimeout},
		{"tmux", runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: runtime.NewFake()}), time.Second},
		{"exec pack without an idle boundary", undeclared, time.Second},
		{"exec pack with an idle boundary, raw", exec.NewProvider(writeHandshakePack(t, idleBoundary)), 5 * time.Second},
		{"exec pack with an idle boundary, seam-backed", declared, 5 * time.Second},
		{"auto routed to the declared exec pack", auto.New(declared, fake()), 5 * time.Second},
		{"auto routed away from the declared exec pack", auto.New(fake(), declared), time.Second},
		{"hybrid routed to the declared exec pack", hybrid.New(fake(), declared, func(string) bool { return true }), 5 * time.Second},
		{"hybrid routed to a local leaf", hybrid.New(fake(), declared, func(string) bool { return false }), time.Second},
		{"leaf budget clamped to the fence bound", budgetLeaf{fake(), 30 * time.Second}, fenceProbeTimeout},
		{"leaf budget below the default", budgetLeaf{fake(), 200 * time.Millisecond}, time.Second},
		{"zero leaf budget", budgetLeaf{fake(), 0}, time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := idleSleepProbeTimeoutFor(tt.sp, "worker"); got != tt.want {
				t.Fatalf("idleSleepProbeTimeoutFor = %v, want %v", got, tt.want)
			}
		})
	}
	if fenceProbeTimeout != 5*time.Second {
		t.Fatalf("fenceProbeTimeout = %v; the idle-proof cap tracks it, recheck the exec budget", fenceProbeTimeout)
	}
}

// timeoutRecorder is a leaf with an idle-proof budget whose WaitForIdle
// reports the timeout it was given and answers waitErr.
type timeoutRecorder struct {
	runtime.Provider
	budget   time.Duration
	timeouts chan time.Duration
	waitErr  error
}

func (r timeoutRecorder) IdleProbeBudget() time.Duration { return r.budget }

func (r timeoutRecorder) WaitForIdle(_ context.Context, _ string, timeout time.Duration) error {
	r.timeouts <- timeout
	return r.waitErr
}

// Both idle-proof call sites, the reconciler's asynchronous idle probe and
// the effect pass's provedIdle, size WaitForIdle by the leaf's budget.
// Kills: a call site left on the 1s constant.
func TestIdleProofCallSitesUseProbeBudget(t *testing.T) {
	newLeaf := func() timeoutRecorder {
		return timeoutRecorder{Provider: runtime.NewFake(), budget: 3 * time.Second, timeouts: make(chan time.Duration, 1)}
	}
	recv := func(t *testing.T, ch <-chan time.Duration) time.Duration {
		t.Helper()
		select {
		case got := <-ch:
			return got
		case <-time.After(10 * time.Second):
			t.Fatal("WaitForIdle was not called")
			return 0
		}
	}

	t.Run("launchIdleProbes", func(t *testing.T) {
		leaf := newLeaf()
		info := sessionpkg.Info{ID: "b1", SessionNameMetadata: "worker"}
		launchIdleProbes(context.Background(), map[string]bool{"b1": true}, []wakeTarget{{info: info}},
			newDrainTracker(), leaf, clock.Real{}, map[string]sessionpkg.Info{"b1": info})
		if got := recv(t, leaf.timeouts); got != 3*time.Second {
			t.Fatalf("idle probe timeout = %v, want the leaf's 3s budget", got)
		}
	})

	t.Run("provedIdle", func(t *testing.T) {
		leaf := newLeaf()
		if !provedIdle(context.Background(), leaf, "worker", time.Now()) {
			t.Fatal("provedIdle = false, want true for an idle leaf with no activity")
		}
		if got := recv(t, leaf.timeouts); got != 3*time.Second {
			t.Fatalf("provedIdle timeout = %v, want the leaf's 3s budget", got)
		}
	})
}
