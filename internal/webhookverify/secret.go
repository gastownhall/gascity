package webhookverify

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// OperatorSecretEnvPrefix is the environment-variable namespace an operator
// controls for webhook secrets. A WebhookVerify.SecretEnv must start with this
// prefix so a pack cannot point secret resolution at an arbitrary ambient
// variable (HOME, AWS_SECRET_ACCESS_KEY, GC_CITY, …). This is the load-bearing
// half of security review R1.
const OperatorSecretEnvPrefix = "GC_WEBHOOK_"

// MinSecretBytes is the minimum accepted secret length. A shorter (or empty)
// secret is rejected so a misconfigured or unset secret fails closed instead of
// producing an HMAC that an attacker could feasibly reproduce.
const MinSecretBytes = 16

// Secret-resolution errors. They are distinguishable with errors.Is so the
// receiver can log the specific fault; all of them mean "refuse the request".
var (
	// ErrSecretEnvUnnamed is returned when SecretEnv is empty for a scheme that
	// requires a secret.
	ErrSecretEnvUnnamed = errors.New("webhookverify: secret_env is not set")
	// ErrSecretEnvPrefix is returned when SecretEnv is outside the operator
	// namespace (does not start with OperatorSecretEnvPrefix).
	ErrSecretEnvPrefix = errors.New("webhookverify: secret_env is not in the operator namespace")
	// ErrSecretUnset is returned when the named variable is not present in the
	// environment. Resolution never falls through to an empty secret.
	ErrSecretUnset = errors.New("webhookverify: secret_env is not present in the environment")
	// ErrSecretTooWeak is returned when the resolved secret is shorter than
	// MinSecretBytes or has no entropy.
	ErrSecretTooWeak = errors.New("webhookverify: resolved secret is too short or has no entropy")
)

// SecretResolver resolves the raw secret material for a webhook from the
// operator-owned environment, enforcing the R1 invariants: operator namespace,
// present-not-empty, and a minimum entropy bar.
type SecretResolver struct {
	// lookup mirrors os.LookupEnv; tests inject a fake environment.
	lookup func(string) (string, bool)
}

// NewSecretResolver returns a resolver reading the process environment.
func NewSecretResolver() *SecretResolver {
	return &SecretResolver{lookup: os.LookupEnv}
}

// NewSecretResolverWithEnv returns a resolver reading from lookup instead of the
// process environment. Intended for tests.
func NewSecretResolverWithEnv(lookup func(string) (string, bool)) *SecretResolver {
	return &SecretResolver{lookup: lookup}
}

// Resolve returns the secret bytes for v.SecretEnv, or an error if the name is
// outside the operator namespace, unset, or too weak. It applies to schemes
// that carry secret material in an env var: the HMAC family (the shared HMAC
// key) and discord-ed25519 (the app public key). jwt-jwks carries no env
// secret — its trust anchor is a [JWTVerifierPolicy] — so callers must not call
// Resolve for it.
func (r *SecretResolver) Resolve(v config.WebhookVerify) ([]byte, error) {
	name := strings.TrimSpace(v.SecretEnv)
	if name == "" {
		return nil, ErrSecretEnvUnnamed
	}
	if !strings.HasPrefix(name, OperatorSecretEnvPrefix) {
		return nil, fmt.Errorf("%w: %q must start with %q", ErrSecretEnvPrefix, name, OperatorSecretEnvPrefix)
	}
	lookup := r.lookup
	if lookup == nil {
		lookup = os.LookupEnv
	}
	val, ok := lookup(name)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrSecretUnset, name)
	}
	secret := []byte(val)
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("%w: %q is %d bytes, need >= %d", ErrSecretTooWeak, name, len(secret), MinSecretBytes)
	}
	if !hasMinimalEntropy(secret) {
		return nil, fmt.Errorf("%w: %q is a single repeated byte", ErrSecretTooWeak, name)
	}
	return secret, nil
}

// hasMinimalEntropy rejects degenerate secrets that clear the length bar but
// carry no entropy (e.g. "AAAAAAAAAAAAAAAA"). It is a floor, not a strength
// estimator: a secret with at least two distinct byte values passes.
func hasMinimalEntropy(secret []byte) bool {
	first := secret[0]
	for _, b := range secret[1:] {
		if b != first {
			return true
		}
	}
	return false
}

// JWTVerifierPolicy pins the jwt-jwks trust anchor. It is operator-owned: the
// receiver populates it from city.toml, never from pack-authored TOML. Holding
// these fields here — separate from config.WebhookVerify — is what makes the R1
// boundary enforceable in the type system: a pack cannot supply an issuer,
// audience, or JWKS URL that this package will trust.
type JWTVerifierPolicy struct {
	// Issuer is the required "iss" claim and is pinned exactly.
	Issuer string
	// Audience is the required "aud" claim (any match).
	Audience string
	// JWKSURL is the https endpoint publishing the signing keys.
	JWKSURL string
}

// validate checks that the policy is fully and safely specified.
func (p JWTVerifierPolicy) validate() error {
	if strings.TrimSpace(p.Issuer) == "" {
		return errors.New("webhookverify: jwt policy issuer is required")
	}
	if strings.TrimSpace(p.Audience) == "" {
		return errors.New("webhookverify: jwt policy audience is required")
	}
	raw := strings.TrimSpace(p.JWKSURL)
	if raw == "" {
		return errors.New("webhookverify: jwt policy jwks_url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("webhookverify: jwt policy jwks_url is invalid: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("webhookverify: jwt policy jwks_url must be https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("webhookverify: jwt policy jwks_url has no host")
	}
	return nil
}
