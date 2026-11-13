package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestResolveProviderCarriesUpstreamEnvThroughBaseChain guards the harness
// serving-env contract across provider inheritance.
//
// A provider written as `base = "builtin:claude"` inherits its base's
// upstream_env binding: MergeProviderOverBuiltin merges the field per
// sub-field during the chain walk. Folding the resolved chain back onto the
// leaf has to carry it too, or the binding is silently lost between that merge
// and the value ResolveProvider hands back.
//
// The consequence is not cosmetic. The upstream axis renders an
// [upstreams.<name>] abstract api_key onto the harness's bound env-var name,
// and a missing binding is a HARD ERROR by design rather than a silent no-op
// ("sets api_key, but its harness declares no upstream_env.api_key binding").
// Losing the binding here therefore fails session start outright for every
// agent selecting such an upstream, against a harness that does declare the
// name.
//
// The config is loaded from a real city.toml rather than hand-built, so the
// test exercises the same decode path a city does.
func TestResolveProviderCarriesUpstreamEnvThroughBaseChain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(path, []byte(`
[workspace]
name = "test"

[providers.zai]
base = "builtin:claude"
env = {ANTHROPIC_API_KEY = "${ACME_KEY}"}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	resolved, err := ResolveProvider(&Agent{Provider: "zai"}, &cfg.Workspace, cfg.Providers, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}

	want := BuiltinProviders()["claude"].UpstreamEnv
	if want.IsZero() {
		t.Fatal("test setup: the built-in claude provider declares no upstream_env binding")
	}
	for _, tc := range []struct {
		field, got, want string
	}{
		{"api_key", resolved.UpstreamEnv.APIKey, want.APIKey},
		{"auth_token", resolved.UpstreamEnv.AuthToken, want.AuthToken},
		{"base_url", resolved.UpstreamEnv.BaseURL, want.BaseURL},
	} {
		if tc.want == "" {
			continue
		}
		if tc.got != tc.want {
			t.Errorf("upstream_env.%s = %q; want %q inherited from builtin:claude — an agent selecting an upstream that sets this field fails session start with %q",
				tc.field, tc.got, tc.want, "declares no upstream_env."+tc.field+" binding")
		}
	}
}

// TestResolveProviderLeafUpstreamEnvWinsOverBase pins the merge direction: a
// leaf declaring its own binding keeps it, so inheriting the base's names
// cannot overwrite an explicit harness contract. The fields the leaf leaves
// unset still come from the base, which is what makes a gateway harness able
// to override only api_key.
func TestResolveProviderLeafUpstreamEnvWinsOverBase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(path, []byte(`
[workspace]
name = "test"

[providers.gateway]
base = "builtin:claude"

[providers.gateway.upstream_env]
api_key = "GATEWAY_API_KEY"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	resolved, err := ResolveProvider(&Agent{Provider: "gateway"}, &cfg.Workspace, cfg.Providers, lookPathAll)
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}

	if got := resolved.UpstreamEnv.APIKey; got != "GATEWAY_API_KEY" {
		t.Errorf("upstream_env.api_key = %q; want the leaf's own GATEWAY_API_KEY", got)
	}
	if want := BuiltinProviders()["claude"].UpstreamEnv.AuthToken; want != "" {
		if got := resolved.UpstreamEnv.AuthToken; got != want {
			t.Errorf("upstream_env.auth_token = %q; want %q — the leaf declared only api_key, so the base's binding must survive", got, want)
		}
	}
}
