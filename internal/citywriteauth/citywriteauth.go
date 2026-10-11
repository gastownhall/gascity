package citywriteauth

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// AudienceCityWrite is the well-known audience for X-GC-City-Write grants. A
// Verifier is still configured with its own Options.Aud (an operator may choose
// a different value), but this is the canonical audience the reference minter
// stamps and the direct-hardened capstone client expects, so both sides can
// single-source it rather than repeating the literal.
const AudienceCityWrite = "gc-city-write"

// Grant is the claim set carried by an X-GC-City-Write token: a single-use,
// request-bound authorization for exactly one city mutation, minted by a
// configured trusted authority and verified here.
type Grant struct {
	Kid  string `json:"kid"`
	Aud  string `json:"aud"`
	City string `json:"city"`
	// CID is the tenancy binding: the org-unique city id the grant was minted
	// for (distinct from City, the per-org city name that feeds the request
	// path). Legacy (pre-cid) grants omit it. When the verifier is configured
	// with a CID, every grant must carry that exact value — see Options.CID.
	CID   string `json:"cid"`
	Epoch int64  `json:"epoch"`
	IAT   int64  `json:"iat"`
	Exp   int64  `json:"exp"`
	JTI   string `json:"jti"`
	Req   string `json:"req"`
}

// Sentinel errors. Callers distinguish failures with errors.Is; every one of
// them is a rejection (fail-closed), never a pass-through.
var (
	ErrMalformed          = errors.New("citywriteauth: malformed token")
	ErrUnknownKey         = errors.New("citywriteauth: unknown kid")
	ErrBadSignature       = errors.New("citywriteauth: signature verification failed")
	ErrAudience           = errors.New("citywriteauth: audience mismatch")
	ErrExpired            = errors.New("citywriteauth: grant expired")
	ErrNotYetValid        = errors.New("citywriteauth: grant not yet valid")
	ErrBadWindow          = errors.New("citywriteauth: grant validity window is non-positive")
	ErrTTLTooLong         = errors.New("citywriteauth: grant ttl exceeds max")
	ErrEpoch              = errors.New("citywriteauth: epoch below floor")
	ErrMissingClaim       = errors.New("citywriteauth: grant missing required claim")
	ErrMissingExpectation = errors.New("citywriteauth: request expectation incomplete")
	ErrCityMismatch       = errors.New("citywriteauth: city mismatch")
	ErrCIDMismatch        = errors.New("citywriteauth: cid mismatch")
	ErrReqMismatch        = errors.New("citywriteauth: request binding mismatch")
	ErrReplay             = errors.New("citywriteauth: replay detected")
	ErrReplayUnavailable  = errors.New("citywriteauth: replay guard unavailable")
)

// ReplayGuard records consumed token identifiers (jti) so a grant cannot be
// verified twice. Implementations must be safe for concurrent use.
type ReplayGuard interface {
	// Use records jti as consumed until exp. It returns ErrReplay (or an error
	// wrapping it) if jti was already consumed. Any other error means the guard
	// could not decide replay state — for example a shared or durable backend
	// being unavailable — and MUST NOT wrap ErrReplay, so Verify can surface it
	// as ErrReplayUnavailable instead of reporting a false duplicate.
	Use(jti string, exp time.Time) error
}

// Options configures a Verifier. The security-critical fields are validated by
// New so a misconfiguration fails loudly at construction rather than silently
// admitting writes.
type Options struct {
	// Aud is the exact expected audience (e.g. "gc-city-write.v2"). Required.
	Aud string
	// LegacyAud, when non-empty, is a second accepted audience so grants from
	// a previous audience generation (e.g. "gc-city-write") keep verifying
	// through a cutover. It is honored ONLY on an untenanted verifier: when
	// CID is set (tenancy-scoped) the legacy audience is not accepted at all,
	// because the v2 audience is minted in lockstep with the cid claim, so a
	// cid-aware verifier must accept only the primary (v2) Aud — see audienceOK.
	LegacyAud string
	// CID, when non-empty, requires every grant to carry this exact cid claim
	// (the verifier's own tenancy identity). A grant with a mismatching or
	// missing cid is rejected, so a grant minted for one tenant's city can
	// never be replayed against another tenant's verifier even when the city
	// names collide. Empty disables the check (untenanted deployments).
	CID string
	// Keys maps a key id (kid) to its ed25519 public key. At least one required.
	Keys map[string]ed25519.PublicKey
	// EpochFloor rejects grants minted before a rotation/teardown boundary.
	EpochFloor int64
	// MaxTTL bounds Exp-IAT. Required; a non-positive TTL is refused.
	MaxTTL time.Duration
	// Skew tolerates clock drift on the iat/exp window.
	Skew time.Duration
	// Now is injectable for tests; defaults to time.Now.
	Now func() time.Time
	// Replay enforces single-use; defaults to an in-memory guard.
	Replay ReplayGuard
}

// Verifier checks X-GC-City-Write tokens. It is verify-only: it never mints.
type Verifier struct {
	aud        string
	legacyAud  string
	cid        string
	keys       map[string]ed25519.PublicKey
	epochFloor int64
	maxTTL     time.Duration
	skew       time.Duration
	now        func() time.Time
	replay     ReplayGuard
}

// Expect carries the request-derived values a valid grant must be bound to.
type Expect struct {
	City      string // the city segment of the request path
	ReqDigest string // ReqDigest(method, path, rawQuery, body) for this exact request
}

// New builds a Verifier, validating that the security-critical options are set.
func New(opts Options) (*Verifier, error) {
	if opts.Aud == "" {
		return nil, errors.New("citywriteauth: Aud is required")
	}
	if len(opts.Keys) == 0 {
		return nil, errors.New("citywriteauth: at least one verifying key is required")
	}
	if opts.MaxTTL <= 0 {
		return nil, errors.New("citywriteauth: MaxTTL must be positive")
	}
	keys := make(map[string]ed25519.PublicKey, len(opts.Keys))
	for kid, pub := range opts.Keys {
		if kid == "" {
			return nil, errors.New("citywriteauth: empty kid in key set")
		}
		if len(pub) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("citywriteauth: key %q has wrong size", kid)
		}
		// ed25519.PublicKey is a mutable []byte, so copying only the map would
		// alias the caller's backing array and let a later mutation of
		// opts.Keys change the verifier's trust root after construction. Clone
		// each key so the verifier owns an immutable copy.
		keys[kid] = append(ed25519.PublicKey(nil), pub...)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	replay := opts.Replay
	if replay == nil {
		replay = NewMemoryReplayGuard()
	}
	return &Verifier{
		aud:        opts.Aud,
		legacyAud:  opts.LegacyAud,
		cid:        opts.CID,
		keys:       keys,
		epochFloor: opts.EpochFloor,
		maxTTL:     opts.MaxTTL,
		skew:       opts.Skew,
		now:        now,
		replay:     replay,
	}, nil
}

// Verify authenticates token and binds it to expect. On success it returns the
// authenticated grant and consumes its jti (single-use). Every failure path
// returns a sentinel error and leaves the jti unconsumed, so a failed check can
// never burn a legitimate grant.
func (v *Verifier) Verify(token string, expect Expect) (*Grant, error) {
	payload, sig, err := splitToken(token)
	if err != nil {
		return nil, err
	}
	var g Grant
	if err := json.Unmarshal(payload, &g); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformed, err)
	}

	// Select the key by the (still-untrusted) kid, then let the signature check
	// authenticate the whole payload. A tampered kid simply fails verification.
	pub, ok := v.keys[g.Kid]
	if !ok {
		return nil, ErrUnknownKey
	}
	if !ed25519.Verify(pub, payload, sig) {
		return nil, ErrBadSignature
	}
	// From here the claims are authentic.

	// Reject empty required claims and request expectations before any equality
	// or replay check. Without this, a grant with an empty city/req/jti would
	// satisfy the equality checks whenever the integration accidentally supplies
	// a zero-value Expect, defeating fail-closed request binding and single-use.
	if err := requireBound(g, expect); err != nil {
		return nil, err
	}

	// Audience gate: the primary (v2) audience always, plus — only on an
	// untenanted verifier — the legacy one. Rejecting here, ahead of the cid
	// gate below, is what keeps a matching cid from carrying a legacy-audience
	// grant past the v2 cutover; audienceOK documents the full reasoning.
	if !v.audienceOK(g.Aud) {
		return nil, ErrAudience
	}

	// Temporal contract: a well-formed, unexpired, in-window grant minted at or
	// above the epoch floor. exp is reused below to bound replay retention.
	exp, err := v.checkFreshness(g)
	if err != nil {
		return nil, err
	}
	if g.City != expect.City {
		return nil, ErrCityMismatch
	}
	// Tenancy binding: a configured cid must match exactly, so a grant with a
	// missing cid (every legacy grant) or another tenant's cid fails closed.
	if v.cid != "" && g.CID != v.cid {
		return nil, ErrCIDMismatch
	}
	if g.Req != expect.ReqDigest {
		return nil, ErrReqMismatch
	}

	// Consume the jti last so a failed check above never invalidates a real grant.
	// Retain it until this verifier's own acceptance deadline (exp+skew), not bare
	// exp: a guard that evicts at exp could drop the record while Verify still
	// accepts the grant during the skew window, reopening single-use.
	if err := v.replay.Use(g.JTI, exp.Add(v.skew)); err != nil {
		if errors.Is(err, ErrReplay) {
			// A genuine duplicate jti: the grant was already consumed.
			return nil, err
		}
		// A durable or shared guard can fail for storage or network reasons.
		// That is not a replay, so surface it under a distinct sentinel: a
		// caller using errors.Is then fails closed on guard unavailability
		// without mistaking it for a duplicate token.
		return nil, fmt.Errorf("%w: %w", ErrReplayUnavailable, err)
	}
	return &g, nil
}

// audienceOK reports whether aud is an audience this verifier accepts: the
// primary (v2) audience always, plus the legacy audience ONLY on an untenanted
// (cid-less) verifier. When a cid is configured the legacy audience is refused
// outright — the v2 audience is minted in lockstep with the cid claim, so a
// cid-aware verifier must accept only the primary audience. Honoring the legacy
// audience under a configured cid would let a mis-minted or rollout-era grant
// carrying the legacy audience *and* a matching cid ride past the v2 cutover's
// deploy-ordering guarantee on the strength of the matching cid alone. The
// non-empty legacyAud guard also keeps an unset (or cid-suppressed) legacy
// audience from ever matching a grant with an empty aud claim.
func (v *Verifier) audienceOK(aud string) bool {
	if aud == v.aud {
		return true
	}
	if v.cid != "" || v.legacyAud == "" {
		return false
	}
	return aud == v.legacyAud
}

// checkFreshness validates the grant's temporal contract: a well-formed
// iat/exp window, a ttl within MaxTTL, and the current time inside the
// skew-tolerant window, plus the epoch floor. It returns the parsed exp so the
// caller can bound replay retention to the same acceptance deadline (exp+skew)
// instead of recomputing it. Every failure is a fail-closed sentinel.
func (v *Verifier) checkFreshness(g Grant) (time.Time, error) {
	iat := time.Unix(g.IAT, 0)
	exp := time.Unix(g.Exp, 0)
	if !exp.After(iat) {
		return exp, ErrBadWindow
	}
	if exp.Sub(iat) > v.maxTTL {
		return exp, ErrTTLTooLong
	}
	now := v.now()
	if now.After(exp.Add(v.skew)) {
		return exp, ErrExpired
	}
	if now.Before(iat.Add(-v.skew)) {
		return exp, ErrNotYetValid
	}
	if g.Epoch < v.epochFloor {
		return exp, ErrEpoch
	}
	return exp, nil
}

// requireBound rejects a grant whose required single-use and request-binding
// claims are empty, or a request expectation that is missing its binding
// values. The equality and replay checks downstream rely on these fields, so an
// empty value on either side would let them pass vacuously.
func requireBound(g Grant, expect Expect) error {
	switch {
	case g.City == "", g.Req == "", g.JTI == "":
		return ErrMissingClaim
	case expect.City == "", expect.ReqDigest == "":
		return ErrMissingExpectation
	}
	return nil
}

func splitToken(token string) (payload, sig []byte, err error) {
	p, s, ok := strings.Cut(token, ".")
	if !ok || p == "" || s == "" {
		return nil, nil, ErrMalformed
	}
	payload, err = base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: payload: %w", ErrMalformed, err)
	}
	sig, err = base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: signature: %w", ErrMalformed, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return nil, nil, fmt.Errorf("%w: signature size", ErrMalformed)
	}
	return payload, sig, nil
}

// ReqDigest binds a grant to one request. It is the lowercase hex SHA-256 of
//
//	method "\n" path [ "\n" canonicalQuery ] "\n" hex(sha256(body))
//
// method is the uppercase HTTP method; path is the request path without query;
// rawQuery is the request's raw URL query (r.URL.RawQuery). The canonical query
// — the sorted, percent-encoded form the handler sees via r.URL.Query() — is
// folded into the preimage ONLY when the request carries one, so a query-less
// request keeps the original method"\n"path"\n"hex(body) preimage byte-for-byte.
// Existing query-less grants and the pinned cross-repo golden vector therefore
// verify unchanged, while a query-bearing request is bound strictly more tightly:
// a grant minted for DELETE .../workflow/{id} no longer authorizes the
// destructive .../workflow/{id}?delete=true variant, and a narrow
// ?scope_kind=rig selector cannot be broadened by dropping the query.
//
// The minter computes the identical value from the buffered request, so a
// captured grant cannot be repurposed for a different mutation.
func ReqDigest(method, path, rawQuery string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	return ReqDigestFromBodyHash(method, path, rawQuery, hex.EncodeToString(bodyHash[:]))
}

// ReqDigestFromBodyHash computes the same request binding as [ReqDigest] but
// takes the body as its lowercase hex SHA-256 rather than the raw bytes. It
// exists for a minter that is handed the body hash only (never the body), so it
// can recompute and re-validate the request binding without seeing the payload.
// bodyHashHex must be the exact value hex(sha256(body)) the client folded in;
// [ReqDigest] delegates here, so the two are byte-identical by construction.
func ReqDigestFromBodyHash(method, path, rawQuery, bodyHashHex string) string {
	preimage := method + "\n" + path
	if canonical := canonicalizeQuery(rawQuery); canonical != "" {
		preimage += "\n" + canonical
	}
	preimage += "\n" + bodyHashHex
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
}

// canonicalizeQuery returns the order-independent encoding of a raw URL query —
// the same view a handler gets from r.URL.Query() — so the digest binds exactly
// what the handler acts on regardless of parameter order. A genuinely empty (or
// semantically empty, e.g. "&") query returns "", which keeps the query-less
// preimage byte-identical to the pre-query-binding contract. A query that fails
// to parse falls back to its raw bytes so it still binds fail-closed rather than
// silently dropping a behavior-affecting parameter out of the digest.
func canonicalizeQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return rawQuery
	}
	return values.Encode()
}
