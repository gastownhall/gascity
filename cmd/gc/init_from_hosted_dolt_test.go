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
	doInitFromDirWithOptionsInternal(src, cityPath, "", &stdout, &stderr, true, true, hostedDoltInitOptions{})

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
	if _, err := os.Stat(filepath.Join(cityPath, ".beads", "config.yaml")); err == nil {
		t.Errorf("no endpoint supplied, but a canonical .beads/config.yaml was forced")
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
	// unrelated step, and that it wrote nothing before rejecting.
	if !strings.Contains(stderr.String(), "--dolt-port") {
		t.Errorf("expected an endpoint-validation error naming --dolt-port; got: %s", stderr.String())
	}
	toml, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	if strings.Contains(string(toml), "dolt.example.com") {
		t.Errorf("rejected endpoint must not be written to city.toml:\n%s", toml)
	}
}
