package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// scriptedRelease wraps a profiled provider, scripting its fresh liveness
// read: the release gate's one probe, and nothing else.
type scriptedRelease struct {
	runtime.Provider
	live runtime.Liveness
	err  error
}

func (s scriptedRelease) ObserveLivenessSince(string, []string, time.Time) (runtime.Liveness, error) {
	return s.live, s.err
}

// unknownRouteRouter routes every name as unknown, as an auto provider whose
// route table was not seeded.
type unknownRouteRouter struct {
	runtime.Provider
	leaf runtime.Provider
}

func (r unknownRouteRouter) RouteFor(string) runtime.Route {
	return runtime.Route{Backend: runtime.Backend{Label: "unseeded", Provider: r.leaf}, Known: false}
}

// TestConfirmRuntimeReleased pins the restart-requested release gate's
// verdicts: only a name proven absent on a fresh-readable routed leaf (with
// every fall-through hop quiet) is released; anything else — running, a
// corpse still holding the name, a failed read, a backend that cannot read
// fresh, an unknown route, a blank name — is not.
func TestConfirmRuntimeReleased(t *testing.T) {
	fresh := func(t *testing.T) (runtime.Provider, *runtime.Fake) {
		t.Helper()
		f := runtime.NewFake()
		return runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: f}), f
	}
	ctx := context.Background()
	since := time.Now().Add(-time.Second)

	t.Run("absent name released", func(t *testing.T) {
		sp, _ := fresh(t)
		if !confirmRuntimeReleased(ctx, sp, nil, "gone-session", since) {
			t.Fatal("absent name on a fresh-readable leaf = false, want true")
		}
	})

	t.Run("running name not released", func(t *testing.T) {
		sp, f := fresh(t)
		if err := f.Start(ctx, "live-session", runtime.Config{Command: "true"}); err != nil {
			t.Fatalf("start session: %v", err)
		}
		if confirmRuntimeReleased(ctx, sp, nil, "live-session", since) {
			t.Fatal("running name = true, want false")
		}
	})

	t.Run("corpse holding the name not released", func(t *testing.T) {
		sp, _ := fresh(t)
		corpse := scriptedRelease{Provider: sp, live: runtime.Liveness{Corpse: true}}
		if confirmRuntimeReleased(ctx, corpse, nil, "corpse-session", since) {
			t.Fatal("listed corpse = true, want false: a corpse still holds the name and would send the start into adoption")
		}
	})

	t.Run("nil ctx with absent name released without panic", func(t *testing.T) {
		sp, _ := fresh(t)
		var nilCtx context.Context // the legacy reconciler's ctx may be nil
		if !confirmRuntimeReleased(nilCtx, sp, nil, "gone-session", since) {
			t.Fatal("absent name with nil ctx = false, want true")
		}
	})

	t.Run("failed fresh read not released", func(t *testing.T) {
		sp, _ := fresh(t)
		failing := scriptedRelease{Provider: sp, err: runtime.ErrRuntimeUnavailable}
		if confirmRuntimeReleased(ctx, failing, nil, "gone-session", since) {
			t.Fatal("failed fresh read = true, want false")
		}
	})

	t.Run("plain fake not released", func(t *testing.T) {
		if confirmRuntimeReleased(ctx, runtime.NewFake(), nil, "gone-session", since) {
			t.Fatal("provider that cannot read fresh = true, want false")
		}
	})

	t.Run("empty name not released", func(t *testing.T) {
		sp, _ := fresh(t)
		if confirmRuntimeReleased(ctx, sp, nil, "  ", since) {
			t.Fatal("blank name = true, want false")
		}
	})

	t.Run("unknown route not released", func(t *testing.T) {
		sp, f := fresh(t)
		router := unknownRouteRouter{Provider: sp, leaf: f}
		if confirmRuntimeReleased(ctx, router, nil, "gone-session", since) {
			t.Fatal("unknown route = true, want false")
		}
	})
}
