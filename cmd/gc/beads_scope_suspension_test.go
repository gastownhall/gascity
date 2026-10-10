package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// providerOwnedHealthFixture is a city with one provider-owned rig whose
// provider script records each op it is asked to run.
func providerOwnedHealthFixture(t *testing.T, rigExtra string) (city, rig, logPath string) {
	t.Helper()
	city = t.TempDir()
	rig = filepath.Join(city, "rigs", "r1")
	if err := os.MkdirAll(rig, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath = filepath.Join(city, "provider.log")
	provider := filepath.Join(city, "gc-beads-bd.sh")
	if err := os.WriteFile(provider, []byte("#!/bin/sh\nprintf '%s\\n' \"$1\" >> \"$GC_TEST_PROVIDER_LOG\"\n"), 0o755); err != nil { //nolint:gosec // fixture must be executable
		t.Fatal(err)
	}
	t.Setenv("GC_TEST_PROVIDER_LOG", logPath)
	t.Setenv("GC_SUSPENDED", "")
	toml := "[beads]\nprovider = \"exec:" + provider + "\"\n[[rigs]]\nname = \"r1\"\npath = \"rigs/r1\"\n" + rigExtra
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(toml), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	if err := persistProviderScopeOwnership(city, rig, providerScopeIntent{Transport: "direct", Target: "local"}); err != nil {
		t.Fatal(err)
	}
	if err := markProviderScopeOwnershipReady(city, rig); err != nil {
		t.Fatal(err)
	}
	return city, rig, logPath
}

func providerOpsLogged(t *testing.T, logPath string) string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func stubBeadsScopeIdleRetired(t *testing.T, retired bool) {
	t.Helper()
	old := beadsScopeIdleRetired
	t.Cleanup(func() { beadsScopeIdleRetired = old })
	beadsScopeIdleRetired = func(string) bool { return retired }
}

// A health pass pings a scope whose proxy is expected up, and leaves alone a
// scope whose proxy retired on its idle timeout: that is healthy, and a ping
// would restart it.
func TestProviderOwnedHealthSkipsAnIdleRetiredScope(t *testing.T) {
	for _, tc := range []struct {
		name     string
		retired  bool
		wantPing bool
	}{
		{name: "expected up", wantPing: true},
		{name: "idle-retired", retired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city, _, logPath := providerOwnedHealthFixture(t, "")
			stubBeadsScopeIdleRetired(t, tc.retired)
			if err := runProviderOwnedScopesLifecycleOp(city, "health"); err != nil {
				t.Fatal(err)
			}
			if got := providerOpsLogged(t, logPath) == "health"; got != tc.wantPing {
				t.Fatalf("health ran = %v, want %v (ops %q)", got, tc.wantPing, providerOpsLogged(t, logPath))
			}
		})
	}
}

// A suspended rig gets no start and no health op, but stop still visits it.
func TestProviderOwnedLifecycleLeavesASuspendedRigCold(t *testing.T) {
	city, rig, logPath := providerOwnedHealthFixture(t, "suspended_on_start = true\n")
	stubBeadsScopeIdleRetired(t, false)
	for _, op := range []string{"health", "start"} {
		roots, err := providerOwnedLifecycleScopeRoots(city, op)
		if err != nil {
			t.Fatal(err)
		}
		for _, root := range roots {
			if samePath(root, rig) {
				t.Fatalf("op %q visits the suspended rig: %v", op, roots)
			}
		}
	}
	if err := runProviderOwnedScopesLifecycleOp(city, "health"); err != nil {
		t.Fatal(err)
	}
	if ops := providerOpsLogged(t, logPath); ops != "" {
		t.Fatalf("a suspended rig was health-checked: %q", ops)
	}
	roots, err := providerOwnedLifecycleScopeRoots(city, "stop")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, root := range roots {
		found = found || samePath(root, rig)
	}
	if !found {
		t.Fatalf("stop does not visit the suspended rig: %v", roots)
	}
}

// A suspended city leaves every scope out of start and health.
func TestProviderOwnedLifecycleLeavesASuspendedCityCold(t *testing.T) {
	city, _, _ := providerOwnedHealthFixture(t, "")
	t.Setenv("GC_SUSPENDED", "1")
	roots, err := providerOwnedLifecycleScopeRoots(city, "health")
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 0 {
		t.Fatalf("health visits %v in a suspended city, want nothing", roots)
	}
}
