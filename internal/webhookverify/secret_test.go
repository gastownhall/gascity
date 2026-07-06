package webhookverify

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func staticEnv(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestSecretResolver_RejectsNonOperatorPrefix(t *testing.T) {
	// R1: a pack must not be able to name an arbitrary ambient variable.
	r := NewSecretResolverWithEnv(staticEnv(map[string]string{
		"AWS_SECRET_ACCESS_KEY": "AKIAabcdefghijklmnop",
		"HOME":                  "/home/ubuntu",
	}))
	for _, env := range []string{"AWS_SECRET_ACCESS_KEY", "HOME", "GC_CITY", "PATH"} {
		_, err := r.Resolve(config.WebhookVerify{SecretEnv: env})
		if !errors.Is(err, ErrSecretEnvPrefix) {
			t.Errorf("SecretEnv=%q: err=%v, want ErrSecretEnvPrefix", env, err)
		}
	}
}

func TestSecretResolver_UnsetVarHardErrors(t *testing.T) {
	// R1: an unset operator var must hard-error, never fall through to empty.
	r := NewSecretResolverWithEnv(staticEnv(map[string]string{}))
	_, err := r.Resolve(config.WebhookVerify{SecretEnv: "GC_WEBHOOK_GITHUB_SECRET"})
	if !errors.Is(err, ErrSecretUnset) {
		t.Fatalf("err=%v, want ErrSecretUnset", err)
	}
}

func TestSecretResolver_RejectsWeakSecret(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"short":       "too-short",        // < 16 bytes
		"fifteen":     "0123456789abcde",  // 15 bytes
		"zeroEntropy": "AAAAAAAAAAAAAAAA", // 16 bytes but no entropy
	}
	for name, val := range cases {
		r := NewSecretResolverWithEnv(staticEnv(map[string]string{"GC_WEBHOOK_X": val}))
		_, err := r.Resolve(config.WebhookVerify{SecretEnv: "GC_WEBHOOK_X"})
		if !errors.Is(err, ErrSecretTooWeak) {
			t.Errorf("%s: err=%v, want ErrSecretTooWeak", name, err)
		}
	}
}

func TestSecretResolver_AcceptsOperatorSecret(t *testing.T) {
	want := "a-perfectly-strong-webhook-secret-value"
	r := NewSecretResolverWithEnv(staticEnv(map[string]string{"GC_WEBHOOK_GITHUB_SECRET": want}))
	got, err := r.Resolve(config.WebhookVerify{SecretEnv: "GC_WEBHOOK_GITHUB_SECRET"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSecretResolver_EmptySecretEnvName(t *testing.T) {
	r := NewSecretResolverWithEnv(staticEnv(map[string]string{}))
	_, err := r.Resolve(config.WebhookVerify{SecretEnv: "   "})
	if !errors.Is(err, ErrSecretEnvUnnamed) {
		t.Fatalf("err=%v, want ErrSecretEnvUnnamed", err)
	}
}

func TestJWTVerifierPolicy_Validate(t *testing.T) {
	good := JWTVerifierPolicy{Issuer: "https://issuer.example", Audience: "gascity", JWKSURL: "https://issuer.example/jwks"}
	if err := good.validate(); err != nil {
		t.Fatalf("good policy rejected: %v", err)
	}
	bad := []JWTVerifierPolicy{
		{Audience: "a", JWKSURL: "https://x/jwks"},             // no issuer
		{Issuer: "i", JWKSURL: "https://x/jwks"},               // no audience
		{Issuer: "i", Audience: "a"},                           // no jwks url
		{Issuer: "i", Audience: "a", JWKSURL: "http://x/jwks"}, // not https
		{Issuer: "i", Audience: "a", JWKSURL: "https:///jwks"}, // no host
		{Issuer: "i", Audience: "a", JWKSURL: "://bad"},        // unparseable
	}
	for i, p := range bad {
		if err := p.validate(); err == nil {
			t.Errorf("bad policy[%d] accepted: %+v", i, p)
		}
	}
}
