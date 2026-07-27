package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestValidateRigEndpointOptionsRejectsWildcardExternalHost(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::"} {
		t.Run(host, func(t *testing.T) {
			err := validateRigEndpointOptions(rigEndpointOptions{External: true, Host: host, Port: "4406"})
			if err == nil || !strings.Contains(err.Error(), "invalid --host") {
				t.Fatalf("validateRigEndpointOptions(%q) error = %v", host, err)
			}
		})
	}
}

func TestValidateRigEndpointOptionsSelfRequiresPort(t *testing.T) {
	err := validateRigEndpointOptions(rigEndpointOptions{Self: true})
	if err == nil || !strings.Contains(err.Error(), "--self requires --port") {
		t.Fatalf("validateRigEndpointOptions(Self without port) error = %v", err)
	}
}

func TestValidateRigEndpointOptionsSelfRejectsHost(t *testing.T) {
	err := validateRigEndpointOptions(rigEndpointOptions{Self: true, Port: "28232", Host: "db.example.com"})
	if err == nil || !strings.Contains(err.Error(), "--self") || !strings.Contains(err.Error(), "--host") {
		t.Fatalf("validateRigEndpointOptions(Self+Host) error = %v", err)
	}
}

func TestValidateRigEndpointOptionsSelfRejectsUser(t *testing.T) {
	err := validateRigEndpointOptions(rigEndpointOptions{Self: true, Port: "28232", User: "someone"})
	if err == nil || !strings.Contains(err.Error(), "--self") || !strings.Contains(err.Error(), "--user") {
		t.Fatalf("validateRigEndpointOptions(Self+User) error = %v", err)
	}
}

func TestValidateRigEndpointOptionsForceRequiresSelf(t *testing.T) {
	err := validateRigEndpointOptions(rigEndpointOptions{External: true, Host: "db.example.com", Port: "3307", Force: true})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("validateRigEndpointOptions(External+Force) error = %v", err)
	}
}

func TestValidateRigEndpointOptionsRejectsMultipleModes(t *testing.T) {
	err := validateRigEndpointOptions(rigEndpointOptions{Inherit: true, Self: true, Port: "28232"})
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("validateRigEndpointOptions(Inherit+Self) error = %v", err)
	}
}

func TestDoRigSetEndpointSelfManagedCityRequiresForce(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointRuntimeState(t, cityDir, 3311)
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		Self: true,
		Port: "28232",
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doRigSetEndpoint(Self, managed_city, no --force) exit = 0, want non-zero")
	}
	if !strings.Contains(stderr.String(), "--force") {
		t.Errorf("want stderr to mention --force, got:\n%s", stderr.String())
	}
	// Canonical config must not have been mutated.
	state := readRigEndpointConfigState(t, rigDir)
	if state.EndpointOrigin != contract.EndpointOriginInheritedCity {
		t.Fatalf("EndpointOrigin = %q, want unchanged %q", state.EndpointOrigin, contract.EndpointOriginInheritedCity)
	}
}

func TestDoRigSetEndpointSelfWithForceSucceeds(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointRuntimeState(t, cityDir, 3311)
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	rigPortFile := filepath.Join(rigDir, ".beads", "dolt-server.port")
	if err := os.WriteFile(rigPortFile, []byte("3311\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origVerify := verifyRigExternalEndpoint
	defer func() { verifyRigExternalEndpoint = origVerify }()
	verifyRigExternalEndpoint = func(contract.ConfigState, string, string) error { return nil }

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		Self:  true,
		Port:  "28232",
		Force: true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint(Self+Force, managed_city) = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "WARN") {
		t.Errorf("want WARN in stderr, got:\n%s", stderr.String())
	}

	state := readRigEndpointConfigState(t, rigDir)
	if state.EndpointOrigin != contract.EndpointOriginExplicit {
		t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginExplicit)
	}
	if state.DoltHost != "127.0.0.1" {
		t.Fatalf("DoltHost = %q, want 127.0.0.1", state.DoltHost)
	}
	if state.DoltPort != "28232" {
		t.Fatalf("DoltPort = %q, want 28232", state.DoltPort)
	}
	if _, err := os.Stat(rigPortFile); !os.IsNotExist(err) {
		t.Fatalf("rig port file after --self --force: err = %v, want not exist", err)
	}
}

func TestDoRigSetEndpointInheritWritesManagedInheritedRigConfig(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointRuntimeState(t, cityDir, 3311)
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "4406",
		DoltUser:       "rig-user",
	})
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte("4406\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{Inherit: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	state := readRigEndpointConfigState(t, rigDir)
	if state.EndpointOrigin != contract.EndpointOriginInheritedCity {
		t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginInheritedCity)
	}
	if state.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("EndpointStatus = %q, want %q", state.EndpointStatus, contract.EndpointStatusVerified)
	}
	if state.DoltHost != "" || state.DoltPort != "" || state.DoltUser != "" {
		t.Fatalf("managed inherited rig should not track endpoint fields: %+v", state)
	}
	cityCfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cityCfg.Rigs[0].DoltHost; got != "" {
		t.Fatalf("city.toml rig DoltHost = %q, want empty", got)
	}
	if got := cityCfg.Rigs[0].DoltPort; got != "" {
		t.Fatalf("city.toml rig DoltPort = %q, want empty", got)
	}

	data, err := os.ReadFile(filepath.Join(rigDir, ".beads", "dolt-server.port"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "3311" {
		t.Fatalf("port file = %q, want %q", strings.TrimSpace(string(data)), "3311")
	}
}

func TestEnsureCanonicalScopeMetadataIfPresentPreservesExistingManagedProbeDatabase(t *testing.T) {
	scopeDir := t.TempDir()
	metadataPath := filepath.Join(scopeDir, ".beads", "metadata.json")
	if err := os.MkdirAll(filepath.Dir(metadataPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, metadataPath, contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: strings.ToUpper(managedDoltProbeDatabase),
	}); err != nil {
		t.Fatalf("EnsureCanonicalMetadata: %v", err)
	}
	if err := ensureCanonicalScopeMetadataIfPresent(fsys.OSFS{}, scopeDir); err != nil {
		t.Fatalf("ensureCanonicalScopeMetadataIfPresent: %v", err)
	}
	got, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, metadataPath)
	if err != nil {
		t.Fatalf("ReadDoltDatabase: %v", err)
	}
	if !ok || got != strings.ToUpper(managedDoltProbeDatabase) {
		t.Fatalf("dolt_database = %q, ok=%v; want existing reserved name preserved", got, ok)
	}
}

func TestDoRigSetEndpointInheritMirrorsExternalCity(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
		DoltUser:       "city-user",
	})
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte("4406\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{Inherit: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	state := readRigEndpointConfigState(t, rigDir)
	if state.EndpointOrigin != contract.EndpointOriginInheritedCity {
		t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginInheritedCity)
	}
	if state.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("EndpointStatus = %q, want %q", state.EndpointStatus, contract.EndpointStatusVerified)
	}
	if state.DoltHost != "db.example.com" || state.DoltPort != "3307" || state.DoltUser != "city-user" {
		t.Fatalf("state = %+v", state)
	}
	cityCfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	// An inherited rig must not carry the deprecated per-rig dolt_host/dolt_port
	// in city.toml (matching the managed-city inherit path). It inherits the city
	// endpoint through its .beads/config.yaml; a stamped target would churn the
	// rig config back to explicit on every reconcile and can drift into a hard
	// error if the city endpoint changes.
	if got := cityCfg.Rigs[0].DoltHost; got != "" {
		t.Fatalf("city.toml rig DoltHost = %q, want empty", got)
	}
	if got := cityCfg.Rigs[0].DoltPort; got != "" {
		t.Fatalf("city.toml rig DoltPort = %q, want empty", got)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("expected inherited external rig to remove port file, stat err = %v", err)
	}
}

func TestDoRigSetEndpointInheritAcceptsLegacyMinimalConfigs(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointRuntimeState(t, cityDir, 3315)
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{IssuePrefix: "gc"})
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{IssuePrefix: "fe"})

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{Inherit: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	state := readRigEndpointConfigState(t, rigDir)
	if state.EndpointOrigin != contract.EndpointOriginInheritedCity {
		t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginInheritedCity)
	}
	if state.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("EndpointStatus = %q, want %q", state.EndpointStatus, contract.EndpointStatusVerified)
	}
	if state.DoltHost != "" || state.DoltPort != "" || state.DoltUser != "" {
		t.Fatalf("legacy managed inherit should scrub endpoint fields: %+v", state)
	}
}

func TestDoRigSetEndpointInheritUsesCompatExternalCityWhenLocalConfigHasNoEndpointAuthority(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf(`[workspace]
name = "test-city"

[dolt]
host = "db.example.com"
port = 4406

[[rigs]]
name = "frontend"
path = %q
prefix = "fe"
`, rigDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{IssuePrefix: "gc"})

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{Inherit: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	state := readRigEndpointConfigState(t, rigDir)
	if state.EndpointOrigin != contract.EndpointOriginInheritedCity {
		t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginInheritedCity)
	}
	if state.DoltHost != "db.example.com" || state.DoltPort != "4406" {
		t.Fatalf("state = %+v", state)
	}
}

func TestDoRigSetEndpointExternalWritesVerifiedExplicitConfig(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte("3311\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	origVerify := verifyRigExternalEndpoint
	defer func() { verifyRigExternalEndpoint = origVerify }()
	called := false
	verifyRigExternalEndpoint = func(state contract.ConfigState, databaseScopeRoot, authScopeRoot string) error {
		called = true
		if databaseScopeRoot != rigDir {
			t.Fatalf("databaseScopeRoot = %q, want %q", databaseScopeRoot, rigDir)
		}
		if authScopeRoot != rigDir {
			t.Fatalf("authScopeRoot = %q, want %q", authScopeRoot, rigDir)
		}
		if state.DoltHost != "rig-db.example.com" || state.DoltPort != "4406" || state.DoltUser != "rig-user" {
			t.Fatalf("state = %+v", state)
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External: true,
		Host:     "rig-db.example.com",
		Port:     "4406",
		User:     "rig-user",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	if !called {
		t.Fatal("verifyRigExternalEndpoint was not called")
	}

	state := readRigEndpointConfigState(t, rigDir)
	if state.EndpointOrigin != contract.EndpointOriginExplicit {
		t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginExplicit)
	}
	if state.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("EndpointStatus = %q, want %q", state.EndpointStatus, contract.EndpointStatusVerified)
	}
	if state.DoltHost != "rig-db.example.com" || state.DoltPort != "4406" || state.DoltUser != "rig-user" {
		t.Fatalf("state = %+v", state)
	}
	cityCfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cityCfg.Rigs[0].DoltHost; got != "rig-db.example.com" {
		t.Fatalf("city.toml rig DoltHost = %q, want %q", got, "rig-db.example.com")
	}
	if got := cityCfg.Rigs[0].DoltPort; got != "4406" {
		t.Fatalf("city.toml rig DoltPort = %q, want %q", got, "4406")
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("expected explicit external rig to remove port file, stat err = %v", err)
	}
}

func TestDoRigSetEndpointPreservesRelativeRigPathInCityToml(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "test-city"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "old-db.example.com",
		DoltPort:       "3307",
	})

	origVerify := verifyRigExternalEndpoint
	defer func() { verifyRigExternalEndpoint = origVerify }()
	verifyRigExternalEndpoint = func(contract.ConfigState, string, string) error { return nil }

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External: true,
		Host:     "new-db.example.com",
		Port:     "4406",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	rawCfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := rawCfg.Rigs[0].Path; got != "frontend" {
		t.Fatalf("city.toml rig path = %q, want %q", got, "frontend")
	}
	if got := rawCfg.Rigs[0].DoltHost; got != "new-db.example.com" {
		t.Fatalf("city.toml rig DoltHost = %q, want %q", got, "new-db.example.com")
	}
	if got := rawCfg.Rigs[0].DoltPort; got != "4406" {
		t.Fatalf("city.toml rig DoltPort = %q, want %q", got, "4406")
	}
}

func TestDoRigSetEndpointExternalAdoptUnverifiedSkipsValidation(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")

	origVerify := verifyRigExternalEndpoint
	defer func() { verifyRigExternalEndpoint = origVerify }()
	called := false
	verifyRigExternalEndpoint = func(contract.ConfigState, string, string) error {
		called = true
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External:        true,
		Host:            "rig-db.example.com",
		Port:            "4406",
		AdoptUnverified: true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	if called {
		t.Fatal("verifyRigExternalEndpoint should not run for --adopt-unverified")
	}

	state := readRigEndpointConfigState(t, rigDir)
	if state.EndpointStatus != contract.EndpointStatusUnverified {
		t.Fatalf("EndpointStatus = %q, want %q", state.EndpointStatus, contract.EndpointStatusUnverified)
	}
	if !strings.Contains(stdout.String(), "gc rig set-endpoint frontend --external --host rig-db.example.com --port 4406") {
		t.Fatalf("stdout = %q, want revalidation command", stdout.String())
	}
}

func TestDoRigSetEndpointInheritManagedRequiresRuntimePublication(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, cityDir, "hq")

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{Inherit: true}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "managed city endpoint unavailable") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDoRigSetEndpointCanonicalizesExistingMetadata(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"fe","dolt_host":"stale.example.com","dolt_server_port":"3307"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	origVerify := verifyRigExternalEndpoint
	defer func() { verifyRigExternalEndpoint = origVerify }()
	verifyRigExternalEndpoint = func(contract.ConfigState, string, string) error { return nil }

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External: true,
		Host:     "rig-db.example.com",
		Port:     "4406",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(rigDir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "dolt_host") || strings.Contains(text, "dolt_server_port") {
		t.Fatalf("metadata retained deprecated endpoint fields: %s", text)
	}
	doltDatabase, ok, err := contract.ReadDoltDatabase(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok || doltDatabase != "fe" {
		t.Fatalf("metadata lost pinned dolt_database: %s", text)
	}
}

func TestDoRigSetEndpointSupportsExecGcBeadsBdProvider(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, rigDir, "fe")
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityDir))

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{External: true, Host: "rig-db.example.com", Port: "4406", AdoptUnverified: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}
}

func TestDoRigSetEndpointRejectsNonBDProvider(t *testing.T) {
	t.Setenv("GC_BEADS", "file")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{Inherit: true}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "only supported for bd-backed beads providers") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDoRigSetEndpointRejectsInvalidCityCanonicalState(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte("issue_prefix: gc\ngc.endpoint_origin: explicit\ngc.endpoint_status: verified\ndolt.auto-start: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{Inherit: true}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "invalid canonical city endpoint state") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDoRigSetEndpointMetadataFailureDoesNotWriteConfig(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"backend":"dolt"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	origVerify := verifyRigExternalEndpoint
	defer func() { verifyRigExternalEndpoint = origVerify }()
	verifyRigExternalEndpoint = func(contract.ConfigState, string, string) error { return nil }

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External: true,
		Host:     "rig-db.example.com",
		Port:     "4406",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "canonicalizing metadata") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".beads", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config.yaml should not be written on metadata failure, stat err = %v", err)
	}
}

func TestDoRigSetEndpointDryRunDoesNotWriteFilesOrValidate(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "old-db.example.com",
		DoltPort:       "3307",
		DoltUser:       "old-user",
	})
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte("3307\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeConfig := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml"))
	beforeMeta := mustReadFile(t, filepath.Join(rigDir, ".beads", "metadata.json"))

	origVerify := verifyRigExternalEndpoint
	defer func() { verifyRigExternalEndpoint = origVerify }()
	called := false
	verifyRigExternalEndpoint = func(contract.ConfigState, string, string) error {
		called = true
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External: true,
		Host:     "new-db.example.com",
		Port:     "4406",
		DryRun:   true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	if called {
		t.Fatal("verifyRigExternalEndpoint should not run for --dry-run")
	}
	if got := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml")); string(got) != string(beforeConfig) {
		t.Fatalf("config.yaml changed during dry-run:\n%s", got)
	}
	if got := mustReadFile(t, filepath.Join(rigDir, ".beads", "metadata.json")); string(got) != string(beforeMeta) {
		t.Fatalf("metadata.json changed during dry-run:\n%s", got)
	}
	if got := strings.TrimSpace(string(mustReadFile(t, filepath.Join(rigDir, ".beads", "dolt-server.port")))); got != "3307" {
		t.Fatalf("port file = %q, want %q", got, "3307")
	}
}

func TestDoRigSetEndpointExternalValidationFailureDoesNotWriteFiles(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "old-db.example.com",
		DoltPort:       "3307",
		DoltUser:       "old-user",
	})
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte("3307\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeConfig := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml"))
	beforeMeta := mustReadFile(t, filepath.Join(rigDir, ".beads", "metadata.json"))

	origVerify := verifyRigExternalEndpoint
	defer func() { verifyRigExternalEndpoint = origVerify }()
	verifyRigExternalEndpoint = func(contract.ConfigState, string, string) error { return fmt.Errorf("boom") }

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External: true,
		Host:     "new-db.example.com",
		Port:     "4406",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if got := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml")); string(got) != string(beforeConfig) {
		t.Fatalf("config.yaml changed after validation failure:\n%s", got)
	}
	if got := mustReadFile(t, filepath.Join(rigDir, ".beads", "metadata.json")); string(got) != string(beforeMeta) {
		t.Fatalf("metadata.json changed after validation failure:\n%s", got)
	}
	if got := strings.TrimSpace(string(mustReadFile(t, filepath.Join(rigDir, ".beads", "dolt-server.port")))); got != "3307" {
		t.Fatalf("port file = %q, want %q", got, "3307")
	}
}

func TestDoRigSetEndpointInheritManagedUnavailableDoesNotWriteFiles(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "old-db.example.com",
		DoltPort:       "3307",
		DoltUser:       "old-user",
	})
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte("3307\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeConfig := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml"))
	beforeMeta := mustReadFile(t, filepath.Join(rigDir, ".beads", "metadata.json"))

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{Inherit: true}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if got := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml")); string(got) != string(beforeConfig) {
		t.Fatalf("config.yaml changed after managed runtime failure:\n%s", got)
	}
	if got := mustReadFile(t, filepath.Join(rigDir, ".beads", "metadata.json")); string(got) != string(beforeMeta) {
		t.Fatalf("metadata.json changed after managed runtime failure:\n%s", got)
	}
	if got := strings.TrimSpace(string(mustReadFile(t, filepath.Join(rigDir, ".beads", "dolt-server.port")))); got != "3307" {
		t.Fatalf("port file = %q, want %q", got, "3307")
	}
}

func TestDoRigSetEndpointInheritPostgresCityIgnoresStaleManagedRuntime(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "metadata.json"), []byte(`{"database":"beads","backend":"postgres","postgres_host":"db.example.test","postgres_port":"5432","postgres_user":"bd","postgres_database":"beads_pg"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "old-db.example.com",
		DoltPort:       "3307",
		DoltUser:       "old-user",
	})
	writeRigEndpointRuntimeState(t, cityDir, 3311)

	beforeConfig := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml"))
	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{Inherit: true}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if got := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml")); string(got) != string(beforeConfig) {
		t.Fatalf("config.yaml changed after stale postgres managed runtime:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("stale managed port should not be copied to postgres-backed rig, stat err = %v", err)
	}
	if !strings.Contains(stderr.String(), "managed city endpoint unavailable") {
		t.Fatalf("stderr = %q, want managed city endpoint unavailable", stderr.String())
	}
}

func TestReadManagedRuntimePublishedPortRejectsDeadState(t *testing.T) {
	cityDir := t.TempDir()
	runtimeDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	state := doltRuntimeState{
		Running: true,
		PID:     999999,
		Port:    port,
		DataDir: filepath.Join(cityDir, ".beads", "dolt"),
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "dolt-state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	if got, err := readManagedRuntimePublishedPort(cityDir); err == nil {
		t.Fatalf("readManagedRuntimePublishedPort() = %q, want error for dead pid", got)
	}
}

func TestWriteDoltPortFileStrictUsesAtomicWrite(t *testing.T) {
	fs := fsys.NewFake()
	dir := "/city/frontend"
	if err := writeDoltPortFileStrict(fs, dir, "3311"); err != nil {
		t.Fatalf("writeDoltPortFileStrict: %v", err)
	}
	var renamed bool
	for _, call := range fs.Calls {
		if call.Method == "Rename" && strings.HasPrefix(call.Path, filepath.Join(dir, ".beads", "dolt-server.port")+".tmp.") {
			renamed = true
			break
		}
	}
	if !renamed {
		t.Fatalf("fs calls = %+v, want atomic rename", fs.Calls)
	}
	if got := strings.TrimSpace(string(fs.Files[filepath.Join(dir, ".beads", "dolt-server.port")])); got != "3311" {
		t.Fatalf("port file = %q, want %q", got, "3311")
	}
}

func TestWriteDoltPortFileStrictWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(t.TempDir(), "ports")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "dolt-server.port")
	if err := os.WriteFile(target, []byte("3307\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(beadsDir, "dolt-server.port")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := writeDoltPortFileStrict(fsys.OSFS{}, dir, "3311"); err != nil {
		t.Fatalf("writeDoltPortFileStrict: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dolt-server.port symlink was replaced by a %v entry; rewrite must write through the link", info.Mode())
	}
	if got := strings.TrimSpace(string(mustReadFile(t, target))); got != "3311" {
		t.Fatalf("target port = %q, want 3311", got)
	}
}

func TestRemoveDoltPortFileStrictClearsThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	targetDir := filepath.Join(t.TempDir(), "ports")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(targetDir, "dolt-server.port")
	if err := os.WriteFile(target, []byte("3311\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(beadsDir, "dolt-server.port")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := removeDoltPortFileStrict(dir); err != nil {
		t.Fatalf("removeDoltPortFileStrict: %v", err)
	}

	// The operator's link must survive; only the resolved target is cleared.
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("dolt-server.port symlink was replaced by a %v entry; cleanup must clear through the link", info.Mode())
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target stat err = %v, want resolved target removed", err)
	}

	// A later publication must write through the preserved link to the
	// operator's target instead of recreating a regular file at the link path.
	if err := writeDoltPortFileStrict(fsys.OSFS{}, dir, "3320"); err != nil {
		t.Fatalf("re-publish writeDoltPortFileStrict: %v", err)
	}
	info, err = os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link after re-publish: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("after re-publish, link mode = %v; want symlink preserved", info.Mode())
	}
	if got := strings.TrimSpace(string(mustReadFile(t, target))); got != "3320" {
		t.Fatalf("re-published target port = %q, want 3320", got)
	}
}

func TestSyncRigEndpointCompatConfigUsesAtomicWrite(t *testing.T) {
	fs := fsys.NewFake()
	cityDir := "/city"
	fs.Dirs[cityDir] = true
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Rigs: []config.Rig{{Name: "frontend", Path: "/city/frontend", Prefix: "fe", DoltHost: "old-db.example.com", DoltPort: "3307"}}}
	if err := syncRigEndpointCompatConfig(fs, cityDir, cfg, "frontend", contract.ConfigState{DoltHost: "new-db.example.com", DoltPort: "4406"}); err != nil {
		t.Fatalf("syncRigEndpointCompatConfig: %v", err)
	}
	var renamed bool
	for _, call := range fs.Calls {
		if call.Method == "Rename" && strings.HasPrefix(call.Path, filepath.Join(cityDir, "city.toml")+".tmp.") {
			renamed = true
			break
		}
	}
	if !renamed {
		t.Fatalf("fs calls = %+v, want atomic rename", fs.Calls)
	}
	if got := string(fs.Files[filepath.Join(cityDir, "city.toml")]); !strings.Contains(got, "new-db.example.com") || !strings.Contains(got, "4406") {
		t.Fatalf("city.toml = %q", got)
	}
}

// setupSymlinkedCityToml creates cityDir/city.toml as a symlink into a
// checkout directory holding the original content, mirroring the ga-lurp5d
// production layout where city.toml links into a checked-out repo.
const symlinkedCityTomlOriginal = "[workspace]\nname = \"test-city\"\n"

func setupSymlinkedCityToml(t *testing.T) (cityDir, link, target string) {
	t.Helper()
	dir := t.TempDir()
	checkoutDir := filepath.Join(dir, "checkout")
	if err := os.MkdirAll(checkoutDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(checkoutDir, "city.toml")
	if err := os.WriteFile(target, []byte(symlinkedCityTomlOriginal), 0o644); err != nil {
		t.Fatal(err)
	}
	cityDir = filepath.Join(dir, "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(cityDir, "city.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	return cityDir, link, target
}

// assertCityTomlSymlinkRestored fails the test unless link is still a symlink
// and target holds the original content after a rollback restore.
func assertCityTomlSymlinkRestored(t *testing.T, link, target, original string) {
	t.Helper()
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("city.toml symlink was replaced by a %v entry; rollback must restore through the link", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != original {
		t.Fatalf("target content = %q, want restored original %q", data, original)
	}
}

// Regression test for the ga-lurp5d follow-up: a failed endpoint change must
// roll back a symlinked city.toml by restoring the link target, not by
// replacing the link with a regular file.
func TestRigEndpointRollbackRestoresThroughCityTomlSymlink(t *testing.T) {
	fs := fsys.OSFS{}
	cityDir, link, target := setupSymlinkedCityToml(t)
	scopeRoot := filepath.Join(cityDir, "rig")
	if err := os.MkdirAll(scopeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	snapshots, err := snapshotRigEndpointFiles(fs, cityDir, scopeRoot)
	if err != nil {
		t.Fatalf("snapshotRigEndpointFiles: %v", err)
	}
	if err := os.WriteFile(target, []byte("[workspace]\nname = \"mutated\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreSnapshots(fs, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}
	assertCityTomlSymlinkRestored(t, link, target, symlinkedCityTomlOriginal)
}

// Regression for the attempt-3 review's consistency finding: cmd-side
// rollback snapshots must resolve every captured file the way the API-side
// capture does, not just city.toml — restoring a symlinked .gc/site.toml
// must write the link target, not replace the link with a regular file.
func TestRigEndpointRollbackRestoresThroughSiteTomlSymlink(t *testing.T) {
	fs := fsys.OSFS{}
	original := "name = \"bound-site\"\n"
	cityDir, _, _ := setupSymlinkedCityToml(t)
	checkout := filepath.Join(cityDir, "site-checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(checkout, "site.toml")
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	link := config.SiteBindingPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	scopeRoot := filepath.Join(cityDir, "rig")
	if err := os.MkdirAll(scopeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	snapshots, err := snapshotRigEndpointFiles(fs, cityDir, scopeRoot)
	if err != nil {
		t.Fatalf("snapshotRigEndpointFiles: %v", err)
	}
	if err := os.WriteFile(target, []byte("name = \"mutated\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreSnapshots(fs, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("site.toml symlink was replaced by a %v entry; rollback must restore through the link", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != original {
		t.Fatalf("target content = %q, want restored original %q", data, original)
	}
}

func TestRigEndpointRollbackRestoresAfterSiteBindingForwardWriteThroughSymlink(t *testing.T) {
	fs := fsys.OSFS{}
	original := "workspace_name = \"bound-site\"\nworkspace_prefix = \"bs\"\n"
	cityDir, _, _ := setupSymlinkedCityToml(t)
	checkout := filepath.Join(cityDir, "site-checkout")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(checkout, "site.toml")
	if err := os.WriteFile(target, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	link := config.SiteBindingPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	scopeRoot := filepath.Join(cityDir, "rig")
	if err := os.MkdirAll(scopeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	snapshots, err := snapshotRigEndpointFiles(fs, cityDir, scopeRoot)
	if err != nil {
		t.Fatalf("snapshotRigEndpointFiles: %v", err)
	}
	if err := config.PersistWorkspaceSiteBinding(fs, cityDir, "mutated-site", "ms"); err != nil {
		t.Fatalf("PersistWorkspaceSiteBinding: %v", err)
	}
	if err := restoreSnapshots(fs, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("site.toml symlink was replaced by a %v entry; rollback must restore the effective linked state", info.Mode())
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile target: %v", err)
	}
	if string(data) != original {
		t.Fatalf("target content = %q, want restored original %q", data, original)
	}
}

func TestDoRigSetEndpointExternalPreservesExistingUserWhenUserFlagOmitted(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "old-db.example.com",
		DoltPort:       "3307",
		DoltUser:       "rig-user",
	})

	origVerify := verifyRigExternalEndpoint
	defer func() { verifyRigExternalEndpoint = origVerify }()
	verifyRigExternalEndpoint = func(state contract.ConfigState, databaseScopeRoot, authScopeRoot string) error {
		if databaseScopeRoot != rigDir {
			t.Fatalf("databaseScopeRoot = %q, want %q", databaseScopeRoot, rigDir)
		}
		if authScopeRoot != rigDir {
			t.Fatalf("authScopeRoot = %q, want %q", authScopeRoot, rigDir)
		}
		if state.DoltUser != "rig-user" {
			t.Fatalf("state.DoltUser = %q, want %q", state.DoltUser, "rig-user")
		}
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External:        true,
		Host:            "new-db.example.com",
		Port:            "4406",
		AdoptUnverified: true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	if got := readRigEndpointConfigState(t, rigDir).DoltUser; got != "rig-user" {
		t.Fatalf("DoltUser = %q, want %q", got, "rig-user")
	}
}

func TestDoRigSetEndpointCompatCityTomlFailureRollsBackCanonicalFiles(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "old-db.example.com",
		DoltPort:       "3307",
	})
	cityTomlPath := filepath.Join(cityDir, "city.toml")
	if err := os.Chmod(cityDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(cityDir, 0o755) }()

	beforeCity := mustReadFile(t, cityTomlPath)
	beforeMeta := mustReadFile(t, filepath.Join(rigDir, ".beads", "metadata.json"))
	beforeConfig := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml"))

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External:        true,
		Host:            "new-db.example.com",
		Port:            "4406",
		AdoptUnverified: true,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "syncing compat city config") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if got := mustReadFile(t, cityTomlPath); string(got) != string(beforeCity) {
		t.Fatalf("city.toml changed after compat write failure:\n%s", got)
	}
	if got := mustReadFile(t, filepath.Join(rigDir, ".beads", "metadata.json")); string(got) != string(beforeMeta) {
		t.Fatalf("metadata rollback failed:\n%s", got)
	}
	if got := mustReadFile(t, filepath.Join(rigDir, ".beads", "config.yaml")); string(got) != string(beforeConfig) {
		t.Fatalf("config rollback failed:\n%s", got)
	}
}

func TestDoRigSetEndpointRequiresCanonicalMetadata(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External:        true,
		Host:            "rig-db.example.com",
		Port:            "4406",
		AdoptUnverified: true,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "missing canonical metadata") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".beads", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config.yaml should not be written without metadata, stat err = %v", err)
	}
}

func TestDoRigSetEndpointConfigFailureRollsBackMetadata(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	metadataPath := filepath.Join(rigDir, ".beads", "metadata.json")
	if err := os.WriteFile(metadataPath, []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"fe","dolt_host":"stale.example.com"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(rigDir, ".beads", "config.yaml")
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "old-db.example.com",
		DoltPort:       "3307",
		DoltUser:       "old-user",
	})
	beforeMeta := mustReadFile(t, metadataPath)
	beforeConfig := mustReadFile(t, configPath)
	if err := os.Chmod(configPath, 0o444); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(configPath, 0o644) }()

	origVerify := verifyRigExternalEndpoint
	defer func() { verifyRigExternalEndpoint = origVerify }()
	verifyRigExternalEndpoint = func(contract.ConfigState, string, string) error { return nil }

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
		External: true,
		Host:     "new-db.example.com",
		Port:     "4406",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if got := mustReadFile(t, metadataPath); string(got) != string(beforeMeta) {
		t.Fatalf("metadata rollback failed:\n%s", got)
	}
	if got := mustReadFile(t, configPath); string(got) != string(beforeConfig) {
		t.Fatalf("config rollback failed:\n%s", got)
	}
}

func TestDoRigSetEndpointPortArtifactFailureRollsBackCanonicalFiles(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointCityConfig(t, cityDir, rigDir)
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointRuntimeState(t, cityDir, 3317)
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "old-db.example.com",
		DoltPort:       "3307",
		DoltUser:       "old-user",
	})
	metadataPath := filepath.Join(rigDir, ".beads", "metadata.json")
	configPath := filepath.Join(rigDir, ".beads", "config.yaml")
	beforeMeta := mustReadFile(t, metadataPath)
	beforeConfig := mustReadFile(t, configPath)
	portPath := filepath.Join(rigDir, ".beads", "dolt-server.port")
	if err := os.MkdirAll(portPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{Inherit: true}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSetEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "syncing managed port artifact") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if got := mustReadFile(t, metadataPath); string(got) != string(beforeMeta) {
		t.Fatalf("metadata rollback failed after port artifact error:\n%s", got)
	}
	if got := mustReadFile(t, configPath); string(got) != string(beforeConfig) {
		t.Fatalf("config rollback failed after port artifact error:\n%s", got)
	}
	info, err := os.Stat(portPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("port artifact should remain a directory after failure")
	}
}

func TestCanonicalValidationPasswordUsesCredentialsFileOverride(t *testing.T) {
	scopeRoot := t.TempDir()
	credentialsPath := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(credentialsPath, []byte("[db.example.com:3307]\npassword=credentials-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEADS_CREDENTIALS_FILE", credentialsPath)

	if got := canonicalValidationPassword("db.example.com", "3307", scopeRoot); got != "credentials-secret" {
		t.Fatalf("canonicalValidationPassword() = %q, want %q", got, "credentials-secret")
	}
}

func TestReadCanonicalProjectIDReadsL1Authoritatively(t *testing.T) {
	scopeRoot := t.TempDir()
	writeRigEndpointMetadata(t, scopeRoot, "hq")
	metadataPath := filepath.Join(scopeRoot, ".beads", "metadata.json")
	writeMetadataProjectID(t, metadataPath, "legacy-l2")
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, scopeRoot, "canonical-l1"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}

	got, err := readCanonicalProjectID(metadataPath)
	if err != nil {
		t.Fatalf("readCanonicalProjectID: %v", err)
	}
	if got != "canonical-l1" {
		t.Fatalf("readCanonicalProjectID() = %q, want canonical-l1", got)
	}
}

func TestReadCanonicalProjectIDFallsBackToL2WhenL1Absent(t *testing.T) {
	scopeRoot := t.TempDir()
	writeRigEndpointMetadata(t, scopeRoot, "hq")
	metadataPath := filepath.Join(scopeRoot, ".beads", "metadata.json")
	writeMetadataProjectID(t, metadataPath, "legacy-l2")

	got, err := readCanonicalProjectID(metadataPath)
	if err != nil {
		t.Fatalf("readCanonicalProjectID: %v", err)
	}
	if got != "legacy-l2" {
		t.Fatalf("readCanonicalProjectID() = %q, want legacy-l2", got)
	}
}

func TestReadCanonicalProjectIDReturnsEmptyWhenL1AndL2Missing(t *testing.T) {
	scopeRoot := t.TempDir()
	writeRigEndpointMetadata(t, scopeRoot, "hq")
	metadataPath := filepath.Join(scopeRoot, ".beads", "metadata.json")

	got, err := readCanonicalProjectID(metadataPath)
	if err != nil {
		t.Fatalf("readCanonicalProjectID: %v", err)
	}
	if got != "" {
		t.Fatalf("readCanonicalProjectID() = %q, want empty", got)
	}
}

func TestValidateExternalDoltIssuesTableScanMapsScanResults(t *testing.T) {
	queryErr := errors.New("query failed")
	wrappedNoRows := fmt.Errorf("query context: %w", sql.ErrNoRows)
	tests := []struct {
		name      string
		scanErr   error
		wantError string
		wantCause error
	}{
		{name: "issues table exists"},
		{
			name:      "issues table missing",
			scanErr:   sql.ErrNoRows,
			wantError: `beads store not usable on external endpoint: database "hq" is missing the issues table`,
		},
		{
			name:      "wrapped no rows remains query failure",
			scanErr:   wrappedNoRows,
			wantError: "beads store not usable on external endpoint: query context: sql: no rows in result set",
			wantCause: wrappedNoRows,
		},
		{
			name:      "query fails",
			scanErr:   queryErr,
			wantError: "beads store not usable on external endpoint: query failed",
			wantCause: queryErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateExternalDoltIssuesTableScan("  hq  ", tt.scanErr)
			if tt.wantError == "" {
				if err != nil {
					t.Fatalf("validateExternalDoltIssuesTableScan() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateExternalDoltIssuesTableScan() error = nil, want %q", tt.wantError)
			}
			if got := err.Error(); got != tt.wantError {
				t.Fatalf("validateExternalDoltIssuesTableScan() error = %q, want %q", got, tt.wantError)
			}
			if tt.wantCause != nil && !errors.Is(err, tt.wantCause) {
				t.Fatalf("validateExternalDoltIssuesTableScan() error = %v, want wrapped cause %v", err, tt.wantCause)
			}
		})
	}
}

func TestVerifyExternalDoltEndpointRejectsProjectIdentityMismatch(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage")
	cityDir := t.TempDir()
	originalProjectID := "external-project-id"
	writeProjectIDMetadataFile(t, cityDir, originalProjectID)
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, cityDir, "different-project-id"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	setup := append(
		[]string{"CREATE TABLE IF NOT EXISTS issues (id VARCHAR(255) PRIMARY KEY)"},
		seedDatabaseProjectIDQueries(originalProjectID)...,
	)
	port, cleanup := startProjectIDTestServer(t, setup...)
	defer cleanup()
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	state := contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "127.0.0.1",
		DoltPort:       port,
		DoltUser:       "root",
	}
	err := verifyExternalDoltEndpoint(state, cityDir, cityDir)
	if err == nil {
		t.Fatal("verifyExternalDoltEndpoint() unexpectedly succeeded for project_id mismatch")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "project identity mismatch") {
		t.Fatalf("verifyExternalDoltEndpoint() error = %v", err)
	}
	if !strings.Contains(err.Error(), "different-project-id") || !strings.Contains(err.Error(), originalProjectID) {
		t.Fatalf("verifyExternalDoltEndpoint() error = %v, want both project ids", err)
	}
}

func TestVerifyExternalDoltEndpointRejectsMissingLocalProjectID(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage")
	cityDir := t.TempDir()
	writeProjectIDMetadataFile(t, cityDir, "")
	setup := append(
		[]string{"CREATE TABLE IF NOT EXISTS issues (id VARCHAR(255) PRIMARY KEY)"},
		seedDatabaseProjectIDQueries("external-project-id")...,
	)
	port, cleanup := startProjectIDTestServer(t, setup...)
	defer cleanup()
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	state := contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "127.0.0.1",
		DoltPort:       port,
		DoltUser:       "root",
	}
	err := verifyExternalDoltEndpoint(state, cityDir, cityDir)
	if err == nil {
		t.Fatal("verifyExternalDoltEndpoint() unexpectedly succeeded for missing local project_id")
	}
	if !strings.Contains(err.Error(), "neither .beads/identity.toml nor .beads/metadata.json carry a project_id") {
		t.Fatalf("verifyExternalDoltEndpoint() error = %v", err)
	}
}

func TestDoRigSetEndpointAllowsBdRigUnderFileBackedCity(t *testing.T) {
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[[rigs]]\nname = \"frontend\"\npath = %q\nprefix = \"fe\"\n", rigDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointMetadata(t, rigDir, "fe")
	writeRigEndpointRuntimeState(t, cityDir, 3311)
	writeRigEndpointCanonicalConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "4406",
		DoltUser:       "rig-user",
	})

	var stdout, stderr bytes.Buffer
	code := doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{Inherit: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSetEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	state := readRigEndpointConfigState(t, rigDir)
	if state.EndpointOrigin != contract.EndpointOriginInheritedCity {
		t.Fatalf("EndpointOrigin = %q, want %q", state.EndpointOrigin, contract.EndpointOriginInheritedCity)
	}
}

func writeRigEndpointCityConfig(t *testing.T, cityDir, rigDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := config.PersistWorkspaceSiteBinding(fsys.OSFS{}, cityDir, "test-city", ""); err != nil {
		t.Fatalf("PersistWorkspaceSiteBinding: %v", err)
	}
	if err := config.PersistRigSiteBindings(fsys.OSFS{}, cityDir, []config.Rig{{Name: "frontend", Path: rigDir}}); err != nil {
		t.Fatalf("PersistRigSiteBindings: %v", err)
	}
	content := "[workspace]\n\n[[rigs]]\nname = \"frontend\"\nprefix = \"fe\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRigEndpointCanonicalConfig(t *testing.T, dir string, state contract.ConfigState) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, filepath.Join(dir, ".beads", "config.yaml"), state); err != nil {
		t.Fatal(err)
	}
}

func writeRigEndpointRuntimeState(t *testing.T, cityDir string, port int) {
	t.Helper()
	runtimeDir := filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := fmt.Sprintf(`{"running":true,"port":%d}`, port)
	if err := os.WriteFile(filepath.Join(runtimeDir, "dolt-state.json"), []byte(payload), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeRigEndpointMetadata(t *testing.T, dir, doltDatabase string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(dir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: doltDatabase,
	}); err != nil {
		t.Fatal(err)
	}
}

func writeMetadataProjectID(t *testing.T, metadataPath string, projectID string) {
	t.Helper()
	data := mustReadFile(t, metadataPath)
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	meta["project_id"] = projectID
	encoded, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(metadataPath, encoded, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func readRigEndpointConfigState(t *testing.T, dir string) contract.ConfigState {
	t.Helper()
	state, ok, err := contract.ReadConfigState(fsys.OSFS{}, filepath.Join(dir, ".beads", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("config state missing")
	}
	return state
}

// Regression for PR #3428 review: the gc rig set-endpoint outer rollback
// snapshots city.toml at its symlink-resolved path, so restoring after a later
// step fails rewrites the real target and leaves the live symlink intact
// instead of replacing it with a regular file.
func TestSnapshotRigEndpointFilesRestoresSymlinkedCityToml(t *testing.T) {
	dir := t.TempDir()
	repoDir := filepath.Join(dir, "repo")
	cityDir := filepath.Join(dir, "city")
	scopeRoot := filepath.Join(dir, "rig")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scopeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	repoCityPath := filepath.Join(repoDir, "city.toml")
	liveCityPath := filepath.Join(cityDir, "city.toml")
	original := []byte("[workspace]\nname = \"test-city\"\n")
	if err := os.WriteFile(repoCityPath, original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "repo", "city.toml"), liveCityPath); err != nil {
		t.Fatal(err)
	}

	fs := fsys.OSFS{}
	snapshots, err := snapshotRigEndpointFiles(fs, cityDir, scopeRoot)
	if err != nil {
		t.Fatalf("snapshotRigEndpointFiles: %v", err)
	}

	// Simulate the endpoint config rewrite mutating the resolved target before
	// a later step fails and triggers rollback.
	mutated := []byte("[workspace]\nname = \"mutated\"\n")
	if err := os.WriteFile(repoCityPath, mutated, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := restoreSnapshots(fs, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}

	info, err := os.Lstat(liveCityPath)
	if err != nil {
		t.Fatalf("Lstat(live city.toml): %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("rollback replaced the live city.toml symlink with a regular file")
	}
	restored, err := os.ReadFile(repoCityPath)
	if err != nil {
		t.Fatalf("read repo city.toml: %v", err)
	}
	if !bytes.Equal(restored, original) {
		t.Fatalf("repo city.toml = %q, want restored original %q", restored, original)
	}
}
