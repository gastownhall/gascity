package webhookverify

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestSchemesMatchConfigKnownSet(t *testing.T) {
	// The verifier registry and the E2 config validator must agree on the set
	// of schemes, or config would accept a scheme with no verifier (or vice
	// versa).
	want := []string{"discord-ed25519", "github-hmac-sha256", "hmac-sha256", "jwt-jwks", "slack-v0"}
	if got := Schemes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Schemes() = %v, want %v", got, want)
	}
}

func TestNewUnknownScheme(t *testing.T) {
	_, err := New("totally-made-up", config.WebhookVerify{}, Options{})
	if !errors.Is(err, ErrUnknownScheme) {
		t.Fatalf("err = %v, want ErrUnknownScheme", err)
	}
}

func TestEveryKnownSchemeHasVerifier(t *testing.T) {
	// Construct each scheme with the minimum inputs it needs; every one must be
	// registered.
	cases := map[string]struct {
		cfg  config.WebhookVerify
		opts Options
	}{
		"github-hmac-sha256": {},
		"hmac-sha256":        {cfg: config.WebhookVerify{SignatureHeader: "X-Sig"}},
		"slack-v0":           {},
		"discord-ed25519":    {},
		"jwt-jwks":           {opts: Options{JWTPolicy: &JWTVerifierPolicy{Issuer: "i", Audience: "a", JWKSURL: "https://x/jwks"}}},
	}
	for scheme, c := range cases {
		if _, err := New(scheme, c.cfg, c.opts); err != nil {
			t.Errorf("New(%q): %v", scheme, err)
		}
	}
}
