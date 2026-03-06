//go:build integration

package acp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

func TestACPConformance(t *testing.T) {
	// Build the fake ACP server binary.
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "fakeacp")
	cmd := exec.Command("go", "build", "-o", binPath, "./testdata/fakeacp")
	cmd.Dir = filepath.Join(mustModRoot(t), "internal", "session", "acp")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("building fakeacp: %v", err)
	}

	dir := filepath.Join(t.TempDir(), "acp-conform")
	p := NewProviderWithDir(dir, Config{})
	var counter int64

	sessiontest.RunProviderTests(t, func(t *testing.T) (session.Provider, session.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		name := fmt.Sprintf("gc-acp-conform-%d", id)
		t.Cleanup(func() { _ = p.Stop(name) })
		return p, session.Config{
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
