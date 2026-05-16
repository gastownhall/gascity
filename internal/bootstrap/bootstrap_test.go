package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureBootstrapLeavesFreshHomesAlone(t *testing.T) {
	gcHome := t.TempDir()
	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("EnsureBootstrap(%s): %v", gcHome, err)
	}
	if _, err := os.Stat(filepath.Join(gcHome, "implicit-import.toml")); !os.IsNotExist(err) {
		t.Fatalf("implicit-import.toml should not be created for fresh homes, stat err = %v", err)
	}
}

func TestEnsureBootstrapPrunesRetiredBootstrapEntries(t *testing.T) {
	gcHome := t.TempDir()
	implicitPath := filepath.Join(gcHome, "implicit-import.toml")
	if err := os.WriteFile(implicitPath, []byte(`
schema = 1

[imports.import]
source = "github.com/gastownhall/gc-import"
version = "0.2.0"
commit = "abc123"

[imports.core]
source = "github.com/gastownhall/gc-core"
version = "0.1.0"
commit = "beef123"

[imports.registry]
source = "github.com/gastownhall/gc-registry"
version = "0.1.0"
commit = "def456"

[imports.custom]
source = "github.com/example/custom-pack"
version = "1.0.0"
commit = "deadbeef"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("EnsureBootstrap(%s): %v", gcHome, err)
	}

	data, err := os.ReadFile(implicitPath)
	if err != nil {
		t.Fatalf("reading implicit-import.toml: %v", err)
	}
	text := string(data)
	if strings.Contains(text, "[imports.import]") {
		t.Fatalf("retired import entry should be pruned:\n%s", text)
	}
	if strings.Contains(text, "[imports.core]") {
		t.Fatalf("retired core entry should be pruned:\n%s", text)
	}
	if strings.Contains(text, "[imports.registry]") {
		t.Fatalf("retired registry entry should be pruned:\n%s", text)
	}
	if !strings.Contains(text, `[imports."custom"]`) {
		t.Fatalf("custom entry should be preserved:\n%s", text)
	}
}
