package beads

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	beadslib "github.com/steveyegge/beads"
	bdhttp "github.com/steveyegge/beads/backend/http"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// gc's native store over a REMOTE beads backend (DESIGN C2): a scope whose
// .beads/metadata.json names a backend the linked beads library registered as
// remote opens through the library's own dispatch
// (beadslib.OpenBestAvailableWith), never through a Dolt server, and never
// falls into BdStore silently.

// remoteBackendRegistration guards the one bdhttp.Register call: Register
// panics on a duplicate, and a test binary can reach the composition root more
// than once.
var remoteBackendRegistration sync.Once

// remoteUserAgent is the User-Agent the registered remote transport and the
// wire_compat handshake identify gc with. Set once by RegisterRemoteBackends.
var remoteUserAgent string

// RegisterRemoteBackends registers the remote beads backends this build links
// (today the beads http backend) with the library registry, so a scope whose
// metadata selects one opens natively. gc's composition root calls it once at
// process start, before any store opens; there is no init() registration, for
// the reason contract/backend_bundle.go gives. Later calls are no-ops.
//
// The registered backend keeps the library's own credential ladder (host-scoped
// BEADS_HTTP_TOKEN, BEADS_HTTP_TOKEN_COMMAND, the credentials file). A per-city
// credential is a per-open option (nativeOpenOptions), not a registration
// property: two cities in one supervisor must not share a bearer.
//
// binary names the embedding build ("gc/<version>"); the backend's own
// identifier is appended, in the shape the beads http backend documents.
func RegisterRemoteBackends(binary string) {
	remoteBackendRegistration.Do(func() {
		remoteUserAgent = strings.TrimSpace(strings.TrimSpace(binary) + " " + bdhttp.UserAgentSuffix)
		bdhttp.Register(bdhttp.Options{UserAgent: remoteUserAgent})
	})
}

// nativeOpenOptions is the per-open injection seam for a remote backend: the
// G7 per-city credential (and a city-level CA client) plugs in here. The zero
// value keeps the library's own credential sources, which is the posture this
// slice ships.
var nativeOpenOptions = func(string) beadslib.OpenOptions { return beadslib.OpenOptions{} }

// openBestAvailableWithNativeOptions is the default native open: the library's
// best-available dispatch, carrying the per-open options for the scope.
func openBestAvailableWithNativeOptions(ctx context.Context, beadsDir string) (beadslib.Storage, error) {
	return beadslib.OpenBestAvailableWith(ctx, beadsDir, nativeOpenOptions(beadsDir))
}

// HTTPNativeOpenRequiredError is the terminal refusal for a scope whose
// metadata names a remote backend while beads.native_transport is auto: the
// native store is required there, and a failure to open it is reported rather
// than replaced by the bd CLI store.
type HTTPNativeOpenRequiredError struct {
	ScopeRoot string
	Backend   string
	Gate      string
	Reason    string
}

// Error implements error. It names the escape hatch.
func (e *HTTPNativeOpenRequiredError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("native store required for remote beads backend %q at %s (gate %s): %s; set beads.native_transport = %q in city.toml to use the bd CLI for this city instead",
		e.Backend, e.ScopeRoot, e.Gate, e.Reason, string(NativeTransportOff))
}

// IsHTTPNativeOpenRequired reports whether err is or wraps a
// *HTTPNativeOpenRequiredError.
func IsHTTPNativeOpenRequired(err error) bool {
	var target *HTTPNativeOpenRequiredError
	return errors.As(err, &target)
}

// ErrConditionalReleaseRemoteUnsupported is BdStore.ReleaseIfCurrent's refusal
// on a remote scope whose bd lacks the conditional release verb: the raw-SQL
// fallback a local scope takes is not available over the wire.
var ErrConditionalReleaseRemoteUnsupported = errors.New("conditional release unsupported on a remote backend")

// ScopeUsesRemoteBackend reports whether the scope's metadata names a
// registered remote backend. Raw-SQL and Dolt-only paths consult it to refuse
// rather than shell out to a verb the remote backend does not serve.
func ScopeUsesRemoteBackend(scopeRoot string) bool {
	return decideMetadataBackend(scopeRoot).Route == metadataBackendRouteRemote
}

// openRemoteNative is the remote route of OpenStoreAtForCity under
// native_transport=auto. Every failure is terminal: there is no BdStore
// fallback on this route.
func (opts StoreOpenOptions) openRemoteNative(ctx context.Context, provider, backend string) (StoreOpenResult, error) {
	refuse := func(gate, reason string) (StoreOpenResult, error) {
		return StoreOpenResult{}, &HTTPNativeOpenRequiredError{ScopeRoot: opts.ScopeRoot, Backend: backend, Gate: gate, Reason: reason}
	}
	if !contract.ProviderUsesBDContract(provider) {
		return refuse(string(contract.PreflightCheckProviderContract), fmt.Sprintf("provider %q does not use the bd contract", provider))
	}
	checker := opts.PreflightChecker
	if checker.WireHandshake == nil {
		checker.WireHandshake = remoteWireHandshake
	}
	result, err := checker.Check(opts.ScopeRoot)
	if err != nil {
		return refuse("preflight_unavailable", err.Error())
	}
	if !result.NativeStoreEligible {
		diag := diagnosticFromPreflight(result)
		return refuse(diag.PreflightGate, diag.PreflightReason)
	}
	if scopeHasExecutableBdHooks(opts.ScopeRoot) {
		return refuse(nativeHooksGate, "bd hooks are installed and the native store would not run them; remove .beads/hooks/on_create,on_update,on_close")
	}
	native, err := opts.openRemoteNativeStore(ctx)
	if err != nil {
		return refuse("native_open", err.Error())
	}
	return opts.stampedResult(StoreOpenResult{
		Store: native,
		Diagnostic: BeadsDiagnostic{
			Store:               storeNameNativeDoltStore,
			NativeStoreEligible: true,
		},
	}, nil)
}

// remoteHandshakeTTL bounds how long a successful handshake answers for a
// scope. A failure is never cached, so a recovered server is seen on the next
// open; a server upgrade that adds capabilities is seen within the TTL.
const remoteHandshakeTTL = 5 * time.Minute

// remoteHandshakeTimeout bounds one handshake dial.
const remoteHandshakeTimeout = 15 * time.Second

type remoteHandshakeEntry struct {
	handshake contract.PreflightWireHandshake
	at        time.Time
}

var (
	remoteHandshakeMu    sync.Mutex
	remoteHandshakeCache = map[string]remoteHandshakeEntry{}
	remoteHandshakeNow   = time.Now
	// remoteHandshakeDial is the handshake dial. It takes the transport from
	// the same per-open options the store open uses (nativeOpenOptions), so
	// the probe and the store reach the server the same way. The per-city
	// credential (G7) joins it there.
	remoteHandshakeDial = func(ctx context.Context, beadsDir string, target bdhttp.Target) (*bdhttp.ServerSnapshot, error) {
		return bdhttp.Handshake(ctx, target, bdhttp.Options{UserAgent: remoteUserAgent, HTTPClient: nativeOpenOptions(beadsDir).HTTPClient})
	}
)

// remoteWireHandshake reads the scope's remote activation and its server's
// handshake, cached per process by (scope, server, pinned project). It dials
// with the library's own credential ladder, the same one the store open uses,
// so a probe can never verify a server the store then cannot reach.
func remoteWireHandshake(scope string) (contract.PreflightWireHandshake, error) {
	beadsDir := filepath.Join(scope, ".beads")
	target, err := bdhttp.LoadTarget(beadsDir)
	if err != nil {
		reason := contract.PreflightWireUnreachable
		if errors.Is(err, bdhttp.ErrNotConnected) {
			reason = contract.PreflightWireNotConnected
		}
		return contract.PreflightWireHandshake{}, &contract.PreflightWireError{Reason: reason, Err: err}
	}
	base := ""
	if target.BaseURL != nil {
		base = target.BaseURL.Redacted()
	}
	key := beadsDir + "\x00" + base + "\x00" + target.ExpectProjectID
	remoteHandshakeMu.Lock()
	entry, ok := remoteHandshakeCache[key]
	remoteHandshakeMu.Unlock()
	if ok && remoteHandshakeNow().Sub(entry.at) < remoteHandshakeTTL {
		return entry.handshake, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteHandshakeTimeout)
	defer cancel()
	snapshot, err := remoteHandshakeDial(ctx, beadsDir, target)
	if err != nil {
		return contract.PreflightWireHandshake{}, &contract.PreflightWireError{Reason: classifyRemoteHandshakeError(err), Err: err}
	}
	if snapshot == nil {
		return contract.PreflightWireHandshake{}, &contract.PreflightWireError{Reason: contract.PreflightWireUnreachable, Err: errors.New("server returned no handshake")}
	}
	handshake := contract.PreflightWireHandshake{
		Snapshot: contract.PreflightWireSnapshot{
			APIVersion:   snapshot.APIVersion,
			BdVersion:    snapshot.BdVersion,
			WireRevision: snapshot.WireRevision,
			ProjectID:    snapshot.ProjectID,
			Capabilities: append([]string(nil), snapshot.Capabilities...),
		},
		TargetProjectID: target.ExpectProjectID,
	}
	remoteHandshakeMu.Lock()
	remoteHandshakeCache[key] = remoteHandshakeEntry{handshake: handshake, at: remoteHandshakeNow()}
	remoteHandshakeMu.Unlock()
	return handshake, nil
}

func classifyRemoteHandshakeError(err error) contract.PreflightWireReason {
	switch {
	case errors.Is(err, bdhttp.ErrProjectMismatch):
		return contract.PreflightWireProjectMismatch
	case errors.Is(err, bdhttp.ErrAPIVersion):
		return contract.PreflightWireAPIVersion
	case errors.Is(err, bdhttp.ErrCapabilityAbsent):
		return contract.PreflightWireCapability
	case errors.Is(err, bdhttp.ErrUnauthenticated):
		return contract.PreflightWireUnauthenticated
	default:
		return contract.PreflightWireUnreachable
	}
}

// openRemoteNativeStore opens the native store for a remote scope. It does NOT
// use OpenNativeStore: a composition root's native opener resolves a Dolt
// environment (managed port, recovery, reconnect hook) that has no meaning for
// a remote backend and could start a local Dolt server for nothing. The
// library's dispatch reads the scope's own activation; gc projects nothing.
func (opts StoreOpenOptions) openRemoteNativeStore(ctx context.Context) (Store, error) {
	if opts.OpenRemoteNativeStore != nil {
		return opts.OpenRemoteNativeStore()
	}
	return newNativeDoltStoreAt(ctx, opts.ScopeRoot, nil)
}
