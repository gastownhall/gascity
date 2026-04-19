package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	testGCBinaryOnce sync.Once
	testGCBinaryPath string
	testGCBinaryErr  error
)

func currentGCBinaryForTests(t *testing.T) string {
	t.Helper()
	testGCBinaryOnce.Do(func() {
		buildDir, err := os.MkdirTemp("", "gc-test-binary-")
		if err != nil {
			testGCBinaryErr = fmt.Errorf("mktemp gc binary dir: %w", err)
			return
		}
		binPath := filepath.Join(buildDir, "gc")
		goModCache := filepath.Join(buildDir, "gomodcache")
		goCache := filepath.Join(buildDir, "gocache")
		goPath := filepath.Join(buildDir, "gopath")
		wd, err := os.Getwd()
		if err != nil {
			testGCBinaryErr = fmt.Errorf("getwd: %w", err)
			return
		}
		cmd := exec.Command("go", "build", "-o", binPath, ".")
		cmd.Dir = wd
		cmd.Env = append(os.Environ(),
			"GOMODCACHE="+goModCache,
			"GOCACHE="+goCache,
			"GOPATH="+goPath,
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			testGCBinaryErr = fmt.Errorf("go build -o %s .: %w\n%s", binPath, err, string(out))
			return
		}
		testGCBinaryPath = binPath
	})
	if testGCBinaryErr != nil {
		t.Fatal(testGCBinaryErr)
	}
	return testGCBinaryPath
}
