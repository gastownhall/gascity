package beads

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/fsys"
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

	// BeadsGateProxiedProvider is the preflight gate recorded when a scope
	// falls back to the bd CLI front door because its persisted dolt_mode is
	// proxied-server. It is an expected, healthy outcome rather than a
	// degradation: bd owns the proxy and its Dolt child, and rc.2 has no
	// library open for such a workspace. Diagnostics consumers (doctor) match
	// on this value, so the factory owns it and they read it.
	BeadsGateProxiedProvider = "proxied_provider"

	storeNameBdStore         = BeadsStoreNameBdStore
	storeNameFileStore       = BeadsStoreNameFileStore
	storeNameExecStore       = BeadsStoreNameExecStore
	storeNameNativeDoltStore = BeadsStoreNameNativeDoltStore
	nativeForceFallbackEnv   = "GC_BEADS_FORCE_FALLBACK"
	nativeForceFallbackGate  = "force_fallback"
	nativeHooksGate          = "bd_hooks"
	proxiedProviderGate      = BeadsGateProxiedProvider
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

	// Proxied carries the proxied-native lane's account of this open, and is
	// nil on every other lane.
	//
	// It is a new omitempty field rather than a change to any existing one, on
	// purpose: `gc doctor --json` and the topology matrix assert on this struct
	// today, and a proxied scope with the rollout flag OFF must serialize
	// byte-identically to what it serializes now. The proxied lane adds a
	// field; it never repurposes one.
	Proxied *ProxiedDiagnostic `json:"proxied,omitempty"`
}

// ProxiedEndpointStamp identifies the proxy generation an open was pinned to.
//
// Generation is the {pid, birth} pair proxyendpoint.PoolKey derives, not a
// counter: a proxy that dies and is restarted at the same port and even the
// same pid is a DIFFERENT generation, and a reader comparing ports alone would
// call two databases one.
type ProxiedEndpointStamp struct {
	Port       int    `json:"port,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Generation string `json:"generation,omitempty"`
}

// ProxiedDiagnostic is the proxied-native lane's account of one store open.
//
// It sits BESIDE doctor's own independent endpoint inspection rather than
// replacing it, so the two can disagree visibly. That is the point: the store's
// account is what gc decided at open, doctor's is what the endpoint looks like
// now, and an operator debugging a proxied city needs to see both in order to
// tell "the proxy changed under us" from "gc read it wrong".
type ProxiedDiagnostic struct {
	// Endpoint is the generation this open pinned. Zero when admission refused
	// before it resolved one.
	Endpoint ProxiedEndpointStamp `json:"endpoint"`
	// Evidence names how the proxy's liveness was established (argv, birth
	// token, and so on) — proxyendpoint.Evidence rendered.
	Evidence string `json:"evidence,omitempty"`
	// IdlePolicy is the proxy's idle-timeout shape: never, or finite.
	IdlePolicy string `json:"idle_policy,omitempty"`
	// Cursors are the database's two migration cursors as the probe read them
	// straight off disk.
	Cursors proxyendpoint.Cursors `json:"cursors"`
	// Verdict is why the lane refused, empty when it did not.
	Verdict ProxiedVerdict `json:"verdict,omitempty"`
	// Detail carries an unexpected opener failure's text. A verdict refusal
	// leaves it empty — the verdict IS the explanation — except head_moved,
	// which is an incident rather than a refusal and carries both hashes.
	Detail string `json:"detail,omitempty"`
	// Demoted reports that a handle which had been serving natively has
	// dropped to the bd leaf. One-way; the wrapper never promotes.
	Demoted bool `json:"demoted,omitempty"`
}

// ProxiedOpenReport is what a proxied opener tells the factory about the open
// it attempted.
//
// It is returned on FAILURE as well as success, because a refusal that also
// reports which generation it was looking at and which cursors it read is a
// diagnostic, and one that reports only a verdict string is a mystery.
type ProxiedOpenReport struct {
	Endpoint   ProxiedEndpointStamp
	Evidence   string
	IdlePolicy string
	Cursors    proxyendpoint.Cursors
	Demoted    bool
}

// diagnostic projects the report, plus whatever ended the open, onto the wire.
func (r ProxiedOpenReport) diagnostic(verdict ProxiedVerdict, detail string) *ProxiedDiagnostic {
	return &ProxiedDiagnostic{
		Endpoint:   r.Endpoint,
		Evidence:   r.Evidence,
		IdlePolicy: r.IdlePolicy,
		Cursors:    r.Cursors,
		Verdict:    verdict,
		Detail:     detail,
		Demoted:    r.Demoted,
	}
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

	// OpenProxiedStore opens the split store for a proxied-server scope:
	// reads on a native handle over bd's proxy data port, mutations on the bd
	// leaf. It is consulted ONLY when the persisted topology is
	// proxied-server, GC_BEADS_PROXIED_NATIVE is on, and it is non-nil — so a
	// composition root that has not been taught about the lane, and every
	// binary with the flag off, take exactly the path they take today.
	//
	// longLived is threaded through the call rather than captured by the
	// closure so one opener per scope serves both shapes: the controller holds
	// its store for the process lifetime, a one-shot command does not, and the
	// idle-policy rule turns on that difference.
	//
	// The report comes back on failure as well as success, because a refusal
	// that names the generation it was looking at is a diagnostic and one that
	// names only a verdict is a mystery.
	OpenProxiedStore func(ctx context.Context, longLived bool) (Store, ProxiedOpenReport, error)

	// LongLived says whether the store this open produces will be held for the
	// process lifetime (a controller or rig store) rather than used once and
	// dropped.
	LongLived bool

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

// persistedDoltModeRefusal reports the diagnostic for a scope whose persisted
// dolt_mode rules out the native store, and whether it does. metadata.json is
// the authority beads writes; .beads/config.yaml is the older location and is
// consulted only when metadata carries no mode.
func persistedDoltModeRefusal(scopeRoot string) (BeadsDiagnostic, bool) {
	bdFallback := func(gate, reason string) BeadsDiagnostic {
		return BeadsDiagnostic{Store: storeNameBdStore, NativeStoreEligible: false, PreflightGate: gate, PreflightReason: reason}
	}
	metadataPath := filepath.Join(scopeRoot, ".beads", "metadata.json")
	metadataBackend, backendOK, _ := contract.ReadMetadataBackend(fsys.OSFS{}, metadataPath)
	if mode, ok, modeErr := contract.ReadDoltMode(fsys.OSFS{}, metadataPath); modeErr == nil && ok && (!backendOK || contract.IsDoltBackend(metadataBackend)) {
		switch strings.ToLower(strings.TrimSpace(mode)) {
		case "proxied-server":
			return bdFallback(proxiedProviderGate, "proxied-server mode is owned by the bd provider"), true
		case "server", "embedded":
			return BeadsDiagnostic{}, false
		default:
			return bdFallback("unsupported_dolt_mode", fmt.Sprintf("unsupported persisted dolt_mode %q", mode)), true
		}
	}
	configPath := filepath.Join(scopeRoot, ".beads", "config.yaml")
	cfg, cfgOK, cfgErr := contract.ReadConfigState(fsys.OSFS{}, configPath)
	if cfgErr != nil && !os.IsNotExist(cfgErr) {
		return bdFallback("config_unreadable", fmt.Sprintf("read beads config: %v", cfgErr)), true
	}
	if !cfgOK {
		return BeadsDiagnostic{}, false
	}
	// config.yaml is a legacy compatibility input for the direct/server shapes
	// only. The proxied binding lives in metadata.json and bd writes no
	// dolt.mode of its own, so "proxied-server" here is drift rather than a
	// topology decision; it is not treated as authority and preflight decides.
	switch strings.ToLower(strings.TrimSpace(cfg.DoltMode)) {
	case "", "server", "embedded", "proxied-server":
		return BeadsDiagnostic{}, false
	default:
		return bdFallback("unsupported_dolt_mode", fmt.Sprintf("unsupported persisted dolt_mode %q", cfg.DoltMode)), true
	}
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

	// The persisted topology is checked before preflight runs. A proxied-server
	// scope has no library open in beads at all, so no preflight verdict can
	// change the outcome — and preflight's bd-context probe does not survive
	// the proxy, which used to leave a healthy proxied city reporting the
	// BdStore front door under gate bd_context_agreement ("bd context is
	// unreachable"). The gate a reader sees has to name the reason that
	// actually decided.
	if diag, refused := persistedDoltModeRefusal(opts.ScopeRoot); refused {
		if diag.PreflightGate == proxiedProviderGate {
			// The proxied arm sits between the persisted-topology refusal and
			// preflight, and preflight is deliberately NEVER reached for a
			// proxied scope: its bd-context probe would add a fork per open,
			// and `bd context` restarts a stopped proxy — so re-enabling it
			// here would make a diagnostic path change the city.
			if served, result, err := opts.openProxiedNative(ctx, &diag); served {
				return result, err
			}
		} else {
			logNativeUnavailable(opts.Logger, opts.ScopeRoot, diag.PreflightGate, diag.PreflightReason)
		}
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

// openProxiedNative is the proxied-server arm.
//
// It reports served=true only when the split store is open and is the result
// the caller should return. On every other outcome it reports served=false,
// having annotated diag with the lane's account, and the caller falls through
// to the bd front door — which is the SAME fallback a proxied scope takes
// today, under the SAME gate. PreflightGate stays proxied_provider on purpose:
// doctor's matcher keys on that value, and a healthy fallback is not a
// degradation to be renamed. The verdict rides in the new omitempty field.
//
// Three guards decide whether the lane is even consulted, and all three must
// pass: the rollout flag, a non-nil opener, and a persisted proxied-server
// topology (the caller's condition). Nothing else in this function can turn
// the lane on.
func (opts StoreOpenOptions) openProxiedNative(ctx context.Context, diag *BeadsDiagnostic) (bool, StoreOpenResult, error) {
	if !proxiedNativeEnabled() || opts.OpenProxiedStore == nil {
		return false, StoreOpenResult{}, nil
	}
	store, report, err := opts.OpenProxiedStore(ctx, opts.LongLived)
	if err == nil && store != nil {
		result, stampErr := opts.stampedResult(StoreOpenResult{
			Store: store,
			Diagnostic: BeadsDiagnostic{
				// The flag-on lane reports the store it actually is. There is
				// no "ProxiedStore" name on the wire: the reads a caller gets
				// are a NativeDoltStore's, and inventing a third store name
				// would break every consumer that already knows two.
				Store:               storeNameNativeDoltStore,
				NativeStoreEligible: true,
				Proxied:             report.diagnostic(ProxiedVerdictNone, ""),
			},
		}, nil)
		return true, result, stampErr
	}
	if err == nil {
		err = errors.New("proxied store opener returned no store and no error")
	}
	if verdictErr, ok := ProxiedVerdictOf(err); ok && verdictErr.Verdict == ProxiedVerdictHeadMoved {
		// NOT an expected refusal: HEAD moved across gc's own library open,
		// which may be gc having committed to bd's database (council pr2 D-F3).
		// The city still gets its store, but the operator is told — at WARN,
		// with both hashes — and the diagnostic keeps the detail, because
		// "head_moved" alone does not say which database or which commit.
		//
		// Through the lane's incident log, NOT logNativeUnavailable: that one
		// returns on a nil Logger, and the controller's rig stores pass none,
		// so the long-lived open this incident is most likely on was the one
		// open that could not report it (council pr2 E-S2).
		diag.Proxied = report.diagnostic(verdictErr.Verdict, verdictErr.Detail)
		logProxiedHeadMoved(opts.Logger, opts.ScopeRoot, ProxiedIncidentSiteOpen, verdictErr)
		return false, StoreOpenResult{}, nil
	}
	if verdictErr, ok := ProxiedVerdictOf(err); ok {
		// An expected refusal. The bd front door is the designed outcome, so
		// this is not logged as an outage: proxied_provider is the one gate
		// logNativeUnavailable deliberately stays quiet about, and a warning
		// per open on every scope of a healthy Finite-idle city would be noise
		// an operator learns to ignore.
		diag.Proxied = report.diagnostic(verdictErr.Verdict, "")
		return false, StoreOpenResult{}, nil
	}
	// An UNTYPED failure is a different thing: admission is supposed to name
	// every outcome, so this is a bug or an unhandled shape. The city still
	// gets its store -- an operator who turned on a rollout flag must not lose
	// a city to it -- but loudly, and with the text preserved.
	diag.Proxied = report.diagnostic(ProxiedVerdictNone, err.Error())
	logNativeUnavailable(opts.Logger, opts.ScopeRoot, proxiedProviderGate, err.Error())
	return false, StoreOpenResult{}, nil
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
