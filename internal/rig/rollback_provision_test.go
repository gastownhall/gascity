package rig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// originalCityTOML is the pre-write fixture a rollback test snapshots and then
// asserts the topology restore recovers byte-for-byte.
const originalCityTOML = "[workspace]\nname = \"rollbackcity\"\n"

// provisionToWritePhase wires a Deps + request that drives Provision all the way
// into the guarded config-write window (steps 14-16): a fresh add of an existing,
// storeless rig directory against a real temp city on disk. NormalizeScopes is
// stubbed to succeed; the caller injects whichever late-stage failure it wants to
// exercise (WriteRoutes or NormalizeScopes) before calling Provision. It returns
// the deps, the request, and the city.toml path the test asserts against.
func provisionToWritePhase(t *testing.T) (Deps, ProvisionRequest, string) {
	t.Helper()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	if err := os.WriteFile(tomlPath, []byte(originalCityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := stubDeps(cityPath)
	deps.NormalizeScopes = func(string, *config.City) error { return nil }
	req := ProvisionRequest{Name: "rollbackrig", Path: t.TempDir()}
	return deps, req, tomlPath
}

func TestProvisionRollsBackWhenWriteRoutesFails(t *testing.T) {
	deps, req, tomlPath := provisionToWritePhase(t)

	original, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}

	writeRoutesErr := errors.New("boom: routes rejected")
	writeRoutesCalled := false
	deps.WriteRoutes = func(string, *config.City) error {
		writeRoutesCalled = true
		return writeRoutesErr
	}

	_, _, provErr := Provision(deps, req)
	if provErr == nil {
		t.Fatal("expected a rollback error when WriteRoutes fails")
	}
	if !writeRoutesCalled {
		t.Fatal("WriteRoutes was never reached; the flow did not get past the config write")
	}
	if !errors.Is(provErr, writeRoutesErr) {
		t.Fatalf("error %v does not wrap the WriteRoutes failure", provErr)
	}
	if got := provErr.Error(); !strings.HasPrefix(got, "writing routes: ") {
		t.Fatalf("error %q lacks the 'writing routes: ' rollback prefix", got)
	}

	// The config write appended a [[rigs]] block before WriteRoutes failed;
	// the topology snapshot must have restored city.toml to its pre-write bytes.
	restored, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatalf("city.toml not restored after rollback:\n got %q\nwant %q", restored, original)
	}
}

func TestProvisionRollsBackWhenNormalizeScopesFails(t *testing.T) {
	deps, req, tomlPath := provisionToWritePhase(t)

	original, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}

	normalizeErr := errors.New("boom: scopes rejected")
	deps.NormalizeScopes = func(string, *config.City) error { return normalizeErr }

	writeRoutesCalled := false
	deps.WriteRoutes = func(string, *config.City) error {
		writeRoutesCalled = true
		return nil
	}

	_, _, provErr := Provision(deps, req)
	if provErr == nil {
		t.Fatal("expected a rollback error when NormalizeScopes fails")
	}
	if !errors.Is(provErr, normalizeErr) {
		t.Fatalf("error %v does not wrap the NormalizeScopes failure", provErr)
	}
	if got := provErr.Error(); !strings.HasPrefix(got, "canonicalizing rig topology: ") {
		t.Fatalf("error %q lacks the 'canonicalizing rig topology: ' rollback prefix", got)
	}
	if writeRoutesCalled {
		t.Error("WriteRoutes ran after NormalizeScopes failed; the config write must short-circuit")
	}

	// NormalizeScopes fails before city.toml is touched, so the file stays at its
	// pre-write bytes (the rollback restore is a no-op here, but must not corrupt).
	restored, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(restored) != string(original) {
		t.Fatalf("city.toml modified despite NormalizeScopes failing before the write:\n got %q\nwant %q", restored, original)
	}
}
