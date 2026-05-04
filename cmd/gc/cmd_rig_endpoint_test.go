package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

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
	if got := cityCfg.Rigs[0].DoltHost; got != "db.example.com" {
		t.Fatalf("city.toml rig DoltHost = %q, want %q", got, "db.example.com")
	}
	if got := cityCfg.Rigs[0].DoltPort; got != "3307" {
		t.Fatalf("city.toml rig DoltPort = %q, want %q", got, "3307")
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

func TestSyncRigEndpointCompatConfigUsesAtomicWrite(t *testing.T) {
	fs := fsys.NewFake()
	cityDir := "/city"
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

func TestRestoreSnapshotUsesAtomicWrite(t *testing.T) {
	fs := fsys.NewFake()
	snap := fileSnapshot{path: "/city/city.toml", data: []byte("updated = true\n"), exists: true}
	if err := restoreSnapshot(fs, snap); err != nil {
		t.Fatalf("restoreSnapshot: %v", err)
	}
	var renamed bool
	for _, call := range fs.Calls {
		if call.Method == "Rename" && strings.HasPrefix(call.Path, snap.path+".tmp.") {
			renamed = true
			break
		}
	}
	if !renamed {
		t.Fatalf("fs calls = %+v, want atomic rename", fs.Calls)
	}
	if got := string(fs.Files[snap.path]); got != "updated = true\n" {
		t.Fatalf("restored file = %q", got)
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

func TestVerifyExternalDoltEndpointRejectsEmptyExternalDoltDatabase(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage")
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := MaterializeBuiltinPacks(cityDir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}
	script := gcBeadsBdScriptPath(cityDir)

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(homeDir, ".gitconfig")
	if err := os.WriteFile(gitConfig, []byte("[user]\n\tname = Test User\n\temail = test@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	poisonRuntimeDir := filepath.Join(t.TempDir(), "poison-runtime")
	poisonPackStateDir := filepath.Join(poisonRuntimeDir, "packs", "dolt")
	poisonStateFile := filepath.Join(poisonPackStateDir, "dolt-provider-state.json")
	t.Setenv("GC_CITY_RUNTIME_DIR", poisonRuntimeDir)
	t.Setenv("GC_PACK_STATE_DIR", poisonPackStateDir)
	t.Setenv("GC_DOLT_STATE_FILE", poisonStateFile)

	scriptEnv := sanitizedBaseEnv(
		"HOME="+homeDir,
		"GIT_CONFIG_GLOBAL="+gitConfig,
		"GC_CITY_PATH="+cityDir,
		"PATH="+strings.Join([]string{"/home/ubuntu/.local/bin", filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)),
	)

	runScript := func(args ...string) {
		t.Helper()
		cmd := exec.Command(script, args...)
		cmd.Env = scriptEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	t.Cleanup(func() {
		cmd := exec.Command(script, "stop")
		cmd.Env = scriptEnv
		_ = cmd.Run()
	})

	runScript("start")
	if _, err := os.Stat(poisonStateFile); !os.IsNotExist(err) {
		t.Fatalf("start leaked ambient GC_* state to %q, stat err = %v", poisonStateFile, err)
	}
	if err := publishManagedDoltRuntimeState(cityDir); err != nil {
		t.Fatalf("publishManagedDoltRuntimeState: %v", err)
	}

	port, err := readManagedRuntimePublishedPort(cityDir)
	if err != nil {
		t.Fatalf("readManagedRuntimePublishedPort: %v", err)
	}

	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%s)/", port))
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS `hq`"); err != nil {
		t.Fatalf("create hq database: %v", err)
	}
	writeRigEndpointMetadata(t, cityDir, "hq")

	state := contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "127.0.0.1",
		DoltPort:       port,
		DoltUser:       "root",
	}
	err = verifyExternalDoltEndpoint(state, cityDir, cityDir)
	if err == nil {
		t.Fatal("verifyExternalDoltEndpoint() unexpectedly succeeded for empty Dolt database")
	}
	if !strings.Contains(err.Error(), "beads store not usable on external endpoint") {
		t.Fatalf("verifyExternalDoltEndpoint() error = %v", err)
	}
}

func TestVerifyExternalDoltEndpointRejectsProjectIdentityMismatch(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage")
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	bdPath := waitTestRealBDPath(t)
	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return currentGCBinaryForTests(t) }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeBuiltinPacks(cityDir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(homeDir, ".gitconfig")
	if err := os.WriteFile(gitConfig, []byte("[user]\n\tname = Test User\n\temail = test@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)))

	if err := ensureBeadsProvider(cityDir); err != nil {
		t.Fatalf("ensureBeadsProvider: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdownBeadsProvider(cityDir)
	})
	if err := initAndHookDir(cityDir, cityDir, "gc"); err != nil {
		t.Fatalf("initAndHookDir(city): %v", err)
	}
	if err := publishManagedDoltRuntimeState(cityDir); err != nil {
		t.Fatalf("publishManagedDoltRuntimeState: %v", err)
	}

	port, err := readManagedRuntimePublishedPort(cityDir)
	if err != nil {
		t.Fatalf("readManagedRuntimePublishedPort: %v", err)
	}

	metadataPath := filepath.Join(cityDir, ".beads", "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("ReadFile(metadata.json): %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("Unmarshal(metadata.json): %v", err)
	}
	originalProjectID := strings.TrimSpace(fmt.Sprint(meta["project_id"]))
	if originalProjectID == "" {
		t.Fatal("metadata project_id not populated")
	}
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%s)/hq", port))
	if err != nil {
		t.Fatalf("sql.Open(hq): %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "INSERT INTO metadata (`key`, value) VALUES ('_project_id', ?) ON DUPLICATE KEY UPDATE value = VALUES(value)", originalProjectID); err != nil {
		t.Fatalf("seed database _project_id: %v", err)
	}
	meta["project_id"] = "different-project-id"
	patched, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(metadata.json): %v", err)
	}
	patched = append(patched, '\n')
	if err := os.WriteFile(metadataPath, patched, 0o644); err != nil {
		t.Fatalf("WriteFile(metadata.json): %v", err)
	}

	state := contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "127.0.0.1",
		DoltPort:       port,
		DoltUser:       "root",
	}
	err = verifyExternalDoltEndpoint(state, cityDir, cityDir)
	if err == nil {
		t.Fatal("verifyExternalDoltEndpoint() unexpectedly succeeded for project_id mismatch")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "project identity mismatch") {
		t.Fatalf("verifyExternalDoltEndpoint() error = %v", err)
	}
}

func TestVerifyExternalDoltEndpointRejectsMissingLocalProjectID(t *testing.T) {
	skipSlowCmdGCTest(t, "requires a managed external dolt endpoint; run make test-cmd-gc-process for full coverage")
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	bdPath := waitTestRealBDPath(t)
	oldResolve := resolveProviderLifecycleGCBinary
	resolveProviderLifecycleGCBinary = func() string { return currentGCBinaryForTests(t) }
	t.Cleanup(func() { resolveProviderLifecycleGCBinary = oldResolve })

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := MaterializeBuiltinPacks(cityDir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks: %v", err)
	}

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitConfig := filepath.Join(homeDir, ".gitconfig")
	if err := os.WriteFile(gitConfig, []byte("[user]\n\tname = Test User\n\temail = test@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GIT_CONFIG_GLOBAL", gitConfig)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	t.Setenv("PATH", strings.Join([]string{filepath.Dir(bdPath), filepath.Dir(doltPath), os.Getenv("PATH")}, string(os.PathListSeparator)))

	if err := ensureBeadsProvider(cityDir); err != nil {
		t.Fatalf("ensureBeadsProvider: %v", err)
	}
	t.Cleanup(func() {
		_ = shutdownBeadsProvider(cityDir)
	})
	if err := initAndHookDir(cityDir, cityDir, "gc"); err != nil {
		t.Fatalf("initAndHookDir(city): %v", err)
	}
	if err := publishManagedDoltRuntimeState(cityDir); err != nil {
		t.Fatalf("publishManagedDoltRuntimeState: %v", err)
	}

	port, err := readManagedRuntimePublishedPort(cityDir)
	if err != nil {
		t.Fatalf("readManagedRuntimePublishedPort: %v", err)
	}

	metadataPath := filepath.Join(cityDir, ".beads", "metadata.json")
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatalf("ReadFile(metadata.json): %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatalf("Unmarshal(metadata.json): %v", err)
	}
	delete(meta, "project_id")
	patched, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent(metadata.json): %v", err)
	}
	patched = append(patched, '\n')
	if err := os.WriteFile(metadataPath, patched, 0o644); err != nil {
		t.Fatalf("WriteFile(metadata.json): %v", err)
	}

	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%s)/hq", port))
	if err != nil {
		t.Fatalf("sql.Open(hq): %v", err)
	}
	defer func() { _ = db.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, "INSERT INTO metadata (`key`, value) VALUES ('_project_id', ?) ON DUPLICATE KEY UPDATE value = VALUES(value)", "external-project-id"); err != nil {
		t.Fatalf("seed database _project_id: %v", err)
	}

	state := contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "127.0.0.1",
		DoltPort:       port,
		DoltUser:       "root",
	}
	err = verifyExternalDoltEndpoint(state, cityDir, cityDir)
	if err == nil {
		t.Fatal("verifyExternalDoltEndpoint() unexpectedly succeeded for missing local project_id")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "missing project_id") {
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
	content := fmt.Sprintf("[workspace]\nname = \"test-city\"\n\n[[rigs]]\nname = \"frontend\"\npath = %q\nprefix = \"fe\"\n", rigDir)
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
