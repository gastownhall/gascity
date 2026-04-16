package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// writeCityTOMLForHooks writes a minimal city.toml to cityDir. When
// installAgentHooks is non-nil, it is written as the workspace
// install_agent_hooks list.
func writeCityTOMLForHooks(t *testing.T, cityDir string, installAgentHooks []string) {
	t.Helper()
	var b strings.Builder
	b.WriteString("[workspace]\nname = \"test-city\"\n")
	if installAgentHooks != nil {
		b.WriteString("install_agent_hooks = [")
		for i, p := range installAgentHooks {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("\"")
			b.WriteString(p)
			b.WriteString("\"")
		}
		b.WriteString("]\n")
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHooksSync_DefaultClaude(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityTOMLForHooks(t, cityDir, nil)

	var stdout, stderr bytes.Buffer
	code := doHooksSync(fsys.OSFS{}, cityDir, nil, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHooksSync = %d, want 0; stderr=%s", code, stderr.String())
	}

	for _, rel := range []string{"hooks/claude.json", ".gc/settings.json"} {
		path := filepath.Join(cityDir, rel)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to exist after sync: %v", rel, err)
		}
	}
	if !strings.Contains(stdout.String(), "synced providers: claude") {
		t.Errorf("stdout = %q, want to contain 'synced providers: claude'", stdout.String())
	}
	if !strings.Contains(stdout.String(), "gc supervisor reload") {
		t.Errorf("stdout = %q, want reload hint", stdout.String())
	}
}

func TestHooksSync_UsesWorkspaceProviders(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityTOMLForHooks(t, cityDir, []string{"claude", "gemini"})

	var stdout, stderr bytes.Buffer
	code := doHooksSync(fsys.OSFS{}, cityDir, nil, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHooksSync = %d, want 0; stderr=%s", code, stderr.String())
	}

	claudeFile := filepath.Join(cityDir, "hooks/claude.json")
	if _, err := os.Stat(claudeFile); err != nil {
		t.Errorf("claude hook missing: %v", err)
	}
	geminiFile := filepath.Join(cityDir, ".gemini/settings.json")
	if _, err := os.Stat(geminiFile); err != nil {
		t.Errorf("gemini hook missing: %v", err)
	}
	if !strings.Contains(stdout.String(), "synced providers: claude, gemini") {
		t.Errorf("stdout = %q, want both providers listed", stdout.String())
	}
}

func TestHooksSync_ProvidersFlagOverrides(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	// Config says claude, but --providers gemini should win.
	writeCityTOMLForHooks(t, cityDir, []string{"claude"})

	var stdout, stderr bytes.Buffer
	code := doHooksSync(fsys.OSFS{}, cityDir, []string{"gemini"}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHooksSync = %d, want 0; stderr=%s", code, stderr.String())
	}

	if _, err := os.Stat(filepath.Join(cityDir, ".gemini/settings.json")); err != nil {
		t.Errorf("gemini hook missing (flag should have selected it): %v", err)
	}
	if _, err := os.Stat(filepath.Join(cityDir, "hooks/claude.json")); err == nil {
		t.Errorf("claude hook should NOT be installed when --providers=gemini overrides config")
	}
	if !strings.Contains(stdout.String(), "synced providers: gemini") {
		t.Errorf("stdout = %q, want 'synced providers: gemini'", stdout.String())
	}
}

func TestHooksSync_ValidationError(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityTOMLForHooks(t, cityDir, nil)

	var stdout, stderr bytes.Buffer
	code := doHooksSync(fsys.OSFS{}, cityDir, []string{"bogus"}, false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doHooksSync = 0, want nonzero for unsupported provider")
	}
	if !strings.Contains(stderr.String(), "bogus") {
		t.Errorf("stderr = %q, want to mention unsupported provider name", stderr.String())
	}
	if !strings.Contains(stderr.String(), "supported:") {
		t.Errorf("stderr = %q, want list of supported providers", stderr.String())
	}
	// Nothing should have been written.
	if _, err := os.Stat(filepath.Join(cityDir, "hooks/claude.json")); err == nil {
		t.Errorf("validation error must not write hook files")
	}
}

func TestHooksSync_Idempotent(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityTOMLForHooks(t, cityDir, nil)

	claudeFile := filepath.Join(cityDir, "hooks/claude.json")

	var stdout, stderr bytes.Buffer
	if code := doHooksSync(fsys.OSFS{}, cityDir, nil, false, &stdout, &stderr); code != 0 {
		t.Fatalf("first sync = %d, want 0; stderr=%s", code, stderr.String())
	}
	first, err := os.ReadFile(claudeFile)
	if err != nil {
		t.Fatalf("reading claude file: %v", err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doHooksSync(fsys.OSFS{}, cityDir, nil, false, &stdout, &stderr); code != 0 {
		t.Fatalf("second sync = %d, want 0; stderr=%s", code, stderr.String())
	}
	second, err := os.ReadFile(claudeFile)
	if err != nil {
		t.Fatalf("reading claude file after second sync: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("second sync changed file content; want idempotent no-op")
	}
}

func TestHooksSync_DryRun(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityTOMLForHooks(t, cityDir, []string{"claude", "gemini"})

	var stdout, stderr bytes.Buffer
	code := doHooksSync(fsys.OSFS{}, cityDir, nil, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHooksSync dry-run = %d, want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "would sync providers: claude, gemini") {
		t.Errorf("stdout = %q, want 'would sync providers: claude, gemini'", stdout.String())
	}
	// Dry-run must write nothing.
	if _, err := os.Stat(filepath.Join(cityDir, "hooks/claude.json")); err == nil {
		t.Errorf("dry-run wrote hooks/claude.json; expected no writes")
	}
	if _, err := os.Stat(filepath.Join(cityDir, ".gemini/settings.json")); err == nil {
		t.Errorf("dry-run wrote gemini settings; expected no writes")
	}
	if strings.Contains(stdout.String(), "supervisor reload") {
		t.Errorf("stdout = %q, dry-run should not print the reload hint", stdout.String())
	}
}

func TestHooksSync_PreservesUserEdits(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityTOMLForHooks(t, cityDir, nil)

	// Seed hooks/claude.json with a custom payload that doesn't match the
	// stale-upgrade pattern — installer must preserve it.
	hooksDir := filepath.Join(cityDir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	custom := []byte(`{"custom":"user-edit"}`)
	claudePath := filepath.Join(hooksDir, "claude.json")
	if err := os.WriteFile(claudePath, custom, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := doHooksSync(fsys.OSFS{}, cityDir, nil, false, &stdout, &stderr); code != 0 {
		t.Fatalf("doHooksSync = %d, want 0; stderr=%s", code, stderr.String())
	}

	got, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, custom) {
		t.Errorf("custom claude.json was overwritten:\n got=%s\nwant=%s", got, custom)
	}
	// The runtime file should be seeded from the user-edited source.
	settings, err := os.ReadFile(filepath.Join(cityDir, ".gc/settings.json"))
	if err != nil {
		t.Fatalf("reading .gc/settings.json: %v", err)
	}
	if !bytes.Equal(settings, custom) {
		t.Errorf(".gc/settings.json should have been seeded from user-edited claude.json:\n got=%s\nwant=%s", settings, custom)
	}
}

func TestHooksSync_UsesInjectedFS(t *testing.T) {
	// With --providers set, doHooksSync skips the config load and goes
	// straight to hooks.Install — which must use the injected FS. Uses
	// fsys.Fake so we can assert writes without touching disk.
	clearGCEnv(t)
	fs := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	code := doHooksSync(fs, "/city", []string{"claude"}, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHooksSync = %d, want 0; stderr=%s", code, stderr.String())
	}

	if _, ok := fs.Files["/city/hooks/claude.json"]; !ok {
		t.Errorf("expected write to /city/hooks/claude.json; files=%v", fs.Files)
	}
	if _, ok := fs.Files["/city/.gc/settings.json"]; !ok {
		t.Errorf("expected write to /city/.gc/settings.json; files=%v", fs.Files)
	}
}

func TestHooksSync_InstallErrorSurfaces(t *testing.T) {
	// Inject a write error via fsys.Fake and confirm the non-zero exit
	// code plus a 'gc hooks sync:' prefixed stderr message.
	clearGCEnv(t)
	fs := fsys.NewFake()
	fs.Errors["/city/hooks/claude.json"] = os.ErrPermission

	var stdout, stderr bytes.Buffer
	code := doHooksSync(fs, "/city", []string{"claude"}, false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doHooksSync = 0, want nonzero when installer fails")
	}
	if !strings.Contains(stderr.String(), "gc hooks sync:") {
		t.Errorf("stderr = %q, want 'gc hooks sync:' prefix", stderr.String())
	}
}

func TestHooksSync_CityResolveError(t *testing.T) {
	clearGCEnv(t)
	// Point at a non-existent directory — loadCityConfig should fail.
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	var stdout, stderr bytes.Buffer
	code := doHooksSync(fsys.OSFS{}, missing, nil, false, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doHooksSync = 0, want nonzero for missing city.toml")
	}
	if !strings.Contains(stderr.String(), "gc hooks sync:") {
		t.Errorf("stderr = %q, want 'gc hooks sync:' prefix", stderr.String())
	}
}
