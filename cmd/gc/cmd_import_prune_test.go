package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// writePruneCacheClone creates a fake cache clone directory with the given mtime.
func writePruneCacheClone(t *testing.T, cacheRoot, name string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(cacheRoot, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	return dir
}

// pruneTestCacheRoot isolates the os.UserHomeDir-based pack cache into a temp
// HOME and returns the resolved cache root, creating it. clearGCEnv must already
// have run so GC_HOME (the supervisor registry home) is isolated separately.
func pruneTestCacheRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root, err := packman.RepoCacheRoot()
	if err != nil {
		t.Fatalf("RepoCacheRoot: %v", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", root, err)
	}
	return root
}

// registerPruneCity writes a minimal city with a packs.lock pinning source@commit
// and registers it in the supervisor registry.
func registerPruneCity(t *testing.T, name, source, commit string) {
	t.Helper()
	cityDir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll city: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \""+name+"\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile city.toml: %v", err)
	}
	lock := "schema = 1\n\n[packs.\"" + source + "\"]\ncommit = \"" + commit + "\"\nversion = \"1.0.0\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, packman.LockfileName), []byte(lock), 0o644); err != nil {
		t.Fatalf("WriteFile packs.lock: %v", err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityDir, name); err != nil {
		t.Fatalf("Register(%q): %v", name, err)
	}
}

func TestDoImportPruneDryRunReportsWithoutDeleting(t *testing.T) {
	clearGCEnv(t)
	cacheRoot := pruneTestCacheRoot(t)

	source := "https://github.com/example/repo"
	commit := "abc123"
	registerPruneCity(t, "alpha", source, commit)

	old := time.Now().Add(-30 * 24 * time.Hour)
	refDir := writePruneCacheClone(t, cacheRoot, packman.RepoCacheKey(source, commit), old)
	orphanDir := writePruneCacheClone(t, cacheRoot, "orphankey0000", old)

	var stdout, stderr bytes.Buffer
	if rc := doImportPrune(true, false, 7, &stdout, &stderr); rc != 0 {
		t.Fatalf("doImportPrune rc=%d stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "1 referenced, 1 unreferenced") {
		t.Fatalf("unexpected summary: %q", out)
	}
	if !strings.Contains(out, "[dry-run]") {
		t.Fatalf("expected dry-run marker: %q", out)
	}
	// Dry run deletes nothing.
	for _, dir := range []string{refDir, orphanDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("dry-run removed %q: %v", dir, err)
		}
	}
}

func TestDoImportPruneApplyRemovesUnreferencedOnly(t *testing.T) {
	clearGCEnv(t)
	cacheRoot := pruneTestCacheRoot(t)

	source := "https://github.com/example/repo"
	commit := "abc123"
	registerPruneCity(t, "beta", source, commit)

	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	refDir := writePruneCacheClone(t, cacheRoot, packman.RepoCacheKey(source, commit), old)
	orphanOld := writePruneCacheClone(t, cacheRoot, "orphanold0000", old)
	orphanFresh := writePruneCacheClone(t, cacheRoot, "orphanfresh00", recent)

	var stdout, stderr bytes.Buffer
	if rc := doImportPrune(true, true, 7, &stdout, &stderr); rc != 0 {
		t.Fatalf("doImportPrune rc=%d stderr=%s", rc, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "[applied]") {
		t.Fatalf("expected applied marker: %q", out)
	}
	if _, err := os.Stat(orphanOld); !os.IsNotExist(err) {
		t.Fatalf("expected orphan-old deleted, stat err=%v", err)
	}
	for _, dir := range []string{refDir, orphanFresh} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("apply removed protected dir %q: %v", dir, err)
		}
	}
}

func TestDoImportPruneRejectsNegativeKeepDays(t *testing.T) {
	clearGCEnv(t)
	var stdout, stderr bytes.Buffer
	if rc := doImportPrune(true, false, -1, &stdout, &stderr); rc == 0 {
		t.Fatalf("expected non-zero rc for negative keep-days")
	}
	if !strings.Contains(stderr.String(), "keep-days") {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}
