package sling

// Builder: replace this file by moving the constants and function body to
// sling_test.go, then delete this file. sweepOrphanSlingPIDPrefixedDirs must
// use syscall.Kill(pid,0) instead of pidAlive, and TestMain must call it for
// both prefixes before m.Run() and defer os.RemoveAll on sharedTestFormulaDir
// and sharedTestCityDir after m.Run().

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const (
	slingTestFormulaDirPrefix = "gc-sling-test-formulas-pid"
	slingTestCityDirPrefix    = "gc-sling-test-city-pid"
)

func sweepOrphanSlingPIDPrefixedDirs(root, prefix string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		pid, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		if err := syscall.Kill(pid, 0); err == nil {
			continue
		}
		_ = os.RemoveAll(filepath.Join(root, name))
	}
}
