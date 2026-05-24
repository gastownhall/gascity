package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
)

func TestDoAgentListJSON(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"

[[agent]]
name = "mayor"
max_active_sessions = 1

[[agent]]
name = "worker"
dir = "frontend"
suspended = true
work_query = "bd ready --label=frontend"
sling_query = "bd update {} --set-metadata gc.routed_to=frontend/worker"
`)

	var stdout, stderr bytes.Buffer
	code := doAgentList(fs, "/city", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentList --json = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stdout lines = %d, want 1; stdout=%q", len(lines), stdout.String())
	}
	var result AgentListJSON
	if err := json.Unmarshal([]byte(lines[0]), &result); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.SchemaVersion != "1" || result.CityName != "test-city" || len(result.Agents) != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	var worker AgentListItem
	for _, item := range result.Agents {
		if item.QualifiedName == "frontend/worker" {
			worker = item
		}
	}
	if worker.QualifiedName != "frontend/worker" || !worker.Suspended {
		t.Fatalf("worker item = %+v, want suspended frontend/worker", worker)
	}
	if worker.WorkQuery != "bd ready --label=frontend" || worker.SlingQuery == "" {
		t.Fatalf("worker routing fields = %+v", worker)
	}
}

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

func TestLoadCityConfigFSEmitsProvenanceWarnings(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"
`)
	fs.Files["/city/pack.toml"] = []byte(`[pack]
name = "test-city"
schema = 2

[agents]
append_fragments = ["footer"]
`)

	var stderr bytes.Buffer
	cfg, err := loadCityConfigFS(fs, "/city/city.toml", &stderr)
	if err != nil {
		t.Fatalf("loadCityConfigFS: %v", err)
	}
	if cfg == nil {
		t.Fatal("loadCityConfigFS returned nil config")
	}
	if !strings.Contains(stderr.String(), "[agents] is a deprecated compatibility alias for [agent_defaults]") {
		t.Fatalf("expected [agents] alias warning, got %q", stderr.String())
	}
}

func TestLoadCityConfigFSEmitsMigrationWarningsAcrossCalls(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"
`)
	fs.Files["/city/pack.toml"] = []byte(`[pack]
name = "test-city"
schema = 2

[agents]
append_fragments = ["footer"]
`)

	var stderr bytes.Buffer
	for i := 0; i < 2; i++ {
		cfg, err := loadCityConfigFS(fs, "/city/city.toml", &stderr)
		if err != nil {
			t.Fatalf("loadCityConfigFS call %d: %v", i+1, err)
		}
		if cfg == nil {
			t.Fatalf("loadCityConfigFS call %d returned nil config", i+1)
		}
	}

	const want = "[agents] is a deprecated compatibility alias for [agent_defaults]"
	if got := strings.Count(stderr.String(), want); got != 2 {
		t.Fatalf("warning count = %d, want 2; stderr=%q", got, stderr.String())
	}
}

func TestLoadCityConfigFSHardErrorsOnUnsupportedLegacyV1Surfaces(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/city/legacy-pack"] = true
	fs.Files["/city/legacy-pack/pack.toml"] = []byte(`[pack]
name = "legacy-pack"
schema = 1
`)
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"
includes = ["legacy-pack"]
default_rig_includes = ["default-pack"]

[[agent]]
name = "worker"

[packs.legacy]
source = "legacy-pack"
`)
	fs.Files["/city/pack.toml"] = []byte(`[pack]
name = "test-city"
schema = 2
`)

	var stderr bytes.Buffer
	_, err := loadCityConfigFS(fs, "/city/city.toml", &stderr)
	if err == nil {
		t.Fatal("loadCityConfigFS unexpectedly accepted unsupported legacy PackV1 surfaces")
	}
	output := err.Error()
	for _, want := range []string{
		"unsupported PackV1 [[agent]] tables",
		"unsupported PackV1 [packs] entries",
		"unsupported PackV1 workspace.includes",
		"unsupported PackV1 workspace.default_rig_includes",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("error missing %q: %q", want, output)
		}
	}
}

func TestResolveAgentIdentityRejectsCanonicalSingletonPoolSuffix(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "frontend", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
		},
	}
	if a, ok := resolveAgentIdentity(cfg, "frontend/worker", ""); !ok || a.QualifiedName() != "frontend/worker" {
		t.Fatalf("resolveAgentIdentity(frontend/worker) = (%q, %v), want canonical template", a.QualifiedName(), ok)
	}
	if _, ok := resolveAgentIdentity(cfg, "frontend/worker-1", ""); ok {
		t.Fatal("resolveAgentIdentity(frontend/worker-1) = true, want false for canonical singleton pool")
	}
	if _, ok := resolveAgentIdentity(cfg, "worker-1", ""); ok {
		t.Fatal("resolveAgentIdentity(worker-1) = true, want false for canonical singleton pool")
	}
}

func TestEmitLoadCityConfigWarningsFiltersNonMigrationWarnings(t *testing.T) {
	var stderr bytes.Buffer
	emitLoadCityConfigWarnings(&stderr, &config.Provenance{
		Warnings: []string{
			`workspace.name redefined by "/city/defaults.toml"`,
			`/city/pack.toml: [agents] is a deprecated compatibility alias for [agent_defaults]; rewrite the table name to [agent_defaults]`,
			`/city/pack.toml: both [agent_defaults] and [agents] are present; [agent_defaults] wins on overlapping keys and [agents] only fills gaps`,
			`/city/pack.toml: "agent_defaults.provider" is not supported in this release wave; keep setting provider per agent in agents/<name>/agent.toml`,
			`/city/city.toml: workspace.provider is deprecated: Set provider per agent in agents/<name>/agent.toml.`,
			`gc: warning: attachment-list fields (` + "`skills`, `mcp`, `skills_append`, `mcp_append`, `shared_skills`" + `) are deprecated as of v0.15.1 and ignored.`,
		},
	})

	output := stderr.String()
	if strings.Contains(output, `workspace.name redefined by "/city/defaults.toml"`) {
		t.Fatalf("non-migration warning should be filtered, got %q", output)
	}
	if !strings.Contains(output, `[agents] is a deprecated compatibility alias for [agent_defaults]`) {
		t.Fatalf("expected alias warning, got %q", output)
	}
	if !strings.Contains(output, `both [agent_defaults] and [agents] are present`) {
		t.Fatalf("expected mixed-table warning, got %q", output)
	}
	if !strings.Contains(output, `"agent_defaults.provider" is not supported`) {
		t.Fatalf("expected unsupported-key warning, got %q", output)
	}
	if strings.Contains(output, `workspace.provider is deprecated`) {
		t.Fatalf("legacy workspace warnings should stay out of generic command stderr, got %q", output)
	}
	if !strings.Contains(output, "attachment-list fields") {
		t.Fatalf("expected attachment deprecation warning, got %q", output)
	}
}

func TestDoAgentSuspendRootPackAgent(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"
`)
	fs.Files["/city/pack.toml"] = []byte(`[pack]
name = "test-city"
schema = 2

[[agent]]
name = "mayor"
`)

	var stdout, stderr bytes.Buffer
	code := doAgentSuspend(fs, "/city", "mayor", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Suspended agent 'mayor'") {
		t.Fatalf("stdout = %q, want suspend message", stdout.String())
	}
	if !strings.Contains(string(fs.Files["/city/pack.toml"]), "suspended = true") {
		t.Fatalf("pack.toml missing suspended = true:\n%s", string(fs.Files["/city/pack.toml"]))
	}
	if strings.Contains(string(fs.Files["/city/city.toml"]), "suspended") {
		t.Fatalf("city.toml should not be rewritten with pack agent suspension:\n%s", string(fs.Files["/city/city.toml"]))
	}
	renamed := false
	for _, call := range fs.Calls {
		if call.Method == "Rename" {
			renamed = true
			break
		}
	}
	if !renamed {
		t.Fatal("expected atomic rename when writing pack.toml")
	}
}

func TestDoAgentResumeRootPackAgent(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"
`)
	fs.Files["/city/pack.toml"] = []byte(`[pack]
name = "test-city"
schema = 2

[[agent]]
name = "mayor"
suspended = true
`)

	var stdout, stderr bytes.Buffer
	code := doAgentResume(fs, "/city", "mayor", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Resumed agent 'mayor'") {
		t.Fatalf("stdout = %q, want resume message", stdout.String())
	}
	if strings.Contains(string(fs.Files["/city/pack.toml"]), "suspended = true") {
		t.Fatalf("pack.toml should clear suspended flag:\n%s", string(fs.Files["/city/pack.toml"]))
	}
	renamed := false
	for _, call := range fs.Calls {
		if call.Method == "Rename" {
			renamed = true
			break
		}
	}
	if !renamed {
		t.Fatal("expected atomic rename when writing pack.toml")
	}
}

func TestDoAgentSuspendRootPackReadError(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"
`)
	fs.Errors["/city/pack.toml"] = fmt.Errorf("permission denied")

	var stdout, stderr bytes.Buffer
	code := doAgentSuspend(fs, "/city", "mayor", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Fatalf("stderr = %q, want permission denied", stderr.String())
	}
}

func TestDoAgentSuspendRootPackPreservesPackFields(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"
`)
	fs.Files["/city/pack.toml"] = []byte(`[pack]
name = "test-city"
schema = 2
version = "0.1.0"
requires_gc = ">=0.16.0"
includes = ["../shared"]

[[pack.requires]]
agent = "mayor"
scope = "city"

[imports.helper]
source = "../helper"

[agent_defaults]
append_fragments = ["shared"]

[providers.claude]
command = "claude"

[formulas]
dir = "custom-formulas"

[[service]]
name = "api"
kind = "workflow"

[[patches.agent]]
name = "mayor"

[[doctor]]
name = "check-env"
script = "doctor/check-env.sh"

[[commands]]
name = "status"
description = "show status"
long_description = "docs/status.md"
script = "commands/status.sh"

[global]
session_live = ["echo live"]

[[agent]]
name = "mayor"
`)

	var stdout, stderr bytes.Buffer
	code := doAgentSuspend(fs, "/city", "mayor", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}

	packToml := string(fs.Files["/city/pack.toml"])
	for _, want := range []string{
		`version = "0.1.0"`,
		`requires_gc = ">=0.16.0"`,
		`includes = ["../shared"]`,
		`[imports.helper]`,
		`append_fragments = ["shared"]`,
		`[providers.claude]`,
		`dir = "custom-formulas"`,
		`[[service]]`,
		`name = "api"`,
		`[[patches.agent]]`,
		`[[doctor]]`,
		`script = "doctor/check-env.sh"`,
		`[[commands]]`,
		`script = "commands/status.sh"`,
		`[global]`,
		`session_live = ["echo live"]`,
		`suspended = true`,
	} {
		if !strings.Contains(packToml, want) {
			t.Fatalf("pack.toml missing %q after suspend:\n%s", want, packToml)
		}
	}
}

func TestStrictFatalLoadConfigWarningsKeepsMixedTableWarningsFatal(t *testing.T) {
	warnings := []string{
		`/city/pack.toml: [agents] is a deprecated compatibility alias for [agent_defaults]; rewrite the table name to [agent_defaults]`,
		`/city/pack.toml: both [agent_defaults] and [agents] are present; [agent_defaults] wins on overlapping keys and [agents] only fills gaps`,
		`/city/pack.toml: "agent_defaults.provider" is not supported in this release wave; keep setting provider per agent in agents/<name>/agent.toml`,
		`/city/city.toml: workspace.provider is deprecated: Set provider per agent in agents/<name>/agent.toml.`,
		`workspace.name redefined by "/city/defaults.toml"`,
	}

	got := strictFatalLoadConfigWarnings(warnings)
	if len(got) != 2 {
		t.Fatalf("strictFatalLoadConfigWarnings len = %d, want 2; got=%q", len(got), got)
	}
	if got[0] != `/city/pack.toml: both [agent_defaults] and [agents] are present; [agent_defaults] wins on overlapping keys and [agents] only fills gaps` {
		t.Fatalf("strictFatalLoadConfigWarnings[0] = %q, want mixed-table warning", got[0])
	}
	if got[1] != `workspace.name redefined by "/city/defaults.toml"` {
		t.Fatalf("strictFatalLoadConfigWarnings[1] = %q, want non-migration warning", got[1])
	}
}

func TestNonTestLoadCityConfigCallersPassWarningWriter(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob(*.go): %v", err)
	}
	bareCall := regexp.MustCompile(`\bloadCityConfig(FS)?\([^,\n)]*\)`)
	var offenders []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") || file == "cmd_agent.go" {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", file, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if bareCall.MatchString(line) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", file, i+1, strings.TrimSpace(line)))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("bare loadCityConfig callers found:\n%s", strings.Join(offenders, "\n"))
	}
}

// ---------------------------------------------------------------------------
// doAgentAdd — v2 scaffold behavior
// ---------------------------------------------------------------------------

func v2CityWithPack(t *testing.T) *fsys.Fake {
	t.Helper()
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte("")
	fs.Files["/city/pack.toml"] = []byte(`[pack]
name = "test-city"
schema = 2
`)
	fs.Files["/city/.gc/site.toml"] = []byte(`workspace_name = "test-city"
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
	if got, ok := fs.Files["/city/agents/worker/agent.toml"]; !ok {
		t.Fatal("agent.toml missing; gc agent add should use the shared convention scaffold writer")
	} else if strings.TrimSpace(string(got)) != "" {
		t.Fatalf("agent.toml = %q, want empty convention config for default scaffold", got)
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

func TestDoAgentAddWritesAgentTomlForSuspended(t *testing.T) {
	fs := v2CityWithPack(t)

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(fs, "/city", "worker", "", "", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	agentToml, ok := fs.Files["/city/agents/worker/agent.toml"]
	if !ok {
		t.Fatal("agent.toml missing")
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
		if a.Dir != "" {
			t.Errorf("Dir = %q, want empty", a.Dir)
		}
		if !a.Suspended {
			t.Error("Suspended = false, want true")
		}
	}
	if !found {
		t.Fatalf("explicit agents = %#v, want worker", explicit)
	}
}

type failAgentTomlRenameFS struct {
	*fsys.Fake
	target string
}

func (f *failAgentTomlRenameFS) Rename(oldpath, newpath string) error {
	if filepath.Clean(newpath) == filepath.Clean(f.target) {
		return errors.New("injected agent.toml write failure")
	}
	return f.Fake.Rename(oldpath, newpath)
}

func TestDoAgentAddRemovesFreshScaffoldWhenConventionConfigWriteFails(t *testing.T) {
	fs := &failAgentTomlRenameFS{
		Fake:   v2CityWithPack(t),
		target: "/city/agents/worker/agent.toml",
	}

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(fs, "/city", "worker", "", "", false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "injected agent.toml write failure") {
		t.Fatalf("stderr = %q, want injected failure", stderr.String())
	}
	for _, path := range []string{
		"/city/agents/worker/prompt.template.md",
		"/city/agents/worker/agent.toml",
	} {
		if _, ok := fs.Files[path]; ok {
			t.Fatalf("%s remains after failed add", path)
		}
	}
	if fs.Dirs["/city/agents/worker"] {
		t.Fatal("fresh agent scaffold directory remains after failed add")
	}
}

func TestDoAgentAddRejectsSymlinkedScaffoldPathBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name             string
		setup            func(t *testing.T, cityDir string) string
		outsideWritePath string
	}{
		{
			name: "agents root",
			setup: func(t *testing.T, cityDir string) string {
				t.Helper()
				outsideAgentsDir := filepath.Join(t.TempDir(), "agents")
				if err := os.MkdirAll(outsideAgentsDir, 0o755); err != nil {
					t.Fatalf("mkdir outside agents: %v", err)
				}
				agentsLink := filepath.Join(cityDir, "agents")
				if err := os.Symlink(outsideAgentsDir, agentsLink); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return agentsLink
			},
			outsideWritePath: filepath.Join("agents", "worker"),
		},
		{
			name: "agent dir",
			setup: func(t *testing.T, cityDir string) string {
				t.Helper()
				agentsDir := filepath.Join(cityDir, "agents")
				if err := os.MkdirAll(agentsDir, 0o755); err != nil {
					t.Fatalf("mkdir agents: %v", err)
				}
				outsideAgentDir := filepath.Join(t.TempDir(), "worker")
				if err := os.MkdirAll(outsideAgentDir, 0o755); err != nil {
					t.Fatalf("mkdir outside agent: %v", err)
				}
				agentLink := filepath.Join(agentsDir, "worker")
				if err := os.Symlink(outsideAgentDir, agentLink); err != nil {
					t.Skipf("symlink unsupported: %v", err)
				}
				return agentLink
			},
			outsideWritePath: "worker",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
				t.Fatalf("write city.toml: %v", err)
			}
			if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test-city\"\nschema = 2\n"), 0o644); err != nil {
				t.Fatalf("write pack.toml: %v", err)
			}
			linkPath := tc.setup(t, cityDir)
			outsidePath := filepath.Join(filepath.Dir(linkPath), tc.outsideWritePath)

			var stdout, stderr bytes.Buffer
			code := doAgentAdd(fsys.OSFS{}, cityDir, "worker", "", "", false, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "not a symlink") {
				t.Fatalf("stderr = %q, want symlink rejection", stderr.String())
			}
			for _, path := range []string{
				filepath.Join(outsidePath, "prompt.template.md"),
				filepath.Join(outsidePath, "agent.toml"),
			} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("%s stat err = %v, want no write through symlink", path, err)
				}
			}
			info, err := os.Lstat(linkPath)
			if err != nil {
				t.Fatalf("lstat symlink: %v", err)
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("link mode = %v, want symlink preserved", info.Mode())
			}
		})
	}
}

func TestDoAgentAddRejectsSchema2RigScopedConventionAgent(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{name: "hello-world/worker"},
		{name: "worker", dir: "hello-world"},
	} {
		t.Run(tc.name+" "+tc.dir, func(t *testing.T) {
			fs := v2CityWithPack(t)

			var stdout, stderr bytes.Buffer
			code := doAgentAdd(fs, "/city", tc.name, "", tc.dir, false, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "schema-2 convention agents are city-scoped") {
				t.Fatalf("stderr = %q, want schema-2 city-scoped validation message", stderr.String())
			}
			for _, path := range []string{
				"/city/agents/worker/agent.toml",
				"/city/agents/worker/prompt.template.md",
			} {
				if _, ok := fs.Files[path]; ok {
					t.Fatalf("%s was created for rejected input", path)
				}
			}
		})
	}
}

func TestDoAgentAddAllowsCityLocalNameSharedWithImportedAgent(t *testing.T) {
	fs := v2CityWithPack(t)
	fs.Files["/city/city.toml"] = []byte(`[imports.helper]
source = "../helper"
`)
	fs.Files["/helper/pack.toml"] = []byte(`[pack]
name = "helper"
schema = 2

[[agent]]
name = "worker"
provider = "claude"
scope = "city"
`)

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(fs, "/city", "worker", "", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}

	cfg, err := loadCityConfigFS(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("loadCityConfigFS: %v", err)
	}
	qualified := map[string]bool{}
	for _, agent := range explicitAgents(cfg.Agents) {
		qualified[agent.QualifiedName()] = true
	}
	for _, want := range []string{"helper.worker", "worker"} {
		if !qualified[want] {
			t.Fatalf("qualified agents = %v, want %q", qualified, want)
		}
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

func TestDoAgentAddRejectsInvalidNamesBeforeFilesystemWrites(t *testing.T) {
	for _, name := range []string{
		"..",
		".hidden",
		"-worker",
		"_worker",
		"worker with space",
		"worker.dotted",
		"rig/worker.dotted",
	} {
		t.Run(name, func(t *testing.T) {
			fs := v2CityWithPack(t)

			var stdout, stderr bytes.Buffer
			code := doAgentAdd(fs, "/city", name, "", "", false, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "name must match") {
				t.Fatalf("stderr = %q, want validation message", stderr.String())
			}
			for _, call := range fs.Calls {
				switch call.Method {
				case "MkdirAll", "WriteFile":
					t.Fatalf("unexpected filesystem mutation before validation: %#v", call)
				}
			}
		})
	}
}

func TestDoAgentSuspendScaffoldedAgentWritesAgentToml(t *testing.T) {
	fs := v2CityWithPack(t)
	if err := fs.MkdirAll("/city/agents/worker", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fs.Files["/city/agents/worker/prompt.template.md"] = []byte("You are the worker.\n")

	var stdout, stderr bytes.Buffer
	code := doAgentSuspend(fs, "/city", "worker", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	agentToml, ok := fs.Files["/city/agents/worker/agent.toml"]
	if !ok {
		t.Fatal("agent.toml missing")
	}
	if !strings.Contains(string(agentToml), "suspended = true") {
		t.Errorf("agent.toml = %q, want suspended = true", agentToml)
	}
	if strings.Contains(string(fs.Files["/city/city.toml"]), "[[patches.agent]]") {
		t.Errorf("city.toml should not gain agent patch:\n%s", fs.Files["/city/city.toml"])
	}

	cfg, err := loadCityConfigFS(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("loadCityConfigFS: %v", err)
	}
	found := false
	for _, a := range cfg.Agents {
		if a.Name != "worker" {
			continue
		}
		found = true
		if !a.Suspended {
			t.Error("Suspended = false, want true")
		}
	}
	if !found {
		t.Fatalf("cfg.Agents = %#v, want worker", cfg.Agents)
	}
}

func TestDoAgentResumeScaffoldedAgentClearsAgentTomlSuspended(t *testing.T) {
	fs := v2CityWithPack(t)
	if err := fs.MkdirAll("/city/agents/worker", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fs.Files["/city/agents/worker/prompt.template.md"] = []byte("You are the worker.\n")
	fs.Files["/city/agents/worker/agent.toml"] = []byte("provider = \"codex\"\nsuspended = true\n")

	var stdout, stderr bytes.Buffer
	code := doAgentResume(fs, "/city", "worker", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	agentToml, ok := fs.Files["/city/agents/worker/agent.toml"]
	if !ok {
		t.Fatal("agent.toml missing")
	}
	if !strings.Contains(string(agentToml), "provider = \"codex\"") {
		t.Errorf("agent.toml = %q, want provider preserved", agentToml)
	}
	if strings.Contains(string(agentToml), "suspended") {
		t.Errorf("agent.toml = %q, want suspended cleared", agentToml)
	}
	if strings.Contains(string(fs.Files["/city/city.toml"]), "[[patches.agent]]") {
		t.Errorf("city.toml should not gain agent patch:\n%s", fs.Files["/city/city.toml"])
	}

	cfg, err := loadCityConfigFS(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("loadCityConfigFS: %v", err)
	}
	found := false
	for _, a := range cfg.Agents {
		if a.Name != "worker" {
			continue
		}
		found = true
		if a.Suspended {
			t.Error("Suspended = true, want false")
		}
		if a.Provider != "codex" {
			t.Errorf("Provider = %q, want codex", a.Provider)
		}
	}
	if !found {
		t.Fatalf("cfg.Agents = %#v, want worker", cfg.Agents)
	}
}

// TestDoAgentSuspendPackDeclaredAgentEditsPackToml ensures the CLI
// fallback (no API) edits the pack.toml [[agent]] entry directly when an
// agent is declared there, even when a conventional prompt template
// exists at agents/<name>/. The conventional prompt template must NOT
// trigger the agent.toml write path because pack.toml takes precedence
// during composition (see internal/configedit.LocalDiscoveredAgent).
//
// This validates the iter-1 finding (was-blocker): a SourceDir-based
// heuristic would have routed the suspend write to a shadowed
// agents/worker/agent.toml. Today the route is updateRootPackAgentSuspended
// (added by #892) → write pack.toml, which is the correct durable
// surface and is consistent with the API path.
func TestDoAgentSuspendPackDeclaredAgentEditsPackToml(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"
`)
	fs.Files["/city/pack.toml"] = []byte(`[pack]
name = "test-city"
schema = 2

[[agent]]
name = "worker"
provider = "claude"
`)
	if err := fs.MkdirAll("/city/agents/worker", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fs.Files["/city/agents/worker/prompt.template.md"] = []byte("You are the worker.\n")

	var stdout, stderr bytes.Buffer
	code := doAgentSuspend(fs, "/city", "worker", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Suspended agent 'worker'") {
		t.Errorf("stdout = %q, want 'Suspended agent worker'", stdout.String())
	}
	// agent.toml must NOT be created — pack.toml [[agent]] is the
	// authoritative declaration and would shadow agent.toml on load.
	if _, ok := fs.Files["/city/agents/worker/agent.toml"]; ok {
		t.Errorf("agent.toml must not be created for pack-declared agent")
	}
	// pack.toml must now carry suspended = true.
	pack := string(fs.Files["/city/pack.toml"])
	if !strings.Contains(pack, "suspended = true") {
		t.Errorf("pack.toml = %q, want suspended = true on the [[agent]] entry", pack)
	}
	// city.toml must NOT gain a [[patches.agent]].
	city := string(fs.Files["/city/city.toml"])
	if strings.Contains(city, "[[patches.agent]]") {
		t.Errorf("city.toml should not gain a patch:\n%s", city)
	}
}

// TestDoAgentResumeStripsLegacyPatchSuspended covers the CLI fallback's
// migration behavior for cities whose city.toml has a stale
// [[patches.agent]] suspended override left behind by older code.
func TestDoAgentResumeStripsLegacyPatchSuspended(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`[workspace]
name = "test-city"

[[patches.agent]]
dir = ""
name = "worker"
suspended = true
`)
	fs.Files["/city/pack.toml"] = []byte(`[pack]
name = "test-city"
schema = 2
`)
	if err := fs.MkdirAll("/city/agents/worker", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	fs.Files["/city/agents/worker/prompt.template.md"] = []byte("You are the worker.\n")
	fs.Files["/city/agents/worker/agent.toml"] = []byte("suspended = true\n")

	var stdout, stderr bytes.Buffer
	code := doAgentResume(fs, "/city", "worker", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	cityToml := string(fs.Files["/city/city.toml"])
	if strings.Contains(cityToml, "[[patches.agent]]") {
		t.Errorf("legacy patch should be stripped; city.toml=\n%s", cityToml)
	}

	cfg, err := loadCityConfigFS(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("loadCityConfigFS: %v", err)
	}
	for _, a := range cfg.Agents {
		if a.Name == "worker" && a.Suspended {
			t.Error("worker should not be suspended after resume")
		}
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
	formulatest.HoldV2ForTest(t)
	oldGraphApply := molecule.IsGraphApplyEnabled()
	t.Cleanup(func() {
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
