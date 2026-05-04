package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestNewBeadsCmdIncludesCityEndpointSubcommands(t *testing.T) {
	cmd := newBeadsCmd(&bytes.Buffer{}, &bytes.Buffer{})
	city, _, err := cmd.Find([]string{"city"})
	if err != nil {
		t.Fatalf("Find(city): %v", err)
	}
	if city == nil || city.Name() != "city" {
		t.Fatalf("city command = %#v", city)
	}
	useManaged, _, err := cmd.Find([]string{"city", "use-managed"})
	if err != nil {
		t.Fatalf("Find(city use-managed): %v", err)
	}
	if useManaged == nil || useManaged.Name() != "use-managed" {
		t.Fatalf("use-managed command = %#v", useManaged)
	}
	useExternal, _, err := cmd.Find([]string{"city", "use-external"})
	if err != nil {
		t.Fatalf("Find(city use-external): %v", err)
	}
	if useExternal == nil || useExternal.Name() != "use-external" {
		t.Fatalf("use-external command = %#v", useExternal)
	}
}

func TestDoBeadsCityEndpointRejectsGCBeadsFileOverride(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", AdoptUnverified: true}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doBeadsCityEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "only supported for bd-backed beads providers") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestValidateCityEndpointOptionsRejectsWildcardExternalHost(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::"} {
		t.Run(host, func(t *testing.T) {
			err := validateCityEndpointOptions(cityEndpointOptions{External: true, Host: host, Port: "4406"})
			if err == nil || !strings.Contains(err.Error(), "invalid --host") {
				t.Fatalf("validateCityEndpointOptions(%q) error = %v", host, err)
			}
		})
	}
}

func TestDoBeadsCityEndpointSupportsExecGcBeadsBdProvider(t *testing.T) {
	cityDir := t.TempDir()
	inheritDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(inheritDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{}, []config.Rig{{Name: "frontend", Path: inheritDir, Prefix: "fe"}})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, inheritDir, "fe")
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityDir))

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", AdoptUnverified: true, DryRun: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}
}

func TestDoBeadsCityUseExternalWritesVerifiedCityAndInheritedRigs(t *testing.T) {
	skipSlowCmdGCTest(t, "exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage")
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	inheritDir := filepath.Join(t.TempDir(), "frontend")
	explicitDir := filepath.Join(t.TempDir(), "ops")
	if err := os.MkdirAll(inheritDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(explicitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{Host: "stale-city.example.com", Port: 3306}, []config.Rig{{Name: "frontend", Path: inheritDir, Prefix: "fe", DoltHost: "stale-frontend.example.com", DoltPort: "3306"}, {Name: "ops", Path: explicitDir, Prefix: "ops", DoltHost: "ops-db.example.com", DoltPort: "5501"}})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, inheritDir, "fe")
	writeRigEndpointMetadata(t, explicitDir, "ops")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity, EndpointStatus: contract.EndpointStatusVerified})
	writeRigEndpointCanonicalConfig(t, inheritDir, contract.ConfigState{IssuePrefix: "fe", EndpointOrigin: contract.EndpointOriginInheritedCity, EndpointStatus: contract.EndpointStatusVerified})
	writeRigEndpointCanonicalConfig(t, explicitDir, contract.ConfigState{IssuePrefix: "ops", EndpointOrigin: contract.EndpointOriginExplicit, EndpointStatus: contract.EndpointStatusVerified, DoltHost: "ops-db.example.com", DoltPort: "5501", DoltUser: "ops-user"})
	for _, dir := range []string{cityDir, inheritDir, explicitDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "dolt-server.port"), []byte("3311\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	origVerify := verifyCityExternalEndpoint
	defer func() { verifyCityExternalEndpoint = origVerify }()
	type verifyCall struct {
		state             contract.ConfigState
		databaseScopeRoot string
		authScopeRoot     string
	}
	var calls []verifyCall
	verifyCityExternalEndpoint = func(state contract.ConfigState, databaseScopeRoot, authScopeRoot string) error {
		calls = append(calls, verifyCall{state: state, databaseScopeRoot: databaseScopeRoot, authScopeRoot: authScopeRoot})
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", User: "city-user"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	if len(calls) != 2 {
		t.Fatalf("verifyCityExternalEndpoint calls = %d, want 2", len(calls))
	}
	if calls[0].databaseScopeRoot != cityDir || calls[0].authScopeRoot != cityDir {
		t.Fatalf("city verify roots = (%q, %q), want (%q, %q)", calls[0].databaseScopeRoot, calls[0].authScopeRoot, cityDir, cityDir)
	}
	if calls[1].databaseScopeRoot != inheritDir || calls[1].authScopeRoot != cityDir {
		t.Fatalf("inherited rig verify roots = (%q, %q), want (%q, %q)", calls[1].databaseScopeRoot, calls[1].authScopeRoot, inheritDir, cityDir)
	}

	cityState := readRigEndpointConfigState(t, cityDir)
	if cityState.EndpointOrigin != contract.EndpointOriginCityCanonical || cityState.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("city state = %+v", cityState)
	}
	if cityState.DoltHost != "db.example.com" || cityState.DoltPort != "4406" || cityState.DoltUser != "city-user" {
		t.Fatalf("city state = %+v", cityState)
	}
	cityToml := readCityEndpointToml(t, cityDir)
	if cityToml.Dolt.Host != "db.example.com" || cityToml.Dolt.Port != 4406 {
		t.Fatalf("city toml = %+v", cityToml.Dolt)
	}

	inheritState := readRigEndpointConfigState(t, inheritDir)
	if inheritState.EndpointOrigin != contract.EndpointOriginInheritedCity || inheritState.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("inherit state = %+v", inheritState)
	}
	if inheritState.DoltHost != "db.example.com" || inheritState.DoltPort != "4406" || inheritState.DoltUser != "city-user" {
		t.Fatalf("inherit state = %+v", inheritState)
	}
	if got := readCityEndpointRigCompat(t, cityDir, "frontend"); got.DoltHost != "db.example.com" || got.DoltPort != "4406" {
		t.Fatalf("frontend city.toml rig compat = %+v", got)
	}

	explicitState := readRigEndpointConfigState(t, explicitDir)
	if explicitState.EndpointOrigin != contract.EndpointOriginExplicit {
		t.Fatalf("explicit state = %+v", explicitState)
	}
	if explicitState.DoltHost != "ops-db.example.com" || explicitState.DoltPort != "5501" || explicitState.DoltUser != "ops-user" {
		t.Fatalf("explicit state = %+v", explicitState)
	}
	if got := readCityEndpointRigCompat(t, cityDir, "ops"); got.DoltHost != "ops-db.example.com" || got.DoltPort != "5501" {
		t.Fatalf("ops city.toml rig compat = %+v", got)
	}

	for _, dir := range []string{cityDir, inheritDir, explicitDir} {
		if _, err := os.Stat(filepath.Join(dir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
			t.Fatalf("expected no managed port file for %s, stat err = %v", dir, err)
		}
	}
}

func TestDoBeadsCityUseExternalUpdatesIncludedInheritedRigs(t *testing.T) {
	skipSlowCmdGCTest(t, "exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage")
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	inheritDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(inheritDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`include = ["rigs.toml"]

[workspace]
name = "test-city"

[dolt]
host = "stale-city.example.com"
port = 3306
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "rigs.toml"), []byte(fmt.Sprintf(`[[rigs]]
name = "frontend"
path = %q
prefix = "fe"
`, inheritDir)), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, inheritDir, "fe")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity, EndpointStatus: contract.EndpointStatusVerified})
	writeRigEndpointCanonicalConfig(t, inheritDir, contract.ConfigState{IssuePrefix: "fe", EndpointOrigin: contract.EndpointOriginInheritedCity, EndpointStatus: contract.EndpointStatusVerified})
	for _, dir := range []string{cityDir, inheritDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "dolt-server.port"), []byte("3311\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	origVerify := verifyCityExternalEndpoint
	defer func() { verifyCityExternalEndpoint = origVerify }()
	var roots []string
	verifyCityExternalEndpoint = func(state contract.ConfigState, databaseScopeRoot, authScopeRoot string) error {
		roots = append(roots, databaseScopeRoot+"|"+authScopeRoot+"|"+state.DoltHost+"|"+state.DoltPort)
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", User: "city-user"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	if len(roots) != 2 {
		t.Fatalf("verifyCityExternalEndpoint calls = %d, want 2 (%v)", len(roots), roots)
	}

	cityState := readRigEndpointConfigState(t, cityDir)
	if cityState.EndpointOrigin != contract.EndpointOriginCityCanonical || cityState.DoltHost != "db.example.com" || cityState.DoltPort != "4406" {
		t.Fatalf("city state = %+v", cityState)
	}
	inheritState := readRigEndpointConfigState(t, inheritDir)
	if inheritState.EndpointOrigin != contract.EndpointOriginInheritedCity || inheritState.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("inherit state = %+v", inheritState)
	}
	if inheritState.DoltHost != "db.example.com" || inheritState.DoltPort != "4406" || inheritState.DoltUser != "city-user" {
		t.Fatalf("inherit state = %+v", inheritState)
	}
	for _, dir := range []string{cityDir, inheritDir} {
		if _, err := os.Stat(filepath.Join(dir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
			t.Fatalf("expected no managed port file for %s, stat err = %v", dir, err)
		}
	}
}

func TestDoBeadsCityUseExternalStopsManagedLocalProvider(t *testing.T) {
	cityDir := t.TempDir()
	callLog := filepath.Join(cityDir, "provider-calls.log")
	script := gcBeadsBdScriptPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$1|${GC_DOLT_HOST:-}|${GC_DOLT_PORT:-}\" >> "+callLog+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{}, nil)
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})

	origVerify := verifyCityExternalEndpoint
	defer func() { verifyCityExternalEndpoint = origVerify }()
	verifyCityExternalEndpoint = func(contract.ConfigState, string, string) error { return nil }

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{
		External:        true,
		Host:            "127.0.0.1",
		Port:            "4406",
		AdoptUnverified: true,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	data, err := os.ReadFile(callLog)
	if err != nil {
		t.Fatalf("reading call log: %v", err)
	}
	ops := strings.TrimSpace(string(data))
	if ops != "stop||" {
		t.Fatalf("provider call log = %q, want stop with managed env captured before external rewrite", ops)
	}
}

func TestDoBeadsCityUseExternalValidationFailureDoesNotStopManagedLocalProvider(t *testing.T) {
	cityDir := t.TempDir()
	callLog := filepath.Join(cityDir, "provider-calls.log")
	script := gcBeadsBdScriptPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$1\" >> "+callLog+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{}, nil)
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})

	origVerify := verifyCityExternalEndpoint
	defer func() { verifyCityExternalEndpoint = origVerify }()
	verifyCityExternalEndpoint = func(contract.ConfigState, string, string) error { return fmt.Errorf("nope") }

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{
		External: true,
		Host:     "127.0.0.1",
		Port:     "4406",
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doBeadsCityEndpoint() = %d, want 1", code)
	}
	if _, err := os.Stat(callLog); !os.IsNotExist(err) {
		t.Fatalf("validation failure should not stop provider, stat err = %v", err)
	}
}

func TestDoBeadsCityUseExternalStopFailureKeepsExternalConfig(t *testing.T) {
	cityDir := t.TempDir()
	inheritDir := filepath.Join(t.TempDir(), "frontend")
	callLog := filepath.Join(cityDir, "provider-calls.log")
	script := gcBeadsBdScriptPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inheritDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+"echo \"$1\" >> "+callLog+"\n"+"if [ \"$1\" = \"stop\" ]; then\n"+"  echo stop-failed >&2\n"+"  exit 1\n"+"fi\n"+"exit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{}, []config.Rig{{Name: "frontend", Path: inheritDir, Prefix: "fe"}})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, inheritDir, "fe")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeRigEndpointCanonicalConfig(t, inheritDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	for _, dir := range []string{cityDir, inheritDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "dolt-server.port"), []byte("3311\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	origVerify := verifyCityExternalEndpoint
	defer func() { verifyCityExternalEndpoint = origVerify }()
	verifyCityExternalEndpoint = func(contract.ConfigState, string, string) error { return nil }

	t.Setenv("GC_BEADS", "exec:"+script)
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityDir)
	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{
		External:        true,
		Host:            "127.0.0.1",
		Port:            "4406",
		AdoptUnverified: true,
	}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doBeadsCityEndpoint() = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "stopping managed local provider") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	state := readRigEndpointConfigState(t, cityDir)
	if state.EndpointOrigin != contract.EndpointOriginManagedCity || state.DoltHost != "" || state.DoltPort != "" {
		t.Fatalf("city state = %+v", state)
	}
	inheritState := readRigEndpointConfigState(t, inheritDir)
	if inheritState.EndpointOrigin != contract.EndpointOriginInheritedCity || inheritState.DoltHost != "" || inheritState.DoltPort != "" {
		t.Fatalf("inherit state = %+v", inheritState)
	}
	for _, dir := range []string{cityDir, inheritDir} {
		if got := strings.TrimSpace(string(mustReadFile(t, filepath.Join(dir, ".beads", "dolt-server.port")))); got != "3311" {
			t.Fatalf("%s port file = %q, want 3311 after rollback", dir, got)
		}
	}
}

func TestDoBeadsCityUseExternalRewritesCompatRigWithRelativePath(t *testing.T) {
	skipSlowCmdGCTest(t, "exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage")
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	inheritRel := "frontend"
	inheritDir := filepath.Join(cityDir, inheritRel)
	if err := os.MkdirAll(inheritDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{Host: "stale-city.example.com", Port: 3306}, []config.Rig{{Name: "frontend", Path: inheritRel, Prefix: "fe", DoltHost: "stale-frontend.example.com", DoltPort: "3306"}})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, inheritDir, "fe")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity, EndpointStatus: contract.EndpointStatusVerified})
	writeRigEndpointCanonicalConfig(t, inheritDir, contract.ConfigState{IssuePrefix: "fe", EndpointOrigin: contract.EndpointOriginInheritedCity, EndpointStatus: contract.EndpointStatusVerified})

	origVerify := verifyCityExternalEndpoint
	defer func() { verifyCityExternalEndpoint = origVerify }()
	verifyCityExternalEndpoint = func(contract.ConfigState, string, string) error {
		return nil
	}

	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	outsideDir := t.TempDir()
	if err := os.Chdir(outsideDir); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if chdirErr := os.Chdir(origWD); chdirErr != nil {
			t.Fatalf("restore cwd: %v", chdirErr)
		}
	}()

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", User: "city-user"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	got := readCityEndpointRigCompat(t, cityDir, "frontend")
	if got.Path != "" {
		t.Fatalf("frontend city.toml rig path = %q, want empty after site-binding write", got.Path)
	}
	if got.DoltHost != "db.example.com" || got.DoltPort != "4406" {
		t.Fatalf("frontend city.toml rig compat = %+v", got)
	}
	binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityDir)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if len(binding.Rigs) != 1 || binding.Rigs[0].Name != "frontend" || binding.Rigs[0].Path != inheritRel {
		t.Fatalf("site binding = %+v, want frontend=%s", binding.Rigs, inheritRel)
	}
	state := readRigEndpointConfigState(t, inheritDir)
	if state.EndpointOrigin != contract.EndpointOriginInheritedCity || state.DoltHost != "db.example.com" || state.DoltPort != "4406" || state.DoltUser != "city-user" {
		t.Fatalf("inherit state = %+v", state)
	}
}

func TestDoBeadsCityUseExternalPreservesCompatOnlyExplicitRigs(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	explicitDir := filepath.Join(t.TempDir(), "ops")
	if err := os.MkdirAll(explicitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{Host: "old-city.example.com", Port: 3306}, []config.Rig{{Name: "ops", Path: explicitDir, Prefix: "ops", DoltHost: "ops-db.example.com", DoltPort: "5501"}})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, explicitDir, "ops")

	origVerify := verifyCityExternalEndpoint
	defer func() { verifyCityExternalEndpoint = origVerify }()
	verifyCityExternalEndpoint = func(contract.ConfigState, string, string) error { return nil }

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", AdoptUnverified: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	cityState := readRigEndpointConfigState(t, cityDir)
	if cityState.EndpointOrigin != contract.EndpointOriginCityCanonical || cityState.EndpointStatus != contract.EndpointStatusUnverified {
		t.Fatalf("city state = %+v", cityState)
	}
	if got := readCityEndpointRigCompat(t, cityDir, "ops"); got.DoltHost != "ops-db.example.com" || got.DoltPort != "5501" {
		t.Fatalf("ops city.toml rig compat = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(explicitDir, ".beads", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("explicit compat-only rig should not gain canonical config, stat err = %v", err)
	}
}

func TestDoBeadsCityUseExternalAdoptUnverifiedSkipsValidation(t *testing.T) {
	skipSlowCmdGCTest(t, "exercises managed bd provider transition behavior; run make test-cmd-gc-process for full coverage")
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	inheritDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(inheritDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{Host: "stale-city.example.com", Port: 3306}, []config.Rig{{Name: "frontend", Path: inheritDir, Prefix: "fe", DoltHost: "stale-frontend.example.com", DoltPort: "3306"}})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, inheritDir, "fe")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity, EndpointStatus: contract.EndpointStatusVerified})
	writeRigEndpointCanonicalConfig(t, inheritDir, contract.ConfigState{IssuePrefix: "fe", EndpointOrigin: contract.EndpointOriginInheritedCity, EndpointStatus: contract.EndpointStatusVerified})

	origVerify := verifyCityExternalEndpoint
	defer func() { verifyCityExternalEndpoint = origVerify }()
	called := false
	verifyCityExternalEndpoint = func(contract.ConfigState, string, string) error {
		called = true
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", AdoptUnverified: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	if called {
		t.Fatal("verifyCityExternalEndpoint should not run for --adopt-unverified")
	}
	if state := readRigEndpointConfigState(t, cityDir); state.EndpointStatus != contract.EndpointStatusUnverified {
		t.Fatalf("city state = %+v", state)
	}
	if state := readRigEndpointConfigState(t, inheritDir); state.EndpointStatus != contract.EndpointStatusUnverified {
		t.Fatalf("inherit state = %+v", state)
	}
	cityToml := readCityEndpointToml(t, cityDir)
	if cityToml.Dolt.Host != "db.example.com" || cityToml.Dolt.Port != 4406 {
		t.Fatalf("city toml = %+v", cityToml.Dolt)
	}
	if got := readCityEndpointRigCompat(t, cityDir, "frontend"); got.DoltHost != "db.example.com" || got.DoltPort != "4406" {
		t.Fatalf("frontend city.toml rig compat = %+v", got)
	}
	if !strings.Contains(stdout.String(), "gc beads city use-external --host db.example.com --port 4406") {
		t.Fatalf("stdout = %q, want follow-up command", stdout.String())
	}
}

func TestDoBeadsCityUseManagedWritesManagedCityAndInheritedRigs(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	inheritDir := filepath.Join(t.TempDir(), "frontend")
	explicitDir := filepath.Join(t.TempDir(), "ops")
	if err := os.MkdirAll(inheritDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(explicitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{Host: "stale-city.example.com", Port: 3306}, []config.Rig{{Name: "frontend", Path: inheritDir, Prefix: "fe", DoltHost: "stale-frontend.example.com", DoltPort: "3306"}, {Name: "ops", Path: explicitDir, Prefix: "ops", DoltHost: "ops-db.example.com", DoltPort: "5501"}})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, inheritDir, "fe")
	writeRigEndpointMetadata(t, explicitDir, "ops")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginCityCanonical, EndpointStatus: contract.EndpointStatusVerified, DoltHost: "db.example.com", DoltPort: "4406", DoltUser: "city-user"})
	writeRigEndpointCanonicalConfig(t, inheritDir, contract.ConfigState{IssuePrefix: "fe", EndpointOrigin: contract.EndpointOriginInheritedCity, EndpointStatus: contract.EndpointStatusVerified, DoltHost: "db.example.com", DoltPort: "4406", DoltUser: "city-user"})
	writeRigEndpointCanonicalConfig(t, explicitDir, contract.ConfigState{IssuePrefix: "ops", EndpointOrigin: contract.EndpointOriginExplicit, EndpointStatus: contract.EndpointStatusVerified, DoltHost: "ops-db.example.com", DoltPort: "5501", DoltUser: "ops-user"})
	writeRigEndpointRuntimeState(t, cityDir, 3311)
	for _, dir := range []string{cityDir, inheritDir, explicitDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".beads", "dolt-server.port"), []byte("4406\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	cityState := readRigEndpointConfigState(t, cityDir)
	if cityState.EndpointOrigin != contract.EndpointOriginManagedCity || cityState.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("city state = %+v", cityState)
	}
	if cityState.DoltHost != "" || cityState.DoltPort != "" || cityState.DoltUser != "" {
		t.Fatalf("city managed state should scrub endpoint fields: %+v", cityState)
	}
	cityToml := readCityEndpointToml(t, cityDir)
	if cityToml.Dolt.Host != "" || cityToml.Dolt.Port != 0 {
		t.Fatalf("city toml = %+v", cityToml.Dolt)
	}
	inheritState := readRigEndpointConfigState(t, inheritDir)
	if inheritState.EndpointOrigin != contract.EndpointOriginInheritedCity || inheritState.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("inherit state = %+v", inheritState)
	}
	if inheritState.DoltHost != "" || inheritState.DoltPort != "" || inheritState.DoltUser != "" {
		t.Fatalf("inherit managed state should scrub endpoint fields: %+v", inheritState)
	}
	if got := readCityEndpointRigCompat(t, cityDir, "frontend"); got.DoltHost != "" || got.DoltPort != "" {
		t.Fatalf("frontend city.toml rig compat = %+v", got)
	}
	explicitState := readRigEndpointConfigState(t, explicitDir)
	if explicitState.DoltHost != "ops-db.example.com" || explicitState.DoltPort != "5501" || explicitState.DoltUser != "ops-user" {
		t.Fatalf("explicit state = %+v", explicitState)
	}
	if got := readCityEndpointRigCompat(t, cityDir, "ops"); got.DoltHost != "ops-db.example.com" || got.DoltPort != "5501" {
		t.Fatalf("ops city.toml rig compat = %+v", got)
	}

	for _, dir := range []string{cityDir, inheritDir} {
		if got := strings.TrimSpace(string(mustReadFile(t, filepath.Join(dir, ".beads", "dolt-server.port")))); got != "3311" {
			t.Fatalf("%s port file = %q, want 3311", dir, got)
		}
	}
	if _, err := os.Stat(filepath.Join(explicitDir, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("explicit rig port file should be removed, stat err = %v", err)
	}
}

func TestDoBeadsCityUseManagedPreservesCompatOnlyExplicitRigs(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	explicitDir := filepath.Join(t.TempDir(), "ops")
	if err := os.MkdirAll(explicitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{Host: "old-city.example.com", Port: 3306}, []config.Rig{{Name: "ops", Path: explicitDir, Prefix: "ops", DoltHost: "ops-db.example.com", DoltPort: "5501"}})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, explicitDir, "ops")

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}

	cityState := readRigEndpointConfigState(t, cityDir)
	if cityState.EndpointOrigin != contract.EndpointOriginManagedCity || cityState.EndpointStatus != contract.EndpointStatusVerified {
		t.Fatalf("city state = %+v", cityState)
	}
	if cityState.DoltHost != "" || cityState.DoltPort != "" || cityState.DoltUser != "" {
		t.Fatalf("city managed state should scrub endpoint fields: %+v", cityState)
	}
	cityToml := readCityEndpointToml(t, cityDir)
	if cityToml.Dolt.Host != "" || cityToml.Dolt.Port != 0 {
		t.Fatalf("city toml = %+v", cityToml.Dolt)
	}
	if got := readCityEndpointRigCompat(t, cityDir, "ops"); got.DoltHost != "ops-db.example.com" || got.DoltPort != "5501" {
		t.Fatalf("ops city.toml rig compat = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(explicitDir, ".beads", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("explicit compat-only rig should not gain canonical config, stat err = %v", err)
	}
}

func TestSyncCityEndpointCompatConfigUsesAtomicWrite(t *testing.T) {
	fs := fsys.NewFake()
	cityDir := "/city"
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Dolt:      config.DoltConfig{Host: "old-city.example.com", Port: 3306},
		Rigs:      []config.Rig{{Name: "frontend", Path: "/city/frontend", Prefix: "fe", DoltHost: "old-frontend.example.com", DoltPort: "3306"}},
	}
	if err := syncCityEndpointCompatConfig(fs, cityDir, filepath.Join(cityDir, "city.toml"), cfg, contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity, EndpointStatus: contract.EndpointStatusVerified}, nil); err != nil {
		t.Fatalf("syncCityEndpointCompatConfig: %v", err)
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
	if got := string(fs.Files[filepath.Join(cityDir, "city.toml")]); !strings.Contains(got, "[workspace]") {
		t.Fatalf("city.toml = %q", got)
	}
}

func TestDoBeadsCityUseExternalDryRunDoesNotWriteFilesOrValidate(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	inheritDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(inheritDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityEndpointCityConfigWithCompat(t, cityDir, config.DoltConfig{Host: "stale-city.example.com", Port: 3306}, []config.Rig{{Name: "frontend", Path: inheritDir, Prefix: "fe", DoltHost: "stale-frontend.example.com", DoltPort: "3306"}})
	writeRigEndpointMetadata(t, cityDir, "hq")
	writeRigEndpointMetadata(t, inheritDir, "fe")
	writeRigEndpointCanonicalConfig(t, cityDir, contract.ConfigState{IssuePrefix: "gc", EndpointOrigin: contract.EndpointOriginManagedCity, EndpointStatus: contract.EndpointStatusVerified})
	beforeCity := mustReadFile(t, filepath.Join(cityDir, ".beads", "config.yaml"))
	beforeMeta := mustReadFile(t, filepath.Join(cityDir, ".beads", "metadata.json"))
	beforeRigMeta := mustReadFile(t, filepath.Join(inheritDir, ".beads", "metadata.json"))

	origVerify := verifyCityExternalEndpoint
	defer func() { verifyCityExternalEndpoint = origVerify }()
	called := false
	verifyCityExternalEndpoint = func(contract.ConfigState, string, string) error {
		called = true
		return nil
	}

	var stdout, stderr bytes.Buffer
	code := doBeadsCityEndpoint(fsys.OSFS{}, cityDir, cityEndpointOptions{External: true, Host: "db.example.com", Port: "4406", DryRun: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doBeadsCityEndpoint() = %d, stderr = %s", code, stderr.String())
	}
	if called {
		t.Fatal("verifyCityExternalEndpoint should not run for dry-run")
	}
	if got := mustReadFile(t, filepath.Join(cityDir, ".beads", "config.yaml")); string(got) != string(beforeCity) {
		t.Fatalf("city config changed during dry-run:\n%s", got)
	}
	if got := mustReadFile(t, filepath.Join(cityDir, ".beads", "metadata.json")); string(got) != string(beforeMeta) {
		t.Fatalf("city metadata changed during dry-run:\n%s", got)
	}
	if got := mustReadFile(t, filepath.Join(inheritDir, ".beads", "metadata.json")); string(got) != string(beforeRigMeta) {
		t.Fatalf("rig metadata changed during dry-run:\n%s", got)
	}
	if !strings.Contains(stdout.String(), "WOULD UPDATE: city endpoint") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func writeCityEndpointCityConfigWithCompat(t *testing.T, cityDir string, dolt config.DoltConfig, rigs []config.Rig) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	var content strings.Builder
	content.WriteString("[workspace]\nname = \"test-city\"\n")
	if dolt.Host != "" || dolt.Port != 0 {
		content.WriteString("\n[dolt]\n")
		if dolt.Host != "" {
			fmt.Fprintf(&content, "host = %q\n", dolt.Host) //nolint:errcheck
		}
		if dolt.Port != 0 {
			fmt.Fprintf(&content, "port = %d\n", dolt.Port) //nolint:errcheck
		}
	}
	for _, rig := range rigs {
		content.WriteString("\n[[rigs]]\n")
		fmt.Fprintf(&content, "name = %q\n", rig.Name)     //nolint:errcheck
		fmt.Fprintf(&content, "path = %q\n", rig.Path)     //nolint:errcheck
		fmt.Fprintf(&content, "prefix = %q\n", rig.Prefix) //nolint:errcheck
		if rig.DoltHost != "" {
			fmt.Fprintf(&content, "dolt_host = %q\n", rig.DoltHost) //nolint:errcheck
		}
		if rig.DoltPort != "" {
			fmt.Fprintf(&content, "dolt_port = %q\n", rig.DoltPort) //nolint:errcheck
		}
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readCityEndpointToml(t *testing.T, cityDir string) *config.City {
	t.Helper()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func readCityEndpointRigCompat(t *testing.T, cityDir, rigName string) config.Rig {
	t.Helper()
	cfg := readCityEndpointToml(t, cityDir)
	for _, rig := range cfg.Rigs {
		if strings.EqualFold(rig.Name, rigName) {
			return rig
		}
	}
	t.Fatalf("rig %q not found in city.toml", rigName)
	return config.Rig{}
}

//nolint:unused // retained as a focused helper for future city endpoint tests
func writeCityEndpointCityConfig(t *testing.T, cityDir string, rigs []config.Rig) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "[workspace]\nname = \"test-city\"\n"
	for _, rig := range rigs {
		content += fmt.Sprintf("\n[[rigs]]\nname = %q\npath = %q\nprefix = %q\n", rig.Name, rig.Path, rig.Prefix)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
