package config

import (
	"errors"
	"fmt"
)

// BuildResolvedProviderCache walks every custom provider's base chain,
// materializes a fully-merged ResolvedProvider per entry, and stores
// the result on cfg.ResolvedProviders. It replaces any previously-built
// cache atomically: on any chain-resolution error, cfg.ResolvedProviders
// is left untouched and the error is returned.
//
// The cache is built after compose + patch have populated cfg.Providers.
// Callers should invoke this once per config load (see LoadWithIncludes).
//
// Design invariants:
//   - Lookups must return deep-copied values so callers cannot poison
//     the shared cache by mutating returned slices/maps.
//   - Built-ins are NOT materialized into the cache (they are the chain
//     terminus). Lookups for built-in-only names still work via
//     BuiltinProviders() / ResolveProvider.
//   - Chain walk errors (cycles, unknown base, wrapper-resume missing)
//     are surfaced during cache build so they fail at config load, not
//     at session spawn.
func BuildResolvedProviderCache(cfg *City) error {
	if cfg == nil {
		return nil
	}
	if len(cfg.Providers) == 0 {
		cfg.ResolvedProviders = nil
		return nil
	}

	// Build into a local map; assign atomically at the end.
	next := make(map[string]ResolvedProvider, len(cfg.Providers))
	var firstErr error
	for name, spec := range cfg.Providers {
		resolved, err := ResolveProviderChain(name, spec, cfg.Providers)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("resolving provider %q: %w", name, err)
			}
			continue
		}
		next[name] = resolved
	}
	if firstErr != nil {
		// Do not overwrite the existing cache on error.
		return firstErr
	}
	cfg.ResolvedProviders = next
	return nil
}

// ResolvedProviderCached returns a deep-copied ResolvedProvider from
// the eager cache. If no cache entry exists for name, ok is false.
// Callers receive an independent copy — mutating returned slices/maps
// does not affect the cache or subsequent lookups.
//
// This is the runtime-facing read path. Pre-compose / quick-parse
// paths that operate on raw ProviderSpec before cache build must NOT
// call this; they should use RawProviderSpec reads.
func ResolvedProviderCached(cfg *City, name string) (ResolvedProvider, bool) {
	if cfg == nil || cfg.ResolvedProviders == nil {
		return ResolvedProvider{}, false
	}
	resolved, ok := cfg.ResolvedProviders[name]
	if !ok {
		return ResolvedProvider{}, false
	}
	return deepCopyResolvedProvider(resolved), true
}

// deepCopyResolvedProvider clones all slice and map fields so the
// caller's copy is independent of the cache entry.
func deepCopyResolvedProvider(r ResolvedProvider) ResolvedProvider {
	dup := r
	if r.Args != nil {
		dup.Args = append([]string(nil), r.Args...)
	}
	if r.ProcessNames != nil {
		dup.ProcessNames = append([]string(nil), r.ProcessNames...)
	}
	if r.PrintArgs != nil {
		dup.PrintArgs = append([]string(nil), r.PrintArgs...)
	}
	if r.Chain != nil {
		dup.Chain = append([]HopIdentity(nil), r.Chain...)
	}
	dup.Provenance = r.Provenance.clone()
	if r.Env != nil {
		dup.Env = make(map[string]string, len(r.Env))
		for k, v := range r.Env {
			dup.Env[k] = v
		}
	}
	if r.PermissionModes != nil {
		dup.PermissionModes = make(map[string]string, len(r.PermissionModes))
		for k, v := range r.PermissionModes {
			dup.PermissionModes[k] = v
		}
	}
	if r.EffectiveDefaults != nil {
		dup.EffectiveDefaults = make(map[string]string, len(r.EffectiveDefaults))
		for k, v := range r.EffectiveDefaults {
			dup.EffectiveDefaults[k] = v
		}
	}
	if r.OptionsSchema != nil {
		dup.OptionsSchema = make([]ProviderOption, len(r.OptionsSchema))
		for i, opt := range r.OptionsSchema {
			nopt := opt
			if opt.Choices != nil {
				nopt.Choices = make([]OptionChoice, len(opt.Choices))
				for j, c := range opt.Choices {
					nc := c
					if c.FlagArgs != nil {
						nc.FlagArgs = append([]string(nil), c.FlagArgs...)
					}
					nopt.Choices[j] = nc
				}
			}
			dup.OptionsSchema[i] = nopt
		}
	}
	return dup
}

// ErrProviderCacheNotBuilt is returned by strict cache-only lookups when
// the cache has not been materialized.
var ErrProviderCacheNotBuilt = errors.New("provider cache not built")
