package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultMailProviderUsesStartedCityPath(t *testing.T) {
	t.Setenv("GC_MAIL", "")
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n\n[mail]\nprovider = \"fake\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	provider := defaultMailProvider(cityDir)
	if provider == nil {
		t.Fatal("defaultMailProvider returned nil")
	}
	if _, err := provider.Send("gc-start-warmup", "mayor", "subject", "body"); err != nil {
		t.Fatalf("fake mail provider Send returned error: %v", err)
	}
}
