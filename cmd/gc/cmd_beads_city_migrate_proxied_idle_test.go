package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func writeMigrateIdleCityToml(t *testing.T, city, rigName, rigPath, rigPrefix, rigExtra string) {
	t.Helper()
	content := "[workspace]\nname = \"test-city\"\n\n[beads]\nproxied_idle_timeout = \"45m\"\n\n" +
		"[[rigs]]\nname = " + strconv.Quote(rigName) + "\npath = " + strconv.Quote(rigPath) + "\nprefix = " + strconv.Quote(rigPrefix) + "\n" + rigExtra
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(content), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
}

func migrateIdleArg(t *testing.T, calls []migrateProxiedBdCall, scope string) string {
	t.Helper()
	for _, call := range calls {
		if call.scope != normalizePathForCompare(scope) || len(call.args) == 0 || call.args[0] != "migrate" {
			continue
		}
		for i, arg := range call.args {
			if arg == "--idle-timeout" && i+1 < len(call.args) {
				return call.args[i+1]
			}
		}
		t.Fatalf("migrate call for %s has no --idle-timeout: %v", scope, call.args)
	}
	t.Fatalf("no migrate call for %s in %+v", scope, calls)
	return ""
}

// migrate-proxied hands bd the configured idle timeout, and a rig standing on
// the city's root takes the city's value even when it sets its own: one proxy
// serves both, and it runs with whatever its spawner's sidecar says.
func TestMigrateProxiedPassesConfiguredIdleTimeoutAndCityValueToSharedRootRig(t *testing.T) {
	t.Setenv(config.ProxiedIdleTimeoutEnv, "")
	city, rigs := newLegacyManagedCityFixture(t, "spike")
	rig := rigs["spike"]
	writeMigrateIdleCityToml(t, city, "spike", rig, "sp", "beads_proxied_idle_timeout = \"0\"\n")
	seedCityDatabaseDir(t, city, "sp")
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))
	calls := stubMigrateProxiedBdFlippingMetadata(t)

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{JSON: true}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if got := migrateIdleArg(t, *calls, city); got != "45m0s" {
		t.Fatalf("city --idle-timeout = %q, want 45m0s", got)
	}
	if got := migrateIdleArg(t, *calls, rig); got != "45m0s" {
		t.Fatalf("shared-root rig --idle-timeout = %q, want the city's 45m0s", got)
	}
	report := decodeMigrateProxiedReport(t, stdout.String())
	var rigWarnings []string
	for _, scope := range report.Scopes {
		if scope.Scope == "rig:spike" {
			rigWarnings = scope.Warnings
		}
	}
	if len(rigWarnings) == 0 || !strings.Contains(strings.Join(rigWarnings, "\n"), "shares the city's proxy root") {
		t.Fatalf("rig warnings = %q, want the ignored-override warning", rigWarnings)
	}
}

// bd's resume path keeps the idle timeout its journal recorded and ignores the
// one passed now. gc reads the sidecar back and says so.
func TestMigrateProxiedWarnsWhenBdKeptADifferentIdleTimeout(t *testing.T) {
	t.Setenv(config.ProxiedIdleTimeoutEnv, "")
	city, _ := newLegacyManagedCityFixture(t)
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nproxied_idle_timeout = \"45m\"\n"), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	initDoltRootMarker(t, filepath.Join(city, ".beads", "dolt"))
	calls := stubMigrateProxiedBdFlippingMetadata(t)
	inner := runBdScopeCommand
	runBdScopeCommand = func(cityPath, scopeRoot string, args ...string) ([]byte, error) {
		out, err := inner(cityPath, scopeRoot, args...)
		if err == nil && len(args) > 0 && args[0] == "migrate" && args[len(args)-1] != "--help" {
			sidecar := filepath.Join(scopeRoot, ".beads", "proxied_server_client_info.json")
			if werr := os.WriteFile(sidecar, []byte(`{"idle_timeout":-1}`), 0o600); werr != nil {
				return nil, fmt.Errorf("write sidecar: %w", werr)
			}
		}
		return out, err
	}

	var stdout, stderr bytes.Buffer
	if code := doBeadsCityMigrateProxied(city, migrateProxiedOptions{}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBeadsCityMigrateProxied() = %d, want 0\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
	}
	if len(*calls) == 0 {
		t.Fatal("bd was never called")
	}
	if !strings.Contains(stdout.String(), "warning: bd kept the idle timeout") || !strings.Contains(stdout.String(), "45m0s") {
		t.Fatalf("stdout = %s, want the kept-idle-timeout warning naming 45m0s", stdout.String())
	}
}
