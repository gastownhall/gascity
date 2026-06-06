package beads

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

const (
	// BeadsStoreNameBdStore is the diagnostic store name for bd-backed stores.
	BeadsStoreNameBdStore = "BdStore"
	// BeadsStoreNameFileStore is the diagnostic store name for file-backed stores.
	BeadsStoreNameFileStore = "FileStore"
	// BeadsStoreNameExecStore is the diagnostic store name for exec-backed stores.
	BeadsStoreNameExecStore = "ExecStore"
	// BeadsStoreNameNativeDoltStore is the diagnostic store name for native Dolt stores.
	BeadsStoreNameNativeDoltStore = "NativeDoltStore"

	storeNameBdStore         = BeadsStoreNameBdStore
	storeNameFileStore       = BeadsStoreNameFileStore
	storeNameExecStore       = BeadsStoreNameExecStore
	storeNameNativeDoltStore = BeadsStoreNameNativeDoltStore
	nativeForceFallbackEnv   = "GC_BEADS_FORCE_FALLBACK"
	nativeForceFallbackGate  = "force_fallback"
	nativeHooksGate          = "bd_hooks"
	nativeUnavailableMessage = "native_store_unavailable"
)

// BeadsDiagnostic summarizes native-store selection for status surfaces.
//
//nolint:revive // The design names this operator-facing struct BeadsDiagnostic.
type BeadsDiagnostic struct {
	Store               string `json:"beads_store"`
	NativeStoreEligible bool   `json:"native_store_eligible"`
	PreflightGate       string `json:"preflight_gate,omitempty"`
	PreflightReason     string `json:"preflight_reason,omitempty"`
}

// StoreOpenOptions holds dependencies for opening a beads Store.
type StoreOpenOptions struct {
	ScopeRoot        string
	CityPath         string
	Provider         string
	PreflightChecker contract.PreflightChecker
	Logger           *slog.Logger
	OpenBdStore      func() (Store, error)
	OpenFileStore    func() (Store, error)
	OpenExecStore    func() (Store, error)
	OpenNativeStore  func() (Store, error)
}

// StoreOpenResult contains the selected Store plus native-selection diagnostics.
type StoreOpenResult struct {
	Store      Store
	Diagnostic BeadsDiagnostic
}

// ExecStoreDiagnostic returns the diagnostic for an explicitly configured exec store.
func ExecStoreDiagnostic() BeadsDiagnostic {
	return BeadsDiagnostic{Store: storeNameExecStore}
}

// OpenStoreAtForCity opens the configured Store for a city or rig scope.
func OpenStoreAtForCity(ctx context.Context, opts StoreOpenOptions) (StoreOpenResult, error) {
	provider := strings.TrimSpace(opts.Provider)
	switch {
	case provider == "file":
		store, err := callStoreOpen("file store", opts.OpenFileStore)
		return StoreOpenResult{Store: store, Diagnostic: BeadsDiagnostic{Store: storeNameFileStore}}, err
	case strings.HasPrefix(provider, "exec:") && !contract.ProviderUsesBDContract(provider):
		store, err := callStoreOpen("exec store", opts.OpenExecStore)
		return StoreOpenResult{Store: store, Diagnostic: BeadsDiagnostic{Store: storeNameExecStore}}, err
	}

	if forceNativeFallback() {
		diag := BeadsDiagnostic{
			Store:               storeNameBdStore,
			NativeStoreEligible: false,
			PreflightGate:       nativeForceFallbackGate,
			PreflightReason:     nativeForceFallbackEnv + "=1",
		}
		logNativeUnavailable(opts.Logger, opts.ScopeRoot, diag.PreflightGate, diag.PreflightReason)
		return opts.openBdFallback(provider, diag)
	}

	if !contract.ProviderUsesBDContract(provider) {
		diag := BeadsDiagnostic{
			Store:               storeNameBdStore,
			NativeStoreEligible: false,
			PreflightGate:       string(contract.PreflightCheckProviderContract),
			PreflightReason:     fmt.Sprintf("provider %q does not use the bd contract", provider),
		}
		logNativeUnavailable(opts.Logger, opts.ScopeRoot, diag.PreflightGate, diag.PreflightReason)
		return opts.openBdFallback(provider, diag)
	}

	result, err := opts.PreflightChecker.Check(opts.ScopeRoot)
	if err != nil {
		diag := BeadsDiagnostic{
			Store:               storeNameBdStore,
			NativeStoreEligible: false,
			PreflightGate:       "preflight_unavailable",
			PreflightReason:     err.Error(),
		}
		logNativeUnavailable(opts.Logger, opts.ScopeRoot, diag.PreflightGate, diag.PreflightReason)
		return opts.openBdFallback(provider, diag)
	}
	diag := diagnosticFromPreflight(result)
	if !result.NativeStoreEligible {
		logNativeUnavailable(opts.Logger, opts.ScopeRoot, diag.PreflightGate, diag.PreflightReason)
		return opts.openBdFallback(provider, diag)
	}

	if scopeHasExecutableBdHooks(opts.ScopeRoot) {
		diag := BeadsDiagnostic{
			Store:               storeNameBdStore,
			NativeStoreEligible: false,
			PreflightGate:       nativeHooksGate,
			PreflightReason:     "bd hooks are installed; remove .beads/hooks/on_create,on_update,on_close after confirming controller cache events cover this deployment",
		}
		logNativeUnavailable(opts.Logger, opts.ScopeRoot, diag.PreflightGate, diag.PreflightReason)
		return opts.openBdFallback(provider, diag)
	}

	native, err := opts.openNativeStore(ctx)
	if err != nil {
		diag := BeadsDiagnostic{
			Store:               storeNameBdStore,
			NativeStoreEligible: false,
			PreflightGate:       "native_open",
			PreflightReason:     err.Error(),
		}
		logNativeUnavailable(opts.Logger, opts.ScopeRoot, diag.PreflightGate, diag.PreflightReason)
		return opts.openBdFallback(provider, diag)
	}
	return StoreOpenResult{
		Store: native,
		Diagnostic: BeadsDiagnostic{
			Store:               storeNameNativeDoltStore,
			NativeStoreEligible: true,
		},
	}, nil
}

func (opts StoreOpenOptions) openBdFallback(provider string, diag BeadsDiagnostic) (StoreOpenResult, error) {
	if strings.HasPrefix(strings.TrimSpace(provider), "exec:") && contract.ProviderUsesBDContract(provider) && opts.OpenExecStore != nil {
		diag.Store = storeNameExecStore
		store, err := callStoreOpen("exec store", opts.OpenExecStore)
		return StoreOpenResult{Store: store, Diagnostic: diag}, err
	}
	diag.Store = storeNameBdStore
	store, err := callStoreOpen("bd store", opts.OpenBdStore)
	return StoreOpenResult{Store: store, Diagnostic: diag}, err
}

func (opts StoreOpenOptions) openNativeStore(ctx context.Context) (Store, error) {
	if opts.OpenNativeStore != nil {
		return opts.OpenNativeStore()
	}
	return newNativeDoltStoreAt(ctx, opts.ScopeRoot, nil)
}

func callStoreOpen(name string, open func() (Store, error)) (Store, error) {
	if open == nil {
		return nil, fmt.Errorf("opening %s: opener is not configured", name)
	}
	return open()
}

func diagnosticFromPreflight(result contract.PreflightResult) BeadsDiagnostic {
	diag := BeadsDiagnostic{
		Store:               storeNameBdStore,
		NativeStoreEligible: result.NativeStoreEligible,
		PreflightReason:     result.FallbackReason,
	}
	for _, check := range result.Checks {
		if check.State == contract.PreflightCheckFail {
			diag.PreflightGate = string(check.ID)
			if diag.PreflightReason == "" {
				diag.PreflightReason = check.Summary
			}
			return diag
		}
	}
	for _, check := range result.Checks {
		if check.State == contract.PreflightCheckWarn {
			diag.PreflightGate = string(check.ID)
			if diag.PreflightReason == "" {
				diag.PreflightReason = check.Summary
			}
			return diag
		}
	}
	return diag
}

func scopeHasExecutableBdHooks(scopeRoot string) bool {
	for _, name := range []string{"on_create", "on_update", "on_close"} {
		info, err := os.Stat(filepath.Join(scopeRoot, ".beads", "hooks", name))
		if err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

func forceNativeFallback() bool {
	value := strings.TrimSpace(os.Getenv(nativeForceFallbackEnv))
	return value == "1" || strings.EqualFold(value, "true")
}

func logNativeUnavailable(logger *slog.Logger, scope, gate, reason string) {
	if logger == nil {
		return
	}
	args := []any{
		slog.String("gate", gate),
		slog.String("reason", reason),
		slog.String("scope", scope),
	}
	if gate == string(contract.PreflightCheckIdentityMatch) {
		logger.Error(nativeUnavailableMessage, args...)
		return
	}
	logger.Warn(nativeUnavailableMessage, args...)
}
