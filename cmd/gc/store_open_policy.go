package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
)

// storeOpenPolicy is the set of openers the shared store-open body uses to
// reach a backend. The recovering policy is what every command has always
// used: its env builders may run the provider health -> recover ladder and its
// bd runners retry through recoverManagedBDCommand. The observation policy
// (observation_store.go) is verify-only. The shared body names none of these
// helpers directly, so which policy a caller picked is visible at the call
// site and to the guard in observation_recovery_census_test.go.
type storeOpenPolicy struct {
	// observation marks the verify-only policy. It keys the invocation memo so
	// the two policies never borrow one another's store.
	observation bool
	// preflight builds the native-store preflight for a scope.
	preflight func(cityPath, provider string, cfg *config.City) contract.PreflightChecker
	// openBd opens the bd-backed store at storePath.
	openBd func(storePath, cityPath string, cfg *config.City, oneShotConfig bool, native beads.NativeTransportMode) (beads.Store, error)
	// openExec opens the exec-provider store.
	openExec func(provider, scopeRoot, cityPath string, cfg *config.City) (beads.Store, error)
	// nativeEnv builds the env a direct native open (and its reconnect hook)
	// projects.
	nativeEnv func(ctx context.Context, cityPath string, cfg *config.City, scopeRoot string, longLived bool) (map[string]string, error)
}

// recoveringStoreOpenPolicy is the policy of every mutating caller and of the
// controller: env builds may recover a stopped managed Dolt.
func recoveringStoreOpenPolicy() storeOpenPolicy {
	return storeOpenPolicy{
		preflight: newBeadsPreflightChecker,
		openBd:    openBdStoreAtScoped,
		openExec:  openExecStoreAtForCityWithConfig,
		nativeEnv: func(ctx context.Context, cityPath string, cfg *config.City, scopeRoot string, longLived bool) (map[string]string, error) {
			if longLived {
				return nativeDoltOpenEnvForScopeContext(ctx, cityPath, cfg, scopeRoot)
			}
			return nativeDoltOneShotOpenEnvForScopeContext(ctx, cityPath, cfg, scopeRoot)
		},
	}
}
