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
		sweepOrphanPIDPrefixedDirs(os.TempDir(), testGCBinaryDirPrefix)
		buildDir, err := os.MkdirTemp("", pidPrefixedTempPattern(testGCBinaryDirPrefix))
		if err != nil {
			testGCBinaryErr = fmt.Errorf("mktemp gc binary dir: %w", err)
			return
		}
		realBinPath := filepath.Join(buildDir, "gc-real")
		binPath := filepath.Join(buildDir, "gc")
		wd, err := os.Getwd()
		if err != nil {
			testGCBinaryErr = fmt.Errorf("getwd: %w", err)
			return
		}
		cmd := exec.Command("go", "build", "-o", realBinPath, ".")
		cmd.Dir = wd
		out, err := cmd.CombinedOutput()
		if err != nil {
			testGCBinaryErr = fmt.Errorf("go build -o %s .: %w\n%s", realBinPath, err, string(out))
			return
		}
		wrapper := fmt.Sprintf("#!/bin/sh\nexport %s=1\nif [ -z \"${%s:-}\" ]; then\n  export %s=$PPID\nfi\nexec %q \"$@\"\n",
			managedDoltTestModeEnv,
			managedDoltTestParentPIDEnv,
			managedDoltTestParentPIDEnv,
			realBinPath,
		)
		if err := os.WriteFile(binPath, []byte(wrapper), 0o755); err != nil {
			testGCBinaryErr = fmt.Errorf("write gc test wrapper: %w", err)
			return
		}
		testGCBinaryPath = binPath
	})
	if testGCBinaryErr != nil {
		t.Fatal(testGCBinaryErr)
	}
	return testGCBinaryPath
}
