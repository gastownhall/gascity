package main

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// The route-recovery lane restores a lost gc.routed_to with a key-level
// compare-and-set (restoreRoute), which resolves its writer through
// beads.MetadataCASWriterFor and so used to bypass the route-clearing
// decorator (ga-l69b9m). These tests drive the real lane over the stacks the
// controller builds and require the capability seam to clear the stale
// executor-identity stamps when the restored route names a different executor,
// and to keep them when it names the executor they already identify.

const (
	laneClearOldExecutor = "gascity/old-executor"
	laneClearNewExecutor = "gascity/new-executor"
)

type laneClearShape struct {
	name string
	// open stacks the shape over a store already holding the bead.
	open func(t *testing.T, engine beads.Store) beads.Store
}

func laneClearShapes() []laneClearShape {
	cached := func(t *testing.T, backing beads.Store) beads.Store {
		t.Helper()
		cache := beads.NewCachingStoreForTest(backing, nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("Prime: %v", err)
		}
		return cache
	}
	return []laneClearShape{
		{"cache over decorator over engine", func(t *testing.T, engine beads.Store) beads.Store {
			return cached(t, beads.WithRouteChangeClearing(engine, routeClearIdentity))
		}},
		{"decorator over cache over engine", func(t *testing.T, engine beads.Store) beads.Store {
			return beads.WithRouteChangeClearing(cached(t, engine), routeClearIdentity)
		}},
		{"decorator over engine", func(_ *testing.T, engine beads.Store) beads.Store {
			return beads.WithRouteChangeClearing(engine, routeClearIdentity)
		}},
	}
}

// laneClearBead is the recoverable shape (open, unassigned, kind-less, a route
// in gc.run_target, none in gc.routed_to) still carrying a previous executor's
// identity stamps.
func laneClearBead(stampedExecutor string) beads.Bead {
	return beads.Bead{Title: "work", Type: "task", Status: "open", Metadata: map[string]string{
		beadmeta.RunTargetMetadataKey:     laneClearNewExecutor,
		beadmeta.SessionNameMetadataKey:   agent.SanitizeQualifiedNameForSession(stampedExecutor),
		beadmeta.WorkDirMetadataKey:       "worktrees/stamped",
		beadmeta.LegacyWorkDirMetadataKey: "worktrees/stamped-legacy",
	}}
}

func restoreThroughLane(t *testing.T, shape laneClearShape, stampedExecutor string) (engine *beads.MemStore, id string) {
	t.Helper()
	engine = beads.NewMemStore()
	created, err := engine.Create(laneClearBead(stampedExecutor))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := shape.open(t, engine)
	live, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	out := newRouteRecoveryLane().restoreRoute(store, live, false)
	if out.err != nil {
		t.Fatalf("restoreRoute: %v", out.err)
	}
	if !out.restored {
		t.Fatalf("restoreRoute outcome = %+v, want a restore", out)
	}
	got, err := engine.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != laneClearNewExecutor {
		t.Fatalf("gc.routed_to = %q, want %q", got.Metadata[beadmeta.RoutedToMetadataKey], laneClearNewExecutor)
	}
	return engine, created.ID
}

func laneStampKeys() []string {
	return []string{
		beadmeta.SessionNameMetadataKey,
		beadmeta.WorkDirMetadataKey,
		beadmeta.LegacyWorkDirMetadataKey,
	}
}

func TestRouteRecoveryLaneRestoreToANewExecutorClearsStaleStamps(t *testing.T) {
	for _, shape := range laneClearShapes() {
		t.Run(shape.name, func(t *testing.T) {
			engine, id := restoreThroughLane(t, shape, laneClearOldExecutor)
			got, err := engine.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			for _, key := range laneStampKeys() {
				if got.Metadata[key] != "" {
					t.Errorf("%s = %q after a restore to a different executor, want it cleared", key, got.Metadata[key])
				}
			}
		})
	}
}

func TestRouteRecoveryLaneRestoreToTheStampedExecutorKeepsStamps(t *testing.T) {
	for _, shape := range laneClearShapes() {
		t.Run(shape.name, func(t *testing.T) {
			engine, id := restoreThroughLane(t, shape, laneClearNewExecutor)
			got, err := engine.Get(id)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			for _, key := range laneStampKeys() {
				if got.Metadata[key] == "" {
					t.Errorf("%s was cleared by a restore to the executor it already names", key)
				}
			}
		})
	}
}
