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
	return scopedBdStoreForCityWithSettings(ctx, cityPath, "", false)
}

func scopedBdStoreForCityWithSettings(ctx context.Context, cityPath, idPrefix string, listSkipLabels bool) (*beads.BdStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	env, err := bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, false)
	if err != nil {
		return nil, err
	}
	return beads.NewBdStoreWithPrefix(
		cityPath,
		beads.ExecCommandRunnerWithEnvContext(ctx, env),
		idPrefix,
		scopedBdStoreOptions(listSkipLabels)...,
	), nil
}

func scopedBdStoreForRigWithSettings(ctx context.Context, cityPath string, cfg *config.City, rigDir, idPrefix string, listSkipLabels bool) (*beads.BdStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	env, err := bdRuntimeEnvForRigWithErrorRecoveryContext(ctx, cityPath, cfg, rigDir, false)
	if err != nil {
		return nil, err
	}
	return beads.NewBdStoreWithPrefix(
		rigDir,
		beads.ExecCommandRunnerWithEnvContext(ctx, env),
		idPrefix,
		scopedBdStoreOptions(listSkipLabels)...,
	), nil
}

func scopedBdStoreOptions(listSkipLabels bool) []beads.BdStoreOption {
	if !listSkipLabels {
		return nil
	}
	return []beads.BdStoreOption{beads.WithBdStoreListSkipLabels(true)}
}

// bdStoreBacking unwraps store through any CachingStore/beadPolicyStore
// layers to find the underlying *beads.BdStore. It returns ok=false for
// stores that aren't bd-CLI-backed (native, file, exec, mem, ...). Those keep
// their existing cancellation behavior; notably exec-backed stores may still
// run their own background-scoped subprocess deadline. Bounded to a handful
// of iterations: real store stacks are only a few
// layers deep (normally beadPolicyStore wrapping CachingStore wrapping the
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

// beadPolicyConfig finds the policy layer, if any, in the same bounded store
// stack understood by bdStoreBacking. A scoped clone must retain this layer:
// policy-aware zero-value List and Ready reads span both logical tiers.
func beadPolicyConfig(store beads.Store) (*config.City, bool) {
	for range 8 {
		if _, policy, ok := unwrapBeadPolicyStore(store); ok {
			return policy.cfg, true
		}
		cached, ok := store.(*beads.CachingStore)
		if !ok || cached == nil || cached.Backing() == nil {
			return nil, false
		}
		store = cached.Backing()
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bs, ok := bdStoreBacking(existing)
	if !ok {
		return nil, nil
	}
	policyCfg, policyWrapped := beadPolicyConfig(existing)
	dir := bs.Dir()
	var scoped beads.Store
	var err error
	if samePath(dir, cityPath) {
		scoped, err = scopedBdStoreForCityWithSettings(ctx, cityPath, bs.IDPrefix(), bs.ListSkipLabelsEnabled())
	} else {
		scoped, err = scopedBdStoreForRigWithSettings(ctx, cityPath, cfg, dir, bs.IDPrefix(), bs.ListSkipLabelsEnabled())
	}
	if err != nil {
		return nil, err
	}
	if policyWrapped {
		scoped = wrapStoreWithBeadPolicies(scoped, policyCfg)
	}
	return scoped, nil
}
