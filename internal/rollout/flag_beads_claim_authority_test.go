package rollout

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestResolveClaimAuthorityPrecedence walks builtin < config < env for the
// claim-authority gate through its own config field and env var.
func TestResolveClaimAuthorityPrecedence(t *testing.T) {
	t.Parallel()
	cityWith := func(raw string) *config.City {
		return &config.City{Beads: config.BeadsConfig{ClaimAuthority: raw}}
	}
	env := func(m map[string]string) ResolveOptions { return ResolveOptions{LookupEnv: envMap(m)} }

	f, err := Resolve(cityWith(""), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if f.BeadsClaimAuthority() != Off || f.OriginOf(keyBeadsClaimAuthority) != OriginBuiltin {
		t.Errorf("unset = %q/%q, want off/builtin", f.BeadsClaimAuthority(), f.OriginOf(keyBeadsClaimAuthority))
	}
	f, err = Resolve(cityWith("auto"), env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if f.BeadsClaimAuthority() != Auto || f.OriginOf(keyBeadsClaimAuthority) != OriginConfig {
		t.Errorf("config auto = %q/%q, want auto/config", f.BeadsClaimAuthority(), f.OriginOf(keyBeadsClaimAuthority))
	}
	f, err = Resolve(cityWith("auto"), env(map[string]string{envBeadsClaimAuthority: "require"}))
	if err != nil {
		t.Fatal(err)
	}
	if f.BeadsClaimAuthority() != Require {
		t.Errorf("env require over config auto = %q, want require", f.BeadsClaimAuthority())
	}
	if got := ForTest(WithBeadsClaimAuthority(Auto)).BeadsClaimAuthority(); got != Auto {
		t.Errorf("WithBeadsClaimAuthority(Auto) = %q, want auto", got)
	}
}
