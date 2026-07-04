package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// scopedBdStoreForCity returns a throwaway BdStore for cityPath whose bd
// subprocess is bound to ctx: on cancellation the child is killed instead
// of surviving past the caller's own budget, unlike the long-lived shared
// store, whose runner is fixed to context.Background() at construction.
// Reuses the same credential/env resolution as bdStoreForCity, minus
// managed-dolt recovery (bdRuntimeEnvWithErrorNoRecovery, not
// bdRuntimeEnvWithError): a short best-effort read should fail fast
// rather than pay a multi-second recovery/health-check/autostart sequence
// — and every concurrent scoped-store construction attempting that
// recovery would multiply exactly the load a read-storm mitigation exists
// to bound. Skips the managed-retry wrapper for the same reason (gascity
// ga-cdmx6x).
func scopedBdStoreForCity(ctx context.Context, cityPath string) (*beads.BdStore, error) {
	env, err := bdRuntimeEnvWithErrorNoRecovery(cityPath)
	if err != nil {
		return nil, err
	}
	return beads.NewBdStore(cityPath, beads.ExecCommandRunnerWithEnvContext(ctx, env)), nil
}

// scopedBdStoreForRig is scopedBdStoreForCity for a rig-scoped store.
func scopedBdStoreForRig(ctx context.Context, cityPath string, cfg *config.City, rigDir string) (*beads.BdStore, error) {
	env, err := bdRuntimeEnvForRigWithErrorNoRecovery(cityPath, cfg, rigDir)
	if err != nil {
		return nil, err
	}
	return beads.NewBdStore(rigDir, beads.ExecCommandRunnerWithEnvContext(ctx, env)), nil
}

// bdStoreBacking unwraps store through any CachingStore/beadPolicyStore
// layers to find the underlying *beads.BdStore. It returns ok=false for
// stores that aren't bd-CLI-backed (native, file, exec, mem, ...) — those
// have no subprocess to leak, so ga-cdmx6x's mitigation doesn't apply to
// them. Bounded to a handful of iterations: real store stacks are at most
// two layers deep (CachingStore wrapping a beadPolicyStore wrapping the
// raw store); the bound just guards against an unexpected wrap cycle.
func bdStoreBacking(store beads.Store) (*beads.BdStore, bool) {
	for range 8 {
		switch v := store.(type) {
		case *beads.BdStore:
			return v, v != nil
		case *beads.CachingStore:
			if v == nil {
				return nil, false
			}
			backing := v.Backing()
			if backing == nil {
				return nil, false
			}
			store = backing
			continue
		}
		if inner, _, ok := unwrapBeadPolicyStore(store); ok {
			store = inner
			continue
		}
		return nil, false
	}
	return nil, false
}

// scopedStoreLike returns a throwaway, ctx-bound clone of existing when
// existing is (or wraps, via CachingStore/beadPolicyStore) a bd-CLI-shell
// backed store: cancellation kills the backend bd subprocess instead of
// abandoning it past ctx's deadline. Returns (nil, nil) when existing is
// not bd-CLI backed — callers should keep reading through existing
// directly in that case (gascity ga-cdmx6x).
func scopedStoreLike(ctx context.Context, cityPath string, cfg *config.City, existing beads.Store) (beads.Store, error) {
	bs, ok := bdStoreBacking(existing)
	if !ok {
		return nil, nil
	}
	dir := bs.Dir()
	if samePath(dir, cityPath) {
		return scopedBdStoreForCity(ctx, cityPath)
	}
	return scopedBdStoreForRig(ctx, cityPath, cfg, dir)
}
