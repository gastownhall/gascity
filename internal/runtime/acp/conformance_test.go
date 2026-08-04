//go:build integration

package acp

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

type acpConformanceFixture struct {
	once    sync.Once
	dir     string
	command string
	err     error
}

func TestACPConformance(t *testing.T) {
	var fixture acpConformanceFixture
	var counter int64

	runtimetest.RunProviderTests(t, func(caseT *testing.T) (runtime.Provider, runtime.Config, string) {
		return NewSeamBackedWithDir(acpConformanceDir(caseT, t, &fixture), Config{}), runtime.Config{
			Command: acpConformanceCommand(caseT, t, &fixture),
			WorkDir: caseT.TempDir(),
		}, fmt.Sprintf("gc-acp-conform-%d", atomic.AddInt64(&counter, 1))
	})
}

func acpConformanceDir(caseT, ownerT *testing.T, fixture *acpConformanceFixture) string {
	caseT.Helper()
	if err := prepareACPConformanceFixture(ownerT, fixture); err != nil {
		caseT.Fatal(err)
	}
	return fixture.dir
}

func acpConformanceCommand(caseT, ownerT *testing.T, fixture *acpConformanceFixture) string {
	caseT.Helper()
	if err := prepareACPConformanceFixture(ownerT, fixture); err != nil {
		caseT.Fatal(err)
	}
	return fixture.command
}

func prepareACPConformanceFixture(ownerT *testing.T, fixture *acpConformanceFixture) error {
	fixture.once.Do(func() {
		// Unix socket paths are capped at 104 bytes on macOS (vs 108 on
		// Linux), so root the fixture directly under /tmp on Darwin.
		root := os.TempDir()
		if goruntime.GOOS == "darwin" {
			root = "/tmp"
		}
		fixtureRoot, err := os.MkdirTemp(root, "acp-conform")
		if err != nil {
			fixture.err = fmt.Errorf("create ACP conformance fixture: %w", err)
			return
		}
		ownerT.Cleanup(func() { _ = os.RemoveAll(fixtureRoot) })

		fixture.dir = filepath.Join(fixtureRoot, "acp")
		if err := os.MkdirAll(fixture.dir, 0o755); err != nil {
			fixture.err = fmt.Errorf("mkdir %q: %w", fixture.dir, err)
			return
		}

		modRoot, err := moduleRoot()
		if err != nil {
			fixture.err = err
			return
		}
		fixture.command = filepath.Join(fixtureRoot, "fakeacp")
		cmd := exec.Command("go", "build", "-o", fixture.command, "./testdata/fakeacp")
		cmd.Dir = filepath.Join(modRoot, "internal", "runtime", "acp")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fixture.err = fmt.Errorf("building fakeacp: %w", err)
		}
	})
	return fixture.err
}

func moduleRoot() (string, error) {
	cmd := exec.Command("go", "env", "GOMOD")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	mod := strings.TrimSpace(string(out))
	if mod == "" || mod == "/dev/null" {
		return "", fmt.Errorf("not in a Go module")
	}
	return filepath.Dir(filepath.Clean(mod)), nil
}
