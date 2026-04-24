package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfigMissing(t *testing.T) {
	cfg, err := LoadConfig("/nonexistent/supervisor.toml")
	if err != nil {
		t.Fatal(err)
	}
	// Defaults should apply.
	if cfg.Supervisor.PortOrDefault() != 8372 {
		t.Errorf("expected default port 8372, got %d", cfg.Supervisor.PortOrDefault())
	}
	if cfg.Supervisor.BindOrDefault() != "127.0.0.1" {
		t.Errorf("expected default bind 127.0.0.1, got %s", cfg.Supervisor.BindOrDefault())
	}
	if cfg.Supervisor.PatrolIntervalDuration() != 10*time.Second {
		t.Errorf("expected default patrol 10s, got %v", cfg.Supervisor.PatrolIntervalDuration())
	}
}

func TestLoadConfigSeedsIsolatedGCHomeConfig(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	path := ConfigPath()
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Supervisor.Port <= 0 {
		t.Fatalf("expected seeded supervisor port, got %d", cfg.Supervisor.Port)
	}
	if cfg.Supervisor.Port == 8372 {
		t.Fatalf("expected isolated GC_HOME to avoid global default port 8372")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "port = ") {
		t.Fatalf("seeded config missing port stanza:\n%s", string(data))
	}

	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Supervisor.Port != cfg.Supervisor.Port {
		t.Fatalf("reloaded supervisor port = %d, want %d", reloaded.Supervisor.Port, cfg.Supervisor.Port)
	}
}

func TestShouldSeedIsolatedSupervisorConfigFalseForCanonicalDefaultUnderSymlinkedHome(t *testing.T) {
	root := t.TempDir()
	realHome := filepath.Join(root, "real-home")
	if err := os.MkdirAll(realHome, 0o755); err != nil {
		t.Fatal(err)
	}
	linkHome := filepath.Join(root, "home-link")
	if err := os.Symlink(realHome, linkHome); err != nil {
		t.Skip("symlinks not supported")
	}

	t.Setenv("HOME", linkHome)
	t.Setenv("GC_HOME", filepath.Join(realHome, ".gc"))
	if shouldSeedIsolatedSupervisorConfig(ConfigPath()) {
		t.Fatal("shouldSeedIsolatedSupervisorConfig() = true, want false for canonical default GC_HOME under symlinked HOME")
	}
}

func TestLoadConfigExplicit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "supervisor.toml")
	if err := os.WriteFile(path, []byte(`
[supervisor]
port = 9090
bind = "0.0.0.0"
patrol_interval = "5s"

[publication]
provider = "hosted"
tenant_slug = "acme"
public_base_domain = "apps.example.com"

[publication.tenant_auth]
policy_ref = "platform-sso"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Supervisor.PortOrDefault() != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Supervisor.PortOrDefault())
	}
	if cfg.Supervisor.BindOrDefault() != "0.0.0.0" {
		t.Errorf("expected bind 0.0.0.0, got %s", cfg.Supervisor.BindOrDefault())
	}
	if cfg.Supervisor.PatrolIntervalDuration() != 5*time.Second {
		t.Errorf("expected patrol 5s, got %v", cfg.Supervisor.PatrolIntervalDuration())
	}
	if cfg.Publication.ProviderOrDefault() != "hosted" {
		t.Errorf("Publication.ProviderOrDefault() = %q, want hosted", cfg.Publication.ProviderOrDefault())
	}
	if cfg.Publication.TenantSlugOrDefault() != "acme" {
		t.Errorf("Publication.TenantSlugOrDefault() = %q, want acme", cfg.Publication.TenantSlugOrDefault())
	}
	if cfg.Publication.BaseDomainForVisibility("public") != "apps.example.com" {
		t.Errorf("Publication.BaseDomainForVisibility(public) = %q, want apps.example.com", cfg.Publication.BaseDomainForVisibility("public"))
	}
	if cfg.Publication.TenantAuth.PolicyRef != "platform-sso" {
		t.Errorf("Publication.TenantAuth.PolicyRef = %q, want platform-sso", cfg.Publication.TenantAuth.PolicyRef)
	}
}

func TestDefaultHomeWithEnv(t *testing.T) {
	t.Setenv("GC_HOME", "/custom/gc")
	if got := DefaultHome(); got != "/custom/gc" {
		t.Errorf("expected /custom/gc, got %s", got)
	}
}

func TestDefaultHomeCanonicalizesSymlinkOverride(t *testing.T) {
	homeDir := t.TempDir()
	canonicalHome := filepath.Join(homeDir, "canonical-gc")
	if err := os.MkdirAll(canonicalHome, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkHome := filepath.Join(homeDir, "gc-link")
	if err := os.Symlink(canonicalHome, symlinkHome); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", symlinkHome)
	if got := DefaultHome(); got != canonicalHome {
		t.Fatalf("DefaultHome() = %q, want canonical %q", got, canonicalHome)
	}
}

func TestDefaultHomeCanonicalizesRelativeOverride(t *testing.T) {
	homeDir := t.TempDir()
	canonicalHome := filepath.Join(homeDir, "relative-gc")
	if err := os.MkdirAll(canonicalHome, 0o755); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(homeDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	t.Setenv("GC_HOME", "relative-gc")
	if got := DefaultHome(); got != canonicalHome {
		t.Fatalf("DefaultHome() = %q, want canonical %q", got, canonicalHome)
	}
}

func TestRuntimeDirWithXDG(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := RuntimeDir(); got != "/run/user/1000/gc" {
		t.Errorf("expected /run/user/1000/gc, got %s", got)
	}
}

func TestRuntimeDirUsesIsolatedGCHomeWhenOverrideDiffersFromDefault(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := RuntimeDir(); got != gcHome {
		t.Fatalf("RuntimeDir() = %q, want isolated GC_HOME %q", got, gcHome)
	}
}

func TestRuntimeDirUsesXDGWhenGCHomeMatchesDefaultHome(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got := RuntimeDir(); got != "/run/user/1000/gc" {
		t.Fatalf("RuntimeDir() = %q, want /run/user/1000/gc", got)
	}
}

func TestUsesIsolatedGCHomeOverride(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "isolated-home"))
	if !UsesIsolatedGCHomeOverride() {
		t.Fatal("UsesIsolatedGCHomeOverride() = false, want true")
	}
}

func TestUsesIsolatedGCHomeOverrideFalseForDefaultHome(t *testing.T) {
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	if UsesIsolatedGCHomeOverride() {
		t.Fatal("UsesIsolatedGCHomeOverride() = true, want false")
	}
}

func TestUsesIsolatedGCHomeOverrideFalseForSymlinkedDefaultHome(t *testing.T) {
	homeDir := t.TempDir()
	defaultHome := filepath.Join(homeDir, ".gc")
	if err := os.MkdirAll(defaultHome, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkHome := filepath.Join(homeDir, "default-home-link")
	if err := os.Symlink(defaultHome, symlinkHome); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", symlinkHome)
	if UsesIsolatedGCHomeOverride() {
		t.Fatal("UsesIsolatedGCHomeOverride() = true, want false for symlinked default home")
	}
}

func TestUsesIsolatedGCHomeOverrideFalseForRelativeDefaultHome(t *testing.T) {
	homeDir := t.TempDir()
	defaultHome := filepath.Join(homeDir, ".gc")
	if err := os.MkdirAll(defaultHome, 0o755); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(homeDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(wd); err != nil {
			t.Fatalf("restore cwd: %v", err)
		}
	})
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", ".gc")
	if UsesIsolatedGCHomeOverride() {
		t.Fatal("UsesIsolatedGCHomeOverride() = true, want false for relative default home")
	}
}

func TestRuntimeDirFallback(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("GC_HOME", t.TempDir())
	got := RuntimeDir()
	expected := DefaultHome()
	if got != expected {
		t.Errorf("expected %s, got %s", expected, got)
	}
}

func TestPublicationsPath(t *testing.T) {
	t.Setenv("GC_HOME", "/custom/gc")
	if got := PublicationsPath("/tmp/demo-city"); got != "/tmp/demo-city/.gc/supervisor/publications.json" {
		t.Errorf("PublicationsPath(city) = %q, want /tmp/demo-city/.gc/supervisor/publications.json", got)
	}
	if got := PublicationsPath(""); got != "/custom/gc/supervisor/publications.json" {
		t.Errorf("PublicationsPath(\"\") = %q, want /custom/gc/supervisor/publications.json", got)
	}
}

func TestDefaultHomePanicsWithoutGCHome(t *testing.T) {
	// Verify the test guard fires when GC_HOME is unset in a test binary.
	t.Setenv("GC_HOME", "")
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when GC_HOME is unset in test binary")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "GC_HOME must be set during tests") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	DefaultHome()
}

func TestRegistryRegisterPanicsOnHostPath(t *testing.T) {
	// Verify the registry guard fires when path points to real ~/.gc.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home dir")
	}
	hostRegistry := filepath.Join(home, ".gc", "cities.toml")
	reg := NewRegistry(hostRegistry)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when writing to host registry in test")
		}
		msg, ok := r.(string)
		if !ok || !strings.Contains(msg, "refusing to write to host registry") {
			t.Fatalf("unexpected panic message: %v", r)
		}
	}()
	_ = reg.Register(t.TempDir(), "test-city")
}
