package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// TestPoolTriggerWorkDirDoesNotCreateDirectories guards the dry-run purity
// contract from gc-r9fx: computing a gc.work_dir metadata patch is a pure
// path computation and must never create directories on disk. The former
// implementation routed through resolveConfiguredWorkDir → resolveAgentDir,
// whose MkdirAll materialized agent base directories during `gc sling
// --dry-run` and other read-only desired-state builds.
func TestPoolTriggerWorkDirDoesNotCreateDirectories(t *testing.T) {
	cityPath := t.TempDir()
	cfgAgent := config.Agent{Name: "claude", Dir: "agents/claude"}
	bp := &agentBuildParams{cityPath: cityPath, cityName: "testcity"}
	request := SessionRequest{WorkBeadID: "gc-abc12", WorkBeadTitle: "fix the thing"}

	workDir := poolTriggerWorkDir(bp, &cfgAgent, "claude", request)
	if workDir == "" {
		t.Fatal("poolTriggerWorkDir returned empty workDir for a valid request")
	}
	if want := filepath.Join(cityPath, "agents", "claude"); !pathutil.PathWithin(want, workDir) {
		t.Fatalf("workDir = %q, want under %q", workDir, want)
	}
	if _, err := os.Stat(filepath.Join(cityPath, "agents")); !os.IsNotExist(err) {
		t.Errorf("pure workDir computation created directories under city path (stat err = %v)", err)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("pure workDir computation created the workDir itself (stat err = %v)", err)
	}
}
