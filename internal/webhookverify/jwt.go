package webhookverify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"

	"github.com/gastownhall/gascity/internal/config"
)

const (
	jwtDefaultTokenHeader = "Authorization"
	jwtBearerPrefix       = "Bearer "
	jwksDefaultTTL        = 5 * time.Minute
	jwksMinRefresh        = 30 * time.Second
	jwksHTTPTimeout       = 10 * time.Second
	jwksMaxBodyBytes      = 1 << 20
)

// jwtAllowedMethods pins the signature algorithms this verifier will accept.
// Restricting to asymmetric methods is the defense against the classic alg-
// confusion attack (a token signed HS256 using the public key as the HMAC
// secret) and rejects "alg":"none".
var jwtAllowedMethods = []string{
	"RS256", "RS384", "RS512",
	"PS256", "PS384", "PS512",
	"ES256", "ES384", "ES512",
	"EdDSA",
}

// errJWKSUnavailable wraps a fetch/parse failure of the JWKS document. It marks
// an operational fault (→ error from Verify) as distinct from a token that
// simply fails to verify (→ OK==false).
var errJWKSUnavailable = errors.New("webhookverify: jwks unavailable")

// errKeyNotFound means the token's kid was not present in the (freshly fetched)
// JWKS. That is a property of the token, not our infrastructure, so it maps to
// a failed verification rather than an operational error.
var errKeyNotFound = errors.New("webhookverify: no jwks key for token kid")

// jwtJWKS validates a bearer JWT against an operator-pinned JWKS. Issuer,
// audience, and JWKS URL come only from the [JWTVerifierPolicy] bound at
// construction — never from the pack-authored config.WebhookVerify — which is
// the type-level enforcement of security review R1.
type jwtJWKS struct {
	policy      JWTVerifierPolicy
	tokenHeader string
	cache       *jwksCache
	now         func() time.Time
}

func newJWTJWKS(cfg config.WebhookVerify, opts Options) (Verifier, error) {
	if opts.JWTPolicy == nil {
		return nil, errors.New("webhookverify: jwt-jwks requires an operator JWTVerifierPolicy (city.toml), none supplied")
	}
	policy := *opts.JWTPolicy
	if err := policy.validate(); err != nil {
		return nil, err
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: jwksHTTPTimeout}
	}
	ttl := opts.JWKSCacheTTL
	if ttl <= 0 {
		ttl = jwksDefaultTTL
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &jwtJWKS{
		policy:      policy,
		tokenHeader: headerOrDefault(cfg.SignatureHeader, jwtDefaultTokenHeader),
		cache: &jwksCache{
			url:        policy.JWKSURL,
			client:     client,
			ttl:        ttl,
			minRefresh: jwksMinRefresh,
			now:        now,
		},
		now: now,
	}, nil
}

func (v *jwtJWKS) Scheme() string { return "jwt-jwks" }

func (v *jwtJWKS) Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error) {
	raw := strings.TrimSpace(req.Header.Get(v.tokenHeader))
	if raw == "" {
		return failf("missing %s token header", v.tokenHeader), nil
	}
	if rest, ok := cutBearerPrefix(raw); ok {
		raw = rest
	}

	now := effectiveNow(req, v.now)
	parser := jwt.NewParser(
		jwt.WithValidMethods(jwtAllowedMethods),
		jwt.WithIssuer(v.policy.Issuer),
		jwt.WithAudience(v.policy.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)

	claims := jwt.MapClaims{}
	_, err := parser.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, _ := token.Header["kid"].(string)
		return v.cache.keyFor(ctx, kid)
	})
	if err != nil {
		if errors.Is(err, errJWKSUnavailable) {
			return VerifyResult{}, err
		}
		return failf("jwt rejected: %v", err), nil
	}

	sub, _ := claims.GetSubject()
	iss, _ := claims.GetIssuer()
	identity := strings.TrimSpace(sub)
	if identity == "" {
		identity = strings.TrimSpace(iss)
	}
	var dedup string
	if jti, ok := claims["jti"].(string); ok {
		dedup = strings.TrimSpace(jti)
	}
	// The jti is inside the signed JWT and is unique per delivery, so it is the
	// one dedup id safe to use as the dedup KEY (DedupIDSigned). Every other
	// scheme's id is unsigned or coarse, so the receiver keys on the body hash.
	return VerifyResult{OK: true, Identity: identity, DedupID: dedup, DedupIDSigned: dedup != ""}, nil
}

// cutBearerPrefix strips a case-insensitive "Bearer " prefix if present.
func cutBearerPrefix(s string) (string, bool) {
	if len(s) >= len(jwtBearerPrefix) && strings.EqualFold(s[:len(jwtBearerPrefix)], jwtBearerPrefix) {
		return strings.TrimSpace(s[len(jwtBearerPrefix):]), true
	}
	return s, false
}

// jwksCache caches the parsed signing keys from a JWKS endpoint with a TTL and
// a minimum refresh interval. keyFor serializes fetches under mu; for the low
// request rates of a webhook receiver this is simpler and correct versus a
// singleflight, and a stale-but-valid cache still serves during the window.
type jwksCache struct {
	url        string
	client     *http.Client
	ttl        time.Duration
	minRefresh time.Duration
	now        func() time.Time

	mu        sync.Mutex
	keys      map[string]any
	anon      []any
	fetchedAt time.Time
}

// keyFor returns the public key for kid, refreshing the JWKS when the cache is
// stale or the kid is unknown (subject to the minimum refresh interval).
func (c *jwksCache) keyFor(ctx context.Context, kid string) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	stale := c.keys == nil || now.Sub(c.fetchedAt) >= c.ttl
	if stale {
		if err := c.refreshLocked(ctx, now); err != nil {
			return nil, err
		}
	}
	if key, ok := c.lookupLocked(kid); ok {
		return key, nil
	}
	// Unknown kid on a fresh cache: a rotation may have landed. Force one
	// refresh if we have not fetched too recently.
	if !stale && now.Sub(c.fetchedAt) >= c.minRefresh {
		if err := c.refreshLocked(ctx, now); err != nil {
			return nil, err
		}
		if key, ok := c.lookupLocked(kid); ok {
			return key, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", errKeyNotFound, kid)
}

// lookupLocked resolves kid against the cached keys. An empty kid resolves only
// when the JWKS publishes exactly one usable key.
func (c *jwksCache) lookupLocked(kid string) (any, bool) {
	if kid != "" {
		key, ok := c.keys[kid]
		return key, ok
	}
	if len(c.keys)+len(c.anon) == 1 {
		if len(c.anon) == 1 {
			return c.anon[0], true
		}
		for _, key := range c.keys {
			return key, true
		}
	}
	return nil, false
}

func (c *jwksCache) refreshLocked(ctx context.Context, now time.Time) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("%w: build request: %w", errJWKSUnavailable, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: fetch %s: %w", errJWKSUnavailable, c.url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: fetch %s: status %d", errJWKSUnavailable, c.url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBodyBytes))
	if err != nil {
		return fmt.Errorf("%w: read %s: %w", errJWKSUnavailable, c.url, err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("%w: parse %s: %w", errJWKSUnavailable, c.url, err)
	}

	keys := make(map[string]any, len(set.Keys))
	var anon []any
	for i := range set.Keys {
		k := set.Keys[i]
		if !k.Valid() {
			continue
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub := publicOf(k)
		if pub == nil {
			continue
		}
		if k.KeyID == "" {
			anon = append(anon, pub)
			continue
		}
		keys[k.KeyID] = pub
	}
	if len(keys)+len(anon) == 0 {
		return fmt.Errorf("%w: %s published no usable signing keys", errJWKSUnavailable, c.url)
	}
	c.keys = keys
	c.anon = anon
	c.fetchedAt = now
	return nil
}

// publicOf returns the public half of a JWK, collapsing a private key to its
// public key when a JWKS erroneously carries private material.
func publicOf(k jose.JSONWebKey) any {
	if k.IsPublic() {
		return k.Key
	}
	pub := k.Public()
	if pub.Key == nil {
		return nil
	}
	return pub.Key
}
