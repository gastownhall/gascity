package main

import (
	"context"
	"reflect"

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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	env, err := bdRuntimeEnvWithErrorRecoveryContext(ctx, cityPath, false)
	if err != nil {
		return nil, err
	}
	runner, err := beadsCommandRunnerWithContextForHostedCity(ctx, cityPath, env)
	if err != nil {
		return nil, err
	}
	return beads.NewBdStore(cityPath, runner), nil
}

// scopedBdStoreForRig is scopedBdStoreForCity for a rig-scoped store.
func scopedBdStoreForRig(ctx context.Context, cityPath string, cfg *config.City, rigDir string) (*beads.BdStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	env, err := bdRuntimeEnvForRigWithErrorRecoveryContext(ctx, cityPath, cfg, rigDir, false)
	if err != nil {
		return nil, err
	}
	runner, err := beadsCommandRunnerWithContextForHostedCity(ctx, cityPath, env)
	if err != nil {
		return nil, err
	}
	return beads.NewBdStore(rigDir, runner), nil
}

func beadsCommandRunnerWithContextForHostedCity(ctx context.Context, cityPath string, env map[string]string) (beads.CommandRunner, error) {
	if env == nil {
		env = make(map[string]string)
	}
	// Store opens degrade rather than refuse: see pinBdGCEnvironmentBestEffort.
	pinBdGCEnvironmentBestEffort(env)
	selected, err := citySelectsHostedBeadsCredentialProvider(cityPath)
	if err != nil {
		return nil, err
	}
	if selected {
		return beads.ExecCommandRunnerWithEnvContextWithoutAmbientBeads(ctx, env), nil
	}
	return beads.ExecCommandRunnerWithEnvContext(ctx, env), nil
}

// bdStoreBacking unwraps store through any CachingStore/beadPolicyStore
// layers to find the underlying *beads.BdStore. It returns ok=false for
// stores that aren't bd-CLI-backed (native, file, exec, mem, ...) — those
// have no subprocess to leak, so ga-cdmx6x's mitigation doesn't apply to
// them. Bounded to a handful of iterations: real store stacks are only a few
// layers deep (normally beadPolicyStore wrapping CachingStore wrapping the
// raw store); the bound just guards against an unexpected wrap cycle.
func bdStoreBacking(store beads.Store) (*beads.BdStore, bool) {
	for range 8 {
		switch v := store.(type) {
		case *beads.BdStore:
			return v, v != nil
		case beads.ProxiedStoreView:
			// The proxied-native split store, and the answer depends on which
			// leaf is serving RIGHT NOW.
			//
			// While the native leaf serves there is no subprocess to bind: the
			// reads are library calls over bd's proxy, and returning a bd store
			// here would make gc status rebuild a clone that forks once per read
			// — turning the lane's zero-fork property into two forks, silently.
			//
			// After a stand-down the wrapper's reads ARE bd forks, and they are
			// the ones ga-cdmx6x is about: gc status runs them under a 3s
			// deadline, so a clone that is not ctx-bound abandons a live child
			// instead of killing it. Unwrapping to the bd leaf is what lets the
			// caller rebuild it bound to the request.
			//
			// One-way, like the demotion itself: a store that was native when
			// this was asked and demotes a millisecond later simply keeps
			// serving through the wrapper for this command.
			// The interface arm guards a TYPED nil as well as an untyped one
			// (council A-F10). `v == nil` is false for an interface holding a
			// nil *ProxiedStore, and (*ProxiedStore).Demoted takes
			// s.mu.RLock() on the nil receiver and panics — in gc status's
			// snapshot path. The two neighboring arms both guard their typed
			// nils explicitly, so this was an inconsistency rather than a
			// choice.
			if v == nil || isNilProxiedStoreView(v) || !v.Demoted() {
				return nil, false
			}
			leaf := v.BdLeaf()
			if leaf == nil {
				return nil, false
			}
			store = leaf
			continue
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
		scoped, err = scopedBdStoreForCity(ctx, cityPath)
	} else {
		scoped, err = scopedBdStoreForRig(ctx, cityPath, cfg, dir)
	}
	if err != nil {
		return nil, err
	}
	if policyWrapped {
		scoped = wrapStoreWithBeadPolicies(scoped, policyCfg)
	}
	return scoped, nil
}

// isNilProxiedStoreView reports whether view is an interface holding a nil
// pointer.
//
// A nil *beads.ProxiedStore stored in a beads.ProxiedStoreView is not `== nil`:
// the interface has a type. Every method on it that takes the store's mutex
// panics, and bdStoreBacking is reached from gc status's snapshot path, where a
// panic is the whole command (council A-F10).
//
// reflect rather than a type switch on *beads.ProxiedStore, because the arm is
// keyed on the INTERFACE on purpose — beadPolicyStore and the class wrappers
// participate in it through ProxiedStoreFrom — and a concrete-type check here
// would guard the one implementation that exists today and miss the next one.
func isNilProxiedStoreView(view beads.ProxiedStoreView) bool {
	value := reflect.ValueOf(view)
	switch value.Kind() {
	case reflect.Ptr, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return value.IsNil()
	default:
		return false
	}
}
