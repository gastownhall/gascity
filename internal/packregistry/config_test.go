package packregistry

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConfigRoundTrip(t *testing.T) {
	home := t.TempDir()
	cfg := Config{Registries: []Registry{
		{Name: "main", Source: "https://registry.example/registry.toml"},
		{Name: "acme", Source: "file:///tmp/acme/registry.toml"},
	}}

	if err := SaveConfig(home, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	got, err := LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.Schema != ConfigSchema {
		t.Fatalf("schema = %d, want %d", got.Schema, ConfigSchema)
	}
	if len(got.Registries) != 2 {
		t.Fatalf("registries = %v, want 2", got.Registries)
	}
	if got.Registries[0].Name != "acme" || got.Registries[1].Name != "main" {
		t.Fatalf("registries not sorted/stable: %+v", got.Registries)
	}
}

func TestLoadConfigValidatesRegistrySources(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(ConfigPath(home)), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	body := "schema = 1\n\n[[registry]]\nname = \"main\"\nsource = \"http://registry.example/registry.toml\"\n"
	if err := os.WriteFile(ConfigPath(home), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := LoadConfig(home); err == nil {
		t.Fatal("LoadConfig accepted invalid registry source")
	}
}

func TestAddRegistryWithCacheDoesNotConfigureWhenCacheWriteFails(t *testing.T) {
	home := t.TempDir()
	saveEmptyConfigForTest(t, home)
	cacheParent := filepath.Join(home, "registry-cache", "main")
	if err := os.MkdirAll(filepath.Dir(cacheParent), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(cacheParent, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("WriteFile(cache blocker): %v", err)
	}
	err := AddRegistryWithCache(home, Registry{Name: "main", Source: "https://example.com/main"}, []byte("schema = 1\n"))
	if err == nil {
		t.Fatal("AddRegistryWithCache succeeded, want cache write error")
	}
	cfg, loadErr := LoadConfig(home)
	if loadErr != nil {
		t.Fatalf("LoadConfig: %v", loadErr)
	}
	if len(cfg.Registries) != 0 {
		t.Fatalf("registry configured despite cache failure: %+v", cfg.Registries)
	}
}

func TestLoadConfigAbsentReturnsDefaultRegistry(t *testing.T) {
	home := t.TempDir()
	cfg, err := LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Schema != ConfigSchema {
		t.Fatalf("schema = %d, want %d", cfg.Schema, ConfigSchema)
	}
	if len(cfg.Registries) != 1 || cfg.Registries[0].Name != DefaultRegistryName || cfg.Registries[0].Source != DefaultRegistrySource {
		t.Fatalf("registries = %+v, want default main registry", cfg.Registries)
	}
}

func TestLoadConfigExplicitEmptyFileReturnsEmptyConfig(t *testing.T) {
	home := t.TempDir()
	saveEmptyConfigForTest(t, home)
	cfg, err := LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Schema != ConfigSchema {
		t.Fatalf("schema = %d, want %d", cfg.Schema, ConfigSchema)
	}
	if len(cfg.Registries) != 0 {
		t.Fatalf("registries = %+v, want none", cfg.Registries)
	}
}

func TestLoadConfigPreservesExistingFile(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(ConfigPath(home)), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	before := []byte("schema = 1\n\n[[registry]]\nname = \"custom\"\nsource = \"https://example.com/custom/registry.toml\"\n")
	if err := os.WriteFile(ConfigPath(home), before, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Registries) != 1 || cfg.Registries[0].Name != "custom" {
		t.Fatalf("registries = %+v, want custom registry", cfg.Registries)
	}
	after, err := os.ReadFile(ConfigPath(home))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("existing registries.toml changed:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestValidateRegistryName(t *testing.T) {
	valid64 := "a" + strings.Repeat("b", 63)
	for _, name := range []string{"main", "acme-1", "r0", valid64} {
		if err := ValidateRegistryName(name); err != nil {
			t.Fatalf("ValidateRegistryName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "Main", "-main", "main_", "main.registry", "main/registry", "main:registry", "a" + strings.Repeat("b", 64)} {
		if err := ValidateRegistryName(name); err == nil {
			t.Fatalf("ValidateRegistryName(%q) succeeded, want error", name)
		}
	}
}

func TestConcurrentAddRegistryWritesValidTOML(t *testing.T) {
	home := t.TempDir()
	saveEmptyConfigForTest(t, home)
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for _, name := range []string{"r1", "r2", "r3", "r4", "r5", "r6", "r7", "r8"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			errCh <- AddRegistry(home, Registry{Name: name, Source: "https://example.com/" + name + "/registry.toml"})
		}(name)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("AddRegistry: %v", err)
		}
	}
	cfg, err := LoadConfig(home)
	if err != nil {
		t.Fatalf("LoadConfig after concurrent writes: %v", err)
	}
	if len(cfg.Registries) != 8 {
		t.Fatalf("registries = %d, want 8: %+v", len(cfg.Registries), cfg.Registries)
	}
}

func TestConfigLockIgnoresUnlockedSidecarFile(t *testing.T) {
	home := t.TempDir()
	saveEmptyConfigForTest(t, home)
	if err := os.WriteFile(ConfigPath(home)+".lock", []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(lock): %v", err)
	}
	if err := AddRegistry(home, Registry{Name: "main", Source: "https://example.com/registry.toml"}); err != nil {
		t.Fatalf("AddRegistry with stale sidecar file: %v", err)
	}
}

func TestAtomicWritePreservesPreviousOnRenameError(t *testing.T) {
	home := t.TempDir()
	if err := SaveConfig(home, Config{Registries: []Registry{{Name: "main", Source: "https://example.com/registry.toml"}}}); err != nil {
		t.Fatalf("SaveConfig initial: %v", err)
	}
	path := ConfigPath(home)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile before: %v", err)
	}

	blocker := path + ".tmp.blocker"
	if err := os.Mkdir(blocker, 0o755); err != nil {
		t.Fatalf("Mkdir blocker: %v", err)
	}
	// Exercise the invariant through the real path by making the parent
	// unwritable where supported. Windows and root-like environments may ignore
	// this, so also assert the previous file remains valid after any error.
	if err := os.Chmod(filepath.Dir(path), 0o555); err != nil {
		t.Fatalf("Chmod parent: %v", err)
	}
	err = SaveConfig(home, Config{Registries: []Registry{{Name: "next", Source: "https://example.com/next/registry.toml"}}})
	_ = os.Chmod(filepath.Dir(path), 0o755)
	_ = os.RemoveAll(blocker)
	if err == nil {
		t.Skip("filesystem allowed write despite read-only directory")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("ReadFile after failed save: %v", readErr)
	}
	if string(after) != string(before) {
		t.Fatalf("config changed after failed save:\n before=%s\n after=%s", before, after)
	}
}

func saveEmptyConfigForTest(t *testing.T, home string) {
	t.Helper()
	if err := SaveConfig(home, Config{}); err != nil {
		t.Fatalf("SaveConfig(empty): %v", err)
	}
}
