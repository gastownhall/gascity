package beads

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
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
// The registration carries no credential (bdhttp.Register refuses one). A
// per-city / per-rig credential is a per-open option (remoteOpenPlanFor ->
// bdhttp.Options.Credential), never a registration property: two cities in one
// supervisor must not share a bearer.
//
// binary names the embedding build ("gc/<version>"); the backend's own
// identifier is appended, in the shape the beads http backend documents.
func RegisterRemoteBackends(binary string) {
	remoteBackendRegistration.Do(func() {
		remoteUserAgent = strings.TrimSpace(strings.TrimSpace(binary) + " " + bdhttp.UserAgentSuffix)
		bdhttp.Register(bdhttp.Options{UserAgent: remoteUserAgent})
	})
}

// nativeOpenOptions is the per-open transport seam: the HTTP client and user
// agent a remote open (and a local library open) dials with. Tests route
// opens to an in-memory server through it. The per-scope credential does not
// come from here; remoteOpenPlanFor resolves it.
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
// The per-open plan (target, transport, user agent, credential) is resolved
// ONCE here (remoteOpenPlanFor), and the wire_compat handshake and the store
// open both dial from this one value, so the probe can never verify a server
// through one transport or credential while the store dials it through
// another. A scope with a per-city / per-rig credential (city.toml [beads]
// credential, rigs.beads_credential) uses that credential alone; a credential
// failure is the terminal remote_credential gate, before any request. No
// process environment is projected or withheld for the remote open, and the
// global env mutex is never held across a network call.
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
	plan, err := remoteOpenPlanFor(ctx, opts.CityPath, opts.ScopeRoot, activationRoot)
	if err != nil {
		return refuse(remoteCredentialGate, err.Error())
	}
	checker := opts.PreflightChecker
	if checker.WireHandshake == nil {
		checker.WireHandshake = func(scope string) (contract.PreflightWireHandshake, error) {
			return remoteWireHandshakeWith(scope, plan)
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
	native, err := opts.openRemoteNativeStore(ctx, activationRoot, plan)
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

// remoteCredentialGate is the terminal gate a per-scope credential failure
// reports under native_transport=auto.
const remoteCredentialGate = "remote_credential"

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
	// remoteHandshakeDial is the handshake dial, over the same target and
	// options the store open uses (remoteOpenPlan), so the probe and the
	// store reach the server the same way, with the same credential.
	remoteHandshakeDial = func(ctx context.Context, _ string, target bdhttp.Target, opts bdhttp.Options) (*bdhttp.ServerSnapshot, error) {
		return bdhttp.Handshake(ctx, target, opts)
	}
)

// remoteOpenPlan is one remote open's resolved dial: the target (activation
// sidecar with the plaintext grant decided), and the bdhttp options carrying
// the transport, user agent and the ONE credential both the handshake and the
// store use. With S6f's bdhttp.Options.Credential, that credential is the only
// one either dial authorizes with: nothing ambient is read during a request.
type remoteOpenPlan struct {
	beadsDir  string
	target    bdhttp.Target
	targetErr error
	options   bdhttp.Options
	// credentialKey keeps handshake cache entries of differently credentialed
	// opens of one activation apart.
	credentialKey string
}

// remoteOpenPlanFor resolves the plan for one remote open.
//
//   - A scope with a per-scope credential: the credential's own target (its
//     plaintext grant is the scope's allow_insecure_credential, never the
//     sidecar's or BEADS_HTTP_ALLOW_INSECURE) and its provider. A failure is a
//     *RemoteCredentialError; there is no fallback to the ambient ladder.
//   - Otherwise (single-city posture): the ambient ladder RESOLVED NOW and
//     carried explicitly, and the ambient plaintext grants (the sidecar's
//     bd connect --allow-plaintext record or BEADS_HTTP_ALLOW_INSECURE),
//     exactly as the bd CLI reads them. Resolving once, under the env mutex
//     every Dolt open's projection holds, means the open never sees a
//     concurrent Dolt open's projection (which withholds
//     BEADS_CREDENTIALS_FILE).
//
// The transport and user agent come from the nativeOpenOptions seam.
func remoteOpenPlanFor(ctx context.Context, cityPath, scopeRoot, activationRoot string) (remoteOpenPlan, error) {
	beadsDir := filepath.Join(activationRoot, ".beads")
	seam := nativeOpenOptions(beadsDir)
	plan := remoteOpenPlan{beadsDir: beadsDir, options: bdhttp.Options{UserAgent: seam.UserAgent, HTTPClient: seam.HTTPClient}}
	if plan.options.UserAgent == "" {
		plan.options.UserAgent = remoteUserAgent
	}
	scoped, ok, err := ResolveScopeRemoteCredential(ctx, cityPath, scopeRoot)
	if err != nil {
		return remoteOpenPlan{}, err
	}
	if ok {
		plan.target = scoped.Target()
		plan.options.Credential = scoped.Provider()
		plan.credentialKey = "scope\x00" + scoped.key()
		return plan, nil
	}
	plan.target, plan.targetErr = bdhttp.LoadTarget(beadsDir)
	plan.credentialKey = "ambient"
	if plan.targetErr != nil || plan.target.BaseURL == nil {
		// The handshake reports the missing or broken activation with its
		// typed reason; there is nothing to resolve a credential for.
		return plan, nil
	}
	if err := applyAmbientCAFile(&plan.target); err != nil {
		// The bd CLI refuses the same value; the handshake reports it.
		plan.targetErr = err
		return plan, nil
	}
	if provided, isProvided := seam.Credential.(bdhttp.ProvidedCredential); isProvided && provided.Provider != nil {
		plan.options.Credential = provided.Provider
	} else {
		plan.options.Credential = cachedAmbientCredential(ctx, beadsDir, plan.target.BaseURL)
	}
	if !plan.target.AllowInsecureCredential && ambientAllowInsecureCredential() {
		plan.target.AllowInsecureCredential = true
	}
	return plan, nil
}

// applyAmbientCAFile carries BEADS_HTTP_CA_FILE onto an ambient-posture
// target, exactly as the bd CLI (and so the BdStore bd child) applies it:
// "host[:port]=path", applied only to the target it names, replacing the
// sidecar's ca_file, and refused when it names a different file than the
// sidecar for the same target. The explicit-credential door this open goes
// through reads no CA env itself (it trusts Target.CAFile alone), so without
// this the native store and the bd child of one ambient scope would trust
// different roots. A scope with a per-scope credential does not read it: its
// bd child gets BEADS_HTTP_CA_FILE blanked instead (SubprocessEnv), so both
// trust the sidecar alone.
func applyAmbientCAFile(target *bdhttp.Target) error {
	raw := strings.TrimSpace(AmbientNativeDoltOpenEnv(beadsHTTPCAFileEnv))
	if raw == "" || target == nil || target.BaseURL == nil {
		return nil
	}
	pattern, path, ok := strings.Cut(raw, "=")
	pattern, path = strings.TrimSpace(pattern), strings.TrimSpace(path)
	switch {
	case !ok || pattern == "" || path == "":
		return fmt.Errorf("%s: want host[:port]=path", beadsHTTPCAFileEnv)
	case !filepath.IsAbs(path):
		return fmt.Errorf("%s: path %q must be absolute", beadsHTTPCAFileEnv, path)
	case strings.HasSuffix(pattern, ":"):
		return fmt.Errorf("%s: host pattern %q ends with \":\" and no port; want host, or host:port", beadsHTTPCAFileEnv, pattern)
	}
	matches, err := caPatternMatchesTarget(pattern, target.BaseURL)
	if err != nil {
		return fmt.Errorf("%s: %w", beadsHTTPCAFileEnv, err)
	}
	if !matches {
		return nil
	}
	if sidecar := strings.TrimSpace(target.CAFile); sidecar != "" && !sameCAFilePath(sidecar, path) {
		return fmt.Errorf("%s names %q for %s, but its sidecar ca_file names %q; remove one so the CA used is unambiguous",
			beadsHTTPCAFileEnv, path, remoteOrigin(target.BaseURL), sidecar)
	}
	target.CAFile = path
	return nil
}

// caPatternMatchesTarget is bd's host-scoped CA match: a pattern without a
// port matches the host on any port, one with a port only that host:port
// (the scheme's default port spelled out). Hosts compare case-insensitively
// without a trailing dot; a non-ASCII host is refused rather than guessed at
// (spell it in punycode).
func caPatternMatchesTarget(pattern string, base *url.URL) (bool, error) {
	host, port := pattern, ""
	if h, p, err := net.SplitHostPort(pattern); err == nil {
		host, port = h, p
	}
	host = strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), ".")
	targetHost := strings.TrimSuffix(base.Hostname(), ".")
	for _, h := range []string{host, targetHost} {
		for _, r := range h {
			if r > 0x7e {
				return false, fmt.Errorf("host %q is not ASCII; spell it in punycode", h)
			}
		}
	}
	if host == "" || !strings.EqualFold(host, targetHost) {
		return false, nil
	}
	if port == "" {
		return true, nil
	}
	_, targetPort, _ := net.SplitHostPort(remoteCredentialHostPort(base))
	return port == targetPort, nil
}

// sameCAFilePath reports whether a and b name the same file.
func sameCAFilePath(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(infoA, infoB)
}

// ambientCredentialMaxAge bounds how long one activation's resolved ambient
// ladder answers for later opens of that activation, so a token command runs
// once per interval rather than once per open. A 401 re-walks it at once
// (resolvedAmbientCredential.Refresh), and a change to the ambient ladder's
// variables starts a new entry.
const ambientCredentialMaxAge = 60 * time.Second

type ambientCredentialEntry struct {
	fingerprint [sha256.Size]byte
	credential  *resolvedAmbientCredential
	at          time.Time
}

var (
	ambientCredentialMu    sync.Mutex
	ambientCredentialCache = map[string]ambientCredentialEntry{}
)

// cachedAmbientCredential returns the activation's resolved ambient ladder,
// reusing one resolved within ambientCredentialMaxAge under the same ambient
// variables. It is keyed by activation and server, the single-city posture's
// own scope.
func cachedAmbientCredential(ctx context.Context, beadsDir string, base *url.URL) *resolvedAmbientCredential {
	key := beadsDir + "\x00" + base.Redacted()
	h := sha256.New()
	for _, name := range []string{bdhttp.TokenEnv, bdhttp.TokenCommandEnv, "BEADS_CREDENTIALS_FILE"} {
		_, _ = h.Write([]byte(name + "=" + AmbientNativeDoltOpenEnv(name) + "\x00"))
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], h.Sum(nil))
	now := remoteHandshakeNow()
	ambientCredentialMu.Lock()
	entry, ok := ambientCredentialCache[key]
	ambientCredentialMu.Unlock()
	if ok && entry.fingerprint == fingerprint && now.Sub(entry.at) < ambientCredentialMaxAge {
		return entry.credential
	}
	credential := newResolvedAmbientCredential(ctx, base)
	ambientCredentialMu.Lock()
	ambientCredentialCache[key] = ambientCredentialEntry{fingerprint: fingerprint, credential: credential, at: now}
	ambientCredentialMu.Unlock()
	return credential
}

// ambientAllowInsecureCredential reads BEADS_HTTP_ALLOW_INSECURE as the bd CLI
// does, from the operator's environment (never a Dolt open's projection). The
// explicit-credential door reads no environment, so the single-city posture
// carries this grant onto the target itself.
func ambientAllowInsecureCredential() bool {
	v := strings.TrimSpace(AmbientNativeDoltOpenEnv(beadsHTTPAllowInsecureEnv))
	return v == "1" || strings.EqualFold(v, "true")
}

// remoteWireHandshakeWith answers the scope's server handshake, cached per
// process by (activation, server, pinned project, credential). It dials with
// the plan the store open uses, so a probe can never verify a server the
// store then cannot reach.
func remoteWireHandshakeWith(scope string, plan remoteOpenPlan) (contract.PreflightWireHandshake, error) {
	beadsDir := filepath.Join(scope, ".beads")
	target, err := plan.target, plan.targetErr
	if filepath.Clean(plan.beadsDir) != filepath.Clean(beadsDir) {
		// The plan's credential and plaintext grant were decided for its own
		// activation; never send them to a target read from another one.
		return contract.PreflightWireHandshake{}, &contract.PreflightWireError{
			Reason: contract.PreflightWireUnreachable,
			Err:    fmt.Errorf("handshake asked for %s, but this open was planned for %s", beadsDir, plan.beadsDir),
		}
	}
	if err == nil && target.BaseURL == nil {
		err = bdhttp.ErrNotConnected
	}
	if err != nil {
		reason := contract.PreflightWireUnreachable
		if errors.Is(err, bdhttp.ErrNotConnected) {
			reason = contract.PreflightWireNotConnected
		}
		return contract.PreflightWireHandshake{}, &contract.PreflightWireError{Reason: reason, Err: err}
	}
	key := beadsDir + "\x00" + target.BaseURL.Redacted() + "\x00" + target.ExpectProjectID + "\x00" + target.CAFile + "\x00" + plan.credentialKey
	remoteHandshakeMu.Lock()
	entry, ok := remoteHandshakeCache[key]
	remoteHandshakeMu.Unlock()
	if ok && remoteHandshakeNow().Sub(entry.at) < remoteHandshakeTTL {
		return entry.handshake, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), remoteHandshakeTimeout)
	defer cancel()
	snapshot, err := remoteHandshakeDial(ctx, beadsDir, target, plan.options)
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

// problemStatus is the HTTP status of a *bdhttp.ProblemError in err's chain,
// or 0.
func problemStatus(err error) int {
	var problem *bdhttp.ProblemError
	if errors.As(err, &problem) {
		return problem.Status
	}
	return 0
}

func classifyRemoteHandshakeError(err error) contract.PreflightWireReason {
	switch {
	case errors.Is(err, bdhttp.ErrProjectMismatch):
		return contract.PreflightWireProjectMismatch
	case errors.Is(err, bdhttp.ErrAPIVersion):
		return contract.PreflightWireAPIVersion
	case errors.Is(err, bdhttp.ErrCapabilityAbsent):
		return contract.PreflightWireCapability
	case errors.Is(err, bdhttp.ErrUnauthenticated), problemStatus(err) == http.StatusForbidden:
		// A 403 is a rejected credential as surely as a 401: a gateway in
		// front of bd serve answering for a token it will not honor.
		return contract.PreflightWireUnauthenticated
	default:
		return contract.PreflightWireUnreachable
	}
}

// openRemoteNativeStore opens the native store for a remote scope. It does NOT
// use OpenNativeStore: a composition root's native opener resolves a Dolt
// environment (managed port, recovery, reconnect hook) that has no meaning for
// a remote backend and could start a local Dolt server for nothing. gc
// projects nothing.
func (opts StoreOpenOptions) openRemoteNativeStore(ctx context.Context, activationRoot string, plan remoteOpenPlan) (Store, error) {
	if opts.OpenRemoteNativeStore != nil {
		return opts.OpenRemoteNativeStore()
	}
	return openRemoteNativeDoltStore(ctx, activationRoot, plan)
}

// remoteNativeOpen is the store dial, held in a variable so a test can observe
// the target and options the remote open hands it. It is beads' explicit
// per-open door (bdhttp.Open with Options.Credential): the target is the one
// the plan resolved, so the scope's plaintext grant and credential are exactly
// the handshake's. (The registry's OpenWith reloads the sidecar itself and
// cannot carry a per-scope plaintext grant.)
var remoteNativeOpen = func(ctx context.Context, target bdhttp.Target, opts bdhttp.Options) (beadslib.Storage, error) {
	return bdhttp.Open(ctx, target, opts)
}

// openRemoteNativeDoltStore opens the native store over a remote activation
// with the given plan. Unlike newNativeDoltStoreAt it projects no environment
// and takes no env mutex: the target and credential are in the plan, so
// nothing it needs lives in the process environment gc would have to fence.
func openRemoteNativeDoltStore(parent context.Context, activationRoot string, plan remoteOpenPlan) (*NativeDoltStore, error) {
	if plan.targetErr != nil {
		return nil, plan.targetErr
	}
	ctx, cancel := nativeDoltOperationContext(parent)
	defer cancel()
	storage, err := remoteNativeOpen(ctx, plan.target, plan.options)
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
