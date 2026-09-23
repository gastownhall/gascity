package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestCLIStorageRoutesDeclineTheRevisionSnapshot pins that the one-shot CLI
// storage-route resolver loads the city config without the load-time
// revision snapshot. The routes never call config.Revision(), but the default
// load builds the snapshot anyway by content-hashing every pack directory;
// reverting to it returns the same routes and passes every functional test,
// only slower — the regression no other test catches. Same guard shape as
// TestHostedCredentialProbeDeclinesTheRevisionSnapshot.
func TestCLIStorageRoutesDeclineTheRevisionSnapshot(t *testing.T) {
	const guarded = "cli_storage_routes.go"
	const capturingCall = "config.LoadWithIncludes("
	const optionsName = "cliStorageRoutesLoad"
	const option = "SkipRevisionSnapshot: true"
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	src, err := os.ReadFile(filepath.Join(filepath.Dir(currentFile), guarded))
	if err != nil {
		t.Fatalf("reading %s: %v", guarded, err)
	}
	text := string(src)
	if strings.Contains(text, capturingCall) {
		t.Errorf("%s calls %s, which captures the load-time revision snapshot; "+
			"use config.LoadWithIncludesOptions with %s", guarded, capturingCall, optionsName)
	}
	declared := false
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "var "+optionsName+" ") {
			declared = true
			if !strings.Contains(trimmed, option) {
				t.Errorf("%s declares %s without %s", guarded, optionsName, option)
			}
		}
	}
	if !declared {
		t.Errorf("%s no longer declares %s; the resolver must keep declining the snapshot", guarded, optionsName)
	}
	loadsWithOptions := false
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "config.LoadWithIncludesOptions(") && strings.Contains(line, optionsName+")") {
			loadsWithOptions = true
		}
	}
	if !loadsWithOptions {
		t.Errorf("%s: resolveCLIStorageRoutes does not load with %s", guarded, optionsName)
	}
}
