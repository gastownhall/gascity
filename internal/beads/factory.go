package beads

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/rollout/gate"
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

	// gcHookStampPrefix is the comment prefix gc embeds in every hook script
	// it installs. Hooks bearing this stamp are gc's own event-forwarding
	// hooks; they do not block native-store eligibility because bd-CLI
	// operations (e.g., agent writes) still invoke them for autoclose, and
	// the controller cache covers the same bead events for gc's own writes.
	gcHookStampPrefix = "# gc-hook-stamp: "
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

	// ConditionalWrites is the resolved city-global beads.conditional_writes
	// mode, stamped onto every store this open produces and latched for the
	// store's lifetime — the factory is the ONE home of the mode (DESIGN
	// §6.3); there is deliberately no per-store caller option. The zero value
	// (unset) maps to Off with a defaulted marker, so an unthreaded open path
	// behaves exactly like today's default and can never raise enforcement.
	ConditionalWrites gate.Mode

	// OnConditionalWritesDegraded receives the first (and only the first)
	// capability degrade of each store this open produces — the composition
	// root converts it into the typed beads.conditional_writes.degraded
	// event wherever a bus exists. Nil on busless paths: the seam's
	// per-resolve diagnostic remains the only surface there.
	OnConditionalWritesDegraded func(ConditionalWritesDegrade)
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
		return opts.stampedResult(StoreOpenResult{Store: store, Diagnostic: BeadsDiagnostic{Store: storeNameFileStore}}, err)
	case strings.HasPrefix(provider, "exec:") && !contract.ProviderUsesBDContract(provider):
		store, err := callStoreOpen("exec store", opts.OpenExecStore)
		return opts.stampedResult(StoreOpenResult{Store: store, Diagnostic: BeadsDiagnostic{Store: storeNameExecStore}}, err)
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
	return opts.stampedResult(StoreOpenResult{
		Store: native,
		Diagnostic: BeadsDiagnostic{
			Store:               storeNameNativeDoltStore,
			NativeStoreEligible: true,
		},
	}, nil)
}

func (opts StoreOpenOptions) openBdFallback(provider string, diag BeadsDiagnostic) (StoreOpenResult, error) {
	if strings.HasPrefix(strings.TrimSpace(provider), "exec:") && contract.ProviderUsesBDContract(provider) && opts.OpenExecStore != nil {
		diag.Store = storeNameExecStore
		store, err := callStoreOpen("exec store", opts.OpenExecStore)
		return opts.stampedResult(StoreOpenResult{Store: store, Diagnostic: diag}, err)
	}
	diag.Store = storeNameBdStore
	store, err := callStoreOpen("bd store", opts.OpenBdStore)
	return opts.stampedResult(StoreOpenResult{Store: store, Diagnostic: diag}, err)
}

// stampedResult stamps the resolved conditional-writes mode onto a
// successfully opened store — the factory is the ONE home of the mode (§6.3),
// so every selection path funnels its result through here. ModeUnset maps to
// Off with the defaulted marker: an unthreaded open path behaves exactly like
// today's default and can never raise enforcement. The default and any
// carrier-less store (exec.Store lives outside this package and cannot
// implement the unexported carrier) are logged at debug rather than recorded
// on the wire-bound BeadsDiagnostic — a deliberate §6.3 deviation; the wire
// surface for per-store verdicts is the §12.5 status wire, stage 4.
func (opts StoreOpenOptions) stampedResult(result StoreOpenResult, err error) (StoreOpenResult, error) {
	if err != nil || result.Store == nil {
		return result, err
	}
	mode, defaulted := opts.ConditionalWrites, false
	if mode == gate.ModeUnset {
		mode, defaulted = gate.Off, true
	}
	carrier, ok := result.Store.(conditionalWritesModeCarrier)
	if !ok {
		return opts.unstampableResult(result, mode, "store cannot carry the conditional-writes mode")
	}
	carrier.setConditionalWritesDegradeCallback(opts.OnConditionalWritesDegraded)
	if !carrier.stampConditionalWritesMode(mode, defaulted) {
		return opts.unstampableResult(result, mode, "store forwards the stamp into a backing that cannot carry it")
	}
	if defaulted && opts.Logger != nil {
		opts.Logger.Debug("conditional_writes mode not threaded; defaulted to off",
			slog.String("store", result.Diagnostic.Store),
			slog.String("scope", opts.ScopeRoot))
	}
	return result, nil
}

// unstampableResult resolves an open whose store cannot carry the
// conditional-writes mode. The outcome follows the gate's own cell contract
// instead of silently succeeding (the pre-review behavior): under require the
// OPEN refuses — a store that cannot enforce the fence must never be handed
// to a caller whose config promises fencing; under auto the open succeeds but
// degrades LOUDLY (warn log plus the degrade notification, fired directly —
// there is no stamp to latch on, and an open happens once per store); off and
// unset stay a debug note.
func (opts StoreOpenOptions) unstampableResult(result StoreOpenResult, mode gate.Mode, reason string) (StoreOpenResult, error) {
	switch mode {
	case gate.Require:
		return StoreOpenResult{}, fmt.Errorf("opening %s at %s: %w",
			result.Diagnostic.Store, opts.ScopeRoot,
			&ConditionalWritesRequiredError{StoreKind: result.Diagnostic.Store, Reason: reason})
	case gate.Auto:
		if opts.Logger != nil {
			opts.Logger.Warn("conditional_writes degraded at open",
				slog.String("store", result.Diagnostic.Store),
				slog.String("mode", string(mode)),
				slog.String("reason", reason),
				slog.String("scope", opts.ScopeRoot))
		}
		if opts.OnConditionalWritesDegraded != nil {
			opts.OnConditionalWritesDegraded(ConditionalWritesDegrade{
				StoreKind: result.Diagnostic.Store,
				Mode:      string(mode),
				Reason:    reason,
			})
		}
		return result, nil
	default:
		if opts.Logger != nil {
			opts.Logger.Debug("conditional_writes stamp skipped",
				slog.String("store", result.Diagnostic.Store),
				slog.String("reason", reason),
				slog.String("scope", opts.ScopeRoot))
		}
		return result, nil
	}
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

// scopeHasExecutableBdHooks reports whether any of the standard bd hooks
// (on_create, on_update, on_close) are executable and NOT installed by gc.
// GC's own stamped forwarder hooks are exempt: the controller-cache event
// path already covers bead events for gc's own writes, and bd-CLI operations
// (e.g., agent writes via bd close) still fire those hooks for autoclose.
func scopeHasExecutableBdHooks(scopeRoot string) bool {
	for _, name := range []string{"on_create", "on_update", "on_close"} {
		path := filepath.Join(scopeRoot, ".beads", "hooks", name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil || !isGCStampedHook(content) {
			return true
		}
	}
	return false
}

// isGCStampedHook reports whether the hook content contains a gc-hook-stamp
// line, meaning the hook was installed by gc as a bead event forwarder. The
// marker must begin a line (matching cmd/gc/hooks.go's stamp format) so an
// incidental occurrence in a comment or echo body cannot exempt a non-gc hook.
func isGCStampedHook(content []byte) bool {
	for _, line := range bytes.Split(content, []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte(gcHookStampPrefix)) {
			return true
		}
	}
	return false
}

func forceNativeFallback() bool {
	value := strings.TrimSpace(os.Getenv(nativeForceFallbackEnv))
	return value == "1" || strings.EqualFold(value, "true")
}

func logNativeUnavailable(logger *slog.Logger, scope, gateName, reason string) {
	if logger == nil {
		return
	}
	args := []any{
		slog.String("gate", gateName),
		slog.String("reason", reason),
		slog.String("scope", scope),
	}
	if gateName == string(contract.PreflightCheckIdentityMatch) {
		logger.Error(nativeUnavailableMessage, args...)
		return
	}
	if gateName == string(contract.PreflightCheckBDContextAgreement) {
		// Benign, expected fallback: the native store declines activation when it
		// cannot cross-verify bd's backend (e.g. the bd context probe is briefly
		// unreachable) and transparently falls back to the bd-backed store. In
		// deployments where the native store is not eligible this fires on every
		// store-open, spamming WARN on routine commands like `gc session attach`.
		// Log at DEBUG instead (still visible with -v); genuine backend
		// disagreements remain surfaced by `gc doctor`'s preflight diagnostic.
		logger.Debug(nativeUnavailableMessage, args...)
		return
	}
	logger.Warn(nativeUnavailableMessage, args...)
}
