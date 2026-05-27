// Package agenticfun_test validates the AgenticFun example configuration.
package agenticfun_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
)

func exampleDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

func loadExpanded(t *testing.T) *config.City {
	t.Helper()
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(exampleDir(), "city.toml"))
	if err != nil {
		t.Fatalf("config.LoadWithIncludes: %v", err)
	}
	if _, err := config.ApplySiteBindings(fsys.OSFS{}, exampleDir(), cfg); err != nil {
		t.Fatalf("config.ApplySiteBindings: %v", err)
	}
	return cfg
}

type packFileConfig struct {
	Pack     config.PackMeta          `toml:"pack"`
	Imports  map[string]config.Import `toml:"imports"`
	Defaults struct {
		Rig struct {
			Imports map[string]config.Import `toml:"imports"`
		} `toml:"rig"`
	} `toml:"defaults"`
}

func discoverPackAgents(t *testing.T) []config.Agent {
	t.Helper()
	packDir := filepath.Join(exampleDir(), "packs", "agenticfun")
	agents, err := config.DiscoverPackAgents(fsys.OSFS{}, packDir, "agenticfun", nil)
	if err != nil {
		t.Fatalf("DiscoverPackAgents: %v", err)
	}
	return agents
}

func TestCityTomlParses(t *testing.T) {
	cfg := loadExpanded(t)
	if cfg.ResolvedWorkspaceName != "agenticfun" {
		t.Errorf("ResolvedWorkspaceName = %q, want agenticfun", cfg.ResolvedWorkspaceName)
	}
	if cfg.Workspace.Provider != "codex" {
		t.Errorf("Workspace.Provider = %q, want codex", cfg.Workspace.Provider)
	}
	if cfg.Session.Provider != "tmux" {
		t.Errorf("Session.Provider = %q, want tmux", cfg.Session.Provider)
	}
}

func TestCityDefinesAgenticFunProjectOrgRigs(t *testing.T) {
	cfg := loadExpanded(t)
	want := map[string]struct {
		path          string
		prefix        string
		defaultBranch string
		projectKind   string
		repoURL       string
		setupCommand  string
		testCommand   string
		buildCommand  string
		preview       string
	}{
		"equipments": {
			path:          "repos/equipments",
			prefix:        "eq",
			defaultBranch: "master",
			projectKind:   "typescript-service",
			repoURL:       "https://github.com/AgenticFunProject/equipments",
			setupCommand:  "npm install",
			testCommand:   "npm test",
			buildCommand:  "npm run build",
			preview:       "npm run dev",
		},
		"quotes": {
			path:          "repos/quotes",
			prefix:        "qu",
			defaultBranch: "main",
			projectKind:   "python-fastapi-service",
			repoURL:       "https://github.com/AgenticFunProject/quotes",
			setupCommand:  "./scripts/bootstrap-venv.sh",
			testCommand:   ". .venv/bin/activate && pytest",
			buildCommand:  ". .venv/bin/activate && python -m build",
			preview:       ". .venv/bin/activate && uvicorn app.main:app --reload",
		},
		"web-page": {
			path:          "repos/web-page",
			prefix:        "wp",
			defaultBranch: "main",
			projectKind:   "static-js-demo",
			repoURL:       "https://github.com/AgenticFunProject/web-page",
			setupCommand:  "true",
			testCommand:   "true",
			buildCommand:  "true",
			preview:       "python3 -m http.server 8080 --directory .",
		},
		"booking": {
			path:          "repos/booking",
			prefix:        "bo",
			defaultBranch: "master",
			projectKind:   "java-spring-spec-service",
			repoURL:       "https://github.com/AgenticFunProject/booking",
			setupCommand:  "true",
			testCommand:   "./mvnw test",
			buildCommand:  "./mvnw compile",
			preview:       "./mvnw spring-boot:run -Dspring-boot.run.profiles=local",
		},
		"users": {
			path:          "repos/users",
			prefix:        "us",
			defaultBranch: "main",
			projectKind:   "typescript-service",
			repoURL:       "https://github.com/AgenticFunProject/users",
			setupCommand:  "npm install",
			testCommand:   "npm test",
			buildCommand:  "npm run build",
			preview:       "npm run dev",
		},
	}
	found := map[string]config.Rig{}
	for _, rig := range cfg.Rigs {
		found[rig.Name] = rig
	}
	for name, wantRig := range want {
		rig, ok := found[name]
		if !ok {
			t.Errorf("missing rig %q", name)
			continue
		}
		if rig.Path != wantRig.path {
			t.Errorf("rig %q path = %q, want %q", name, rig.Path, wantRig.path)
		}
		if rig.Prefix != wantRig.prefix {
			t.Errorf("rig %q prefix = %q, want %q", name, rig.Prefix, wantRig.prefix)
		}
		if rig.DefaultBranch != wantRig.defaultBranch {
			t.Errorf("rig %q default_branch = %q, want %q", name, rig.DefaultBranch, wantRig.defaultBranch)
		}
		if rig.DefaultSlingTarget != "agenticfun.builder" {
			t.Errorf("rig %q default_sling_target = %q, want agenticfun.builder", name, rig.DefaultSlingTarget)
		}
		if got := rig.FormulaVars["project_kind"]; got != wantRig.projectKind {
			t.Errorf("rig %q project_kind = %q, want %q", name, got, wantRig.projectKind)
		}
		if got := rig.FormulaVars["repo_url"]; got != wantRig.repoURL {
			t.Errorf("rig %q repo_url = %q, want %q", name, got, wantRig.repoURL)
		}
		if got := rig.FormulaVars["setup_command"]; got != wantRig.setupCommand {
			t.Errorf("rig %q setup_command = %q, want %q", name, got, wantRig.setupCommand)
		}
		if got := rig.FormulaVars["test_command"]; got != wantRig.testCommand {
			t.Errorf("rig %q test_command = %q, want %q", name, got, wantRig.testCommand)
		}
		if got := rig.FormulaVars["build_command"]; got != wantRig.buildCommand {
			t.Errorf("rig %q build_command = %q, want %q", name, got, wantRig.buildCommand)
		}
		if got := rig.FormulaVars["preview_command"]; got != wantRig.preview {
			t.Errorf("rig %q preview_command = %q, want %q", name, got, wantRig.preview)
		}
	}
	if len(cfg.Rigs) != len(want) {
		t.Errorf("got %d rigs, want %d", len(cfg.Rigs), len(want))
	}
}

func TestRootPackImportsAgenticFunForCityAndRigConfigs(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(exampleDir(), "pack.toml"))
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}
	var pc packFileConfig
	if _, err := toml.Decode(string(data), &pc); err != nil {
		t.Fatalf("parsing pack.toml: %v", err)
	}
	if pc.Pack.Name != "agenticfun" {
		t.Errorf("[pack] name = %q, want agenticfun", pc.Pack.Name)
	}
	if pc.Pack.Schema != 2 {
		t.Errorf("[pack] schema = %d, want 2", pc.Pack.Schema)
	}
	if pc.Imports["agenticfun"].Source != "packs/agenticfun" {
		t.Errorf("imports.agenticfun.source = %q, want packs/agenticfun", pc.Imports["agenticfun"].Source)
	}
	if len(pc.Defaults.Rig.Imports) != 0 {
		t.Errorf("defaults.rig.imports = %v, want rig imports declared in city.toml", pc.Defaults.Rig.Imports)
	}
	cfg := loadExpanded(t)
	for _, rig := range cfg.Rigs {
		if rig.Imports["agenticfun"].Source != "packs/agenticfun" {
			t.Errorf("rig %q imports.agenticfun.source = %q, want packs/agenticfun", rig.Name, rig.Imports["agenticfun"].Source)
		}
	}
}

func TestAgenticFunBootstrapBuildsCityLocalGCBinary(t *testing.T) {
	scriptPath := filepath.Join(exampleDir(), "scripts", "build-gc.sh")
	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("stat build-gc.sh: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Fatalf("build-gc.sh is not executable: mode %s", info.Mode())
	}
	data, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read build-gc.sh: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		"go build",
		"-trimpath",
		"-ldflags=-s -w",
		"./cmd/gc",
		".gc/bin",
		"/gc",
		"export PATH=",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("build-gc.sh missing %q", want)
		}
	}
}

func TestAgenticFunUsesResourceConservativeDaemonCadence(t *testing.T) {
	cfg := loadExpanded(t)
	if cfg.Daemon.PatrolInterval != "2m" {
		t.Errorf("daemon patrol_interval = %q, want 2m", cfg.Daemon.PatrolInterval)
	}
}

func TestAgenticFunDocsPutCityLocalGCBinaryOnPATH(t *testing.T) {
	for _, name := range []string{"README.md", "USAGE.md", "OPERATIONS.md"} {
		data, err := os.ReadFile(filepath.Join(exampleDir(), name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(data)
		if !strings.Contains(body, "./scripts/build-gc.sh") {
			t.Errorf("%s does not mention ./scripts/build-gc.sh", name)
		}
		if !strings.Contains(body, `.gc/bin`) {
			t.Errorf("%s does not mention .gc/bin", name)
		}
	}
}

func TestPackDefinesProjectSpecificAgents(t *testing.T) {
	agents := discoverPackAgents(t)
	want := map[string]string{
		"director":      "city",
		"hq-builder":    "city",
		"hq-reviewer":   "city",
		"hq-integrator": "city",
		"ops":           "city",
		"architect":     "rig",
		"builder":       "rig",
		"reviewer":      "rig",
		"playtester":    "rig",
		"integrator":    "rig",
	}
	found := map[string]config.Agent{}
	for _, agent := range agents {
		found[agent.Name] = agent
	}
	for name, scope := range want {
		agent, ok := found[name]
		if !ok {
			t.Errorf("missing pack agent %q", name)
			continue
		}
		if agent.Scope != scope {
			t.Errorf("agent %q scope = %q, want %q", name, agent.Scope, scope)
		}
		if agent.PromptTemplate == "" {
			t.Errorf("agent %q has empty prompt template", name)
		}
		if agent.Provider != "codex" {
			t.Errorf("agent %q provider = %q, want codex", name, agent.Provider)
		}
	}
	if len(agents) != len(want) {
		t.Errorf("pack has %d agents, want %d", len(agents), len(want))
	}
}

func TestPackKeepsOnlyDirectorAlwaysOn(t *testing.T) {
	cfg := loadExpanded(t)
	want := map[string]string{
		"agenticfun.director":      "always",
		"agenticfun.hq-builder":    "on_demand",
		"agenticfun.hq-reviewer":   "on_demand",
		"agenticfun.hq-integrator": "on_demand",
		"agenticfun.ops":           "on_demand",
	}
	got := map[string]string{}
	for _, session := range cfg.NamedSessions {
		template := session.TemplateQualifiedName()
		if !strings.Contains(template, "agenticfun.") {
			continue
		}
		got[template] = session.ModeOrDefault()
		if session.Scope == "rig" {
			t.Errorf("named_session %q is rig-scoped; AgenticFun should wake repo agents on demand", session.QualifiedName())
		}
	}
	for name, mode := range want {
		if got[name] != mode {
			t.Errorf("named_session %q mode = %q, want %q", name, got[name], mode)
		}
	}
	if len(got) != len(want) {
		t.Errorf("named sessions = %v, want only %v", got, want)
	}
}

func TestDirectorHasBacklogTriageWorkQuery(t *testing.T) {
	var director *config.Agent
	for _, agent := range discoverPackAgents(t) {
		if agent.Name == "director" {
			director = &agent
			break
		}
	}
	if director == nil {
		t.Fatal("missing director agent")
	}
	if director.WorkQuery == "" {
		t.Fatal("director work_query is empty; gc hook would only see directly assigned work")
	}
	for _, want := range []string{"bd ready", "--exclude-type=epic", "--exclude-type=session", "--json"} {
		if !strings.Contains(director.WorkQuery, want) {
			t.Errorf("director work_query = %q, want substring %q", director.WorkQuery, want)
		}
	}
}

func TestDirectorDocumentsHQBuilderDelegation(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("packs", "agenticfun", "agents", "director", "prompt.template.md"))
	if err != nil {
		t.Fatalf("ReadFile(director prompt): %v", err)
	}
	if !strings.Contains(string(data), "hq-builder") {
		t.Fatal("director prompt must document the city-scoped HQ builder target")
	}
}

func TestAgenticFunDelegationTargetsResolveToWakeableAgents(t *testing.T) {
	cfg := loadExpanded(t)
	explicit := map[string]config.Agent{}
	for _, agent := range cfg.Agents {
		if agent.Implicit {
			continue
		}
		explicit[agent.QualifiedName()] = agent
	}

	var targets []string
	targets = append(targets, "agenticfun.director", "agenticfun.hq-builder", "agenticfun.hq-reviewer", "agenticfun.hq-integrator", "agenticfun.ops")
	for _, rig := range cfg.Rigs {
		for _, role := range []string{"architect", "builder", "reviewer", "playtester", "integrator"} {
			targets = append(targets, rig.Name+"/agenticfun."+role)
		}
	}

	for _, target := range targets {
		agent, ok := explicit[target]
		if !ok {
			t.Errorf("delegation target %q is not a configured explicit agent", target)
			continue
		}
		if target != "agenticfun.director" && agent.EffectiveMinActiveSessions() != 0 {
			t.Errorf("delegation target %q min_active_sessions = %d, want 0 so it wakes only on demand", target, agent.EffectiveMinActiveSessions())
		}
		if max := agent.EffectiveMaxActiveSessions(); max == nil || *max < 1 {
			t.Errorf("delegation target %q max_active_sessions = %v, want at least 1", target, max)
		}
	}
}

func TestRolePromptsUseWakeProducingDelegationCommands(t *testing.T) {
	packDir := filepath.Join(exampleDir(), "packs", "agenticfun")
	checks := map[string][]string{
		filepath.Join("agents", "director", "prompt.template.md"): {
			"gc sling <rig>/{{ .BindingPrefix }}architect",
			"gc sling <rig>/{{ .BindingPrefix }}builder <bead-id> --on mol-agenticfun-slice-build --nudge",
			"gc sling {{ .BindingPrefix }}hq-builder <task-bead-id> --nudge",
			"gc sling {{ .BindingPrefix }}hq-reviewer <bead-id> --nudge",
			"gc sling {{ .BindingPrefix }}hq-integrator <bead-id> --nudge",
		},
		filepath.Join("agents", "architect", "prompt.template.md"): {
			"gc sling {{ .RigName }}/{{ .BindingPrefix }}builder <bead-id> --on mol-agenticfun-slice-build --nudge",
		},
		filepath.Join("agents", "builder", "prompt.template.md"): {
			"gc sling {{ .RigName }}/{{ .BindingPrefix }}reviewer <bead-id> --nudge",
		},
		filepath.Join("agents", "reviewer", "prompt.template.md"): {
			"gc sling {{ .RigName }}/{{ .BindingPrefix }}builder <bead-id> --on mol-agenticfun-slice-build --nudge",
			"gc sling {{ .RigName }}/{{ .BindingPrefix }}playtester <bead-id> --on mol-agenticfun-playtest-loop --nudge",
			"gc sling {{ .RigName }}/{{ .BindingPrefix }}integrator <bead-id> --on mol-agenticfun-integrate --nudge",
		},
		filepath.Join("agents", "playtester", "prompt.template.md"): {
			"gc sling {{ .RigName }}/{{ .BindingPrefix }}integrator <bead-id> --on mol-agenticfun-integrate --nudge",
			"gc sling {{ .RigName }}/{{ .BindingPrefix }}builder <bead-id> --on mol-agenticfun-slice-build --nudge",
		},
		filepath.Join("agents", "integrator", "prompt.template.md"): {
			"gc sling {{ .RigName }}/{{ .BindingPrefix }}builder <bead-id> --on mol-agenticfun-slice-build --nudge",
		},
		filepath.Join("agents", "hq-builder", "prompt.template.md"): {
			"gc sling agenticfun.hq-reviewer <bead-id> --nudge",
		},
		filepath.Join("agents", "hq-reviewer", "prompt.template.md"): {
			"gc sling agenticfun.hq-builder <bead-id> --nudge",
			"gc sling agenticfun.hq-integrator <bead-id> --nudge",
		},
		filepath.Join("agents", "hq-integrator", "prompt.template.md"): {
			"gc sling agenticfun.hq-builder <bead-id> --nudge",
		},
	}
	for rel, wants := range checks {
		data, err := os.ReadFile(filepath.Join(packDir, rel))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", rel, err)
		}
		text := string(data)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing delegation command %q", rel, want)
			}
		}
	}
}

func TestWorkflowFormulasUseWakeProducingHandoffs(t *testing.T) {
	formulaDir := filepath.Join(exampleDir(), "packs", "agenticfun", "formulas")
	checks := map[string][]string{
		"mol-agenticfun-idea-to-slice.toml": {
			"gc sling \"$TARGET\" <bead-id> --on mol-agenticfun-slice-build --nudge",
		},
		"mol-agenticfun-slice-build.toml": {
			"gc sling \"$TARGET\" <bead-id> --nudge",
		},
		"mol-agenticfun-playtest-loop.toml": {
			"gc sling \"{{integrator_target}}\" <bead-id> --on mol-agenticfun-integrate --nudge",
			"gc sling \"{{builder_target}}\" <bead-id> --on mol-agenticfun-slice-build --nudge",
		},
		"mol-agenticfun-integrate.toml": {
			"gc sling \"$GC_RIG/{{binding_prefix}}builder\" <bead-id> --on mol-agenticfun-slice-build --nudge",
		},
	}
	for name, wants := range checks {
		data, err := os.ReadFile(filepath.Join(formulaDir, name))
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		text := string(data)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing wake-producing handoff %q", name, want)
			}
		}
	}
}

func TestAgenticFunCityStartsUnsuspendedForDelegation(t *testing.T) {
	cfg := loadExpanded(t)
	if cfg.Workspace.Suspended {
		t.Fatal("AgenticFun city must start unsuspended so director can list, create, and sling beads")
	}
}

func TestHQReviewAndIntegrationTargetsAreCityScoped(t *testing.T) {
	cfg := loadExpanded(t)
	agents := map[string]config.Agent{}
	for _, agent := range cfg.Agents {
		if agent.Implicit {
			continue
		}
		agents[agent.QualifiedName()] = agent
	}

	for _, target := range []string{"agenticfun.hq-reviewer", "agenticfun.hq-integrator"} {
		agent, ok := agents[target]
		if !ok {
			t.Fatalf("missing HQ handoff target %q", target)
		}
		if agent.Scope != "city" {
			t.Errorf("HQ handoff target %q scope = %q, want city", target, agent.Scope)
		}
		if agent.Dir != "" {
			t.Errorf("HQ handoff target %q dir = %q, want city scope with no rig dir", target, agent.Dir)
		}
		if max := agent.EffectiveMaxActiveSessions(); max == nil || *max < 1 {
			t.Errorf("HQ handoff target %q max_active_sessions = %v, want at least 1", target, max)
		}
	}

	if _, ok := agents["agenticfun.reviewer"]; ok {
		t.Fatal("city-scoped agenticfun.reviewer should remain absent; product reviewers are rig-scoped")
	}
	if _, ok := agents["agenticfun.integrator"]; ok {
		t.Fatal("city-scoped agenticfun.integrator should remain absent; product integrators are rig-scoped")
	}
}

func TestHQRepoRootAgentsUseCityAwareHookWorkQuery(t *testing.T) {
	cfg := loadExpanded(t)
	agents := map[string]config.Agent{}
	for _, agent := range cfg.Agents {
		if agent.Implicit {
			continue
		}
		agents[agent.QualifiedName()] = agent
	}

	for _, target := range []string{"agenticfun.hq-builder", "agenticfun.hq-reviewer", "agenticfun.hq-integrator"} {
		agent, ok := agents[target]
		if !ok {
			t.Fatalf("missing HQ handoff target %q", target)
		}
		if agent.WorkDir != "../.." {
			t.Errorf("HQ repo-root agent %q work_dir = %q, want ../..", target, agent.WorkDir)
		}
		if strings.TrimSpace(agent.WorkQuery) == "" {
			t.Fatalf("HQ repo-root agent %q has empty work_query; gc hook would fall back to raw bd from repo root", target)
		}
		for _, want := range []string{
			"gc bd list --status in_progress",
			"gc bd ready --assignee",
			"gc bd ready --metadata-field gc.routed_to=" + target,
			"--unassigned",
			"--exclude-type=epic",
			"--json",
		} {
			if !strings.Contains(agent.WorkQuery, want) {
				t.Errorf("HQ repo-root agent %q work_query = %q, want substring %q", target, agent.WorkQuery, want)
			}
		}
		if strings.Contains(agent.WorkQuery, "bd ready --metadata-field gc.routed_to="+target) &&
			!strings.Contains(agent.WorkQuery, "gc bd ready --metadata-field gc.routed_to="+target) {
			t.Errorf("HQ repo-root agent %q work_query routes through raw bd instead of gc bd: %q", target, agent.WorkQuery)
		}

		fakeBin := t.TempDir()
		logPath := filepath.Join(fakeBin, "gc.calls")
		fakeGC := filepath.Join(fakeBin, "gc")
		if err := os.WriteFile(fakeGC, []byte(`#!/bin/sh
printf '%s|%s\n' "$PWD" "$*" >> "$GC_FAKE_LOG"
if [ "$1" = "bd" ] && [ "$2" = "ready" ] && [ "$3" = "--metadata-field" ]; then
	case "$4" in
		gc.routed_to=agenticfun.hq-*)
			printf '[{"id":"ag-test"}]'
			exit 0
			;;
	esac
fi
printf '[]'
`), 0o700); err != nil {
			t.Fatalf("WriteFile(fake gc): %v", err)
		}

		repoRoot := filepath.Clean(filepath.Join(exampleDir(), "..", ".."))
		cmd := exec.Command("sh", "-c", agent.WorkQuery)
		cmd.Dir = repoRoot
		cmd.Env = []string{
			"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"GC_FAKE_LOG=" + logPath,
			"GC_SESSION_ID=",
			"GC_SESSION_NAME=",
			"GC_ALIAS=",
			"GC_AGENT=",
			"GC_TEMPLATE=",
			"GC_TEMPLATE_SESSION_NAME=",
			"GC_SESSION_ORIGIN=",
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("HQ repo-root agent %q work_query failed from %s: %v", target, repoRoot, err)
		}
		if got, want := strings.TrimSpace(string(out)), `[{"id":"ag-test"}]`; got != want {
			t.Fatalf("HQ repo-root agent %q work_query output = %q, want %q", target, got, want)
		}
		calls, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatalf("ReadFile(fake gc log): %v", err)
		}
		wantCall := repoRoot + "|bd ready --metadata-field gc.routed_to=" + target
		if !strings.Contains(string(calls), wantCall) {
			t.Fatalf("HQ repo-root agent %q work_query calls = %q, want call containing %q", target, calls, wantCall)
		}
	}
}

func TestAgenticFunControlDispatchersDoNotPreinitializeTmuxSessions(t *testing.T) {
	cfg := loadExpanded(t)
	want := map[string]bool{
		"control-dispatcher":            true,
		"equipments/control-dispatcher": true,
		"quotes/control-dispatcher":     true,
		"web-page/control-dispatcher":   true,
		"booking/control-dispatcher":    true,
		"users/control-dispatcher":      true,
	}
	for _, session := range cfg.NamedSessions {
		identity := session.QualifiedName()
		if !want[identity] {
			continue
		}
		if got := session.ModeOrDefault(); got != "on_demand" {
			t.Errorf("control dispatcher named_session %q mode = %q, want on_demand", identity, got)
		}
		delete(want, identity)
	}
	for identity := range want {
		t.Errorf("missing control dispatcher named_session %q", identity)
	}
}

func TestPackCapsConcurrentAgenticFunSessionsConservatively(t *testing.T) {
	for _, agent := range discoverPackAgents(t) {
		maxActive := agent.EffectiveMaxActiveSessions()
		if maxActive == nil {
			t.Errorf("agent %q max_active_sessions is unset; AgenticFun should cap expensive Codex sessions", agent.Name)
			continue
		}
		if agent.Scope == "city" && *maxActive > 1 {
			t.Errorf("city agent %q max_active_sessions = %d, want <= 1", agent.Name, *maxActive)
		}
		if agent.Scope == "rig" && *maxActive > 2 {
			t.Errorf("rig agent %q max_active_sessions = %d, want <= 2", agent.Name, *maxActive)
		}
	}
}

func TestRigScopedAgentsAreDemandDrivenAndSleepAfterIdle(t *testing.T) {
	agents := discoverPackAgents(t)
	wantRigScoped := map[string]bool{
		"architect":  true,
		"builder":    true,
		"reviewer":   true,
		"playtester": true,
		"integrator": true,
	}
	for _, agent := range agents {
		if agent.Scope != "rig" {
			continue
		}
		if !wantRigScoped[agent.Name] {
			t.Errorf("unexpected rig-scoped agent %q", agent.Name)
			continue
		}
		if got := agent.EffectiveMinActiveSessions(); got != 0 {
			t.Errorf("agent %q min_active_sessions = %d, want 0 for demand-driven startup", agent.Name, got)
		}
		if agent.SleepAfterIdle != "5m" {
			t.Errorf("agent %q sleep_after_idle = %q, want 5m", agent.Name, agent.SleepAfterIdle)
		}
		delete(wantRigScoped, agent.Name)
	}
	for name := range wantRigScoped {
		t.Errorf("missing rig-scoped agent %q", name)
	}
}

func TestCityExpandsOrgRigAgents(t *testing.T) {
	cfg := loadExpanded(t)
	if err := config.ValidateAgents(cfg.Agents); err != nil {
		t.Fatalf("ValidateAgents: %v", err)
	}
	explicit := map[string]config.Agent{}
	for _, agent := range cfg.Agents {
		if agent.Implicit {
			continue
		}
		name := agent.Name
		if agent.BindingName != "" {
			name = agent.BindingName + "." + name
		}
		if agent.Dir != "" {
			name = agent.Dir + "/" + name
		}
		explicit[name] = agent
	}
	want := []string{
		"agenticfun.director",
		"agenticfun.hq-builder",
		"agenticfun.hq-reviewer",
		"agenticfun.hq-integrator",
		"agenticfun.ops",
		"equipments/agenticfun.architect",
		"equipments/agenticfun.builder",
		"equipments/agenticfun.reviewer",
		"equipments/agenticfun.playtester",
		"equipments/agenticfun.integrator",
		"quotes/agenticfun.architect",
		"quotes/agenticfun.builder",
		"quotes/agenticfun.reviewer",
		"quotes/agenticfun.playtester",
		"quotes/agenticfun.integrator",
		"web-page/agenticfun.architect",
		"web-page/agenticfun.builder",
		"web-page/agenticfun.reviewer",
		"web-page/agenticfun.playtester",
		"web-page/agenticfun.integrator",
		"booking/agenticfun.architect",
		"booking/agenticfun.builder",
		"booking/agenticfun.reviewer",
		"booking/agenticfun.playtester",
		"booking/agenticfun.integrator",
		"users/agenticfun.architect",
		"users/agenticfun.builder",
		"users/agenticfun.reviewer",
		"users/agenticfun.playtester",
		"users/agenticfun.integrator",
	}
	for _, name := range want {
		if _, ok := explicit[name]; !ok {
			t.Errorf("missing expanded agent %q", name)
		}
	}
	if len(explicit) != len(want) {
		t.Fatalf("expanded explicit agents = %d, want %d", len(explicit), len(want))
	}
}

func TestPromptTemplatesExist(t *testing.T) {
	for _, agent := range discoverPackAgents(t) {
		if agent.PromptTemplate == "" {
			continue
		}
		if _, err := os.Stat(agent.PromptTemplate); err != nil {
			t.Errorf("agent %q prompt_template %q: %v", agent.Name, agent.PromptTemplate, err)
		}
	}
}

func TestWorkspaceGlobalFragmentsExist(t *testing.T) {
	cfg := loadExpanded(t)
	for _, name := range cfg.Workspace.GlobalFragments {
		path := filepath.Join(exampleDir(), "packs", "agenticfun", "template-fragments", name+".template.md")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("workspace global fragment %q at %s: %v", name, path, err)
		}
		tmpl, err := template.New("fragment").Parse(string(data))
		if err != nil {
			t.Fatalf("parse workspace global fragment %q: %v", name, err)
		}
		if tmpl.Lookup(name) == nil {
			t.Fatalf("workspace global fragment %q does not define template %q", name, name)
		}
	}
}

func TestAgenticFunFormulasParse(t *testing.T) {
	dir := filepath.Join(exampleDir(), "packs", "agenticfun", "formulas")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading formulas dir: %v", err)
	}
	want := map[string]bool{
		"mol-agenticfun-idea-to-slice.toml": false,
		"mol-agenticfun-slice-build.toml":   false,
		"mol-agenticfun-playtest-loop.toml": false,
		"mol-agenticfun-integrate.toml":     false,
		"mol-agenticfun-fun-converge.toml":  false,
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		want[entry.Name()] = true
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("reading formula %s: %v", entry.Name(), err)
		}
		parsed, err := formula.NewParser(dir).ParseTOML(data)
		if err != nil {
			t.Errorf("formula %s does not parse: %v", entry.Name(), err)
			continue
		}
		if err := parsed.Validate(); err != nil {
			t.Errorf("formula %s does not validate: %v", entry.Name(), err)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing formula %s", name)
		}
	}
}

func TestAgenticFunOrdersParseAndValidate(t *testing.T) {
	scanned, err := orders.ScanRoots(fsys.OSFS{}, []orders.ScanRoot{{
		Dir: filepath.Join(exampleDir(), "packs", "agenticfun", "orders"),
	}}, nil)
	if err != nil {
		t.Fatalf("orders.ScanRoots: %v", err)
	}
	want := map[string]string{
		"stale-branch-sweep": "exec",
		"preview-health":     "exec",
		"playtest-cadence":   "mol-agenticfun-playtest-loop",
	}
	found := map[string]orders.Order{}
	for _, order := range scanned {
		found[order.Name] = order
		if err := orders.Validate(order); err != nil {
			t.Errorf("order %q invalid: %v", order.Name, err)
		}
	}
	for name, dispatch := range want {
		order, ok := found[name]
		if !ok {
			t.Errorf("missing order %q", name)
			continue
		}
		if dispatch == "exec" && order.Exec == "" {
			t.Errorf("order %q Exec is empty", name)
		}
		if dispatch != "exec" && order.Formula != dispatch {
			t.Errorf("order %q Formula = %q, want %q", name, order.Formula, dispatch)
		}
	}
}
