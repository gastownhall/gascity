package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestImplicitImportCacheCheckNoopsWhenBootstrapImportsRetired(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	implicitPath := filepath.Join(gcHome, "implicit-import.toml")
	if err := os.WriteFile(implicitPath, []byte(`
schema = 1

[imports.import]
source = "github.com/gastownhall/gc-import"
version = "0.2.0"
commit = "abc123"
`), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", implicitPath, err)
	}

	check := &ImplicitImportCacheCheck{}
	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("status = %v, want OK; msg = %s; details = %v", result.Status, result.Message, result.Details)
	}
	if err := check.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix(): %v", err)
	}
}
