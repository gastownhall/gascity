package webhookverify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

// Verifier authenticates one inbound webhook delivery under a single scheme.
// Implementations are stateless with respect to the request and safe for
// concurrent use; per-scheme configuration is bound at construction via [New].
type Verifier interface {
	// Verify reports whether req is an authentic delivery. A non-nil error
	// means the check could not be performed (operator fault); a VerifyResult
	// with OK==false means the check ran and the delivery is not authentic.
	Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error)
	// Scheme returns the scheme identifier this verifier implements.
	Scheme() string
}

// VerifyRequest carries everything a verifier needs to authenticate a delivery.
// The receiver (E3) populates it from the raw HTTP request plus the
// operator-resolved secret material; verifiers never touch the process
// environment or the network for secret material themselves (jwt-jwks fetches
// its public JWKS, which is not secret).
type VerifyRequest struct {
	// Body is the exact raw request body. Signatures are computed over these
	// bytes; a re-serialized payload would not verify.
	Body []byte
	// Header is the inbound request header set. Verifiers read scheme-specific
	// signature, timestamp, and event headers from it via Header.Get.
	Header http.Header
	// Secret is the operator-resolved secret material for the scheme: the HMAC
	// key for the HMAC family, or the Ed25519 public key (hex or raw 32 bytes)
	// for discord-ed25519. It is unused by jwt-jwks, whose trust anchor is the
	// operator [JWTVerifierPolicy] bound at construction. Resolve it with a
	// [SecretResolver] so the operator-namespace and entropy checks run.
	Secret []byte
	// Now optionally overrides the clock for replay and expiry checks. Nil uses
	// the verifier's configured clock (or time.Now). Intended for tests.
	Now func() time.Time
}

// VerifyResult is the outcome of a verification attempt.
type VerifyResult struct {
	// OK is true only when the delivery is cryptographically authentic and all
	// scheme-specific replay/claim checks passed.
	OK bool
	// EventType is the provider event type when the scheme surfaces it from a
	// header (e.g. X-GitHub-Event). Payload-derived event typing is the rule
	// layer's job (E5) and is left empty here.
	EventType string
	// DedupID is a stable per-delivery identifier for at-least-once dedup when
	// the scheme exposes one (e.g. X-GitHub-Delivery, the Slack timestamp, or
	// the JWT id). Empty when the scheme carries none.
	DedupID string
	// Identity is the verified principal for identity-bearing schemes — the JWT
	// subject (falling back to the issuer) for jwt-jwks. Empty for signature-
	// only schemes.
	Identity string
	// Reason is a short human-readable explanation when OK is false.
	Reason string
}

// Options carries the resolved, operator-owned inputs a verifier constructor
// needs beyond the WebhookVerify config.
//
// JWTPolicy is kept here rather than being read from config.WebhookVerify on
// purpose: it is the API boundary that enforces security review R1. A
// pack-authored WebhookVerify literally cannot set the issuer, audience, or
// JWKS URL used for trust — only the receiver, populating this struct from the
// operator's city.toml, can. This package never reads WebhookVerify.Issuer,
// WebhookVerify.Audience, or WebhookVerify.JWKSURL.
type Options struct {
	// JWTPolicy pins the jwt-jwks trust anchor. Required for scheme "jwt-jwks",
	// ignored otherwise.
	JWTPolicy *JWTVerifierPolicy
	// HTTPClient overrides the client used to fetch JWKS. Nil uses a default
	// client with a bounded timeout.
	HTTPClient *http.Client
	// JWKSCacheTTL overrides the JWKS cache lifetime. Zero uses the default.
	JWKSCacheTTL time.Duration
	// Now overrides the verifier's clock for replay/expiry checks. Nil uses
	// time.Now. A per-request VerifyRequest.Now takes precedence over this.
	Now func() time.Time
}

// Constructor builds a Verifier for one scheme from its config and options.
type Constructor func(cfg config.WebhookVerify, opts Options) (Verifier, error)

// registry maps a scheme string to its constructor. It is the single source of
// truth for which schemes exist; config.knownWebhookSchemes (E2) validates the
// same set at parse time.
var registry = map[string]Constructor{
	"github-hmac-sha256": newGitHubHMAC,
	"hmac-sha256":        newGenericHMAC,
	"slack-v0":           newSlackV0,
	"discord-ed25519":    newDiscordEd25519,
	"jwt-jwks":           newJWTJWKS,
}

// ErrUnknownScheme is returned by New for a scheme with no registered verifier.
var ErrUnknownScheme = errors.New("webhookverify: unknown scheme")

// New builds the verifier for scheme, binding cfg and opts. It returns
// ErrUnknownScheme for an unregistered scheme and a construction error when the
// scheme's required inputs are missing or invalid (e.g. a jwt-jwks policy).
func New(scheme string, cfg config.WebhookVerify, opts Options) (Verifier, error) {
	ctor, ok := registry[scheme]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownScheme, scheme)
	}
	return ctor(cfg, opts)
}

// Schemes returns the registered scheme identifiers in sorted order.
func Schemes() []string {
	out := make([]string, 0, len(registry))
	for s := range registry {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// effectiveNow resolves the clock for a verification: the per-request override
// wins, then the verifier's configured clock, then time.Now.
func effectiveNow(req VerifyRequest, configured func() time.Time) time.Time {
	if req.Now != nil {
		return req.Now()
	}
	if configured != nil {
		return configured()
	}
	return time.Now()
}

// failf builds a failed (OK==false) result with a formatted reason.
func failf(format string, args ...any) VerifyResult {
	return VerifyResult{OK: false, Reason: fmt.Sprintf(format, args...)}
}
