package beads

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	bdhttp "github.com/steveyegge/beads/backend/http"

	"github.com/gastownhall/gascity/internal/beads/credsource"
)

// Per-city / per-rig credentials for a remote beads backend (DESIGN C3, G7).
//
// A city (or rig) may name its bearer's source in city.toml. When it does, that
// source is resolved ONCE per scope at open and is then the only credential the
// scope uses: the native store and the wire_compat handshake carry it as
// bdhttp.Options.Credential (beads' explicit door: no ambient token, token
// command, credentials file or BEADS_HTTP_ALLOW_INSECURE is read), and the bd
// subprocess gets it scoped to that one child (RemoteCredentialSubprocessEnv).
// Nothing is registered process-wide: two cities in one supervisor each hold
// their own provider. With no per-scope credential configured, a scope keeps
// the ambient ladder, which is keyed by host and port and so only correct for
// a single city per server and process.

// RemoteCredentialConfig is the configured credential for one scope, as the
// composition root's lookup reports it.
type RemoteCredentialConfig struct {
	// Source is where the bearer lives.
	Source credsource.Source
	// AllowInsecure maps to bdhttp.Target.AllowInsecureCredential: it permits
	// the bearer over plain http to a non-loopback server.
	AllowInsecure bool
	// Scope names where it was configured ("[beads]", `rig "x"`).
	Scope string
	// FromCity reports the city's [beads] credential inherited by a rig (or
	// the city scope itself). It is only sent to the city's own server.
	FromCity bool
	// Dir resolves a relative file source and runs a command source: the
	// city directory.
	Dir string
}

// RemoteCredentialLookup reports the configured credential for a scope, or
// ok=false when none is configured (the scope keeps the ambient ladder).
type RemoteCredentialLookup func(cityPath, scopeRoot string) (RemoteCredentialConfig, bool, error)

var (
	remoteCredentialLookupMu sync.RWMutex
	remoteCredentialLookup   RemoteCredentialLookup
)

// SetRemoteCredentialLookup installs the composition root's per-scope
// credential lookup (gc reads it from city.toml). It is a lookup by city and
// scope, not a credential registry: every answer is that city's own. Without
// one (a library caller), every scope keeps the ambient ladder.
func SetRemoteCredentialLookup(fn RemoteCredentialLookup) {
	remoteCredentialLookupMu.Lock()
	remoteCredentialLookup = fn
	remoteCredentialLookupMu.Unlock()
}

func currentRemoteCredentialLookup() RemoteCredentialLookup {
	remoteCredentialLookupMu.RLock()
	defer remoteCredentialLookupMu.RUnlock()
	return remoteCredentialLookup
}

// RemoteCredentialReason classifies a *RemoteCredentialError.
type RemoteCredentialReason string

const (
	// RemoteCredentialReasonLookup means the city config could not be read.
	RemoteCredentialReasonLookup RemoteCredentialReason = "lookup"
	// RemoteCredentialReasonNotConnected means the scope has no usable activation
	// (http_target.json) to bind the credential to.
	RemoteCredentialReasonNotConnected RemoteCredentialReason = "not_connected"
	// RemoteCredentialReasonResolve means the source failed to produce a token.
	RemoteCredentialReasonResolve RemoteCredentialReason = "resolve"
	// RemoteCredentialReasonInsecure means plain http to a non-loopback server
	// without allow_insecure_credential.
	RemoteCredentialReasonInsecure RemoteCredentialReason = "insecure_transport"
	// RemoteCredentialReasonServerMismatch means the city's credential would go to
	// a server other than the city's own.
	RemoteCredentialReasonServerMismatch RemoteCredentialReason = "server_mismatch"
)

var (
	// ErrRemoteCredentialInsecure is the plaintext refusal (errors.Is on a
	// *RemoteCredentialError).
	ErrRemoteCredentialInsecure = errors.New("refusing to send a bearer over plain http to a non-loopback server")
	// ErrRemoteCredentialServerMismatch is the refusal to send the city's
	// credential to a server other than the city's own.
	ErrRemoteCredentialServerMismatch = errors.New("the city's credential is only sent to the city's own server")
)

// RemoteCredentialError is a typed per-scope credential failure. It names the
// configured source (redacted: variable name, command program, file path) and
// the scope, never the credential.
type RemoteCredentialError struct {
	ScopeRoot string
	Scope     string
	Source    string
	Reason    RemoteCredentialReason
	Err       error
}

// Error implements error.
func (e *RemoteCredentialError) Error() string {
	if e == nil {
		return ""
	}
	where := e.Scope
	if where == "" {
		where = "beads credential"
	}
	src := ""
	if e.Source != "" {
		src = " (" + e.Source + ")"
	}
	return fmt.Sprintf("remote beads credential %s%s for %s [%s]: %v", where, src, e.ScopeRoot, e.Reason, e.Err)
}

// Unwrap exposes the cause.
func (e *RemoteCredentialError) Unwrap() error { return e.Err }

// IsRemoteCredentialError reports whether err is or wraps a
// *RemoteCredentialError.
func IsRemoteCredentialError(err error) bool {
	var target *RemoteCredentialError
	return errors.As(err, &target)
}

// ScopeRemoteCredential is one scope's resolved credential, bound to the
// target it may be sent to.
type ScopeRemoteCredential struct {
	config   RemoteCredentialConfig
	target   bdhttp.Target
	provider *sourceCredential
}

// Target is the scope's activation with the plaintext grant set from the
// scope's configuration alone (allow_insecure_credential), never from the
// sidecar or the environment.
func (c *ScopeRemoteCredential) Target() bdhttp.Target { return c.target }

// Provider is the bearer provider to pass as bdhttp.Options.Credential.
func (c *ScopeRemoteCredential) Provider() bdhttp.CredentialProvider { return c.provider }

// Source names the configured source, redacted.
func (c *ScopeRemoteCredential) Source() string { return c.config.Source.String() }

// key identifies the configuration this credential was resolved from.
func (c *ScopeRemoteCredential) key() string {
	return remoteCredentialConfigKey(c.config, c.target)
}

func remoteCredentialConfigKey(cfg RemoteCredentialConfig, target bdhttp.Target) string {
	base := ""
	if target.BaseURL != nil {
		base = target.BaseURL.Redacted()
	}
	return fmt.Sprintf("%s\x00%t\x00%s\x00%s", cfg.Source.Key(), cfg.AllowInsecure, cfg.Dir, base)
}

// Subprocess env keys of bd's own credential ladder.
const (
	beadsHTTPTokenEnv         = "BEADS_HTTP_TOKEN"         // #nosec G101 -- env var name
	beadsHTTPTokenCommandEnv  = "BEADS_HTTP_TOKEN_COMMAND" // #nosec G101 -- env var name
	beadsHTTPAllowInsecureEnv = "BEADS_HTTP_ALLOW_INSECURE"
)

// SubprocessEnv is the environment a bd child for this scope needs, and only
// that child: its own token in bd's highest ladder rung (BEADS_HTTP_TOKEN,
// scoped to the target's host:port), the token command rung blanked so an
// ambient helper is never run, and the plaintext grant set from the scope's
// configuration alone. The token never goes on argv.
func (c *ScopeRemoteCredential) SubprocessEnv(ctx context.Context) (map[string]string, error) {
	token, err := c.provider.current(ctx)
	if err != nil {
		return nil, err
	}
	env := map[string]string{
		beadsHTTPTokenEnv:         remoteCredentialHostPort(c.target.BaseURL) + "=" + token,
		beadsHTTPTokenCommandEnv:  "",
		beadsHTTPAllowInsecureEnv: "",
	}
	if c.config.AllowInsecure {
		env[beadsHTTPAllowInsecureEnv] = "1"
	}
	return env, nil
}

// remoteCredentialHostPort renders bd's host-scoped env pattern for base,
// spelling a scheme-default port out.
func remoteCredentialHostPort(base *url.URL) string {
	if base == nil {
		return ""
	}
	port := base.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(base.Scheme, "https") {
			port = "443"
		}
	}
	return net.JoinHostPort(base.Hostname(), port)
}

// ResolveScopeRemoteCredential resolves the configured credential for a scope
// served by a registered remote backend. ok is false (with a nil error) when
// the scope is not remote, no lookup is installed, or no credential is
// configured for it: the caller keeps the ambient ladder. Every failure is a
// *RemoteCredentialError and is terminal for the scope: a configured
// credential never falls back to an ambient one.
//
// The checks run before any request: the scope must be activated, a city
// credential is sent only to the city's own server (scheme, host, port), and
// plain http to a non-loopback host needs allow_insecure_credential.
func ResolveScopeRemoteCredential(ctx context.Context, cityPath, scopeRoot string) (*ScopeRemoteCredential, bool, error) {
	return ResolveScopeRemoteCredentialCached(ctx, cityPath, scopeRoot, nil)
}

// ResolveScopeRemoteCredentialCached is ResolveScopeRemoteCredential that
// reuses prev (and its resolved token) when the scope's configuration and
// target are unchanged, so a long-lived runner does not re-run a token
// command for every bd child. Every check except the token resolution runs
// again on each call.
func ResolveScopeRemoteCredentialCached(ctx context.Context, cityPath, scopeRoot string, prev *ScopeRemoteCredential) (*ScopeRemoteCredential, bool, error) {
	cfg, target, ok, err := prepareScopeRemoteCredential(cityPath, scopeRoot)
	if err != nil || !ok {
		return nil, ok, err
	}
	if prev != nil && remoteCredentialConfigKey(cfg, target) == prev.key() {
		return prev, true, nil
	}
	provider := &sourceCredential{source: cfg.Source, dir: cfg.Dir}
	if _, err := provider.current(ctx); err != nil {
		return nil, false, &RemoteCredentialError{ScopeRoot: scopeRoot, Scope: cfg.Scope, Source: cfg.Source.String(), Reason: RemoteCredentialReasonResolve, Err: err}
	}
	return &ScopeRemoteCredential{config: cfg, target: target, provider: provider}, true, nil
}

// prepareScopeRemoteCredential reads the scope's configured credential and
// binds it to the scope's target, running every pre-request check. It reads
// config and the activation sidecars only; it resolves no token.
func prepareScopeRemoteCredential(cityPath, scopeRoot string) (RemoteCredentialConfig, bdhttp.Target, bool, error) {
	activationRoot, remote := RemoteBackendActivationRoot(scopeRoot, cityPath)
	if !remote {
		return RemoteCredentialConfig{}, bdhttp.Target{}, false, nil
	}
	lookup := currentRemoteCredentialLookup()
	if lookup == nil {
		return RemoteCredentialConfig{}, bdhttp.Target{}, false, nil
	}
	cfg, ok, err := lookup(cityPath, scopeRoot)
	if err != nil {
		return RemoteCredentialConfig{}, bdhttp.Target{}, false, &RemoteCredentialError{ScopeRoot: scopeRoot, Reason: RemoteCredentialReasonLookup, Err: err}
	}
	if !ok || cfg.Source.IsZero() {
		return RemoteCredentialConfig{}, bdhttp.Target{}, false, nil
	}
	fail := func(reason RemoteCredentialReason, err error) (RemoteCredentialConfig, bdhttp.Target, bool, error) {
		return RemoteCredentialConfig{}, bdhttp.Target{}, false, &RemoteCredentialError{ScopeRoot: scopeRoot, Scope: cfg.Scope, Source: cfg.Source.String(), Reason: reason, Err: err}
	}
	target, err := bdhttp.LoadTarget(filepath.Join(activationRoot, ".beads"))
	if err != nil {
		return fail(RemoteCredentialReasonNotConnected, err)
	}
	if target.BaseURL == nil {
		return fail(RemoteCredentialReasonNotConnected, errors.New("activation names no server"))
	}
	if cfg.FromCity && strings.TrimSpace(cityPath) != "" && !sameScopeRoot(activationRoot, cityPath) {
		cityTarget, cityErr := bdhttp.LoadTarget(filepath.Join(cityPath, ".beads"))
		if cityErr != nil || cityTarget.BaseURL == nil || remoteOrigin(cityTarget.BaseURL) != remoteOrigin(target.BaseURL) {
			return fail(RemoteCredentialReasonServerMismatch, fmt.Errorf("%w: this scope is attached to %s, the city to %s; set the rig's beads_credential",
				ErrRemoteCredentialServerMismatch, remoteOrigin(target.BaseURL), cityOriginForMessage(cityTarget, cityErr)))
		}
	}
	if remotePlaintextNonLoopback(target.BaseURL) && !cfg.AllowInsecure {
		return fail(RemoteCredentialReasonInsecure, fmt.Errorf("%w %s; serve it over https, bind it to loopback, or set allow_insecure_credential = true for this scope",
			ErrRemoteCredentialInsecure, remoteOrigin(target.BaseURL)))
	}
	// The plaintext grant comes from this scope's configuration alone, never
	// from the sidecar's bd connect --allow-plaintext record.
	target.AllowInsecureCredential = cfg.AllowInsecure
	return cfg, target, true, nil
}

func cityOriginForMessage(target bdhttp.Target, err error) string {
	if err != nil || target.BaseURL == nil {
		return "no server"
	}
	return remoteOrigin(target.BaseURL)
}

// remoteOrigin is scheme://host:port with the default port spelled out.
func remoteOrigin(base *url.URL) string {
	if base == nil {
		return ""
	}
	return strings.ToLower(base.Scheme) + "://" + strings.ToLower(remoteCredentialHostPort(base))
}

// remotePlaintextNonLoopback reports a plain-http target that is not loopback.
func remotePlaintextNonLoopback(base *url.URL) bool {
	if base == nil || !strings.EqualFold(base.Scheme, "http") {
		return false
	}
	host := strings.TrimSuffix(strings.ToLower(base.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return false
	}
	return true
}

// sourceCredential is the bdhttp.CredentialProvider over one configured
// source. It resolves on first use and caches the token for the life of the
// store (or subprocess runner) holding it; after a 401 it re-reads the source
// once (a rotated token). The token appears only in the Authorization header
// and in the one bd child it is handed to.
type sourceCredential struct {
	source credsource.Source
	dir    string

	mu       sync.Mutex
	resolved bool
	token    string
}

func (c *sourceCredential) resolve(ctx context.Context) (string, error) {
	return c.source.Resolve(ctx, credsource.ResolveOptions{
		// The process environment as the operator set it, never a Dolt
		// open's in-flight projection.
		Getenv:  AmbientNativeDoltOpenEnv,
		Environ: ProcessEnvSnapshotExcludingNativeDoltOpen,
		Dir:     c.dir,
	})
}

func (c *sourceCredential) current(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved {
		return c.token, nil
	}
	token, err := c.resolve(ctx)
	if err != nil {
		return "", err
	}
	c.token, c.resolved = token, true
	return token, nil
}

// Authorize implements the beads CredentialProvider contract.
func (c *sourceCredential) Authorize(ctx context.Context, req *http.Request) error {
	token, err := c.current(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// Refresh implements the beads CredentialProvider contract: re-read the
// source; retry only when it now yields a different token.
func (c *sourceCredential) Refresh(ctx context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	token, err := c.resolve(ctx)
	if err != nil {
		return false, err
	}
	changed := !c.resolved || token != c.token
	c.token, c.resolved = token, true
	return changed, nil
}

// RemoteCredentialSubprocessEnv is the per-runner holder a bd subprocess
// runner uses: it resolves the scope's credential once (on the first command)
// and re-resolves only when the scope's configuration changes. A failed
// resolution is not cached. It answers ok=false when the scope has no
// per-scope credential, and the runner then leaves bd's ambient ladder alone.
type RemoteCredentialSubprocessEnv struct {
	cityPath  string
	scopeRoot string

	mu     sync.Mutex
	cached *ScopeRemoteCredential
}

// NewRemoteCredentialSubprocessEnv builds the holder for one scope's runner.
func NewRemoteCredentialSubprocessEnv(cityPath, scopeRoot string) *RemoteCredentialSubprocessEnv {
	return &RemoteCredentialSubprocessEnv{cityPath: cityPath, scopeRoot: scopeRoot}
}

// Apply writes the scope's credential env into env (the overrides of one bd
// child) when the scope has a per-scope credential. It reports whether it did.
func (h *RemoteCredentialSubprocessEnv) Apply(ctx context.Context, env map[string]string) (bool, error) {
	if h == nil || env == nil {
		return false, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	fresh, ok, err := ResolveScopeRemoteCredentialCached(ctx, h.cityPath, h.scopeRoot, h.cached)
	if err != nil {
		return false, err
	}
	if !ok {
		h.cached = nil
		return false, nil
	}
	h.cached = fresh
	projected, err := fresh.SubprocessEnv(ctx)
	if err != nil {
		h.cached = nil
		return false, &RemoteCredentialError{ScopeRoot: h.scopeRoot, Scope: fresh.config.Scope, Source: fresh.Source(), Reason: RemoteCredentialReasonResolve, Err: err}
	}
	for k, v := range projected {
		env[k] = v
	}
	return true, nil
}
