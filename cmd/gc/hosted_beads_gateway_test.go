package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/execenv"
)

// hostedEnvEntriesToMap collapses KEY=VALUE entries into a map for assertions
// (last write wins, mirroring process-env semantics).
func hostedEnvEntriesToMap(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if k, v, ok := strings.Cut(e, "="); ok {
			out[k] = v
		}
	}
	return out
}

// TestPreserveHostedBeadsCredentialEnv pins the re-add logic in isolation:
// passthrough keys present in the original environ are restored, keys carried by
// an override are left to the caller, and unrelated keys are never injected.
func TestPreserveHostedBeadsCredentialEnv(t *testing.T) {
	const (
		credCmd   = "eia-helper --audience beads"
		serverTLS = "/etc/gc/ca.crt"
		keyFile   = "/var/run/secrets/orchestrator.key"
		audience  = "beads"
		stsToken  = "https://id.example/sts/v0/token"
		stsMach   = "https://id.example/sts/v0/machine"
	)
	// EIA_SCOPES is intentionally absent from environ; STS_MACHINE_URL is carried
	// by an override; RANDOM_TOKEN is a non-passthrough sensitive key.
	environ := []string{
		"BEADS_DOLT_CREDENTIAL_COMMAND=" + credCmd,
		"BEADS_DOLT_SERVER_TLS=" + serverTLS,
		"ORCHESTRATOR_KEY_FILE=" + keyFile,
		"EIA_AUDIENCE=" + audience,
		"STS_TOKEN_URL=" + stsToken,
		"STS_MACHINE_URL=" + stsMach,
		"RANDOM_TOKEN=should-not-pass",
		"PATH=/usr/bin",
	}
	// `out` starts as if FilterInherited already stripped the sensitive keys, plus
	// a caller-supplied override that must win.
	out := []string{"PATH=/usr/bin", "GC_DOLT_HOST=gw.beads.example"}
	overrides := map[string]string{"STS_MACHINE_URL": "https://override.example/sts"}

	got := hostedEnvEntriesToMap(preserveHostedBeadsCredentialEnv(out, environ, overrides))

	for key, want := range map[string]string{
		"BEADS_DOLT_CREDENTIAL_COMMAND": credCmd,
		"BEADS_DOLT_SERVER_TLS":         serverTLS,
		"ORCHESTRATOR_KEY_FILE":         keyFile,
		"EIA_AUDIENCE":                  audience,
		"STS_TOKEN_URL":                 stsToken,
	} {
		if got[key] != want {
			t.Errorf("preserveHostedBeadsCredentialEnv() %s = %q, want %q", key, got[key], want)
		}
	}
	// Overridden key: preserve must NOT re-add the inherited value (the caller
	// already owns it via the override).
	if v, ok := got["STS_MACHINE_URL"]; ok && v == stsMach {
		t.Errorf("STS_MACHINE_URL was restored from environ %q, but an override is present; override must win", stsMach)
	}
	// Absent-from-environ passthrough key stays absent.
	if _, ok := got["EIA_SCOPES"]; ok {
		t.Errorf("EIA_SCOPES is absent from environ; it must not be synthesized")
	}
	// Non-passthrough sensitive key is never preserved.
	if _, ok := got["RANDOM_TOKEN"]; ok {
		t.Errorf("RANDOM_TOKEN is not a passthrough key; it must not be preserved")
	}
}

// TestMergeRuntimeEnvPreservesHostedBeadsCredentialEnv proves the passthrough is
// load-bearing end-to-end: FilterInherited strips the credential command (it
// carries a CREDENTIAL marker), yet mergeRuntimeEnv restores it so a gc-spawned
// bd can authenticate to a hosted beads-gateway — while genuinely unrelated
// sensitive env stays stripped.
func TestMergeRuntimeEnvPreservesHostedBeadsCredentialEnv(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin",
		"BEADS_DOLT_CREDENTIAL_COMMAND=eia-helper --audience beads",
		"STS_TOKEN_URL=https://id.example/sts/v0/token",
		"BEADS_DOLT_SERVER_TLS=/etc/gc/ca.crt",
		"EIA_AUDIENCE=beads",
		"SOME_OTHER_TOKEN=secret-should-be-dropped",
	}

	// Precondition: without the passthrough this test would be vacuous — confirm
	// the generic inherited-env filter really drops the credential command.
	for _, e := range execenv.FilterInherited(environ) {
		if strings.HasPrefix(e, "BEADS_DOLT_CREDENTIAL_COMMAND=") {
			t.Fatalf("precondition failed: FilterInherited kept BEADS_DOLT_CREDENTIAL_COMMAND; passthrough no longer needed?")
		}
	}

	out := hostedEnvEntriesToMap(mergeRuntimeEnv(environ, map[string]string{"GC_DOLT_HOST": "gw.beads.example"}))

	for key, want := range map[string]string{
		"BEADS_DOLT_CREDENTIAL_COMMAND": "eia-helper --audience beads",
		"STS_TOKEN_URL":                 "https://id.example/sts/v0/token",
		"BEADS_DOLT_SERVER_TLS":         "/etc/gc/ca.crt",
		"EIA_AUDIENCE":                  "beads",
		"GC_DOLT_HOST":                  "gw.beads.example",
	} {
		if out[key] != want {
			t.Errorf("mergeRuntimeEnv() %s = %q, want %q", key, out[key], want)
		}
	}
	if _, ok := out["SOME_OTHER_TOKEN"]; ok {
		t.Errorf("SOME_OTHER_TOKEN is not a passthrough key; FilterInherited must strip it")
	}
}

// TestOverlayEnvEntriesPreservesHostedBeadsCredentialEnv covers the second call
// site (overlayEnvEntries also filters inherited env before re-adding overrides).
func TestOverlayEnvEntriesPreservesHostedBeadsCredentialEnv(t *testing.T) {
	environ := []string{
		"BEADS_DOLT_CREDENTIAL_COMMAND=eia-helper",
		"PATH=/usr/bin",
	}
	out := hostedEnvEntriesToMap(overlayEnvEntries(environ, map[string]string{"GC_DOLT_HOST": "gw"}))
	if got := out["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != "eia-helper" {
		t.Fatalf("overlayEnvEntries() BEADS_DOLT_CREDENTIAL_COMMAND = %q, want preserved", got)
	}
}

// TestVerifyManagedDoltDatabaseExistsAfterInitSkipsExternalDolt verifies the
// post-init catalog check short-circuits for an external/hosted dolt endpoint.
// A per-tenant beads-gateway scopes each connection to its own project DB and
// denies the SHOW DATABASES listing this guard relies on, so the managed-catalog
// lister must never run. The test plants a resolvable managed port so that,
// without the external short-circuit, verify would reach (and fail at) the
// lister — making the assertion bite if the guard regresses.
func TestVerifyManagedDoltDatabaseExistsAfterInitSkipsExternalDolt(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	for _, k := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: e2e
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: gw.beads.example.com
dolt.port: 3306
dolt.user: orchestrator
`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Make currentResolvableManagedDoltPort resolve a real port via the provider
	// managed-dolt state fallback.
	writeReachableProviderManagedDoltState(t, cityPath)

	if !isExternalDolt(cityPath) {
		t.Fatalf("precondition: expected isExternalDolt(cityPath)=true for a canonical external config")
	}
	if !cityUsesBdStoreContract(cityPath) {
		t.Fatalf("precondition: expected cityUsesBdStoreContract(cityPath)=true for the default bd provider")
	}
	if port := currentResolvableManagedDoltPort(cityPath); port == "" {
		t.Fatalf("precondition: expected a resolvable managed port so the guard regression would be observable")
	}

	orig := managedDoltListUserDatabasesAfterInit
	t.Cleanup(func() { managedDoltListUserDatabasesAfterInit = orig })
	called := false
	managedDoltListUserDatabasesAfterInit = func(string) ([]string, error) {
		called = true
		return nil, fmt.Errorf("managed-catalog lister must not run for an external dolt endpoint")
	}

	if err := verifyManagedDoltDatabaseExistsAfterInit(cityPath, cityPath, "bd_prj_47890a40d5bee1d9"); err != nil {
		t.Fatalf("verifyManagedDoltDatabaseExistsAfterInit() for external dolt = %v, want nil", err)
	}
	if called {
		t.Fatalf("managed-catalog lister was called for an external dolt endpoint; the SHOW DATABASES guard must be skipped")
	}
}

// TestCityRuntimeProcessEnvProjectsHostedBeadsCredentialCommand pins the
// exec-provider / city process-env projection path. mirrorBeadsDoltEnv derives
// BEADS_DOLT_CREDENTIAL_COMMAND for a controller that exports only the
// non-sensitive GC_DOLT_CRED_CMD, but cityRuntimeProcessEnvWithError copies the
// resolved source map through a backend-key whitelist. That whitelist
// (execProjectedBackendEnvKeys) omits the credential command because it is a
// preserve-from-ambient passthrough key, not a strip-and-reproject projectedDolt
// key — so before execProjectedBackendCopyKeys carried it, a GC_DOLT_CRED_CMD-only
// controller silently dropped the helper on the projected process env and bd fell
// back to the root user (gateway Error 1045). The ambient BEADS_DOLT_CREDENTIAL_COMMAND
// is left unset here so preserveHostedBeadsCredentialEnv has nothing to fall back
// on: the projection is the only channel that can carry the derived value.
func TestCityRuntimeProcessEnvProjectsHostedBeadsCredentialCommand(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_CRED_CMD", "/usr/local/bin/eia-helper")
	t.Setenv("BEADS_DOLT_CREDENTIAL_COMMAND", "")
	_ = os.Unsetenv("BEADS_DOLT_CREDENTIAL_COMMAND")
	for _, k := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(`issue_prefix: e2e
gc.endpoint_origin: city_canonical
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: gw.beads.example.com
dolt.port: 3306
dolt.user: orchestrator
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if !cityUsesBdStoreContract(cityPath) {
		t.Fatalf("precondition: expected cityUsesBdStoreContract(cityPath)=true for the default bd provider")
	}

	env, err := cityRuntimeProcessEnvWithError(cityPath)
	if err != nil {
		t.Fatalf("cityRuntimeProcessEnvWithError() error = %v", err)
	}
	if got := hostedEnvEntriesToMap(env)["BEADS_DOLT_CREDENTIAL_COMMAND"]; got != "/usr/local/bin/eia-helper" {
		t.Fatalf("BEADS_DOLT_CREDENTIAL_COMMAND = %q, want %q (projected from ambient GC_DOLT_CRED_CMD)", got, "/usr/local/bin/eia-helper")
	}
}
