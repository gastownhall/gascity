package beads

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bdhttp "github.com/steveyegge/beads/backend/http"

	"github.com/gastownhall/gascity/internal/beads/credsource"
)

// Per-city / per-rig credentials for a remote beads backend (DESIGN C3, G7).
//
// A city (or rig) may name its bearer's source in city.toml. When it does, that
// source is the only credential the scope uses: the native store and the
// wire_compat handshake carry it as bdhttp.Options.Credential (beads' explicit
// door: no ambient token, token command, credentials file, CA env or
// BEADS_HTTP_ALLOW_INSECURE is read), and the bd subprocess gets it scoped to
// that one child (RemoteCredentialSubprocessEnv). One provider per scope holds
// the token (scopeCredentialCache, keyed by scope, never by host): two cities
// in one supervisor each hold their own, and a scope's opens and children
// share one refresh policy. With no per-scope credential configured, a scope keeps
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
	// cacheKey identifies the scope and the configuration the provider was
	// resolved from (scopeCredentialCache).
	cacheKey string
}

// Target is the scope's activation with the plaintext grant set from the
// scope's configuration alone (allow_insecure_credential), never from the
// sidecar or the environment.
func (c *ScopeRemoteCredential) Target() bdhttp.Target { return c.target }

// Provider is the bearer provider to pass as bdhttp.Options.Credential.
func (c *ScopeRemoteCredential) Provider() bdhttp.CredentialProvider { return c.provider }

// Source names the configured source, redacted.
func (c *ScopeRemoteCredential) Source() string { return c.config.Source.String() }

// key identifies the scope and the configuration this credential was resolved
// from.
func (c *ScopeRemoteCredential) key() string { return c.cacheKey }

func remoteCredentialConfigKey(cfg RemoteCredentialConfig, target bdhttp.Target) string {
	base := ""
	if target.BaseURL != nil {
		base = target.BaseURL.Redacted()
	}
	return fmt.Sprintf("%s\x00%t\x00%s\x00%s\x00%s", cfg.Source.Key(), cfg.AllowInsecure, cfg.Dir, base, target.CAFile)
}

// Subprocess env keys of bd's own credential ladder.
const (
	beadsHTTPTokenEnv         = "BEADS_HTTP_TOKEN"         // #nosec G101 -- env var name
	beadsHTTPTokenCommandEnv  = "BEADS_HTTP_TOKEN_COMMAND" // #nosec G101 -- env var name
	beadsHTTPAllowInsecureEnv = "BEADS_HTTP_ALLOW_INSECURE"
	beadsHTTPCAFileEnv        = "BEADS_HTTP_CA_FILE"
)

// SubprocessEnv is the environment a bd child for this scope needs, and only
// that child: its own token in bd's highest ladder rung (BEADS_HTTP_TOKEN,
// scoped to the target's host:port), the token command rung blanked so an
// ambient helper is never run, the plaintext grant set from the scope's
// configuration alone, and BEADS_HTTP_CA_FILE blanked so the child trusts the
// sidecar's ca_file alone, exactly as the native store's explicit door does.
// The token never goes on argv. The token is the provider's current one: it
// is re-read per the source's refresh policy (sourceCredential.current), so a
// rotated token reaches the next child.
func (c *ScopeRemoteCredential) SubprocessEnv(ctx context.Context) (map[string]string, error) {
	token, err := c.provider.current(ctx)
	if err != nil {
		return nil, err
	}
	env := map[string]string{
		beadsHTTPTokenEnv:         remoteCredentialHostPort(c.target.BaseURL) + "=" + token,
		beadsHTTPTokenCommandEnv:  "",
		beadsHTTPAllowInsecureEnv: "",
		beadsHTTPCAFileEnv:        "",
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
// plain http to a non-loopback host needs allow_insecure_credential. They run
// again on every call; only the token is cached, per scope
// (scopeCredentialCache), so the native opens and the bd children of one
// scope share one provider: a token command runs once per refresh interval,
// not once per open or per bd command, and a 401 refresh by any of them is
// seen by all of them.
func ResolveScopeRemoteCredential(ctx context.Context, cityPath, scopeRoot string) (*ScopeRemoteCredential, bool, error) {
	cfg, target, ok, err := prepareScopeRemoteCredential(cityPath, scopeRoot)
	if err != nil || !ok {
		return nil, ok, err
	}
	scope := scopeCredentialScope(cityPath, scopeRoot)
	key := scope + "\x00" + remoteCredentialConfigKey(cfg, target)
	provider := cachedScopeCredentialProvider(scope, key, cfg)
	if _, err := provider.current(ctx); err != nil {
		return nil, false, &RemoteCredentialError{ScopeRoot: scopeRoot, Scope: cfg.Scope, Source: cfg.Source.String(), Reason: RemoteCredentialReasonResolve, Err: err}
	}
	return &ScopeRemoteCredential{config: cfg, target: target, provider: provider, cacheKey: key}, true, nil
}

// scopeCredentialCache holds one provider per scope (city, scope root): the
// token a scope's source last yielded, shared by that scope's native opens and
// bd subprocess runners. It is keyed by scope, never by host, so two scopes on
// one server never share an entry; a scope whose configuration or target
// changes replaces its entry.
var (
	scopeCredentialCacheMu sync.Mutex
	scopeCredentialCache   = map[string]scopeCredentialCacheEntry{}
)

type scopeCredentialCacheEntry struct {
	key      string
	provider *sourceCredential
}

func scopeCredentialScope(cityPath, scopeRoot string) string {
	return filepath.Clean(cityPath) + "\x00" + filepath.Clean(scopeRoot)
}

func cachedScopeCredentialProvider(scope, key string, cfg RemoteCredentialConfig) *sourceCredential {
	scopeCredentialCacheMu.Lock()
	defer scopeCredentialCacheMu.Unlock()
	if entry, ok := scopeCredentialCache[scope]; ok && entry.key == key {
		return entry.provider
	}
	provider := &sourceCredential{source: cfg.Source, dir: cfg.Dir, maxAge: scopeCredentialMaxAge(cfg.Source)}
	scopeCredentialCache[scope] = scopeCredentialCacheEntry{key: key, provider: provider}
	return provider
}

// resetScopeCredentialCache drops every cached scope provider (tests).
func resetScopeCredentialCache() {
	scopeCredentialCacheMu.Lock()
	scopeCredentialCache = map[string]scopeCredentialCacheEntry{}
	scopeCredentialCacheMu.Unlock()
}

// scopeCommandCredentialMaxAge bounds how long a command source's token is
// reused before the command runs again: a rotated token reaches the native
// store and the bd children within it even when no request is refused. A
// 401 re-runs it at once (sourceCredential.Refresh).
var scopeCommandCredentialMaxAge = 60 * time.Second

// scopeCredentialMaxAge is a source's refresh policy. An env or file source is
// cheap to read, so it is re-read on every use (a rotated file is seen by the
// next request and the next bd child); a command source is cached for
// scopeCommandCredentialMaxAge.
func scopeCredentialMaxAge(src credsource.Source) time.Duration {
	if src.Kind == credsource.KindCommand {
		return scopeCommandCredentialMaxAge
	}
	return 0
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
// source, shared by one scope's native stores and bd children
// (scopeCredentialCache). It caches the token for maxAge (zero: the source is
// re-read on every use) and, after a 401, re-reads the source at once (a
// rotated token). A failed read is never cached and never falls back to the
// previous token. The token appears only in the Authorization header and in
// the bd children of its own scope.
type sourceCredential struct {
	source credsource.Source
	dir    string
	maxAge time.Duration

	mu       sync.Mutex
	resolved bool
	token    string
	at       time.Time
}

// sourceCredentialNow is the provider's clock (tests).
var sourceCredentialNow = time.Now

func (c *sourceCredential) resolve(ctx context.Context) (string, error) {
	return c.source.Resolve(ctx, credsource.ResolveOptions{
		// The process environment as the operator set it, never a Dolt
		// open's in-flight projection. An env: source is moved out of the
		// process environment on first read (sequesterCredentialEnv), so no
		// child gc spawns afterwards inherits it.
		Getenv:  sequesterCredentialEnv,
		Environ: ProcessEnvSnapshotExcludingNativeDoltOpen,
		Dir:     c.dir,
	})
}

func (c *sourceCredential) current(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resolved && c.maxAge > 0 && sourceCredentialNow().Sub(c.at) < c.maxAge {
		return c.token, nil
	}
	token, err := c.resolve(ctx)
	if err != nil {
		c.resolved, c.token = false, ""
		return "", err
	}
	c.token, c.resolved, c.at = token, true, sourceCredentialNow()
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
// source now; retry only when it now yields a different token.
func (c *sourceCredential) Refresh(ctx context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	token, err := c.resolve(ctx)
	if err != nil {
		c.resolved, c.token = false, ""
		return false, err
	}
	changed := !c.resolved || token != c.token
	c.token, c.resolved, c.at = token, true, sourceCredentialNow()
	return changed, nil
}

// credentialEnvVault holds the values of env: credential sources moved out of
// the process environment (sequesterCredentialEnv). Names and values never
// leave it except as a scope's resolved token.
var (
	credentialEnvVaultMu sync.Mutex
	credentialEnvVault   = map[string]string{}
)

// SequesterRemoteCredentialEnv moves each named variable (the NAME of an
// env:NAME credential source) out of the process environment into gc's
// private vault, so no child process gc spawns from then on (agents, bd
// subprocesses, hooks, any runtime provider that inherits os.Environ()) sees
// it: in a supervisor running several cities, a token sourced from the
// supervisor's environment must not reach another city's children, nor
// this city's agents. The scope's own bd children still get the token, scoped
// to their server, through BEADS_HTTP_TOKEN.
//
// The composition root calls it as soon as a city's config is read, before
// that city spawns anything; resolving an env: source calls it too. A child
// spawned before the first call (or a tmux server started before it) keeps
// what it inherited, which is why file: or command: sources are preferred when
// cities share a supervisor. Unset names are a no-op.
func SequesterRemoteCredentialEnv(names ...string) {
	for _, name := range names {
		_ = sequesterCredentialEnv(name)
	}
}

// sequesterCredentialEnv returns name's value, moving it out of the process
// environment on first sight. A value re-exported later (an operator's, or a
// test's) replaces the vaulted one and is moved out the same way. It reads
// under the env mutex every Dolt open's projection holds.
func sequesterCredentialEnv(name string) string {
	if strings.TrimSpace(name) == "" {
		return ""
	}
	nativeDoltOpenEnvMu.Lock()
	defer nativeDoltOpenEnvMu.Unlock()
	credentialEnvVaultMu.Lock()
	defer credentialEnvVaultMu.Unlock()
	if value, ok := os.LookupEnv(name); ok {
		_ = os.Unsetenv(name)
		if value != "" {
			credentialEnvVault[name] = value
			return value
		}
		delete(credentialEnvVault, name)
		return ""
	}
	return credentialEnvVault[name]
}

// resetCredentialEnvVault forgets every vaulted value (tests).
func resetCredentialEnvVault() {
	credentialEnvVaultMu.Lock()
	credentialEnvVault = map[string]string{}
	credentialEnvVaultMu.Unlock()
}

// RemoteCredentialSubprocessEnv projects one scope's credential into each bd
// child a subprocess runner starts. Every Apply re-resolves the scope (config,
// target and every pre-request check) and takes the token from the scope's
// shared provider, under its refresh policy: a rotated file or env token
// reaches the very next child, a command's within its cache interval, and a
// 401 refresh by the scope's native store at once. A failed resolution stops
// the child. It answers ok=false when the scope has no per-scope credential,
// and the runner then leaves bd's ambient ladder alone.
type RemoteCredentialSubprocessEnv struct {
	cityPath  string
	scopeRoot string
}

// NewRemoteCredentialSubprocessEnv builds the projector for one scope's runner.
func NewRemoteCredentialSubprocessEnv(cityPath, scopeRoot string) *RemoteCredentialSubprocessEnv {
	return &RemoteCredentialSubprocessEnv{cityPath: cityPath, scopeRoot: scopeRoot}
}

// Apply writes the scope's credential env into env (the overrides of one bd
// child) when the scope has a per-scope credential. It reports whether it did.
func (h *RemoteCredentialSubprocessEnv) Apply(ctx context.Context, env map[string]string) (bool, error) {
	if h == nil || env == nil {
		return false, nil
	}
	scoped, ok, err := ResolveScopeRemoteCredential(ctx, h.cityPath, h.scopeRoot)
	if err != nil || !ok {
		return false, err
	}
	projected, err := scoped.SubprocessEnv(ctx)
	if err != nil {
		return false, &RemoteCredentialError{ScopeRoot: h.scopeRoot, Scope: scoped.config.Scope, Source: scoped.Source(), Reason: RemoteCredentialReasonResolve, Err: err}
	}
	for k, v := range projected {
		env[k] = v
	}
	return true, nil
}
