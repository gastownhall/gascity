package beads

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	beadslib "github.com/steveyegge/beads"
	bdhttp "github.com/steveyegge/beads/backend/http"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
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

// inheritedRemoteBackend reports the city's registered remote backend for a
// rig scope that has no metadata.json of its own. Only a missing file
// inherits: metadata that exists but names nothing, or cannot be parsed, is
// the scope's own and stays with the preflight that reports it.
func inheritedRemoteBackend(scopeRoot, cityPath string) (string, bool) {
	if strings.TrimSpace(cityPath) == "" || sameScopeRoot(scopeRoot, cityPath) {
		return "", false
	}
	osfs := fsys.OSFS{}
	if _, err := osfs.Stat(filepath.Join(scopeRoot, ".beads", "metadata.json")); !errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	backend, named, err := contract.ReadMetadataBackend(fsys.OSFS{}, filepath.Join(cityPath, ".beads", "metadata.json"))
	backend = strings.TrimSpace(backend)
	if err != nil || !named || !contract.BackendIsRemote(backend) {
		return "", false
	}
	return backend, true
}

// sameScopeRoot compares two scope roots after cleaning, resolving symlinks
// where it can.
func sameScopeRoot(a, b string) bool {
	clean := func(p string) string {
		p = filepath.Clean(p)
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return resolved
		}
		return p
	}
	return clean(a) == clean(b)
}

// ScopeUsesRemoteBackend reports whether the scope's own metadata names a
// registered remote backend. Raw-SQL and Dolt-only paths consult it to refuse
// rather than shell out to a verb the remote backend does not serve.
func ScopeUsesRemoteBackend(scopeRoot string) bool {
	return decideMetadataBackend(scopeRoot, "").Route == metadataBackendRouteRemote
}

// RemoteBackendActivationRoot reports, for a scope served by a registered
// remote backend, the scope whose .beads holds the remote activation: the
// scope itself when its own metadata names the backend, or the city for a rig
// with no metadata of its own whose city's does. The native open and the bd
// CLI (native_transport = "off") both reach the store through it, so the two
// lanes of one scope always talk to the same server.
func RemoteBackendActivationRoot(scopeRoot, cityPath string) (string, bool) {
	verdict := decideMetadataBackend(scopeRoot, cityPath)
	if verdict.Route != metadataBackendRouteRemote {
		return "", false
	}
	return verdict.ActivationRoot, true
}

// openRemoteNative is the remote route of OpenStoreAtForCity under
// native_transport=auto. Every failure is terminal: there is no BdStore
// fallback on this route.
//
// The per-open options (transport, user agent, credential) are resolved ONCE
// here, and the wire_compat handshake and the store open both take them from
// this one value, so the probe can never verify a server through one
// transport while the store dials it through another. No process environment
// is projected or withheld for the remote open, and the global env mutex is
// never held across a network call: it is taken only to resolve the
// credential (remoteOpenOptions), which reads the environment and dials
// nothing.
func (opts StoreOpenOptions) openRemoteNative(ctx context.Context, provider string, route metadataBackendVerdict) (StoreOpenResult, error) {
	backend := route.Backend
	refuse := func(gate, reason string) (StoreOpenResult, error) {
		return StoreOpenResult{}, &HTTPNativeOpenRequiredError{ScopeRoot: opts.ScopeRoot, Backend: backend, Gate: gate, Reason: reason}
	}
	if !contract.ProviderUsesBDContract(provider) {
		return refuse(string(contract.PreflightCheckProviderContract), fmt.Sprintf("provider %q does not use the bd contract", provider))
	}
	activationRoot := route.ActivationRoot
	if activationRoot == "" {
		activationRoot = opts.ScopeRoot
	}
	beadsDir := filepath.Join(activationRoot, ".beads")
	openOpts := remoteOpenOptions(ctx, beadsDir)
	checker := opts.PreflightChecker
	if checker.WireHandshake == nil {
		checker.WireHandshake = func(scope string) (contract.PreflightWireHandshake, error) {
			return remoteWireHandshakeWith(scope, openOpts)
		}
	}
	result, err := checker.Check(activationRoot)
	if err != nil {
		return refuse("preflight_unavailable", err.Error())
	}
	if !result.NativeStoreEligible {
		diag := diagnosticFromPreflight(result)
		return refuse(diag.PreflightGate, diag.PreflightReason)
	}
	if scopeHasExecutableBdHooks(activationRoot) {
		return refuse(nativeHooksGate, "bd hooks are installed and the native store would not run them; remove .beads/hooks/on_create,on_update,on_close")
	}
	native, err := opts.openRemoteNativeStore(ctx, activationRoot, openOpts)
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
	// remoteHandshakeDial is the handshake dial, over the per-open options the
	// store open uses (remoteHandshakeOptions), so the probe and the store
	// reach the server the same way.
	remoteHandshakeDial = func(ctx context.Context, _ string, target bdhttp.Target, opts beadslib.OpenOptions) (*bdhttp.ServerSnapshot, error) {
		return bdhttp.Handshake(ctx, target, remoteHandshakeOptions(opts))
	}
)

// remoteHandshakeOptions maps one open's per-open options onto the
// handshake's: the same transport and user agent the store dials with.
//
// The credential is the one difference, and it is the linked library's: the
// pinned beads (S6, 4e9d3dcba9) gives bdhttp.Handshake no per-call credential
// seam, so the probe authorizes with the library's ambient ladder, read at
// request time. The remote open projects and withholds no environment, so
// that read sees the same ambient ladder (BEADS_CREDENTIALS_FILE included)
// the store's credential was resolved from, and the store holds its resolved
// credential explicitly (OpenOptions.Credential) for its lifetime. beads S6f
// (43a9c73571) adds bdhttp.Options.Credential; once the pin moves there, the
// probe takes the very same resolved credential with one line in the literal
// below: Credential: opts.Credential.(bdhttp.ProvidedCredential).Provider
// (the value remoteOpenOptions always sets when it resolved one).
func remoteHandshakeOptions(opts beadslib.OpenOptions) bdhttp.Options {
	userAgent := opts.UserAgent
	if userAgent == "" {
		userAgent = remoteUserAgent
	}
	return bdhttp.Options{UserAgent: userAgent, HTTPClient: opts.HTTPClient}
}

// remoteWireHandshake is remoteWireHandshakeWith over the scope's default
// per-open options.
func remoteWireHandshake(scope string) (contract.PreflightWireHandshake, error) {
	return remoteWireHandshakeWith(scope, nativeOpenOptions(filepath.Join(scope, ".beads")))
}

// remoteWireHandshakeWith reads the scope's remote activation and its server's
// handshake, cached per process by (scope, server, pinned project). It dials
// through the same per-open options the store open uses, so a probe can never
// verify a server the store then cannot reach.
func remoteWireHandshakeWith(scope string, openOpts beadslib.OpenOptions) (contract.PreflightWireHandshake, error) {
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
	snapshot, err := remoteHandshakeDial(ctx, beadsDir, target, openOpts)
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
// library's dispatch reads the activation; gc projects nothing.
func (opts StoreOpenOptions) openRemoteNativeStore(ctx context.Context, activationRoot string, openOpts beadslib.OpenOptions) (Store, error) {
	if opts.OpenRemoteNativeStore != nil {
		return opts.OpenRemoteNativeStore()
	}
	return openRemoteNativeDoltStore(ctx, activationRoot, openOpts)
}

// remoteNativeOpenBestAvailable is the library's per-open dispatch, held in a
// variable so a test can observe the options the remote open hands it.
var remoteNativeOpenBestAvailable = beadslib.OpenBestAvailableWith

// openRemoteNativeDoltStore opens the native store over a remote activation
// with the given per-open options. Unlike newNativeDoltStoreAt it projects no
// environment and takes no env mutex: the remote backend reads its activation
// from the workspace and its credential from openOpts, so nothing it needs
// lives in the process environment gc would have to fence.
func openRemoteNativeDoltStore(parent context.Context, activationRoot string, openOpts beadslib.OpenOptions) (*NativeDoltStore, error) {
	ctx, cancel := nativeDoltOperationContext(parent)
	defer cancel()
	storage, err := remoteNativeOpenBestAvailable(ctx, filepath.Join(activationRoot, ".beads"), openOpts)
	if err != nil {
		return nil, err
	}
	prefix, err := nativeReadIssuePrefix(ctx, storage)
	if err != nil {
		_ = storage.Close()
		return nil, fmt.Errorf("reading native issue prefix: %w", err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, nativeDoltStoreActor, prefix)
	store.localStrings = newLocalSidecar(filepath.Join(activationRoot, ".beads", "local-strings.json"))
	return store, nil
}

// remoteOpenOptions resolves the per-open options for one remote open: the
// nativeOpenOptions seam (transport, user agent, and the G7 per-city
// credential when one is plugged in there), plus, when the seam supplies no
// credential, the library's ambient bearer ladder RESOLVED NOW and carried
// explicitly as OpenOptions.Credential. Resolving it once, under the env
// mutex every Dolt open's environment projection holds, means the store
// authorizes with the ambient ladder as the operator configured it, never
// with a projection some concurrent Dolt open had installed (a Dolt open
// withholds BEADS_CREDENTIALS_FILE while it runs). The mutex covers only the
// resolution, which reads the environment and the credentials file (and runs
// a configured token command); no network call happens under it.
func remoteOpenOptions(ctx context.Context, beadsDir string) beadslib.OpenOptions {
	opts := nativeOpenOptions(beadsDir)
	if opts.Credential != nil {
		return opts
	}
	target, err := bdhttp.LoadTarget(beadsDir)
	if err != nil || target.BaseURL == nil {
		// The handshake reports the missing or broken activation with its
		// typed reason; there is nothing to resolve a credential for.
		return opts
	}
	opts.Credential = bdhttp.ProvidedCredential{Provider: newResolvedAmbientCredential(ctx, target.BaseURL)}
	return opts
}
