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
	storeNameBdStore         = "BdStore"
	storeNameCachingStore    = "CachingStore"
	storeNameFileStore       = "FileStore"
	storeNameExecStore       = "ExecStore"
	nativeForceFallbackEnv   = "GC_BEADS_FORCE_FALLBACK"
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
	case strings.HasPrefix(provider, "exec:") && !providerUsesBdContract(provider):
		store, err := callStoreOpen("exec store", opts.OpenExecStore)
		return StoreOpenResult{Store: store, Diagnostic: BeadsDiagnostic{Store: storeNameExecStore}}, err
	}

	if forceNativeFallback() {
		diag := BeadsDiagnostic{
			Store:               storeNameBdStore,
			NativeStoreEligible: false,
			PreflightGate:       string(contract.PreflightCheckProviderContract),
			PreflightReason:     nativeForceFallbackEnv + "=1",
		}
		logNativeUnavailable(opts.Logger, opts.ScopeRoot, diag.PreflightGate, diag.PreflightReason)
		return opts.openBdFallback(diag)
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
		return opts.openBdFallback(diag)
	}
	diag := diagnosticFromPreflight(result)
	if !result.NativeStoreEligible {
		logNativeUnavailable(opts.Logger, opts.ScopeRoot, diag.PreflightGate, diag.PreflightReason)
		return opts.openBdFallback(diag)
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
		return opts.openBdFallback(diag)
	}
	return StoreOpenResult{
		Store: NewCachingStore(native, nil),
		Diagnostic: BeadsDiagnostic{
			Store:               storeNameCachingStore,
			NativeStoreEligible: true,
		},
	}, nil
}

func (opts StoreOpenOptions) openBdFallback(diag BeadsDiagnostic) (StoreOpenResult, error) {
	store, err := callStoreOpen("bd store", opts.OpenBdStore)
	return StoreOpenResult{Store: store, Diagnostic: diag}, err
}

func (opts StoreOpenOptions) openNativeStore(ctx context.Context) (Store, error) {
	if opts.OpenNativeStore != nil {
		return opts.OpenNativeStore()
	}
	return newNativeDoltStoreAt(ctx, opts.ScopeRoot)
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

func providerUsesBdContract(provider string) bool {
	provider = strings.TrimSpace(provider)
	if provider == "" || provider == "bd" {
		return true
	}
	if !strings.HasPrefix(provider, "exec:") {
		return false
	}
	base := strings.TrimSuffix(filepath.Base(strings.TrimPrefix(provider, "exec:")), ".sh")
	return base == "gc-beads-bd"
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
