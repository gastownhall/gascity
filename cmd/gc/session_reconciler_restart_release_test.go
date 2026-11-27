package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
)

// corpseReads is a backend that lists the names in corpses as remain-on-exit
// corpses: present, nothing running.
type corpseReads struct {
	runtime.FullFake
	corpses map[string]bool
}

func (r corpseReads) ObserveLivenessSince(name string, pn []string, since time.Time) (runtime.Liveness, error) {
	if r.corpses[name] {
		return runtime.Liveness{Corpse: true}, nil
	}
	return r.FullFake.ObserveLivenessSince(name, pn, since)
}

func TestConfirmRuntimeReleased(t *testing.T) {
	ctx := context.Background()
	since := time.Now()

	t.Run("absent name", func(t *testing.T) {
		sp, _ := fakeProfile(runtime.ProfileTmux)
		if !confirmRuntimeReleased(ctx, sp, nil, "rt_a", since) {
			t.Fatal("a fresh read finding the name absent did not confirm its release")
		}
	})
	t.Run("running name", func(t *testing.T) {
		sp, f := fakeProfile(runtime.ProfileTmux)
		if err := f.Start(ctx, "rt_a", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		if confirmRuntimeReleased(ctx, sp, nil, "rt_a", since) {
			t.Fatal("a running name confirmed released")
		}
	})
	t.Run("corpse holds the name", func(t *testing.T) {
		f := runtime.NewFake()
		sp := runtime.NewFakeProfile(runtime.ProfileTmux, corpseReads{runtime.FullFake{Fake: f}, map[string]bool{"rt_a": true}})
		if confirmRuntimeReleased(ctx, sp, nil, "rt_a", since) {
			t.Fatal("a corpse still holding the name confirmed released")
		}
	})
	t.Run("nil ctx", func(t *testing.T) {
		sp, _ := fakeProfile(runtime.ProfileTmux)
		var nilCtx context.Context // the legacy reconciler's ctx may be nil
		if !confirmRuntimeReleased(nilCtx, sp, nil, "rt_a", since) {
			t.Fatal("a nil ctx did not read the absent name")
		}
	})
	t.Run("fresh read fails", func(t *testing.T) {
		leaf := newFenceLeaf(runtime.ProfileTmux)
		leaf.LivenessErrors["rt_a"] = runtime.ErrRuntimeUnavailable
		if confirmRuntimeReleased(ctx, leaf.P, nil, "rt_a", since) {
			t.Fatal("a failed read confirmed release")
		}
	})
	t.Run("backend cannot read fresh", func(t *testing.T) {
		if confirmRuntimeReleased(ctx, runtime.NewFake(), nil, "rt_a", since) {
			t.Fatal("a backend without fresh reads confirmed release")
		}
	})
	t.Run("empty name", func(t *testing.T) {
		sp, _ := fakeProfile(runtime.ProfileTmux)
		if confirmRuntimeReleased(ctx, sp, nil, " ", since) {
			t.Fatal("an empty name confirmed released")
		}
	})
	t.Run("nil provider", func(t *testing.T) {
		if confirmRuntimeReleased(ctx, nil, nil, "rt_a", since) {
			t.Fatal("a nil provider confirmed release")
		}
	})
	t.Run("unknown route", func(t *testing.T) {
		tmux, _ := fakeProfile(runtime.ProfileTmux)
		acp, _ := fakeProfile(runtime.ProfileACP)
		sp := auto.New(tmux, acp) // route table not seeded
		if confirmRuntimeReleased(ctx, sp, nil, "rt_a", since) {
			t.Fatal("an unknown route confirmed release")
		}
	})
}
