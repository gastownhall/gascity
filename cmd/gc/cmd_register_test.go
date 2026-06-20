package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestDoRegister(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"my-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegister([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Registered city") {
		t.Errorf("expected registration message, got: %s", stdout.String())
	}

	// Verify it's in the registry.
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	// Registry.Register stores the same canonical comparison form used by
	// runtime path comparisons.
	resolvedCityPath := canonicalTestPath(cityPath)
	if len(entries) != 1 || entries[0].Path != resolvedCityPath {
		t.Errorf("expected 1 entry at %s, got %v", resolvedCityPath, entries)
	}
}

func TestDoRegisterJSONFailureReplaysHelperProgressToStderr(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = func(_ string, _ string, stdout, _ io.Writer) (bool, int) {
		_, _ = fmt.Fprintln(stdout, "helper progress before failure")
		return true, 1
	}
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"my-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegisterWithOptionsJSON([]string{cityPath}, "", true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty JSON stdout on failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "helper progress before failure") {
		t.Fatalf("stderr = %q, want helper progress replayed", stderr.String())
	}
}

func TestDoUnregisterJSONFailureReplaysHelperProgressToStderr(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_HOME", dir)

	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"my-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "my-city"); err != nil {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(stdout, stderr io.Writer) int {
			_, _ = fmt.Fprintln(stdout, "reload progress before failure")
			_, _ = fmt.Fprintln(stderr, "reload failed")
			return 1
		},
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := doUnregisterJSON([]string{cityPath}, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty JSON stdout on failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "reload progress before failure") {
		t.Fatalf("stderr = %q, want helper progress replayed", stderr.String())
	}
}

func TestDoRegisterWithNameOverrideStoresAliasInRegistryWithoutMutatingCityToml(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"workspace-name\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	packToml := "[pack]\nname = \"pack-name\"\nschema = 2\n"
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegisterWithOptions([]string{cityPath}, "machine-alias", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "machine-alias") {
		t.Fatalf("stdout = %q, want machine-local alias", stdout.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %v", entries)
	}
	if entries[0].Name != "machine-alias" {
		t.Fatalf("registry name = %q, want %q", entries[0].Name, "machine-alias")
	}

	wantCityToml := "[workspace]\nname = \"workspace-name\"\n"
	gotCityToml, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotCityToml) != wantCityToml {
		t.Fatalf("city.toml mutated during register --name (gascity#602):\n--- want\n%s\n--- got\n%s", wantCityToml, string(gotCityToml))
	}
	gotPackToml, err := os.ReadFile(filepath.Join(cityPath, "pack.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotPackToml) != packToml {
		t.Fatalf("pack.toml changed during register --name:\n%s", string(gotPackToml))
	}
}

func TestRegisteredCityNamePreservesExistingRegistryAlias(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"workspace-name\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"pack-name\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "machine-alias"); err != nil {
		t.Fatal(err)
	}

	got, err := registeredCityName(cityPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "machine-alias" {
		t.Fatalf("registeredCityName = %q, want existing machine-local alias", got)
	}
}

func TestRestartRegistrationNameCapturesExistingRegistryAlias(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := ensureCityScaffold(cityPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"workspace-name\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"pack-name\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "machine-alias"); err != nil {
		t.Fatal(err)
	}

	got, err := restartRegistrationName([]string{cityPath})
	if err != nil {
		t.Fatal(err)
	}
	if got != "machine-alias" {
		t.Fatalf("restartRegistrationName = %q, want existing machine-local alias", got)
	}
}

func TestDoRegisterWithoutNameStillUsesWorkspaceName(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"workspace-name\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"pack-name\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegister([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %v", entries)
	}
	if entries[0].Name != "workspace-name" {
		t.Fatalf("registry name = %q, want %q", entries[0].Name, "workspace-name")
	}
}

func TestDoRegisterWithoutNameUsesSiteBoundWorkspaceName(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "site.toml"), []byte("workspace_name = \"site-name\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegister([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %v", entries)
	}
	if entries[0].Name != "site-name" {
		t.Fatalf("registry name = %q, want %q", entries[0].Name, "site-name")
	}
}

func TestDoRegisterWithoutNameFallsBackToDirBasenameWithoutMutatingCityToml(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"pack-name\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegister([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %v", entries)
	}
	if entries[0].Name != "my-city" {
		t.Fatalf("registry name = %q, want %q", entries[0].Name, "my-city")
	}

	gotCityToml, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotCityToml) != cityToml {
		t.Fatalf("city.toml mutated during fallback (gascity#602):\n--- want\n%s\n--- got\n%s", cityToml, string(gotCityToml))
	}
}

func TestDoRegisterWithoutNameUsesDirBasenameWhenWorkspaceAndPackNameMissing(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegister([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %v", entries)
	}
	if entries[0].Name != "my-city" {
		t.Fatalf("registry name = %q, want %q", entries[0].Name, "my-city")
	}
}

func TestDoRegisterWithNameOverrideRejectsInvalidCityTomlBeforeRegistryWrite(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"pack-name\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegisterWithOptions([]string{cityPath}, "machine-alias", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "city.toml") {
		t.Fatalf("stderr = %q, want city.toml parse error", stderr.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("registry entries = %v, want none after invalid city.toml", entries)
	}
}

func TestDoRegisterNotCity(t *testing.T) {
	dir := t.TempDir()
	notCity := filepath.Join(dir, "not-a-city")
	if err := os.MkdirAll(notCity, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	var stdout, stderr bytes.Buffer
	code := doRegister([]string{notCity}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	if !strings.Contains(stderr.String(), "not a city directory") {
		t.Errorf("expected 'not a city directory' error, got: %s", stderr.String())
	}
}

func TestDoUnregister(t *testing.T) {
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", dir)

	// Register first.
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, ""); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doUnregister([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
	}

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("expected 0 entries after unregister, got %d", len(entries))
	}
}

// gc unregister takes a city directory path, not a name. When the argument
// does not resolve to a registered city — the common footgun is passing the
// NAME shown by `gc cities`, or a wrong path — unregister must fail loudly
// instead of silently exiting 0 (non-JSON) or reporting a false success
// (JSON). Regression for ga-m3ev9r.
func TestDoUnregisterUnknownTargetFailsLoudly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_HOME", dir)

	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "my-city"); err != nil {
		t.Fatal(err)
	}

	// An absolute path that is not registered — the same not-registered code
	// path a bare name like "my-city" hits once it is resolved relative to cwd.
	ghostPath := filepath.Join(dir, "ghost-city")

	var stdout, stderr bytes.Buffer
	code := doUnregister([]string{ghostPath}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "no registered city") {
		t.Fatalf("stderr = %q, want a 'no registered city' diagnostic", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc cities") {
		t.Fatalf("stderr = %q, want a pointer to 'gc cities'", stderr.String())
	}
	if strings.Contains(stdout.String(), "Unregistered city") {
		t.Fatalf("stdout = %q, want no false success message", stdout.String())
	}

	// The real registration must be left untouched.
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("registry entries = %v, want the original city left registered", entries)
	}
}

func TestDoUnregisterJSONUnknownTargetDoesNotReportFalseSuccess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_HOME", dir)

	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "my-city"); err != nil {
		t.Fatal(err)
	}

	ghostPath := filepath.Join(dir, "ghost-city")

	var stdout, stderr bytes.Buffer
	code := doUnregisterJSON([]string{ghostPath}, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	// doUnregisterJSON must not emit a success record; the central JSON
	// failure envelope is written by run() when RunE returns errExit.
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty JSON stdout on failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no registered city") {
		t.Fatalf("stderr = %q, want a 'no registered city' diagnostic", stderr.String())
	}

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("registry entries = %v, want the original city left registered", entries)
	}
}

// End-to-end through run(): the --json failure envelope must report ok:false,
// not the previous fabricated {"ok":true,"message":"City unregistered."}.
func TestUnregisterUnknownTargetJSONEnvelopeReportsNotOK(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_HOME", dir)

	cityPath := filepath.Join(dir, "my-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "my-city"); err != nil {
		t.Fatal(err)
	}

	ghostPath := filepath.Join(dir, "ghost-city")

	var stdout, stderr bytes.Buffer
	code := run([]string{"unregister", ghostPath, "--json"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "City unregistered") {
		t.Fatalf("stdout = %q, want no false success message", stdout.String())
	}
	var env struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, stdout.String())
	}
	if env.OK {
		t.Fatalf("stdout ok = true, want false: %s", stdout.String())
	}
}

// The name-vs-path hint must fire for a bare NAME (the common footgun of
// passing the name shown by `gc cities`). Regression for ga-m3ev9r.
func TestWriteUnregisterNotRegisteredNameHint(t *testing.T) {
	var stderr bytes.Buffer
	writeUnregisterNotRegistered(&stderr, "my-city", "/abs/my-city")
	out := stderr.String()
	if !strings.Contains(out, "no registered city at /abs/my-city") {
		t.Fatalf("stderr = %q, want a 'no registered city' line", out)
	}
	if !strings.Contains(out, "not a name") {
		t.Fatalf("stderr = %q, want the name-vs-path hint for a bare name", out)
	}
	if !strings.Contains(out, "gc cities") {
		t.Fatalf("stderr = %q, want the 'gc cities' guidance", out)
	}
}

// The name-vs-path hint must NOT fire for a path-shaped argument (relative
// path, trailing slash, or nested path). Those are genuine paths that simply
// were not registered, so claiming they "look like a name" contradicts the
// diagnostic. Comparing the raw token to the fully normalized cityPath used to
// misfire here. Regression for ga-m3ev9r finding #2.
func TestWriteUnregisterNotRegisteredPathShapedNoNameHint(t *testing.T) {
	for _, rawArg := range []string{"./my-city", "/abs/my-city/", "sub/my-city"} {
		var stderr bytes.Buffer
		writeUnregisterNotRegistered(&stderr, rawArg, "/abs/my-city")
		out := stderr.String()
		if !strings.Contains(out, "no registered city at /abs/my-city") {
			t.Fatalf("rawArg=%q stderr = %q, want a 'no registered city' line", rawArg, out)
		}
		if strings.Contains(out, "not a name") {
			t.Fatalf("rawArg=%q stderr = %q, name-vs-path hint must not fire for a path-shaped arg", rawArg, out)
		}
		if !strings.Contains(out, "gc cities") {
			t.Fatalf("rawArg=%q stderr = %q, want the 'gc cities' guidance", rawArg, out)
		}
	}
}

func TestDoCities(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_HOME", dir)

	// Empty list.
	var stdout, stderr bytes.Buffer
	code := doCities(false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "No cities registered") {
		t.Errorf("expected empty message, got: %s", stdout.String())
	}

	// Register a city and list again.
	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, ""); err != nil {
		t.Fatal(err)
	}

	stdout.Reset()
	stderr.Reset()
	code = doCities(false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "bright-lights") {
		t.Errorf("expected 'bright-lights' in output, got: %s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"cities", "list", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc cities list --json exit %d: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want 1: %q", len(lines), stdout.String())
	}
	var got citiesListJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != "1" {
		t.Fatalf("schema_version = %q, want 1", got.SchemaVersion)
	}
	if len(got.Cities) != 1 || got.Cities[0].Name != "bright-lights" || got.Cities[0].Path != cityPath {
		t.Fatalf("cities = %+v, want bright-lights at %s", got.Cities, cityPath)
	}
}

// Regression for gastownhall/gascity#602:
// gc register --name must not mutate committed city.toml. The supervisor
// registry is the machine-local source of truth for registration aliases.
func TestDoRegister_Regression602_DoesNotMutateCityToml(t *testing.T) {
	oldRegister := registerCityWithSupervisorTestHook
	registerCityWithSupervisorTestHook = nil
	t.Cleanup(func() { registerCityWithSupervisorTestHook = oldRegister })

	cases := []struct {
		name         string
		cityToml     string
		packToml     string
		nameOverride string
		wantRegName  string
	}{
		{
			name:         "name override differs from workspace.name — city.toml unchanged",
			cityToml:     "[workspace]\nname = \"workspace-name\"\n",
			packToml:     "[pack]\nname = \"pack-name\"\nschema = 2\n",
			nameOverride: "machine-alias",
			wantRegName:  "machine-alias",
		},
		{
			name:         "no --name, workspace.name empty, basename fallback — city.toml unchanged",
			cityToml:     "[workspace]\n",
			packToml:     "[pack]\nname = \"pack-name\"\nschema = 2\n",
			nameOverride: "",
			wantRegName:  "my-city",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cityPath := filepath.Join(dir, "my-city")
			if err := os.MkdirAll(cityPath, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(tc.cityToml), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte(tc.packToml), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GC_HOME", dir)

			var stdout, stderr bytes.Buffer
			code := doRegisterWithOptions([]string{cityPath}, tc.nameOverride, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("expected exit 0, got %d: %s", code, stderr.String())
			}

			gotCityToml, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
			if err != nil {
				t.Fatal(err)
			}
			if string(gotCityToml) != tc.cityToml {
				t.Fatalf("city.toml mutated by gc register (#602):\n--- want (byte-identical input)\n%s\n--- got\n%s", tc.cityToml, string(gotCityToml))
			}

			reg := supervisor.NewRegistry(supervisor.RegistryPath())
			entries, err := reg.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("expected 1 registry entry, got %v", entries)
			}
			if entries[0].Name != tc.wantRegName {
				t.Fatalf("registry name = %q, want %q (alias belongs in registry, not city.toml)", entries[0].Name, tc.wantRegName)
			}
		})
	}
}

func TestCitiesListSubcommandAliasesDefaultAction(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_HOME", dir)

	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, ""); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	cmd := newCitiesCmd(&stdout, &stderr)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("gc cities list: %v; stderr=%s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "bright-lights") {
		t.Fatalf("stdout = %q, want bright-lights listing", stdout.String())
	}
}
