package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeProviderTestCity writes a minimal city.toml to a temp dir and
// materializes the builtin packs so loadCityConfigWithBuiltinPacks succeeds.
func writeProviderTestCity(t *testing.T, cityToml string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	materializeBuiltinPacksForTest(t, dir)
	return dir
}
