package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
)

// ---------------------------------------------------------------------------
// doAgentSuspend/Resume — bad config error path (no existing coverage)
// ---------------------------------------------------------------------------

func TestDoAgentSuspendBadConfig(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`invalid ][`)

	var stdout, stderr bytes.Buffer
	code := doAgentSuspend(fs, "/city", "mayor", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Error("stderr should contain error message")
	}
}

func TestDoAgentResumeBadConfig(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`invalid ][`)

	var stdout, stderr bytes.Buffer
	code := doAgentResume(fs, "/city", "mayor", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Error("stderr should contain error message")
	}
}

// ---------------------------------------------------------------------------
// Pack-preservation tests: write-back must NOT expand includes
// ---------------------------------------------------------------------------

// packConfigWithFragment sets up a fake FS with a city.toml that uses
// include = [...] pointing to a fragment file with agents. Returns the FS.
func packConfigWithFragment(t *testing.T) fsys.Fake {
	t.Helper()
	fs := fsys.NewFake()
	// City config with include directive and one inline agent.
	// include must be top-level (before any [section] header).
	fs.Files["/city/city.toml"] = []byte(`include = ["packs/mypack/agents.toml"]

[workspace]
name = "test-city"

[[agent]]
name = "inline-agent"
`)
	// Fragment that defines a pack-derived agent.
	fs.Files["/city/packs/mypack/agents.toml"] = []byte(`[[agent]]
name = "pack-worker"
dir = "myrig"
`)
	return *fs
}

// assertConfigPreserved checks the written city.toml still has the include
// directive and does NOT contain the pack-derived agent name.
func assertConfigPreserved(t *testing.T, fs *fsys.Fake, tomlPath string) {
	t.Helper()
	data := string(fs.Files[tomlPath])
	if !strings.Contains(data, "packs/mypack/agents.toml") {
		t.Errorf("city.toml should preserve include directive:\n%s", data)
	}
	if strings.Contains(data, "pack-worker") {
		t.Errorf("city.toml should NOT contain expanded pack agent:\n%s", data)
	}
}

func TestDoAgentSuspendInlinePreservesConfig(t *testing.T) {
	fs := packConfigWithFragment(t)

	var stdout, stderr bytes.Buffer
	code := doAgentSuspend(&fs, "/city", "inline-agent", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	assertConfigPreserved(t, &fs, "/city/city.toml")
	data := string(fs.Files["/city/city.toml"])
	if !strings.Contains(data, "suspended = true") {
		t.Errorf("city.toml should contain suspended = true:\n%s", data)
	}
}

func TestDoAgentSuspendPackDerivedError(t *testing.T) {
	fs := packConfigWithFragment(t)

	var stdout, stderr bytes.Buffer
	code := doAgentSuspend(&fs, "/city", "myrig/pack-worker", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 for pack-derived agent", code)
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "defined by a pack") {
		t.Errorf("stderr should mention pack: %s", errMsg)
	}
	if !strings.Contains(errMsg, "[[patches]]") {
		t.Errorf("stderr should mention patches: %s", errMsg)
	}
	// Config must NOT have been modified.
	assertConfigPreserved(t, &fs, "/city/city.toml")
}

func TestDoAgentResumePackDerivedError(t *testing.T) {
	fs := packConfigWithFragment(t)

	var stdout, stderr bytes.Buffer
	code := doAgentResume(&fs, "/city", "myrig/pack-worker", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1 for pack-derived agent", code)
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "defined by a pack") {
		t.Errorf("stderr should mention pack: %s", errMsg)
	}
	if !strings.Contains(errMsg, "[[patches]]") {
		t.Errorf("stderr should mention patches: %s", errMsg)
	}
}

// ---------------------------------------------------------------------------
// doAgentAdd — v2 scaffold behavior
// ---------------------------------------------------------------------------

func v2CityWithPack(t *testing.T) *fsys.Fake {
	t.Helper()
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"
`)
	fs.Files["/city/pack.toml"] = []byte(`[pack]
name = "test-city"
schema = 2
`)
	return fs
}

func TestDoAgentAddScaffoldsAgentDirectory(t *testing.T) {
	fs := v2CityWithPack(t)

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(fs, "/city", "worker", "", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Scaffolded agent 'worker'") {
		t.Errorf("stdout = %q, want scaffold message", stdout.String())
	}

	if _, ok := fs.Files["/city/city.toml"]; !ok {
		t.Fatal("city.toml missing")
	}
	if strings.Contains(string(fs.Files["/city/city.toml"]), "worker") {
		t.Errorf("city.toml should not be rewritten:\n%s", fs.Files["/city/city.toml"])
	}

	promptPath := filepath.Join("/city", "agents", "worker", "prompt.template.md")
	gotPrompt, ok := fs.Files[promptPath]
	if !ok {
		t.Fatalf("%s missing", promptPath)
	}
	if !strings.Contains(string(gotPrompt), "{{ .AgentName }}") {
		t.Errorf("prompt scaffold = %q, want template placeholder", gotPrompt)
	}

	cfg, err := loadCityConfigFS(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("loadCityConfigFS: %v", err)
	}
	explicit := explicitAgents(cfg.Agents)
	found := false
	for _, a := range explicit {
		if a.Name != "worker" {
			continue
		}
		found = true
		if !strings.HasSuffix(a.PromptTemplate, "agents/worker/prompt.template.md") {
			t.Errorf("PromptTemplate = %q, want agents/worker/prompt.template.md", a.PromptTemplate)
		}
	}
	if !found {
		t.Fatalf("explicit agents = %#v, want worker", explicit)
	}
}

func TestDoAgentAddCopiesPromptTemplate(t *testing.T) {
	fs := v2CityWithPack(t)
	fs.Files["/city/templates/worker.md"] = []byte("You are the worker.\n")

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(fs, "/city", "worker", "templates/worker.md", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	got, ok := fs.Files["/city/agents/worker/prompt.template.md"]
	if !ok {
		t.Fatal("copied prompt template missing")
	}
	if string(got) != "You are the worker.\n" {
		t.Errorf("copied prompt = %q, want source contents", got)
	}
}

func TestDoAgentAddWritesAgentTomlForDirAndSuspended(t *testing.T) {
	fs := v2CityWithPack(t)

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(fs, "/city", "hello-world/worker", "", "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	agentToml, ok := fs.Files["/city/agents/worker/agent.toml"]
	if !ok {
		t.Fatal("agent.toml missing")
	}
	if !strings.Contains(string(agentToml), "dir = \"hello-world\"") {
		t.Errorf("agent.toml = %q, want dir", agentToml)
	}
	if !strings.Contains(string(agentToml), "suspended = true") {
		t.Errorf("agent.toml = %q, want suspended", agentToml)
	}
	cfg, err := loadCityConfigFS(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("loadCityConfigFS: %v", err)
	}
	explicit := explicitAgents(cfg.Agents)
	found := false
	for _, a := range explicit {
		if a.Name != "worker" {
			continue
		}
		found = true
		if a.Dir != "hello-world" {
			t.Errorf("Dir = %q, want hello-world", a.Dir)
		}
		if !a.Suspended {
			t.Error("Suspended = false, want true")
		}
	}
	if !found {
		t.Fatalf("explicit agents = %#v, want worker", explicit)
	}
}

func TestDoAgentAddDuplicateScaffold(t *testing.T) {
	fs := v2CityWithPack(t)

	var stdout, stderr bytes.Buffer
	if code := doAgentAdd(fs, "/city", "worker", "", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("first doAgentAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	stderr.Reset()
	stdout.Reset()
	if code := doAgentAdd(fs, "/city", "worker", "", "", false, &stdout, &stderr); code != 1 {
		t.Fatalf("second doAgentAdd = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr = %q, want 'already exists'", stderr.String())
	}
}

func TestDoAgentAddRequiresPackToml(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"
`)

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(fs, "/city", "worker", "", "", false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	errMsg := stderr.String()
	if !strings.Contains(errMsg, "city directory with pack.toml") {
		t.Errorf("stderr = %q, want pack.toml city requirement", errMsg)
	}
	if !strings.Contains(errMsg, `gc doctor`) || !strings.Contains(errMsg, `gc doctor --fix`) {
		t.Errorf("stderr = %q, want migration hint to gc doctor / gc doctor --fix", errMsg)
	}
}

func TestLoadCityConfigFSAppliesFeatureFlags(t *testing.T) {
	oldFormulaV2 := formula.IsFormulaV2Enabled()
	oldGraphApply := molecule.IsGraphApplyEnabled()
	t.Cleanup(func() {
		formula.SetFormulaV2Enabled(oldFormulaV2)
		molecule.SetGraphApplyEnabled(oldGraphApply)
	})

	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"

[daemon]
formula_v2 = true
`)

	cfg, err := loadCityConfigFS(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("loadCityConfigFS() error = %v", err)
	}
	if !cfg.Daemon.FormulaV2 {
		t.Fatalf("cfg.Daemon.FormulaV2 = false, want true")
	}
	if !formula.IsFormulaV2Enabled() {
		t.Fatalf("formula.IsFormulaV2Enabled() = false, want true")
	}
	if !molecule.IsGraphApplyEnabled() {
		t.Fatalf("molecule.IsGraphApplyEnabled() = false, want true")
	}
}
