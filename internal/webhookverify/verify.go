package webhookverify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
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
	// EventType is the resolved provider event type the rule layer (E5) matches
	// on. A header-typed scheme surfaces it from the configured header (e.g.
	// X-GitHub-Event); a body-typed scheme (slack-v0, discord-ed25519) derives it
	// from the VERIFIED body so payload-carried event rules actually match — see
	// slackEventType / discordEventType. Empty when the scheme carries no type or
	// the body is not the expected shape, in which case only a "*" rule matches.
	EventType string
	// DedupID is a stable per-delivery identifier for at-least-once dedup when
	// the scheme exposes one (e.g. X-GitHub-Delivery, the Slack timestamp, or
	// the JWT id). Empty when the scheme carries none. It is safe for
	// observability (event logs) but NOT necessarily safe as a dedup KEY — see
	// DedupIDSigned.
	DedupID string
	// DedupIDSigned is true only when DedupID is BOTH covered by the delivery's
	// signature AND unique per delivery, so the receiver may key dedup on it.
	// It is set only for jwt-jwks (the signed "jti"). It is false for schemes
	// whose DedupID is an unsigned/attacker-mutable header (github's
	// X-GitHub-Delivery, generic-hmac's dedup header, discord) or a coarse signed
	// value that can collide across distinct deliveries (slack's second-granular
	// timestamp). The receiver keys dedup on the body hash for those, since the
	// body is signed under every scheme (tamper-proof and per-delivery-unique).
	DedupIDSigned bool
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

// maxReplayWindow is the operator-enforced ceiling on any pack-authorable
// replay_window. WebhookVerify (including replay_window) is pack/fragment-
// authored, so a pack could set an enormous window to neuter the very
// freshness control it benefits from weakening. Clamping to this ceiling
// mirrors the downward-only rate-limit clamp (config.EffectiveRateLimit): the
// operator owns the maximum; a pack may only make the window stricter.
const maxReplayWindow = 15 * time.Minute

// resolveReplayWindow resolves a pack-authored replay_window to the effective
// duration a timestamp-freshness check should enforce: the default when unset
// or non-positive, clamped down to maxReplayWindow. A malformed duration string
// is an operator/pack configuration fault surfaced as a construction error.
func resolveReplayWindow(raw string, def time.Duration) (time.Duration, error) {
	w := def
	if raw = strings.TrimSpace(raw); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil {
			return 0, fmt.Errorf("webhookverify: replay_window is invalid: %w", err)
		}
		w = d
	}
	if w <= 0 {
		w = def
	}
	if w > maxReplayWindow {
		w = maxReplayWindow
	}
	return w, nil
}

// withinReplayWindow reports whether a signed unix-second timestamp is within
// window of now. It compares integer seconds and bounds tsSecs against
// [now-window, now+window] rather than subtracting attacker-controlled values,
// because time.Time.Sub clamps a far-future/past difference to
// math.MinInt64/math.MaxInt64 — and negating math.MinInt64 stays negative, so a
// naive `abs(skew) > window` check would silently PASS a far-future timestamp
// (its clamped-negative "skew" never exceeds the window). now.Unix() is a real
// wall-clock value and window is bounded by maxReplayWindow, so now±windowSecs
// cannot overflow; tsSecs appears only in comparisons, so any int64 is safe.
func withinReplayWindow(now time.Time, tsSecs int64, window time.Duration) bool {
	windowSecs := int64(window / time.Second)
	if windowSecs < 0 {
		windowSecs = 0
	}
	nowSecs := now.Unix()
	return tsSecs >= nowSecs-windowSecs && tsSecs <= nowSecs+windowSecs
}
