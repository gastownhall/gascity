package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveTemplateUsesWorkDirWithoutChangingRigIdentity(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		rigs:       []config.Rig{{Name: "demo", Path: rigRoot}},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{
		Name:    "witness",
		Dir:     "demo",
		WorkDir: ".gc/agents/{{.Rig}}/witness",
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	wantWorkDir := filepath.Join(cityPath, ".gc", "agents", "demo", "witness")
	if tp.WorkDir != wantWorkDir {
		t.Fatalf("WorkDir = %q, want %q", tp.WorkDir, wantWorkDir)
	}
	if tp.RigName != "demo" {
		t.Fatalf("RigName = %q, want demo", tp.RigName)
	}
	if tp.RigRoot != rigRoot {
		t.Fatalf("RigRoot = %q, want %q", tp.RigRoot, rigRoot)
	}
	if tp.Env["GC_RIG"] != "demo" {
		t.Fatalf("GC_RIG = %q, want demo", tp.Env["GC_RIG"])
	}
	if tp.Env["GC_RIG_ROOT"] != rigRoot {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", tp.Env["GC_RIG_ROOT"], rigRoot)
	}
	if tp.Env["BEADS_DIR"] != filepath.Join(rigRoot, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", tp.Env["BEADS_DIR"], filepath.Join(rigRoot, ".beads"))
	}
	if tp.Env["GT_ROOT"] != cityPath {
		t.Fatalf("GT_ROOT = %q, want city root %q", tp.Env["GT_ROOT"], cityPath)
	}
}

func TestResolveTemplateUsesWorkDirForCityScopedAgents(t *testing.T) {
	cityPath := t.TempDir()

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{
		Name:    "mayor",
		WorkDir: ".gc/agents/mayor",
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	wantWorkDir := filepath.Join(cityPath, ".gc", "agents", "mayor")
	if tp.WorkDir != wantWorkDir {
		t.Fatalf("WorkDir = %q, want %q", tp.WorkDir, wantWorkDir)
	}
	if tp.RigName != "" {
		t.Fatalf("RigName = %q, want empty", tp.RigName)
	}
	if tp.Env["GC_RIG"] != "" {
		t.Fatalf("GC_RIG = %q, want empty", tp.Env["GC_RIG"])
	}
	if tp.Env["GT_ROOT"] != cityPath {
		t.Fatalf("GT_ROOT = %q, want %q", tp.Env["GT_ROOT"], cityPath)
	}
}

func TestResolveTemplateDefaultsRigScopedAgentsToRigRootWithoutWorkDir(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		rigs:       []config.Rig{{Name: "demo", Path: rigRoot}},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{
		Name: "refinery",
		Dir:  "demo",
	}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if tp.WorkDir != rigRoot {
		t.Fatalf("WorkDir = %q, want %q", tp.WorkDir, rigRoot)
	}
	if tp.RigRoot != rigRoot {
		t.Fatalf("RigRoot = %q, want %q", tp.RigRoot, rigRoot)
	}
	if tp.Env["BEADS_DIR"] != filepath.Join(rigRoot, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", tp.Env["BEADS_DIR"], filepath.Join(rigRoot, ".beads"))
	}
	if tp.Env["GT_ROOT"] != cityPath {
		t.Fatalf("GT_ROOT = %q, want city root %q", tp.Env["GT_ROOT"], cityPath)
	}
}

func TestResolveTemplateRigScopedEnvCarriesRigRoots(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	params := &agentBuildParams{
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		rigs:       []config.Rig{{Name: "demo", Path: rigRoot}},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}

	agent := &config.Agent{Name: "witness", Dir: "demo"}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}

	if tp.Env["GC_RIG_ROOT"] != rigRoot {
		t.Fatalf("GC_RIG_ROOT = %q, want %q", tp.Env["GC_RIG_ROOT"], rigRoot)
	}
	if tp.Env["BEADS_DIR"] != filepath.Join(rigRoot, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", tp.Env["BEADS_DIR"], filepath.Join(rigRoot, ".beads"))
	}
	if tp.Env["GT_ROOT"] != cityPath {
		t.Fatalf("GT_ROOT = %q, want city root %q", tp.Env["GT_ROOT"], cityPath)
	}
}
