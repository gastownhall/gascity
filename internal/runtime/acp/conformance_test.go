//go:build integration

package acp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestACPConformance(t *testing.T) {
	// Build the fake ACP server binary.
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "fakeacp")
	cmd := exec.Command("go", "build", "-o", binPath, "./testdata/fakeacp")
	cmd.Dir = filepath.Join(mustModRoot(t), "internal", "runtime", "acp")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building fakeacp: %v", err)
	}

	// Unix socket paths are capped at 104 bytes on macOS (vs 108 on
	// Linux). The default t.TempDir() on Darwin lives under
	// /var/folders/.../T/ which already eats ~60 chars — a few more
	// directory levels plus the hashed "s<8hex>.sock" filename puts
	// us over the limit. testutil.ShortTempDir roots the directory
	// under /tmp on Darwin to keep the socket path small.
	dir := filepath.Join(testutil.ShortTempDir(t, "acp-conform"), "acp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
	p := NewProviderWithDir(dir, Config{})
	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		name := fmt.Sprintf("gc-acp-conform-%d", id)
		t.Cleanup(func() { _ = p.Stop(name) })
		return p, runtime.Config{
			Command: binPath,
			WorkDir: t.TempDir(),
		}, name
	})
}

// mustModRoot returns the module root directory.
func mustModRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go env GOMOD: %v", err)
	}
	mod := string(out)
	if mod == "" || mod == "/dev/null" {
		t.Fatal("not in a Go module")
	}
	return filepath.Dir(filepath.Clean(mod[:len(mod)-1])) // trim trailing newline
}
