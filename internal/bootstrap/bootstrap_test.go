package bootstrap

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
)

func TestEnsureBootstrapPopulatesCacheAndWritesImplicitFile(t *testing.T) {
	assetsRoot := t.TempDir()
	writeBootstrapPackAsset(t, assetsRoot, "packs/import", `
[pack]
name = "import"
version = "0.2.0"
schema = 1

[[agent]]
name = "runner"
scope = "city"
`)

	oldFS := bootstrapAssets
	bootstrapAssets = os.DirFS(assetsRoot)
	t.Cleanup(func() { bootstrapAssets = oldFS })

	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "import",
		Source:   "github.com/gastownhall/gc-import",
		Version:  "0.2.0",
		AssetDir: "packs/import",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(gcHome, "implicit-import.toml"))
	if err != nil {
		t.Fatalf("reading implicit-import.toml: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `[imports."import"]`) {
		t.Fatalf("implicit-import.toml missing import entry:\n%s", text)
	}
	if !strings.Contains(text, `version = "0.2.0"`) {
		t.Fatalf("implicit-import.toml missing version:\n%s", text)
	}

	entries, err := readImplicitFile(filepath.Join(gcHome, "implicit-import.toml"))
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	entry := entries["import"]
	cacheDir := config.GlobalRepoCachePath(gcHome, entry.Source, entry.Commit)
	if _, err := os.Stat(filepath.Join(cacheDir, "pack.toml")); err != nil {
		t.Fatalf("bootstrap cache missing pack.toml: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("bootstrap cache should not contain .git, stat err = %v", err)
	}
}

func TestEnsureBootstrapPreservesExistingEntriesAndIsIdempotent(t *testing.T) {
	assetsRoot := t.TempDir()
	writeBootstrapPackAsset(t, assetsRoot, "packs/registry", `
[pack]
name = "registry"
version = "0.1.0"
schema = 1
`)

	oldFS := bootstrapAssets
	bootstrapAssets = os.DirFS(assetsRoot)
	t.Cleanup(func() { bootstrapAssets = oldFS })

	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "registry",
		Source:   "github.com/gastownhall/gc-registry",
		Version:  "0.1.0",
		AssetDir: "packs/registry",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	if err := os.WriteFile(filepath.Join(gcHome, "implicit-import.toml"), []byte(`
schema = 1

[imports.custom]
source = "example.com/custom"
version = "1.0.0"
commit = "deadbeef"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("first EnsureBootstrap: %v", err)
	}
	implicitPath := filepath.Join(gcHome, "implicit-import.toml")
	wantTime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(implicitPath, wantTime, wantTime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("second EnsureBootstrap: %v", err)
	}
	info, err := os.Stat(implicitPath)
	if err != nil {
		t.Fatalf("Stat(%s): %v", implicitPath, err)
	}
	if !info.ModTime().Equal(wantTime) {
		t.Fatalf("implicit-import.toml modtime changed on idempotent bootstrap: got %v, want %v", info.ModTime(), wantTime)
	}

	entries, err := readImplicitFile(implicitPath)
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	if _, ok := entries["custom"]; !ok {
		t.Fatal("custom implicit entry was removed")
	}
	if _, ok := entries["registry"]; !ok {
		t.Fatal("registry bootstrap entry missing")
	}
}

func TestEnsureBootstrapEmbedsImportPackRuntimeFiles(t *testing.T) {
	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "import",
		Source:   "github.com/gastownhall/gc-import",
		Version:  "0.2.0",
		AssetDir: "packs/import",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	if err := EnsureBootstrap(gcHome); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}

	entries, err := readImplicitFile(filepath.Join(gcHome, "implicit-import.toml"))
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	entry := entries["import"]
	cacheDir := config.GlobalRepoCachePath(gcHome, entry.Source, entry.Commit)

	for _, rel := range []string{"pack.toml", "commands/add.py", "lib/implicit.py"} {
		if _, err := os.Stat(filepath.Join(cacheDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("embedded import asset %s missing from cache: %v", rel, err)
		}
	}

	info, err := os.Stat(filepath.Join(cacheDir, "doctor", "check-python.sh"))
	if err != nil {
		t.Fatalf("embedded doctor script missing from cache: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("doctor/check-python.sh should be executable, mode = %o", info.Mode().Perm())
	}
}

func TestEnsureBootstrapAllowsConcurrentCallers(t *testing.T) {
	assetsRoot := t.TempDir()
	writeBootstrapPackAsset(t, assetsRoot, "packs/import", `
[pack]
name = "import"
version = "0.2.0"
schema = 1
`)
	commandsDir := filepath.Join(assetsRoot, "packs", "import", "commands")
	if err := os.MkdirAll(commandsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commandsDir, "add.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldFS := bootstrapAssets
	bootstrapAssets = os.DirFS(assetsRoot)
	t.Cleanup(func() { bootstrapAssets = oldFS })

	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "import",
		Source:   "github.com/gastownhall/gc-import",
		Version:  "0.2.0",
		AssetDir: "packs/import",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	gcHome := t.TempDir()
	const callers = 8
	start := make(chan struct{})
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- EnsureBootstrap(gcHome)
		}()
	}

	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("EnsureBootstrap under concurrency: %v", err)
		}
	}

	entries, err := readImplicitFile(filepath.Join(gcHome, "implicit-import.toml"))
	if err != nil {
		t.Fatalf("readImplicitFile: %v", err)
	}
	entry, ok := entries["import"]
	if !ok {
		t.Fatalf("missing import entry after concurrent bootstrap: %v", entries)
	}

	cacheDir := config.GlobalRepoCachePath(gcHome, entry.Source, entry.Commit)
	for _, rel := range []string{"pack.toml", "commands/add.py"} {
		if _, err := os.Stat(filepath.Join(cacheDir, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("bootstrap cache missing %s after concurrent bootstrap: %v", rel, err)
		}
	}
	stageGlobs, err := filepath.Glob(cacheDir + ".tmp-*")
	if err != nil {
		t.Fatalf("Glob(stage tmp): %v", err)
	}
	if len(stageGlobs) != 0 {
		t.Fatalf("bootstrap temp dirs should be cleaned up, found %v", stageGlobs)
	}
	fileGlobs, err := filepath.Glob(filepath.Join(gcHome, "implicit-import.toml.tmp-*"))
	if err != nil {
		t.Fatalf("Glob(implicit tmp): %v", err)
	}
	if len(fileGlobs) != 0 {
		t.Fatalf("implicit-import temp files should be cleaned up, found %v", fileGlobs)
	}
}

func TestEnsureBootstrapFailsWhenAssetMissing(t *testing.T) {
	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "import",
		Source:   "github.com/gastownhall/gc-import",
		Version:  "0.2.0",
		AssetDir: "packs/missing",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	if err := EnsureBootstrap(t.TempDir()); err == nil {
		t.Fatal("EnsureBootstrap should fail for missing asset")
	}
}

func TestEnsureBootstrapFailsWhenPackTomlMissing(t *testing.T) {
	assetsRoot := t.TempDir()
	path := filepath.Join(assetsRoot, "packs", "import", "commands")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "add.py"), []byte("print('hi')\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldFS := bootstrapAssets
	bootstrapAssets = os.DirFS(assetsRoot)
	t.Cleanup(func() { bootstrapAssets = oldFS })

	old := BootstrapPacks
	BootstrapPacks = []Entry{{
		Name:     "import",
		Source:   "github.com/gastownhall/gc-import",
		Version:  "0.2.0",
		AssetDir: "packs/import",
	}}
	t.Cleanup(func() { BootstrapPacks = old })

	err := EnsureBootstrap(t.TempDir())
	if err == nil {
		t.Fatal("EnsureBootstrap should fail when pack.toml is missing")
	}
	if !strings.Contains(err.Error(), "missing pack.toml") {
		t.Fatalf("EnsureBootstrap error = %v, want missing pack.toml", err)
	}
}

func writeBootstrapPackAsset(t *testing.T, root, dir, packToml string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(dir))
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "pack.toml"), []byte(strings.TrimSpace(packToml)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

var _ fs.FS = os.DirFS(".")
