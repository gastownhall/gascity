package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gastownExamplePath resolves the bundled example city used as a --from source.
func gastownExamplePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join("..", "..", "examples", "gastown"))
	if err != nil {
		t.Fatalf("resolving examples/gastown: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p, "city.toml")); err != nil {
		t.Skipf("example source missing: %v", err)
	}
	return p
}

// TestInitFromPinsHostedDoltEndpoint verifies that `gc init --from` honors an
// external Dolt endpoint (the regression: --from previously ignored --dolt-*/
// GC_DOLT_* and let the copied template's managed-local assumption win). The
// pinned endpoint must land in city.toml [dolt] and the canonical
// .beads/config.yaml.
func TestInitFromPinsHostedDoltEndpoint(t *testing.T) {
	clearGCEnv(t)

	src := gastownExamplePath(t)
	cityPath := filepath.Join(t.TempDir(), "city")

	hosted := hostedDoltInitOptions{
		Host:      "dolt.example.com",
		Port:      "3307",
		User:      "root",
		Database:  "ci",
		ProjectID: "11111111-1111-1111-1111-111111111111",
	}

	var stdout, stderr bytes.Buffer
	code := doInitFromDirWithOptionsInternal(src, cityPath, "", &stdout, &stderr, true, true, hosted)
	if code != 0 {
		t.Fatalf("doInitFromDirWithOptionsInternal = %d, want 0; stderr: %s", code, stderr.String())
	}

	toml, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	if !strings.Contains(string(toml), "dolt.example.com") {
		t.Errorf("city.toml should pin the external dolt host; got:\n%s", toml)
	}

	cfgYaml, err := os.ReadFile(filepath.Join(cityPath, ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read .beads/config.yaml: %v", err)
	}
	if !strings.Contains(string(cfgYaml), "dolt.example.com") {
		t.Errorf(".beads/config.yaml should record the external endpoint; got:\n%s", cfgYaml)
	}
}

// TestInitFromWithoutHostedPreservesTemplate verifies that when no endpoint is
// supplied the copied template is preserved unchanged (no [dolt] section, no
// canonical external config.yaml is forced).
func TestInitFromWithoutHostedPreservesTemplate(t *testing.T) {
	clearGCEnv(t)

	src := gastownExamplePath(t)
	cityPath := filepath.Join(t.TempDir(), "city")

	var stdout, stderr bytes.Buffer
	// disabled hosted options => template preserved
	code := doInitFromDirWithOptionsInternal(src, cityPath, "", &stdout, &stderr, true, true, hostedDoltInitOptions{})
	// The return code is not asserted: finalizeInit's hard-dependency checks
	// depend on what the host box has provisioned. What must hold is that no
	// endpoint validation ran at all.
	t.Logf("doInitFromDirWithOptionsInternal = %d; stderr: %s", code, stderr.String())
	if strings.Contains(stderr.String(), "--dolt") {
		t.Errorf("no endpoint supplied, but init reported a --dolt validation error: %s", stderr.String())
	}

	// The hosted block runs before the scaffold and before finalizeInit, so the
	// copied config is fully determined by this point regardless of whether the
	// later managed-Dolt steps can complete in this environment.
	toml, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	if strings.Contains(string(toml), "dolt.example.com") {
		t.Errorf("no endpoint supplied, but city.toml gained an external dolt host:\n%s", toml)
	}
	// A managed-local .beads/config.yaml is written by the normal bootstrap
	// whenever bd is available, so its mere existence proves nothing. The
	// invariant is that no *external* endpoint was pinned.
	if cityExternalDoltEndpointUnverified(cityPath) {
		cfgYaml, _ := os.ReadFile(filepath.Join(cityPath, ".beads", "config.yaml")) //nolint:errcheck // diagnostic only
		t.Errorf("no endpoint supplied, but the canonical config pins an unverified external endpoint:\n%s", cfgYaml)
	}
}

// TestInitFromRejectsIncompleteHostedEndpoint verifies that an incomplete
// endpoint (host without required port/database/project id) fails before
// leaving a partially-configured city.
func TestInitFromRejectsIncompleteHostedEndpoint(t *testing.T) {
	clearGCEnv(t)

	src := gastownExamplePath(t)
	cityPath := filepath.Join(t.TempDir(), "city")

	// host set but port/database/project-id missing -> validate() must fail
	hosted := hostedDoltInitOptions{Host: "dolt.example.com"}

	var stdout, stderr bytes.Buffer
	code := doInitFromDirWithOptionsInternal(src, cityPath, "", &stdout, &stderr, true, true, hosted)
	if code == 0 {
		t.Fatalf("expected failure for incomplete endpoint, got success")
	}
	// Assert it failed on endpoint validation specifically, not on some later
	// unrelated step, and that it touched the filesystem not at all: a
	// half-copied destination would make the corrected retry fail with
	// "already initialized".
	if !strings.Contains(stderr.String(), "--dolt-port") {
		t.Errorf("expected an endpoint-validation error naming --dolt-port; got: %s", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(cityPath, "city.toml")); !os.IsNotExist(err) {
		t.Errorf("rejected endpoint must leave no destination behind; os.Stat city.toml = %v", err)
	}
}

// TestInitFromRejectsDoltFlagsWithoutHost verifies that partial --dolt-* flags
// with no host are an error rather than a silent no-op. Before --dolt-* became
// compatible with --from, cobra's mutual-exclusion rejected the combination;
// the endpoint validation now has to carry that contract.
func TestInitFromRejectsDoltFlagsWithoutHost(t *testing.T) {
	clearGCEnv(t)

	src := gastownExamplePath(t)
	cityPath := filepath.Join(t.TempDir(), "city")

	var stdout, stderr bytes.Buffer
	code := doInitFromDirWithOptionsInternal(src, cityPath, "", &stdout, &stderr, true, true, hostedDoltInitOptions{Port: "3307"})
	if code == 0 {
		t.Fatalf("expected failure for --dolt-port without a host, got success")
	}
	if !strings.Contains(stderr.String(), "--dolt-host") {
		t.Errorf("expected an error naming --dolt-host; got: %s", stderr.String())
	}
}

// TestInitFromHostedPreservesRigSiteBindings verifies that pinning a hosted
// endpoint does not erase the .gc/site.toml rig bindings written by the
// identity rewrite. The identity rewrite strips rig paths from city.toml and
// persists them to site.toml; the hosted rewrite of the same city.toml must
// re-supply them or the write path treats each rig as unbound and drops it.
func TestInitFromHostedPreservesRigSiteBindings(t *testing.T) {
	clearGCEnv(t)

	// No bundled example has both a pack.toml and rigs with paths — which is
	// exactly why this gap survived CI — so build the source shape here.
	src := t.TempDir()
	const rigPath = "/tmp/example-rig"
	writeInitSourceFile(t, src, "city.toml", `[workspace]
name = "fleet"
prefix = "fl"
provider = "claude"

[providers.claude]
base = "builtin:claude"

[[rigs]]
name = "example"
path = "`+rigPath+`"
`)
	writeInitSourceFile(t, src, "pack.toml", `[pack]
name = "fleet"
schema = 2
`)

	cityPath := filepath.Join(t.TempDir(), "city")
	hosted := hostedDoltInitOptions{
		Host:      "dolt.example.com",
		Port:      "3307",
		User:      "root",
		Database:  "ci",
		ProjectID: "11111111-1111-1111-1111-111111111111",
	}

	var stdout, stderr bytes.Buffer
	code := doInitFromDirWithOptionsInternal(src, cityPath, "", &stdout, &stderr, true, true, hosted)
	if code != 0 {
		t.Fatalf("doInitFromDirWithOptionsInternal = %d, want 0; stderr: %s", code, stderr.String())
	}

	site, err := os.ReadFile(filepath.Join(cityPath, ".gc", "site.toml"))
	if err != nil {
		t.Fatalf("read .gc/site.toml: %v", err)
	}
	if !strings.Contains(string(site), rigPath) {
		t.Errorf("hosted rewrite erased the rig site binding; .gc/site.toml:\n%s", site)
	}
}

// writeInitSourceFile writes one file of a synthetic `gc init --from` source.
func writeInitSourceFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}
