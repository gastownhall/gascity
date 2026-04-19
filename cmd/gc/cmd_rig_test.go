package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func isolateRigRegistryEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc-home"))
}

func TestDoRigAdd_Basic(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")
	isolateRigRegistryEnv(t)

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Adding rig 'my-frontend'") {
		t.Errorf("output missing rig name: %s", output)
	}
	if !strings.Contains(output, "Prefix: mf") {
		t.Errorf("output missing prefix: %s", output)
	}
	if !strings.Contains(output, "Rig added.") {
		t.Errorf("output missing completion: %s", output)
	}

	// Verify city.toml was updated with [[rigs]] entry.
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "my-frontend") {
		t.Errorf("city.toml should contain rig name:\n%s", data)
	}
}

func TestDoRigAdd_DoesNotWriteConfigWhenCanonicalBdNormalizationFails(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	origToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(origToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads", "metadata.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	isolateRigRegistryEnv(t)
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigAdd should fail when canonical bd normalization fails, got code %d, stderr: %s", code, stderr.String())
	}
	if errMsg := stderr.String(); !strings.Contains(errMsg, "snapshot canonical files") && !strings.Contains(errMsg, "canonicalizing city metadata") {
		t.Fatalf("stderr should mention canonical metadata failure, got: %s", errMsg)
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != origToml {
		t.Fatalf("city.toml should remain unchanged when canonical bd normalization fails.\nBefore:\n%s\nAfter:\n%s", origToml, data)
	}
}

func TestDoRigAddFailsOnInvalidCanonicalCityEndpointState(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	origToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(origToml), 0o644); err != nil {
		t.Fatal(err)
	}
	invalidCfg := `issue_prefix: gc
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
dolt.host: invalid-db.example.com
dolt.port: 3307
`
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "config.yaml"), []byte(invalidCfg), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	isolateRigRegistryEnv(t)
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigAdd should fail for invalid canonical city endpoint state, got code %d, stderr: %s", code, stderr.String())
	}
	if errMsg := stderr.String(); !strings.Contains(errMsg, "invalid canonical city endpoint state") {
		t.Fatalf("stderr should mention invalid canonical city endpoint state, got: %s", errMsg)
	}
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != origToml {
		t.Fatalf("city.toml should remain unchanged when canonical endpoint state is invalid.\nBefore:\n%s\nAfter:\n%s", origToml, data)
	}
}

func TestDoRigAdd_SkipDoltReportsDeferredInit(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\nprefix = \"gc\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	seedDeferredManagedBeads(cityPath, cityPath, "gc", "hq")

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	isolateRigRegistryEnv(t)
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "Beads init deferred to controller") {
		t.Fatalf("stdout should report deferred init, got: %s", out)
	} else if strings.Contains(out, "Initialized beads database") {
		t.Fatalf("stdout should not claim beads database initialized when GC_DOLT=skip: %s", out)
	}
}

func TestDoRigAdd_SkipDoltWaitsForControllerStoreInit(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"
prefix = "gc"

[[agent]]
name = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	seedDeferredManagedBeads(cityPath, cityPath, "gc", "hq")

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	oldReload := rigReloadControllerConfig
	oldWait := rigWaitForStoreAccessible
	t.Cleanup(func() {
		rigReloadControllerConfig = oldReload
		rigWaitForStoreAccessible = oldWait
	})

	reloadCalls := 0
	waitCalls := 0
	var gotCity, gotRig string
	var gotTimeout time.Duration
	rigReloadControllerConfig = func(city string) error {
		reloadCalls++
		gotCity = city
		return nil
	}
	rigWaitForStoreAccessible = func(city string, rig string, timeout time.Duration) error {
		waitCalls++
		gotCity = city
		gotRig = rig
		gotTimeout = timeout
		return nil
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")
	isolateRigRegistryEnv(t)

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}
	if reloadCalls != 1 {
		t.Fatalf("reload calls = %d, want 1", reloadCalls)
	}
	if waitCalls != 1 {
		t.Fatalf("wait calls = %d, want 1", waitCalls)
	}
	if gotCity != cityPath {
		t.Fatalf("wait city = %q, want %q", gotCity, cityPath)
	}
	if gotRig != rigPath {
		t.Fatalf("wait rig = %q, want %q", gotRig, rigPath)
	}
	if gotTimeout != rigDeferredStoreInitWait {
		t.Fatalf("wait timeout = %v, want %v", gotTimeout, rigDeferredStoreInitWait)
	}
	if errOut := stderr.String(); errOut != "" {
		t.Fatalf("stderr = %q, want empty", errOut)
	}
}

func TestDoRigAdd_SkipDoltWarnsWhenControllerInitStillPending(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"
prefix = "gc"

[[agent]]
name = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	seedDeferredManagedBeads(cityPath, cityPath, "gc", "hq")

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	oldReload := rigReloadControllerConfig
	oldWait := rigWaitForStoreAccessible
	t.Cleanup(func() {
		rigReloadControllerConfig = oldReload
		rigWaitForStoreAccessible = oldWait
	})
	rigReloadControllerConfig = func(string) error { return nil }
	rigWaitForStoreAccessible = func(_ string, _ string, _ time.Duration) error {
		return fmt.Errorf("still pending")
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")
	isolateRigRegistryEnv(t)

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}
	if errOut := stderr.String(); !strings.Contains(errOut, "controller init still pending") {
		t.Fatalf("stderr should warn about pending controller init, got: %s", errOut)
	}
}

func TestDoRigAdd_DuplicateNameDifferentPath(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"/some/path\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigAdd should fail for duplicate with different path, got code %d", code)
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "already registered") {
		t.Errorf("stderr should mention already registered: %s", errMsg)
	}
	if !strings.Contains(errMsg, "/some/path") {
		t.Errorf("stderr should mention existing path: %s", errMsg)
	}
}

func TestDoRigAdd_IdempotentSameNameSamePath(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Config already has this rig at the same path.
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed for same name+path, got code %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Re-initializing rig") {
		t.Errorf("output should say re-initializing: %s", output)
	}
	if !strings.Contains(output, "Rig re-initialized.") {
		t.Errorf("output should say re-initialized: %s", output)
	}

	// Re-add should migrate the machine-local path out of city.toml while
	// preserving the effective rig binding.
	newData, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	wantCityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\n"
	if string(newData) != wantCityToml {
		t.Errorf("city.toml should be rewritten without rig.path on re-add.\nWant:\n%s\nGot:\n%s", wantCityToml, newData)
	}
	binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if len(binding.Rigs) != 1 || binding.Rigs[0].Name != "my-frontend" || binding.Rigs[0].Path != rigPath {
		t.Fatalf("site binding = %+v, want my-frontend=%s", binding.Rigs, rigPath)
	}
}

func TestDoRigAddSkipsBDPortMirrorForFileProvider(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	ln := listenOnRandomPort(t)
	t.Cleanup(func() { _ = ln.Close() })
	if err := writeDoltState(cityPath, doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityPath, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "test-external")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	if _, err := os.Stat(filepath.Join(rigPath, ".beads", "dolt-server.port")); !os.IsNotExist(err) {
		t.Fatalf("expected no bd port mirror for file provider, stat err = %v", err)
	}
}

// Regression: re-add must use the rig's configured prefix, not re-derive it.
func TestDoRigAdd_ReAddUsesExistingPrefix(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Rig has explicit prefix "fe" (different from derived "mf").
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nprefix = \"fe\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	// Must show the configured prefix "fe", not the derived "mf".
	if !strings.Contains(output, "Prefix: fe") {
		t.Errorf("output should show configured prefix 'fe': %s", output)
	}
	if strings.Contains(output, "Prefix: mf") {
		t.Errorf("output should NOT show derived prefix 'mf': %s", output)
	}
}

func TestDoRigAdd_ReAddWarnsDifferingFlags(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Existing rig is NOT suspended.
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	// Re-add with --start-suspended=true (differs from existing).
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/new"}, "", "", true, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "warning") {
		t.Errorf("stderr should warn about flag mismatch: %s", errMsg)
	}
	if !strings.Contains(errMsg, "--start-suspended") {
		t.Errorf("stderr should mention --start-suspended: %s", errMsg)
	}
	if !strings.Contains(errMsg, "--include") {
		t.Errorf("stderr should mention --include: %s", errMsg)
	}
}

func TestDoRigAdd_ReAddNoSpuriousWarning(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir()) // isolate global rig registry
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Existing rig IS suspended with includes.
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nsuspended = true\nincludes = [\"packs/old\"]\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	// Re-add with default flags (no --start-suspended, no --include).
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "warning") {
		t.Errorf("stderr should NOT warn when using default flags: %s", stderr.String())
	}
}

func TestDoRigAdd_NotADirectory(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	filePath := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(filePath, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, filePath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for non-directory, got code %d", code)
	}
}

func TestDoRigAdd_RoutesGenerated(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	// Verify routes.jsonl was created for city.
	cityRoutes := filepath.Join(cityPath, ".beads", "routes.jsonl")
	if _, err := os.Stat(cityRoutes); err != nil {
		t.Errorf("city routes.jsonl not created: %v", err)
	}

	// Verify routes.jsonl was created for rig.
	rigRoutes := filepath.Join(rigPath, ".beads", "routes.jsonl")
	if _, err := os.Stat(rigRoutes); err != nil {
		t.Errorf("rig routes.jsonl not created: %v", err)
	}
}

// Regression: Bug 1 — city.toml must not be modified if rig infrastructure
// creation fails. This prevents phantom rigs in config.
func TestDoRigAdd_ConfigUnchangedOnInfraFailure(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	originalToml := "[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"
	tomlPath := filepath.Join(cityPath, "city.toml")
	if err := os.WriteFile(tomlPath, []byte(originalToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Use a fake FS that fails on beads init for the rig.
	f := fsys.NewFake()
	f.Dirs["/fake-rig"] = true
	f.Files[tomlPath] = []byte(originalToml)
	f.Errors[filepath.Join("/fake-rig", ".beads")] = os.ErrPermission

	var stdout, stderr bytes.Buffer
	code := doRigAdd(f, cityPath, "/fake-rig", nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure, got code %d", code)
	}

	// Verify city.toml was NOT modified.
	data, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "fake-rig") {
		t.Errorf("city.toml should be unchanged after infrastructure failure:\n%s", data)
	}
}

func TestDoRigList_WithRigs(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create .beads/metadata.json for HQ.
	beadsDir := filepath.Join(cityPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nprefix = \"fe\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigList(fsys.OSFS{}, cityPath, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "test-city (HQ)") {
		t.Errorf("output missing HQ: %s", output)
	}
	if !strings.Contains(output, "Prefix: tc") {
		t.Errorf("output missing HQ prefix: %s", output)
	}
	if !strings.Contains(output, "Beads:  initialized") {
		t.Errorf("output missing HQ beads status: %s", output)
	}
	if !strings.Contains(output, "my-frontend") {
		t.Errorf("output missing rig name: %s", output)
	}
	if !strings.Contains(output, "Prefix: fe") {
		t.Errorf("output missing rig prefix: %s", output)
	}
	if !strings.Contains(output, "not initialized") {
		t.Errorf("output missing rig beads status: %s", output)
	}
}

func TestDoRigList_Empty(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigList(fsys.OSFS{}, cityPath, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "test-city (HQ)") {
		t.Errorf("output missing HQ: %s", output)
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, "Path:") {
			t.Errorf("should have no rig paths when empty, got line: %s", line)
		}
	}
}

// Regression: Bug 6 — resolveRigForAgent should match agents to rigs.
func TestResolveRigForAgent(t *testing.T) {
	rigs := []config.Rig{
		{Name: "frontend", Path: "/home/user/frontend"},
		{Name: "backend", Path: "/home/user/backend"},
	}

	if got := resolveRigForAgent("/home/user/frontend", rigs); got != "frontend" {
		t.Errorf("resolveRigForAgent(frontend path) = %q, want %q", got, "frontend")
	}
	if got := resolveRigForAgent("/home/user/backend", rigs); got != "backend" {
		t.Errorf("resolveRigForAgent(backend path) = %q, want %q", got, "backend")
	}
	if got := resolveRigForAgent("/home/user/other", rigs); got != "" {
		t.Errorf("resolveRigForAgent(unmatched path) = %q, want empty", got)
	}
	if got := resolveRigForAgent("/home/user/frontend", nil); got != "" {
		t.Errorf("resolveRigForAgent(nil rigs) = %q, want empty", got)
	}
}

// Regression: trailing slash in rig path must still match.
func TestResolveRigForAgent_TrailingSlash(t *testing.T) {
	rigs := []config.Rig{
		{Name: "frontend", Path: "/home/user/frontend/"},
	}
	if got := resolveRigForAgent("/home/user/frontend", rigs); got != "frontend" {
		t.Errorf("resolveRigForAgent(no trailing slash) = %q, want %q", got, "frontend")
	}

	// Also test workDir with trailing slash, rig path without.
	rigs2 := []config.Rig{
		{Name: "backend", Path: "/home/user/backend"},
	}
	if got := resolveRigForAgent("/home/user/backend/", rigs2); got != "backend" {
		t.Errorf("resolveRigForAgent(trailing slash workDir) = %q, want %q", got, "backend")
	}
}

// ---------------------------------------------------------------------------
// gc rig suspend / resume tests
// ---------------------------------------------------------------------------

func TestDoRigSuspend(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"/some/path\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSuspend returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Suspended rig 'frontend'") {
		t.Errorf("output = %q, want suspend message", stdout.String())
	}

	// Verify config written with suspended=true.
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 || !cfg.Rigs[0].Suspended {
		t.Errorf("rig should be suspended, got %+v", cfg.Rigs)
	}
}

func TestDoRigSuspendNotFound(t *testing.T) {
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte(cityToml)

	var stdout, stderr bytes.Buffer
	code := doRigSuspend(f, "/city", "nonexistent", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigSuspend should fail for unknown rig, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want not found message", stderr.String())
	}
}

func TestDoRigSuspendAlreadySuspended(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"/some/path\"\nsuspended = true\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigSuspend(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigSuspend should be idempotent, got code %d, stderr: %s", code, stderr.String())
	}
}

func TestDoRigResume(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"/some/path\"\nsuspended = true\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigResume(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigResume returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Resumed rig 'frontend'") {
		t.Errorf("output = %q, want resume message", stdout.String())
	}

	// Verify config written with suspended=false.
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 || cfg.Rigs[0].Suspended {
		t.Errorf("rig should not be suspended, got %+v", cfg.Rigs)
	}
}

func TestDoRigResumeNotFound(t *testing.T) {
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte(cityToml)

	var stdout, stderr bytes.Buffer
	code := doRigResume(f, "/city", "nonexistent", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigResume should fail for unknown rig, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want not found message", stderr.String())
	}
}

func TestDoRigResumeNotSuspended(t *testing.T) {
	cityPath := t.TempDir()
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"frontend\"\npath = \"/some/path\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigResume(fsys.OSFS{}, cityPath, "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigResume should be idempotent, got code %d, stderr: %s", code, stderr.String())
	}
}

func TestDoRigListShowsSuspended(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nsuspended = true\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigList(fsys.OSFS{}, cityPath, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "my-frontend (suspended)") {
		t.Errorf("output = %q, want suspended annotation", stdout.String())
	}
}

// TestDoRigList_JSON_RelativeRigPath verifies that doRigList emits absolute
// paths in JSON output when the rig path in city.toml is relative.
func TestDoRigList_JSON_RelativeRigPath(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Create the rig directory at the resolved absolute location.
	relRigPath := "rigs/local-rig"
	absRigPath := filepath.Join(cityPath, relRigPath)
	if err := os.MkdirAll(absRigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create .beads/metadata.json in the rig so its beads status reads "initialized".
	beadsDir := filepath.Join(absRigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Write city.toml with a relative path for the rig.
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"local-rig\"\npath = \"" + relRigPath + "\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigList(fsys.OSFS{}, cityPath, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}

	var result RigListJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\noutput: %s", err, stdout.String())
	}

	// Find the non-HQ rig entry.
	var rigItem *RigListItem
	for i := range result.Rigs {
		if !result.Rigs[i].HQ {
			rigItem = &result.Rigs[i]
			break
		}
	}
	if rigItem == nil {
		t.Fatal("no non-HQ rig found in JSON output")
	}

	if !filepath.IsAbs(rigItem.Path) {
		t.Errorf("rig path %q is not absolute", rigItem.Path)
	}
	if rigItem.Path != absRigPath {
		t.Errorf("rig path = %q, want %q", rigItem.Path, absRigPath)
	}
	if rigItem.Beads != "initialized" {
		t.Errorf("rig beads = %q, want \"initialized\"", rigItem.Beads)
	}
}

// TestDoRigList_JSON_AbsolutePathPreserved verifies that doRigList does not
// mangle rig paths that are already absolute in city.toml (idempotency guard).
func TestDoRigList_JSON_AbsolutePathPreserved(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	absRigPath := filepath.Join(t.TempDir(), "external-rig")
	if err := os.MkdirAll(absRigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"external-rig\"\npath = \"" + absRigPath + "\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigList(fsys.OSFS{}, cityPath, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList returned %d, stderr: %s", code, stderr.String())
	}

	var result RigListJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse JSON output: %v\noutput: %s", err, stdout.String())
	}

	var rigItem *RigListItem
	for i := range result.Rigs {
		if !result.Rigs[i].HQ {
			rigItem = &result.Rigs[i]
			break
		}
	}
	if rigItem == nil {
		t.Fatal("no non-HQ rig found in JSON output")
	}

	// The path must be byte-identical to the original absolute path (no double-prefix).
	if rigItem.Path != absRigPath {
		t.Errorf("rig path = %q, want byte-identical %q", rigItem.Path, absRigPath)
	}
}

func TestDoRigAdd_WithPack(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/gastown"}, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Include: packs/gastown") {
		t.Errorf("output missing include: %s", output)
	}

	// Verify city.toml has includes field.
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if len(cfg.Rigs[0].Includes) != 1 || cfg.Rigs[0].Includes[0] != "packs/gastown" {
		t.Errorf("rig includes = %v, want [packs/gastown]; city.toml:\n%s", cfg.Rigs[0].Includes, data)
	}
}

func TestDoRigAdd_WithoutPack(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if strings.Contains(output, "Include:") {
		t.Errorf("output should not contain include line when not set: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if len(cfg.Rigs[0].Includes) != 0 {
		t.Errorf("rig includes should be empty, got %v", cfg.Rigs[0].Includes)
	}
}

func TestDoRigAdd_DefaultRigIncludes(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// City with default_rig_includes set.
	cityToml := "[workspace]\nname = \"test-city\"\ndefault_rig_includes = [\"packs/gastown\"]\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	// No --include flag → should fall back to default_rig_includes.
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Include: packs/gastown (default)") {
		t.Errorf("output missing default include: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if len(cfg.Rigs[0].Includes) != 1 || cfg.Rigs[0].Includes[0] != "packs/gastown" {
		t.Errorf("rig includes = %v, want [packs/gastown]", cfg.Rigs[0].Includes)
	}
}

func TestDoRigAdd_RootPackDefaultRigIncludes(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\ndefault_rig_includes = [\"packs/city-pack\"]\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	packToml := `[pack]
name = "test-city"
schema = 2

[defaults.rig.imports.z-pack]
source = "packs/z-pack"

[defaults.rig.imports.a-pack]
source = "packs/a-pack"
`
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Include: packs/z-pack, packs/a-pack, packs/city-pack (default)") {
		t.Errorf("output missing root pack default includes in declaration order: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	want := []string{"packs/z-pack", "packs/a-pack", "packs/city-pack"}
	if !reflect.DeepEqual(cfg.Rigs[0].Includes, want) {
		t.Errorf("rig includes = %v, want %v", cfg.Rigs[0].Includes, want)
	}
	if !reflect.DeepEqual(cfg.Workspace.DefaultRigIncludes, []string{"packs/city-pack"}) {
		t.Errorf("derived root pack defaults should not be written back to city.toml, got %v", cfg.Workspace.DefaultRigIncludes)
	}
}

func TestDoRigAdd_ExplicitIncludeOverridesDefault(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// City with default_rig_includes set.
	cityToml := "[workspace]\nname = \"test-city\"\ndefault_rig_includes = [\"packs/gastown\"]\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	// Explicit --include should override default_rig_includes.
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/custom"}, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Include: packs/custom") {
		t.Errorf("output missing explicit include: %s", output)
	}
	if strings.Contains(output, "(default)") {
		t.Errorf("output should not show (default) for explicit include: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	if len(cfg.Rigs[0].Includes) != 1 || cfg.Rigs[0].Includes[0] != "packs/custom" {
		t.Errorf("rig includes = %v, want [packs/custom]", cfg.Rigs[0].Includes)
	}
}

// Regression for #782: --include repeated on the CLI populates rig.Includes
// in order so students can compose a rig from multiple packs in one command.
func TestDoRigAdd_MultipleIncludes(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-mini-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	includes := []string{"packs/planner", "packs/architect"}
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, includes, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Include: packs/planner, packs/architect") {
		t.Errorf("output missing joined include line: %s", output)
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rigs) != 1 {
		t.Fatalf("expected 1 rig, got %d", len(cfg.Rigs))
	}
	got := cfg.Rigs[0].Includes
	if len(got) != 2 || got[0] != "packs/planner" || got[1] != "packs/architect" {
		t.Errorf("rig includes = %v, want [packs/planner packs/architect]", got)
	}
}

// Regression for #782: `--include=` (empty string) and whitespace-only
// entries must be filtered so rig.Includes never contains a blank path
// (which downstream pack resolution would read as the city root).
func TestDoRigAdd_EmptyIncludesFiltered(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\ndefault_rig_includes = [\"packs/default\"]\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"", "  ", "packs/real", ""}, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Rigs[0].Includes
	if len(got) != 1 || got[0] != "packs/real" {
		t.Errorf("rig includes = %v, want [packs/real] (blank entries filtered)", got)
	}
}

// Regression for #782: if ALL --include flags are empty, fall back to
// default_rig_includes rather than producing a rig with blank pack paths.
func TestDoRigAdd_AllEmptyIncludesFallsBackToDefaults(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\ndefault_rig_includes = [\"packs/default\"]\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-project")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"", "  "}, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	got := cfg.Rigs[0].Includes
	if len(got) != 1 || got[0] != "packs/default" {
		t.Errorf("rig includes = %v, want [packs/default] (defaults apply when all --include blank)", got)
	}
}

// Regression for #782: re-add warns only when the --include set differs from
// the existing rig. Order matters: slices.Equal is order-sensitive, so a
// reorder is treated as a mismatch (pack precedence depends on order).
func TestDoRigAdd_ReAddIncludesWarning(t *testing.T) {
	tests := []struct {
		name        string
		newIncludes []string
		wantWarn    bool
	}{
		{"identical no warn", []string{"packs/a", "packs/b"}, false},
		{"different values warns", []string{"packs/a", "packs/c"}, true},
		{"reordered warns", []string{"packs/b", "packs/a"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateRigRegistryEnv(t)
			cityPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
				t.Fatal(err)
			}

			rigPath := filepath.Join(t.TempDir(), "my-frontend")
			if err := os.MkdirAll(rigPath, 0o755); err != nil {
				t.Fatal(err)
			}

			cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nincludes = [\"packs/a\", \"packs/b\"]\n"
			if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
				t.Fatal(err)
			}

			t.Setenv("GC_DOLT", "skip")
			t.Setenv("GC_BEADS", "file")

			var stdout, stderr bytes.Buffer
			code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, tc.newIncludes, "", "", false, false, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
			}
			gotWarn := strings.Contains(stderr.String(), "--include")
			if gotWarn != tc.wantWarn {
				t.Errorf("--include warning = %v, want %v; stderr: %s", gotWarn, tc.wantWarn, stderr.String())
			}
		})
	}
}

// Regression: doRigAdd must reject rigs with colliding prefixes.
func TestDoRigAdd_PrefixCollision(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// City "my-city" (prefix "mc") already has rig "my-frontend" (prefix "mf").
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"/some/path\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Try to add "my-foo" — derives prefix "mf", collides with "my-frontend".
	rigPath := filepath.Join(t.TempDir(), "my-foo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doRigAdd should fail for prefix collision, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "collides") {
		t.Errorf("stderr should mention collision: %s", stderr.String())
	}
}

// Explicit --prefix resolves a collision that would otherwise fail.
func TestDoRigAdd_ExplicitPrefixResolvesCollision(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// City "my-city" already has rig "my-frontend" (derived prefix "mf").
	existingRigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(existingRigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf("[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = %q\n", existingRigPath)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// "my-foo" also derives "mf", but an explicit prefix avoids the collision.
	rigPath := filepath.Join(t.TempDir(), "my-foo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "mfoo", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	// Verify the explicit prefix is persisted in city.toml.
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, r := range cfg.Rigs {
		if r.Name == "my-foo" {
			found = true
			if r.Prefix != "mfoo" {
				t.Errorf("rig prefix = %q, want %q", r.Prefix, "mfoo")
			}
			if r.EffectivePrefix() != "mfoo" {
				t.Errorf("EffectivePrefix() = %q, want %q", r.EffectivePrefix(), "mfoo")
			}
		}
	}
	if !found {
		t.Fatal("rig my-foo not found in config")
	}
}

// --prefix must be rejected when the rig's .beads/config.yaml has a different prefix.
func TestDoRigAdd_ExplicitPrefixConflictsWithExistingBeads(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rig already has .beads/config.yaml with prefix "ab".
	rigPath := filepath.Join(t.TempDir(), "alpha-beta")
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: ab\nissue-prefix: ab\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "xx", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for conflicting prefix, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "already has bead prefix") {
		t.Errorf("stderr should explain conflict: %s", stderr.String())
	}
}

// Auto-derived prefix must also be rejected when it conflicts with existing .beads.
func TestDoRigAdd_DerivedPrefixConflictsWithExistingBeads(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rig "alpha-beta" would derive prefix "ab", but .beads already has "zz".
	rigPath := filepath.Join(t.TempDir(), "alpha-beta")
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: zz\nissue-prefix: zz\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for conflicting derived prefix, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "already has bead prefix") {
		t.Errorf("stderr should explain conflict: %s", stderr.String())
	}
}

// Matching prefix (explicit or derived) succeeds even when .beads exists.
func TestDoRigAdd_MatchingPrefixSucceeds(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"my-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rig "alpha-beta" derives prefix "ab", and .beads already has "ab".
	rigPath := filepath.Join(t.TempDir(), "alpha-beta")
	beadsDir := filepath.Join(rigPath, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"),
		[]byte("issue_prefix: ab\nissue-prefix: ab\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected success for matching prefix, got code %d; stderr: %s", code, stderr.String())
	}
}

func TestReadBeadsPrefix(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
		wantOK  bool
	}{
		{"found", "issue_prefix: ab\n", "ab", true},
		{"with extra keys", "backend: dolt\nissue_prefix: xy\nissue-prefix: xy\n", "xy", true},
		{"missing", "backend: dolt\n", "", false},
		{"empty value", "issue_prefix: \n", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			beadsDir := filepath.Join(dir, ".beads")
			if err := os.MkdirAll(beadsDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte(tt.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, ok := readBeadsPrefix(fsys.OSFS{}, dir)
			if ok != tt.wantOK || got != tt.want {
				t.Errorf("readBeadsPrefix() = (%q, %v), want (%q, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}

	t.Run("no .beads dir", func(t *testing.T) {
		got, ok := readBeadsPrefix(fsys.OSFS{}, t.TempDir())
		if ok || got != "" {
			t.Errorf("readBeadsPrefix() = (%q, %v), want (\"\", false)", got, ok)
		}
	})

	t.Run("dash form only", func(t *testing.T) {
		dir := t.TempDir()
		beadsDir := filepath.Join(dir, ".beads")
		if err := os.MkdirAll(beadsDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beadsDir, "config.yaml"), []byte("issue-prefix: zz\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		got, ok := readBeadsPrefix(fsys.OSFS{}, dir)
		if !ok || got != "zz" {
			t.Errorf("readBeadsPrefix() = (%q, %v), want (\"zz\", true)", got, ok)
		}
	})
}

func TestDoRigAdd_ReAddWarnsDifferingPrefix(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"my-frontend\"\npath = \"" + rigPath + "\"\nprefix = \"mf\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	// Re-add with differing --prefix should warn.
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "xx", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "--prefix=xx ignored") {
		t.Errorf("stderr should warn about --prefix mismatch: %s", errMsg)
	}
}

func TestDoRigAdd_PrefixCanonicalizedToLowercase(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "AB", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd should succeed, got code %d, stderr: %s", code, stderr.String())
	}
	// Output should show the lowercased prefix.
	if !strings.Contains(stdout.String(), "Prefix: ab") {
		t.Errorf("prefix should be lowercased to 'ab', got stdout: %s", stdout.String())
	}

	// Verify city.toml stores the lowercase prefix (not raw "AB").
	cfg, err := loadCityConfigFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("loading city.toml: %v", err)
	}
	for _, r := range cfg.Rigs {
		if r.Name == "my-rig" {
			if r.Prefix != "ab" {
				t.Errorf("city.toml Prefix = %q, want %q", r.Prefix, "ab")
			}
			if r.EffectivePrefix() != "ab" {
				t.Errorf("EffectivePrefix() = %q, want %q", r.EffectivePrefix(), "ab")
			}
			break
		}
	}

	// Verify re-add succeeds (no false-positive conflict with .beads).
	var stdout2, stderr2 bytes.Buffer
	code2 := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout2, &stderr2)
	if code2 != 0 {
		t.Errorf("re-add should succeed, got code %d, stderr: %s", code2, stderr2.String())
	}
}

func TestDoRigAdd_PrefixRejectsHyphens(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "my-app", false, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for hyphenated prefix, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "must not contain hyphens") {
		t.Errorf("expected hyphen error, got: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptExistingBeads(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "adopted-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"adopted-rig","issue_prefix":"ar"}`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	configYaml := "issue_prefix: ar\n"
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(configYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ar", false, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd --adopt returned %d, stderr: %s", code, stderr.String())
	}

	output := stdout.String()
	if !strings.Contains(output, "Adopted existing beads database") {
		t.Errorf("output should mention adoption: %s", output)
	}
	if strings.Contains(output, "Initialized beads database") {
		t.Errorf("output should NOT mention initialization when adopting: %s", output)
	}
}

func TestDoRigAdd_AdoptRequiresMetadataJSON(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "no-beads-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure when .beads/metadata.json missing, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "--adopt requires .beads/metadata.json") {
		t.Errorf("error should mention missing metadata.json: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptRequiresExistingDir(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "does-not-exist")

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure for non-existent dir with --adopt, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "--adopt requires an existing directory") {
		t.Errorf("error should mention existing directory requirement: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptNonGitDirSucceeds(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create rig without .git — should succeed with --adopt.
	rigPath := filepath.Join(t.TempDir(), "no-git-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"no-git-rig","issue_prefix":"ng"}`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	configYaml := "issue_prefix: ng\n"
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte(configYaml), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ng", false, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd --adopt on non-git dir returned %d, stderr: %s", code, stderr.String())
	}

	// Non-git dirs should succeed without printing the git detection message.
	if strings.Contains(stdout.String(), "Detected git repo") {
		t.Errorf("non-git dir should not trigger git detection message, got: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "Adopted existing beads database") {
		t.Errorf("output should mention adoption: %s", stdout.String())
	}
}

func TestDoRigAdd_AdoptRequiresConfigYaml(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create rig with metadata.json but no config.yaml.
	rigPath := filepath.Join(t.TempDir(), "no-config-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"no-config-rig","issue_prefix":"nc"}`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "nc", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure when .beads/config.yaml missing, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "valid issue_prefix") {
		t.Errorf("error should mention missing prefix: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptRejectsEmptyConfigYaml(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create rig with config.yaml that has no issue_prefix key.
	rigPath := filepath.Join(t.TempDir(), "empty-config-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"empty-config-rig"}`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	// config.yaml exists but has no issue_prefix
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("some_other_key: val\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ec", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected failure when config.yaml lacks issue_prefix, got code %d", code)
	}
	if !strings.Contains(stderr.String(), "valid issue_prefix") {
		t.Errorf("error should mention missing prefix: %s", stderr.String())
	}
}

func TestDoRigAdd_AdoptWithoutPrefixMismatch(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "prefix-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := `{"name":"prefix-rig","issue_prefix":"pr"}`
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("issue_prefix: pr\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected adopt without explicit prefix to succeed, got code %d, stderr: %s", code, stderr.String())
	}
}

func TestDoRigAdd_AdoptBootstrapsScopedFileStoreLayout(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileStoreLayoutMarkerPath(cityPath), []byte(fileStoreLayoutScopedV1+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "adopted-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(`{"name":"adopted-rig","issue_prefix":"ar"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("issue_prefix: ar\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "ar", false, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd --adopt returned %d, stderr: %s", code, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".gc", "beads.json")); err != nil {
		t.Fatalf("scoped file-store beads.json missing after adopt: %v", err)
	}
}

func TestDoRigAdd_AdoptRollsBackConfigWhenRouteGenerationFails(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	rigPath := filepath.Join(t.TempDir(), "route-fail-rig")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "metadata.json"), []byte(`{"name":"route-fail-rig","issue_prefix":"rf"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigPath, ".beads", "config.yaml"), []byte("issue_prefix: rf\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := writeAllRigRoutes
	writeAllRigRoutes = func([]rigRoute) error { return errors.New("injected routes failure") }
	t.Cleanup(func() { writeAllRigRoutes = orig })

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "rf", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected route failure, got code %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "writing routes") {
		t.Fatalf("stderr = %q, want writing routes", stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != cityToml {
		t.Fatalf("city.toml changed after route rollback:\nwant: %s\n got: %s", cityToml, data)
	}
	if _, err := os.Stat(filepath.Join(rigPath, ".beads", "hooks")); !os.IsNotExist(err) {
		t.Fatalf(".beads/hooks should not be created on rolled-back adopt, stat err = %v", err)
	}
}

type failCityTomlWriteFS struct {
	fsys.OSFS
	target string
	failed bool
}

func (f *failCityTomlWriteFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	if !f.failed && filepath.Clean(name) == filepath.Clean(f.target) {
		f.failed = true
		return errors.New("injected write failure")
	}
	return f.OSFS.WriteFile(name, data, perm)
}

func (f *failCityTomlWriteFS) Rename(oldpath, newpath string) error {
	if !f.failed && filepath.Clean(newpath) == filepath.Clean(f.target) {
		f.failed = true
		return errors.New("injected write failure")
	}
	return f.OSFS.Rename(oldpath, newpath)
}

func TestDoRigAdd_RollsBackCanonicalFilesWhenConfigWriteFails(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	cityMetaPath := filepath.Join(cityPath, ".beads", "metadata.json")
	cityConfigPath := filepath.Join(cityPath, ".beads", "config.yaml")
	cityPortPath := filepath.Join(cityPath, ".beads", "dolt-server.port")
	cityMeta := []byte(`{"name":"test-city","issue_prefix":"gc"}`)
	cityConfig := []byte("issue_prefix: gc\n")
	cityPort := []byte("3307\n")
	if err := os.WriteFile(cityMetaPath, cityMeta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cityConfigPath, cityConfig, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cityPortPath, cityPort, 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(filepath.Join(rigPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	rigMetaPath := filepath.Join(rigPath, ".beads", "metadata.json")
	rigConfigPath := filepath.Join(rigPath, ".beads", "config.yaml")
	rigPortPath := filepath.Join(rigPath, ".beads", "dolt-server.port")
	rigMeta := []byte(`{"name":"frontend","issue_prefix":"fr"}`)
	rigConfig := []byte("issue_prefix: fr\n")
	rigPort := []byte("3307\n")
	if err := os.WriteFile(rigMetaPath, rigMeta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigConfigPath, rigConfig, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rigPortPath, rigPort, 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")
	isolateRigRegistryEnv(t)

	fs := &failCityTomlWriteFS{target: filepath.Join(cityPath, "city.toml")}
	var stdout, stderr bytes.Buffer
	code := doRigAdd(fs, cityPath, rigPath, nil, "", "fr", false, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("expected config write failure, got code %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "writing config") {
		t.Fatalf("stderr should mention config write failure: %s", stderr.String())
	}
	gotCityToml, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(gotCityToml) != cityToml {
		t.Fatalf("city.toml changed after rollback:\nwant: %s\n got: %s", cityToml, gotCityToml)
	}
	if _, err := os.Stat(config.SiteBindingPath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf(".gc/site.toml should be absent after rollback, stat err = %v", err)
	}
	for _, tc := range []struct {
		path string
		want []byte
	}{
		{cityMetaPath, cityMeta},
		{cityConfigPath, cityConfig},
		{cityPortPath, cityPort},
		{rigMetaPath, rigMeta},
		{rigConfigPath, rigConfig},
		{rigPortPath, rigPort},
	} {
		got, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("reading %s after rollback: %v", tc.path, err)
		}
		if string(got) != string(tc.want) {
			t.Fatalf("rollback mismatch for %s\nwant: %s\n got: %s", tc.path, tc.want, got)
		}
	}
}

// ---------------------------------------------------------------------------
// Pack-preservation tests: write-back must NOT expand includes
// ---------------------------------------------------------------------------

func TestDoRigSuspendPreservesConfig(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte(`include = ["packs/mypack/agents.toml"]

[workspace]
name = "test-city"

[[agent]]
name = "inline-agent"

[[rigs]]
name = "frontend"
path = "/some/path"
`)
	f.Files["/city/packs/mypack/agents.toml"] = []byte(`[[agent]]
name = "pack-worker"
dir = "myrig"
`)

	var stdout, stderr bytes.Buffer
	code := doRigSuspend(f, "/city", "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	data := string(f.Files["/city/city.toml"])
	if !strings.Contains(data, "packs/mypack/agents.toml") {
		t.Errorf("city.toml should preserve include directive:\n%s", data)
	}
	if strings.Contains(data, "pack-worker") {
		t.Errorf("city.toml should NOT contain expanded pack agent:\n%s", data)
	}
}

func TestDoRigResumePreservesConfig(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte(`include = ["packs/mypack/agents.toml"]

[workspace]
name = "test-city"

[[agent]]
name = "inline-agent"

[[rigs]]
name = "frontend"
path = "/some/path"
suspended = true
`)
	f.Files["/city/packs/mypack/agents.toml"] = []byte(`[[agent]]
name = "pack-worker"
dir = "myrig"
`)

	var stdout, stderr bytes.Buffer
	code := doRigResume(f, "/city", "frontend", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	data := string(f.Files["/city/city.toml"])
	if !strings.Contains(data, "packs/mypack/agents.toml") {
		t.Errorf("city.toml should preserve include directive:\n%s", data)
	}
	if strings.Contains(data, "pack-worker") {
		t.Errorf("city.toml should NOT contain expanded pack agent:\n%s", data)
	}
}

func TestDoRigAddPreservesConfig(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create city.toml with include directive (must be top-level, before any [section]).
	cityToml := `include = ["packs/mypack/agents.toml"]

[workspace]
name = "test-city"

[[agent]]
name = "inline-agent"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create the pack fragment (so LoadWithIncludes would find it, but we don't use it).
	packDir := filepath.Join(cityPath, "packs", "mypack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "agents.toml"), []byte("[[agent]]\nname = \"pack-worker\"\ndir = \"myrig\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "my-rig")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "packs/mypack/agents.toml") {
		t.Errorf("city.toml should preserve include directive:\n%s", data)
	}
	if strings.Contains(string(data), "pack-worker") {
		t.Errorf("city.toml should NOT contain expanded pack agent:\n%s", data)
	}
	if !strings.Contains(string(data), "my-rig") {
		t.Errorf("city.toml should contain new rig:\n%s", data)
	}
}

func TestResolveRigAddPath(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	got, err := resolveRigAddPath(cityPath, "frontend")
	if err != nil {
		t.Fatalf("resolveRigAddPath(relative): %v", err)
	}
	if want := filepath.Join(cityPath, "frontend"); got != want {
		t.Fatalf("resolveRigAddPath(relative) = %q, want %q", got, want)
	}

	wd := t.TempDir()
	setCwd(t, wd)
	got, err = resolveRigAddPath(cityPath, ".")
	if err != nil {
		t.Fatalf("resolveRigAddPath(dot): %v", err)
	}
	if want := wd; got != want {
		t.Fatalf("resolveRigAddPath(dot) = %q, want %q", got, want)
	}

	abs := filepath.Join(t.TempDir(), "repo")
	got, err = resolveRigAddPath(cityPath, abs)
	if err != nil {
		t.Fatalf("resolveRigAddPath(abs): %v", err)
	}
	if got != abs {
		t.Fatalf("resolveRigAddPath(abs) = %q, want %q", got, abs)
	}
}

func TestCmdRigAddStoresMachinePathInSiteBindingWhenOutsideCity(t *testing.T) {
	origCityFlag := cityFlag
	origRigFlag := rigFlag
	defer func() {
		cityFlag = origCityFlag
		rigFlag = origRigFlag
	}()

	cityPath := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := "[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[[agent]]\nname = \"mayor\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	isolateRigRegistryEnv(t)
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")
	cityFlag = cityPath
	rigFlag = ""
	setCwd(t, t.TempDir())

	var stdout, stderr bytes.Buffer
	code := cmdRigAdd([]string{"frontend"}, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	wantRigPath := filepath.Join(cityPath, "frontend")
	if _, err := os.Stat(wantRigPath); err != nil {
		t.Fatalf("rig dir %q not created: %v", wantRigPath, err)
	}
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parse city.toml: %v\ncontents:\n%s", err, data)
	}
	if len(parsed.Rigs) != 1 {
		t.Fatalf("parsed rigs = %d, want 1\ncontents:\n%s", len(parsed.Rigs), data)
	}
	if got := parsed.Rigs[0].Path; got != "" {
		t.Fatalf("city.toml rig.path = %q, want empty (moved to site binding)\ncontents:\n%s", got, data)
	}
	binding, err := config.LoadSiteBinding(fsys.OSFS{}, cityPath)
	if err != nil {
		t.Fatalf("LoadSiteBinding: %v", err)
	}
	if len(binding.Rigs) != 1 || binding.Rigs[0].Name != "frontend" || binding.Rigs[0].Path != wantRigPath {
		t.Fatalf("site binding = %+v, want frontend=%s", binding.Rigs, wantRigPath)
	}
}

func TestDoRigAddEmitsLoadWarningsFromReloadedConfig(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[agent_defaults]
skills = ["demo"]

[[agent]]
name = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}

	rigPath := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "file")
	isolateRigRegistryEnv(t)

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "attachment-list fields") {
		t.Fatalf("stderr = %q, want deprecated attachment-list warning", stderr.String())
	}
}
