package api

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/citywriteauth"
)

// Remote-client transport budgets. A remote city is reached over a WAN, so the
// REST ceiling is generous (a federated read of a Dolt-backed rig store can take
// many seconds) while the dial and TLS handshakes are bounded tightly to fail
// fast on an unreachable host. The stream client has NO overall timeout — a
// long-lived SSE stream must not be capped — and is instead bounded by the
// per-frame idle watchdog in waitForEvent.
const (
	remoteRESTTimeout           = 120 * time.Second
	remoteDialTimeout           = 15 * time.Second
	remoteTLSHandshakeTimeout   = 15 * time.Second
	remoteResponseHeaderTimeout = 30 * time.Second
	remoteStreamIdleTimeout     = 45 * time.Second
)

// TokenSource yields a fresh transport bearer for each request and each SSE
// (re)connect. It is invoked live — never captured once — so a per-attempt 401
// re-mint takes effect. A nil TokenSource means no Authorization header is
// attached (a city that authenticates on X-GC-Request alone, or one fronted by
// a grant rather than a bearer).
type TokenSource func() (string, error)

// GrantBinding is the request binding a city-write grant is bound to. The
// transport computes it from the FINAL outgoing request and hands it to a
// GrantSource, which mints a grant covering exactly this method/path/query/body.
// CanonicalQuery is the request's raw URL query (r.URL.RawQuery); BodySHA256 is
// the lowercase hex sha256 of the body; ReqDigest is citywriteauth.ReqDigest
// over the four — the same value the server independently recomputes.
type GrantBinding struct {
	Method         string
	Path           string
	CanonicalQuery string
	BodySHA256     string
	ReqDigest      string
}

// GrantSource mints a single-use X-GC-City-Write grant for one mutating request.
// It is invoked live per mutating request (never captured), receives the binding
// the transport computed, and returns the grant token. A nil GrantSource
// attaches no grant. It mirrors TokenSource.
type GrantSource func(GrantBinding) (string, error)

// RemoteOptions configures the transport of a remote-city client.
type RemoteOptions struct {
	// Token, when non-nil, supplies the Authorization: Bearer <token> credential
	// (consumed by an edge/proxy; the controller ignores Authorization).
	Token TokenSource
	// RefreshToken, when non-nil, force-mints a fresh bearer (bypassing any
	// expiry cache). The transport calls it once on a 401 and retries the request
	// with the new bearer, so an edge that rejects a still-unexpired token (key
	// rotation, early revocation) recovers without a fresh gc invocation. Only
	// applied to requests that carry no single-use X-GC-City-Write grant.
	RefreshToken TokenSource
	// Grant, when non-nil, mints an X-GC-City-Write grant for each mutating
	// request (a direct hardened self-host). Reads never carry a grant.
	Grant GrantSource
	// CAFile is a PEM bundle used to verify the server certificate. Empty uses
	// the system roots.
	CAFile string
	// TLSServerName overrides the SNI / certificate name (for a host reached by
	// IP or through a fronting name).
	TLSServerName string
	// InsecureSkipVerify disables TLS verification (development only).
	InsecureSkipVerify bool
	// RESTTimeout overrides the overall REST timeout; 0 uses remoteRESTTimeout.
	// It is never applied to the SSE stream client.
	RESTTimeout time.Duration
}

// NewRemoteCityScopedClient builds a client that operates a REMOTE city at
// baseURL over the control plane. Unlike the local NewCityScopedClient, a
// malformed baseURL (or bad CA file) is a hard error at construction — a remote
// client is never a fallback-eligible stub. The returned client is marked
// isRemote so every error it produces is non-fallbackable (gate G1).
func NewRemoteCityScopedClient(baseURL, cityName string, opts RemoteOptions) (*Client, error) {
	rest, stream, err := newRemoteHTTPClients(opts)
	if err != nil {
		return nil, err
	}
	c := &Client{
		baseURL:      baseURL,
		cityName:     cityName,
		isRemote:     true,
		streamClient: stream,
		tokenSource:  opts.Token,
		grantSource:  opts.Grant,
	}
	cw, err := genclient.NewClientWithResponses(
		baseURL,
		genclient.WithHTTPClient(rest),
		genclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("X-GC-Request", "true")
			return nil
		}),
		genclient.WithRequestEditorFn(remoteAuthEditor(c)),
		// The grant editor is attached LAST so any body/query editor has already
		// run: the grant digest must bind the exact bytes that go on the wire.
		genclient.WithRequestEditorFn(remoteGrantEditor(c)),
	)
	if err != nil {
		return nil, fmt.Errorf("building remote client for %q: %w", baseURL, err)
	}
	c.cw = cw
	return c, nil
}

// NewRemoteEventsClient builds a genclient for the events feed against a remote
// city. It is backed by the NO-TIMEOUT stream HTTP client — a --follow SSE
// stream must not be cut by the bounded REST timeout — and carries the same auth
// as the REST client: the X-GC-Request CSRF header, an Authorization bearer from
// opts.Token, and a 401 re-mint via opts.RefreshToken (wrapped into the stream
// transport by newRemoteHTTPClients). It is the events path's authenticated
// client for a --context/--city-url remote target. No X-GC-City-Write grant: the
// events feed is read-only.
func NewRemoteEventsClient(baseURL string, opts RemoteOptions) (*genclient.ClientWithResponses, error) {
	_, stream, err := newRemoteHTTPClients(opts)
	if err != nil {
		return nil, err
	}
	genOpts := []genclient.ClientOption{
		genclient.WithHTTPClient(stream),
		genclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("X-GC-Request", "true")
			return nil
		}),
	}
	if opts.Token != nil {
		tok := opts.Token
		genOpts = append(genOpts, genclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			t, terr := tok()
			if terr != nil {
				return terr
			}
			if t != "" {
				req.Header.Set("Authorization", "Bearer "+t)
			}
			return nil
		}))
	}
	return genclient.NewClientWithResponses(baseURL, genOpts...)
}

// remoteAuthEditor returns a genclient request editor that attaches a fresh
// bearer (from the client's token source) to every REST request. It closes over
// the client so the token is fetched live, not captured at construction.
func remoteAuthEditor(c *Client) genclient.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		tok, err := c.bearerToken()
		if err != nil {
			return err
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		return nil
	}
}

// remoteGrantEditor returns a genclient request editor that, for a MUTATING
// request, computes the request binding over the final request body and query,
// mints a single-use grant via the client's grant source, and attaches it as
// X-GC-City-Write. A read (GET/HEAD/OPTIONS) or a nil grant source attaches
// nothing. It is attached last (after the body/query are settled) so the digest
// binds exactly what goes on the wire, and it closes over the client so the
// grant is minted live per request.
func remoteGrantEditor(c *Client) genclient.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		if c.grantSource == nil || !isMutatingMethod(req.Method) {
			return nil
		}
		body, err := bufferRequestBody(req)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		token, err := c.grantSource(GrantBinding{
			Method:         req.Method,
			Path:           req.URL.Path,
			CanonicalQuery: req.URL.RawQuery,
			BodySHA256:     hex.EncodeToString(sum[:]),
			ReqDigest:      citywriteauth.ReqDigest(req.Method, req.URL.Path, req.URL.RawQuery, body),
		})
		if err != nil {
			return fmt.Errorf("minting city-write grant: %w", err)
		}
		if strings.TrimSpace(token) == "" {
			return fmt.Errorf("grant source returned an empty token")
		}
		req.Header.Set("X-GC-City-Write", token)
		return nil
	}
}

// isMutatingMethod reports whether an HTTP method mutates server state and thus
// needs a city-write grant. GET/HEAD/OPTIONS are reads and carry none.
func isMutatingMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}

// bufferRequestBody reads a copy of req's body via GetBody, leaving the actual
// send body intact (GetBody returns a fresh reader, so no reset is needed). A
// body-less request yields nil. A body without GetBody (a non-replayable stream)
// is a hard error: a grant must bind the exact bytes on the wire, which cannot
// be guaranteed without a replayable body.
func bufferRequestBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	if req.GetBody == nil {
		return nil, fmt.Errorf("cannot compute city-write grant digest: request body is not replayable")
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil, fmt.Errorf("reading request body for grant digest: %w", err)
	}
	defer rc.Close() //nolint:errcheck // read-only copy
	return io.ReadAll(rc)
}

// newRemoteHTTPClients builds the two client shapes from a single TLS/redirect
// policy: a REST client (bounded overall timeout + tight dial/TLS budgets) and a
// stream client (no overall timeout; idle-bounded by the caller). Both refuse
// credential-leaking redirects.
func newRemoteHTTPClients(opts RemoteOptions) (rest, stream *http.Client, err error) {
	tlsCfg, err := remoteTLSConfig(opts)
	if err != nil {
		return nil, nil, err
	}
	newTransport := func() *http.Transport {
		return &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   remoteDialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSClientConfig:       tlsCfg,
			TLSHandshakeTimeout:   remoteTLSHandshakeTimeout,
			ResponseHeaderTimeout: remoteResponseHeaderTimeout,
			ForceAttemptHTTP2:     true,
			ExpectContinueTimeout: 1 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	restTimeout := opts.RESTTimeout
	if restTimeout <= 0 {
		restTimeout = remoteRESTTimeout
	}
	// Wrap both transports so a 401 triggers one bearer re-mint + retry when a
	// RefreshToken is supplied (no-op otherwise).
	wrap := func(base http.RoundTripper) http.RoundTripper {
		if opts.RefreshToken == nil {
			return base
		}
		return &reauthRoundTripper{base: base, refresh: opts.RefreshToken}
	}
	rest = &http.Client{
		Timeout:       restTimeout,
		Transport:     wrap(newTransport()),
		CheckRedirect: remoteCheckRedirect,
	}
	stream = &http.Client{
		Timeout:       0, // never cap a long-lived SSE stream; see remoteStreamIdleTimeout
		Transport:     wrap(newTransport()),
		CheckRedirect: remoteCheckRedirect,
	}
	return rest, stream, nil
}

// reauthRoundTripper retries a request ONCE on a 401 after force-minting a fresh
// bearer, so an edge that rejects a still-unexpired token (key rotation, early
// revocation) recovers transparently. It deliberately does NOT retry a request
// carrying an X-GC-City-Write grant: that grant is single-use and bound to the
// exact bytes, and the request editors (which mint it) do not re-run at the
// transport layer — a fresh grant needs a fresh gc invocation. A body without
// GetBody (non-replayable) is also left alone.
type reauthRoundTripper struct {
	base    http.RoundTripper
	refresh TokenSource
}

func (rt *reauthRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := rt.base.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized || rt.refresh == nil {
		return resp, err
	}
	if req.Header.Get("X-GC-City-Write") != "" {
		return resp, err // single-use grant; cannot re-mint here
	}
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return resp, err // non-replayable body
	}
	tok, rerr := rt.refresh()
	if rerr != nil || strings.TrimSpace(tok) == "" {
		return resp, err // keep the original 401
	}
	retry := req.Clone(req.Context())
	if req.GetBody != nil {
		body, gerr := req.GetBody()
		if gerr != nil {
			return resp, err // cannot replay; keep the original 401 (untouched)
		}
		retry.Body = body
	}
	retry.Header.Set("Authorization", "Bearer "+tok)
	// Committed to the retry: drain + close the first response, then re-send.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return rt.base.RoundTrip(retry)
}

// remoteTLSConfig builds the client TLS config from the options: a custom CA
// bundle, an SNI/name override, and (dev-only) verification skip. MinVersion is
// pinned to TLS 1.2.
func remoteTLSConfig(opts RemoteOptions) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.TLSServerName != "" {
		cfg.ServerName = opts.TLSServerName
	}
	if opts.InsecureSkipVerify {
		cfg.InsecureSkipVerify = true
	}
	if opts.CAFile != "" {
		pem, err := os.ReadFile(opts.CAFile)
		if err != nil {
			return nil, fmt.Errorf("reading ca_file %q: %w", opts.CAFile, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file %q: no valid PEM certificates found", opts.CAFile)
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}

// remoteCheckRedirect is the redirect policy for every remote request. It
// refuses a cross-host redirect and an https->http downgrade outright (either
// could exfiltrate a bearer/grant or drop it onto plaintext), and — defense in
// depth — strips the Authorization and every X-GC-* header from any redirect it
// does allow (a same-host, same-or-upgraded-scheme hop).
func remoteCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	orig := via[0]
	// A city-write grant is single-use and bound to the EXACT original
	// method/path/query/body, so following any redirect would either break that
	// binding or waste the one-shot grant on a request the server never
	// authorized. Refuse every redirect on a grant-bearing request outright
	// (gate G18), even a same-host one that the reads path would allow.
	if orig.Header.Get("X-GC-City-Write") != "" {
		return fmt.Errorf("refusing redirect on a grant-bearing request (the grant is single-use and request-bound)")
	}
	if !strings.EqualFold(req.URL.Host, orig.URL.Host) {
		return fmt.Errorf("refusing cross-host redirect from %s to %s (credentials are per-host)", orig.URL.Host, req.URL.Host)
	}
	if orig.URL.Scheme == "https" && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing https->%s downgrade redirect to %s", req.URL.Scheme, req.URL.Host)
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after %d redirects", len(via))
	}
	stripSensitiveHeaders(req.Header)
	return nil
}

// RequestIDForError extracts the server-minted X-GC-Request-Id from a response
// header, formatted as "request_id=<id>" for inclusion in an error message (or
// "" when absent). It lets a failed request be correlated with the server's
// api: log line and the SupervisorRequest audit record. The remote read-set
// enablement threads this into the api.Client error strings and the CLI's
// failure target-echo line (gate G9, client half).
func RequestIDForError(h http.Header) string {
	id := strings.TrimSpace(h.Get("X-GC-Request-Id"))
	if id == "" {
		return ""
	}
	return "request_id=" + id
}

// stripSensitiveHeaders removes the Authorization header and every X-GC-*
// control/grant header from h. Header map keys are already canonicalized by
// net/http (X-GC-Request -> X-Gc-Request), so a case-insensitive x-gc- prefix
// test catches them all.
func stripSensitiveHeaders(h http.Header) {
	h.Del("Authorization")
	for key := range h {
		if strings.HasPrefix(strings.ToLower(key), "x-gc-") {
			h.Del(key)
		}
	}
}
