package main

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gascitypacks "github.com/gastownhall/gascity-packs"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

const formulaFilesystemSearchGuidance = "**Never use wide filesystem searches when a CLI command exists.**"

// materializeEmbeddedGastownPack writes the module-embedded gastown pack
// (the exact bytes the gc binary bundles) to a t.TempDir()-rooted directory
// and returns the pack root (pack.toml at top level). The checked-in
// example no longer carries a pack copy — the pack arrives via the pinned
// public import — so pack-content tests run against the embedded bytes,
// with the same file modes the runtime materializer applies.
func materializeEmbeddedGastownPack(t *testing.T) string {
	t.Helper()
	src := gascitypacks.Gastown()
	root := filepath.Join(t.TempDir(), "gastown")
	err := fs.WalkDir(src, ".", func(rel string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		dst := filepath.Join(root, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := fs.ReadFile(src, rel)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, data, builtinpacks.MaterializedFileMode(rel))
	})
	if err != nil {
		t.Fatalf("materializing embedded gastown pack: %v", err)
	}
	return root
}

func TestRenderPromptEmptyPath(t *testing.T) {
	f := fsys.NewFake()
	got := renderPrompt(f, "/city", "", "", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "" {
		t.Errorf("renderPrompt(empty path) = %q, want empty", got)
	}
}

func TestRenderPromptMissingFile(t *testing.T) {
	f := fsys.NewFake()
	got := renderPrompt(f, "/city", "", "prompts/missing.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "" {
		t.Errorf("renderPrompt(missing) = %q, want empty", got)
	}
}

func TestRenderPromptNoExpressions(t *testing.T) {
	f := fsys.NewFake()
	content := "# Simple Prompt\n\nNo template expressions here.\n"
	f.Files["/city/prompts/plain.md"] = []byte(content)
	got := renderPrompt(f, "/city", "", "prompts/plain.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != content {
		t.Errorf("renderPrompt(plain) = %q, want %q", got, content)
	}
}

func TestRenderPromptPlainMarkdownDoesNotExecuteTemplates(t *testing.T) {
	f := fsys.NewFake()
	content := "Hello {{ .AgentName }}\n"
	f.Files["/city/prompts/plain.md"] = []byte(content)
	got := renderPrompt(f, "/city", "", "prompts/plain.md", PromptContext{AgentName: "mayor"}, "", io.Discard, nil, nil, nil)
	if got != content {
		t.Errorf("renderPrompt(plain markdown) = %q, want raw content %q", got, content)
	}
}

func TestRenderPromptBasicVars(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.template.md"] = []byte("City: {{ .CityRoot }}\nAgent: {{ .AgentName }}\n")
	ctx := PromptContext{
		CityRoot:  "/home/user/bright-lights",
		AgentName: "hello-world/polecat-1",
	}
	got := renderPrompt(f, "/city", "bright-lights", "prompts/test.template.md", ctx, "", io.Discard, nil, nil, nil)
	want := "City: /home/user/bright-lights\nAgent: hello-world/polecat-1\n"
	if got != want {
		t.Errorf("renderPrompt(vars) = %q, want %q", got, want)
	}
}

func TestRenderPromptAbsolutePath(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/agents/ada/prompt.template.md"] = []byte("Agent: {{ .AgentName }}\n")
	got := renderPrompt(f, "/city", "", "/city/agents/ada/prompt.template.md", PromptContext{AgentName: "ada"}, "", io.Discard, nil, nil, nil)
	if got != "Agent: ada\n" {
		t.Errorf("renderPrompt(absolute path) = %q, want %q", got, "Agent: ada\n")
	}
}

func TestRenderPromptLegacyTemplateSuffixStillRenders(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Agent: {{ .AgentName }}\n")
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{AgentName: "mayor"}, "", io.Discard, nil, nil, nil)
	if got != "Agent: mayor\n" {
		t.Errorf("renderPrompt(legacy suffix) = %q, want %q", got, "Agent: mayor\n")
	}
}

func TestRenderPromptCanonicalSharedTemplateOverridesLegacy(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.template.md"] = []byte(`Hello {{ template "footer" . }}`)
	f.Files["/city/prompts/shared/footer.md.tmpl"] = []byte(`{{ define "footer" }}legacy{{ end }}`)
	f.Files["/city/prompts/shared/footer.template.md"] = []byte(`{{ define "footer" }}canonical{{ end }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "Hello canonical" {
		t.Errorf("renderPrompt(canonical shared override) = %q, want %q", got, "Hello canonical")
	}
}

func TestRenderPromptAgentsAliasAppendFragmentsAffectRenderedPrompt(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[agents]
append_fragments = ["footer"]

[[agent]]
name = "mayor"
prompt_template = "agents/mayor/prompt.template.md"
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	f := fsys.NewFake()
	f.Files["/city/agents/mayor/prompt.template.md"] = []byte("Hello")
	f.Files["/city/agents/mayor/template-fragments/footer.template.md"] = []byte(`{{ define "footer" }}Goodbye{{ end }}`)
	got := renderPrompt(f, "/city", "", "agents/mayor/prompt.template.md", PromptContext{}, "", io.Discard, nil, cfg.AgentDefaults.AppendFragments, nil)
	if got != "Hello\n\nGoodbye" {
		t.Errorf("renderPrompt(agents alias append_fragments) = %q, want %q", got, "Hello\n\nGoodbye")
	}
}

func TestRenderPromptAgentDefaultsAppendFragmentsAffectRenderedPrompt(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[agent_defaults]
append_fragments = ["footer"]

[[agent]]
name = "mayor"
prompt_template = "agents/mayor/prompt.template.md"
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	f := fsys.NewFake()
	f.Files["/city/agents/mayor/prompt.template.md"] = []byte("Hello")
	f.Files["/city/agents/mayor/template-fragments/footer.template.md"] = []byte(`{{ define "footer" }}Goodbye{{ end }}`)
	got := renderPrompt(f, "/city", "", "agents/mayor/prompt.template.md", PromptContext{}, "", io.Discard, nil, cfg.AgentDefaults.AppendFragments, nil)
	if got != "Hello\n\nGoodbye" {
		t.Errorf("renderPrompt(agent_defaults append_fragments) = %q, want %q", got, "Hello\n\nGoodbye")
	}
}

func TestRenderPromptAgentBlockAppendFragmentsAffectRenderedPrompt(t *testing.T) {
	data := []byte(`
[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "agents/mayor/prompt.template.md"
append_fragments = ["footer"]
`)
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("config.Parse: %v", err)
	}
	var mayor config.Agent
	found := false
	for _, a := range cfg.Agents {
		if a.Name == "mayor" {
			mayor = a
			found = true
			break
		}
	}
	if !found {
		t.Fatalf(`expected [[agent]] with name "mayor" in parsed config`)
	}
	if got := mayor.AppendFragments; len(got) != 1 || got[0] != "footer" {
		t.Fatalf("[[agent]] AppendFragments = %v, want [footer]", got)
	}
	f := fsys.NewFake()
	f.Files["/city/agents/mayor/prompt.template.md"] = []byte("Hello")
	f.Files["/city/agents/mayor/template-fragments/footer.template.md"] = []byte(`{{ define "footer" }}Goodbye{{ end }}`)
	fragments := effectivePromptFragments(
		cfg.Workspace.GlobalFragments,
		mayor.InjectFragments,
		mayor.AppendFragments,
		mayor.InheritedAppendFragments,
		cfg.AgentDefaults.AppendFragments,
	)
	got := renderPrompt(f, "/city", "", "agents/mayor/prompt.template.md", PromptContext{}, "", io.Discard, nil, fragments, nil)
	if got != "Hello\n\nGoodbye" {
		t.Errorf("renderPrompt([[agent]] append_fragments) = %q, want %q", got, "Hello\n\nGoodbye")
	}
}

func TestRenderPromptPatchedTemplateSuffixRenders(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/patches/gastown-mayor-prompt.template.md"] = []byte("Hello {{ .AgentName }}")
	got := renderPrompt(f, "/city", "", "patches/gastown-mayor-prompt.template.md", PromptContext{AgentName: "gastown.mayor"}, "", io.Discard, nil, nil, nil)
	if got != "Hello gastown.mayor" {
		t.Errorf("renderPrompt(patched template suffix) = %q, want %q", got, "Hello gastown.mayor")
	}
}

func TestRenderPromptPatchedPlainMarkdownStaysInert(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/patches/gastown-mayor-prompt.md"] = []byte("Hello {{ .AgentName }}")
	f.Files["/city/patches/template-fragments/footer.template.md"] = []byte(`{{ define "footer" }}Goodbye{{ end }}`)
	got := renderPrompt(f, "/city", "", "patches/gastown-mayor-prompt.md", PromptContext{AgentName: "gastown.mayor"}, "", io.Discard, nil, []string{"footer"}, nil)
	if got != "Hello {{ .AgentName }}" {
		t.Errorf("renderPrompt(patched plain markdown) = %q, want raw markdown", got)
	}
}

func TestRenderPromptTemplateName(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Template: {{ .TemplateName }}")
	ctx := PromptContext{TemplateName: "polecat"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Template: polecat" {
		t.Errorf("renderPrompt(template name) = %q, want %q", got, "Template: polecat")
	}
}

func TestRenderPromptBasenameFunction(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Instance: {{ basename .AgentName }}")
	ctx := PromptContext{AgentName: "hello-world/polecat-3"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Instance: polecat-3" {
		t.Errorf("renderPrompt(basename) = %q, want %q", got, "Instance: polecat-3")
	}
}

func TestRenderPromptBasenameSingleton(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Instance: {{ basename .AgentName }}")
	ctx := PromptContext{AgentName: "mayor"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Instance: mayor" {
		t.Errorf("renderPrompt(basename singleton) = %q, want %q", got, "Instance: mayor")
	}
}

func TestRenderPromptCmdFunction(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Run `{{ cmd }}` to start")
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	// cmd returns filepath.Base(os.Args[0]) — in tests this is the test binary name.
	// Just verify it doesn't contain "{{ cmd }}" (i.e., the function was called).
	if strings.Contains(got, "{{ cmd }}") {
		t.Errorf("renderPrompt(cmd) still contains template expression: %q", got)
	}
	if !strings.Contains(got, "Run `") {
		t.Errorf("renderPrompt(cmd) missing prefix: %q", got)
	}
}

func TestRenderPromptSessionFunction(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte(`Session: {{ session "deacon" }}`)
	got := renderPrompt(f, "/city", "gastown", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "Session: deacon" {
		t.Errorf("renderPrompt(session) = %q, want %q", got, "Session: deacon")
	}
}

func TestRenderPromptSessionFunctionCustomTemplate(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte(`Session: {{ session "deacon" }}`)
	got := renderPrompt(f, "/city", "gastown", "prompts/test.md.tmpl", PromptContext{}, "{{.City}}-{{.Agent}}", io.Discard, nil, nil, nil)
	if got != "Session: gastown-deacon" {
		t.Errorf("renderPrompt(session custom) = %q, want %q", got, "Session: gastown-deacon")
	}
}

func TestRenderPromptMissingKeyEmptyString(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Branch: {{ .Branch }}")
	// Branch not set → should be empty string (missingkey=zero).
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "Branch: " {
		t.Errorf("renderPrompt(missing key) = %q, want %q", got, "Branch: ")
	}
}

func TestRenderPromptEnvMerge(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Custom: {{ .MyCustomVar }}")
	ctx := PromptContext{
		Env: map[string]string{"MyCustomVar": "hello"},
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Custom: hello" {
		t.Errorf("renderPrompt(env) = %q, want %q", got, "Custom: hello")
	}
}

func TestRenderPromptDefaultBranch(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Branch: {{ .DefaultBranch }}")
	ctx := PromptContext{DefaultBranch: "main"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Branch: main" {
		t.Errorf("renderPrompt(DefaultBranch) = %q, want %q", got, "Branch: main")
	}
}

func TestDefaultBranchForRig_PrefersStoredValue(t *testing.T) {
	rigs := []config.Rig{
		{Name: "scamper", Path: "/scamper", DefaultBranch: "master"},
		{Name: "other", Path: "/other"},
	}
	got := defaultBranchForRig("scamper", rigs, "/nonexistent/path")
	if got != "master" {
		t.Errorf("defaultBranchForRig(scamper) = %q, want %q (stored value)", got, "master")
	}
}

func TestDefaultBranchForRig_FallsBackToProbeWhenUnset(t *testing.T) {
	rigs := []config.Rig{
		{Name: "other", Path: "/other"}, // no DefaultBranch
	}
	// No matching rig — fall back to defaultBranchFor("") which returns "main".
	got := defaultBranchForRig("missing", rigs, "")
	if got != "main" {
		t.Errorf("defaultBranchForRig(missing) = %q, want %q (probe fallback)", got, "main")
	}
}

func TestDefaultBranchForRig_EmptyRigName(t *testing.T) {
	got := defaultBranchForRig("", nil, "")
	if got != "main" {
		t.Errorf("defaultBranchForRig() with empty rig = %q, want %q", got, "main")
	}
}

func TestRenderPromptEnvOverridePriority(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Root: {{ .CityRoot }}")
	ctx := PromptContext{
		CityRoot: "/real/path",
		Env:      map[string]string{"CityRoot": "/env/path"},
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	// SDK vars take priority over Env.
	if got != "Root: /real/path" {
		t.Errorf("renderPrompt(override) = %q, want %q", got, "Root: /real/path")
	}
}

func TestRenderPromptParseErrorFallback(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/bad.md.tmpl"] = []byte("Bad: {{ .Unclosed")
	var stderr strings.Builder
	got := renderPrompt(f, "/city", "", "prompts/bad.md.tmpl", PromptContext{}, "", &stderr, nil, nil, nil)
	// Should return raw text on parse error.
	if got != "Bad: {{ .Unclosed" {
		t.Errorf("renderPrompt(parse error) = %q, want raw text", got)
	}
	if !strings.Contains(stderr.String(), "prompt template") {
		t.Errorf("stderr = %q, want warning about prompt template", stderr.String())
	}
}

func TestRenderPromptReadError(t *testing.T) {
	f := fsys.NewFake()
	f.Errors["/city/prompts/broken.md"] = errExit
	got := renderPrompt(f, "/city", "", "prompts/broken.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "" {
		t.Errorf("renderPrompt(read error) = %q, want empty", got)
	}
}

func TestRenderPromptMultiVariable(t *testing.T) {
	f := fsys.NewFake()
	tmpl := `# {{ .AgentName }} in {{ .RigName }}
Working in {{ .WorkDir }}
City: {{ .CityRoot }}
Template: {{ .TemplateName }}
Basename: {{ basename .AgentName }}
Prefix: {{ .IssuePrefix }}
Branch: {{ .Branch }}
Run {{ cmd }} to start
Session: {{ session "deacon" }}
Custom: {{ .DefaultBranch }}
Binding: {{ .BindingName }} {{ .BindingPrefix }}
`
	f.Files["/city/prompts/full.md.tmpl"] = []byte(tmpl)
	ctx := PromptContext{
		CityRoot:      "/home/user/city",
		AgentName:     "myrig/polecat-1",
		TemplateName:  "polecat",
		BindingName:   "gastown",
		BindingPrefix: "gastown.",
		RigName:       "myrig",
		WorkDir:       "/home/user/city/myrig/polecats/polecat-1",
		IssuePrefix:   "mr-",
		Branch:        "feature/foo",
		DefaultBranch: "main",
	}
	got := renderPrompt(f, "/city", "gastown", "prompts/full.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if !strings.Contains(got, "# myrig/polecat-1 in myrig") {
		t.Errorf("missing agent/rig: %q", got)
	}
	if !strings.Contains(got, "Working in /home/user/city/myrig/polecats/polecat-1") {
		t.Errorf("missing workdir: %q", got)
	}
	if !strings.Contains(got, "City: /home/user/city") {
		t.Errorf("missing city: %q", got)
	}
	if !strings.Contains(got, "Template: polecat") {
		t.Errorf("missing template name: %q", got)
	}
	if !strings.Contains(got, "Basename: polecat-1") {
		t.Errorf("missing basename: %q", got)
	}
	if !strings.Contains(got, "Prefix: mr-") {
		t.Errorf("missing prefix: %q", got)
	}
	if !strings.Contains(got, "Branch: feature/foo") {
		t.Errorf("missing branch: %q", got)
	}
	if !strings.Contains(got, "Session: deacon") {
		t.Errorf("missing session: %q", got)
	}
	if !strings.Contains(got, "Custom: main") {
		t.Errorf("missing env var: %q", got)
	}
	if !strings.Contains(got, "Binding: gastown gastown.") {
		t.Errorf("missing binding namespace: %q", got)
	}
}

func TestRenderPromptBindingPrefixReachesTemplate(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/peer.template.md"] = []byte("peer={{ .RigName }}/{{ .BindingPrefix }}worker\nbinding={{ .BindingName }}\n")
	cases := []struct {
		name string
		ctx  PromptContext
		want string
	}{
		{
			name: "bound",
			ctx: PromptContext{
				BindingName:   "gastown",
				BindingPrefix: "gastown.",
				RigName:       "demo",
			},
			want: "peer=demo/gastown.worker\nbinding=gastown\n",
		},
		{
			name: "unbound",
			ctx: PromptContext{
				RigName: "demo",
			},
			want: "peer=demo/worker\nbinding=\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			got := renderPrompt(f, "/city", "", "prompts/peer.template.md", tc.ctx, "", &stderr, nil, nil, nil)
			if stderr.Len() > 0 {
				t.Fatalf("renderPrompt stderr: %s", stderr.String())
			}
			if got != tc.want {
				t.Errorf("renderPrompt() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderPromptWorkQuery(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Work: {{ .WorkQuery }}")
	ctx := PromptContext{WorkQuery: "bd ready --assignee=mayor"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Work: bd ready --assignee=mayor" {
		t.Errorf("renderPrompt(WorkQuery) = %q, want %q", got, "Work: bd ready --assignee=mayor")
	}
}

func TestRenderPromptAssignedReadyQuery(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Assigned: {{ .AssignedReadyQuery }}")
	ctx := PromptContext{AssignedReadyQuery: "bd ready --assignee=worker"}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if got != "Assigned: bd ready --assignee=worker" {
		t.Errorf("renderPrompt(AssignedReadyQuery) = %q", got)
	}
}

func TestRenderPromptSplitWorkQueries(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Recovery: {{ .AssignedInProgressQuery }}\nPool: {{ .RoutedPoolQuery }}")
	ctx := PromptContext{
		AssignedInProgressQuery: "bd list --status=in_progress --assignee=worker",
		RoutedPoolQuery:         "bd ready --metadata-field gc.routed_to=worker --unassigned",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	want := "Recovery: bd list --status=in_progress --assignee=worker\nPool: bd ready --metadata-field gc.routed_to=worker --unassigned"
	if got != want {
		t.Errorf("renderPrompt(split queries) = %q, want %q", got, want)
	}
}

func TestBuildTemplateData(t *testing.T) {
	ctx := PromptContext{
		CityRoot:                "/city",
		AgentName:               "a/b",
		TemplateName:            "b",
		BindingName:             "dep",
		BindingPrefix:           "dep.",
		RigName:                 "a",
		WorkDir:                 "/city/a",
		IssuePrefix:             "te-",
		Branch:                  "main",
		DefaultBranch:           "main",
		AssignedInProgressQuery: "bd list --assignee=a/b --status=in_progress",
		AssignedReadyQuery:      "bd ready --assignee=a/b",
		RoutedPoolQuery:         "bd ready --metadata-field gc.routed_to=a/b --unassigned",
		Env:                     map[string]string{"Custom": "val", "CityRoot": "override"},
	}
	data := buildTemplateData(ctx)
	// SDK vars override Env.
	if data["CityRoot"] != "/city" {
		t.Errorf("CityRoot = %q, want %q", data["CityRoot"], "/city")
	}
	if data["Custom"] != "val" {
		t.Errorf("Custom = %q, want %q", data["Custom"], "val")
	}
	if data["TemplateName"] != "b" {
		t.Errorf("TemplateName = %q, want %q", data["TemplateName"], "b")
	}
	if data["BindingName"] != "dep" {
		t.Errorf("BindingName = %q, want %q", data["BindingName"], "dep")
	}
	if data["BindingPrefix"] != "dep." {
		t.Errorf("BindingPrefix = %q, want %q", data["BindingPrefix"], "dep.")
	}
	if data["DefaultBranch"] != "main" {
		t.Errorf("DefaultBranch = %q, want %q", data["DefaultBranch"], "main")
	}
	if data["AssignedReadyQuery"] != "bd ready --assignee=a/b" {
		t.Errorf("AssignedReadyQuery = %q, want %q", data["AssignedReadyQuery"], "bd ready --assignee=a/b")
	}
	if data["AssignedInProgressQuery"] != "bd list --assignee=a/b --status=in_progress" {
		t.Errorf("AssignedInProgressQuery = %q, want %q", data["AssignedInProgressQuery"], "bd list --assignee=a/b --status=in_progress")
	}
	if data["RoutedPoolQuery"] != "bd ready --metadata-field gc.routed_to=a/b --unassigned" {
		t.Errorf("RoutedPoolQuery = %q, want %q", data["RoutedPoolQuery"], "bd ready --metadata-field gc.routed_to=a/b --unassigned")
	}
}

func TestDefaultBranchFor_EmptyDir(t *testing.T) {
	// Empty dir should return "main" (safe fallback).
	got := defaultBranchFor("")
	if got != "main" {
		t.Errorf("defaultBranchFor(\"\") = %q, want %q", got, "main")
	}
}

func TestDefaultBranchFor_NonGitDir(t *testing.T) {
	// Non-git directory should return "main" (safe fallback).
	got := defaultBranchFor(t.TempDir())
	if got != "main" {
		t.Errorf("defaultBranchFor(tmpdir) = %q, want %q", got, "main")
	}
}

func TestDefaultBranchFor_PreservesSlashesInBranchName(t *testing.T) {
	// Regression test for #719: defaultBranchFor must preserve slashes in
	// the default branch name. Previously strings.LastIndex(ref, "/") in
	// DefaultBranch() truncated "refs/remotes/origin/team/feature/x" to "x",
	// leaking the wrong branch name into PromptContext.DefaultBranch and
	// the direct cmd_sling / cmd_prime consumers.
	repoDir := newRepoWithOriginHead(t, "team/feature/x")
	got := defaultBranchFor(repoDir)
	if got != "team/feature/x" {
		t.Errorf("defaultBranchFor(repo with slashy default) = %q, want %q", got, "team/feature/x")
	}
}

func TestBuildTemplateDataDefaultBranchOverridesEnv(t *testing.T) {
	ctx := PromptContext{
		DefaultBranch: "develop",
		Env:           map[string]string{"DefaultBranch": "env-main"},
	}
	data := buildTemplateData(ctx)
	// SDK field (DefaultBranch) should override Env value.
	if data["DefaultBranch"] != "develop" {
		t.Errorf("DefaultBranch = %q, want %q (SDK override)", data["DefaultBranch"], "develop")
	}
}

func TestBuildTemplateDataEmptyEnv(t *testing.T) {
	ctx := PromptContext{AgentName: "test"}
	data := buildTemplateData(ctx)
	if data["AgentName"] != "test" {
		t.Errorf("AgentName = %q, want %q", data["AgentName"], "test")
	}
}

func TestRenderPromptSharedTemplates(t *testing.T) {
	f := fsys.NewFake()
	// Shared template defines a named block.
	f.Files["/city/prompts/shared/greeting.template.md"] = []byte(
		`{{ define "greeting" }}Hello, {{ .AgentName }}!{{ end }}`)
	// Main template uses it.
	f.Files["/city/prompts/test.template.md"] = []byte(
		`# Prompt\n{{ template "greeting" . }}`)
	ctx := PromptContext{AgentName: "mayor"}
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", ctx, "", io.Discard, nil, nil, nil)
	if !strings.Contains(got, "Hello, mayor!") {
		t.Errorf("shared template not rendered: %q", got)
	}
}

func TestRenderPromptSharedMissingDir(t *testing.T) {
	f := fsys.NewFake()
	// No shared/ directory — should render normally without error.
	f.Files["/city/prompts/test.md.tmpl"] = []byte("No shared templates here.")
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "No shared templates here." {
		t.Errorf("renderPrompt(no shared) = %q, want plain text", got)
	}
}

func TestRenderPromptSharedParseError(t *testing.T) {
	f := fsys.NewFake()
	// Bad shared template — should warn but still render main.
	f.Files["/city/prompts/shared/bad.md.tmpl"] = []byte(`{{ define "broken" }}{{ .Unclosed`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Main template works.")
	var stderr strings.Builder
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", &stderr, nil, nil, nil)
	if got != "Main template works." {
		t.Errorf("renderPrompt(bad shared) = %q, want main text", got)
	}
	if !strings.Contains(stderr.String(), "shared template") {
		t.Errorf("stderr = %q, want shared template warning", stderr.String())
	}
}

func TestRenderPromptSharedVariableAccess(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/info.md.tmpl"] = []byte(
		`{{ define "info" }}Template: {{ .TemplateName }}, Work: {{ .WorkQuery }}{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte(`{{ template "info" . }}`)
	ctx := PromptContext{
		TemplateName: "polecat",
		WorkQuery:    "bd ready --label=pool:rig/polecat",
	}
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", ctx, "", io.Discard, nil, nil, nil)
	if !strings.Contains(got, "Template: polecat") {
		t.Errorf("missing TemplateName in shared: %q", got)
	}
	if !strings.Contains(got, "Work: bd ready --label=pool:rig/polecat") {
		t.Errorf("missing WorkQuery in shared: %q", got)
	}
}

func TestRenderPromptSharedMultipleFiles(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/alpha.md.tmpl"] = []byte(
		`{{ define "alpha" }}A{{ end }}`)
	f.Files["/city/prompts/shared/beta.md.tmpl"] = []byte(
		`{{ define "beta" }}B{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte(
		`{{ template "alpha" . }}-{{ template "beta" . }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "A-B" {
		t.Errorf("renderPrompt(multi shared) = %q, want %q", got, "A-B")
	}
}

func TestRenderPromptSharedIgnoresNonTemplate(t *testing.T) {
	f := fsys.NewFake()
	// A .md file (not .template.md or legacy .md.tmpl) should be ignored.
	f.Files["/city/prompts/shared/readme.md"] = []byte(`{{ define "oops" }}should not load{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Plain text.")
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "Plain text." {
		t.Errorf("renderPrompt(non-template) = %q, want plain text", got)
	}
}

func TestRenderPromptSharedCanonicalOverridesLegacy(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/info.md.tmpl"] = []byte(
		`{{ define "info" }}legacy{{ end }}`)
	f.Files["/city/prompts/shared/info.template.md"] = []byte(
		`{{ define "info" }}canonical{{ end }}`)
	f.Files["/city/prompts/test.template.md"] = []byte(`{{ template "info" . }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "canonical" {
		t.Errorf("canonical shared template = %q, want %q", got, "canonical")
	}
}

func TestRenderPromptCrossPackShared(t *testing.T) {
	f := fsys.NewFake()
	// Pack dir with prompts/shared/ containing a named template.
	f.Dirs["/extra/prompts/shared"] = true
	f.Files["/extra/prompts/shared/greet.md.tmpl"] = []byte(
		`{{ define "greet" }}Hi from cross-pack!{{ end }}`)
	// Main template references it.
	f.Files["/city/prompts/test.md.tmpl"] = []byte(`{{ template "greet" . }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard,
		[]string{"/extra"}, nil, nil)
	if got != "Hi from cross-pack!" {
		t.Errorf("cross-pack shared = %q, want %q", got, "Hi from cross-pack!")
	}
}

func TestRenderPromptCrossPackPriority(t *testing.T) {
	f := fsys.NewFake()
	// Pack dir with prompts/shared/ defining "info".
	f.Dirs["/extra/prompts/shared"] = true
	f.Files["/extra/prompts/shared/info.md.tmpl"] = []byte(
		`{{ define "info" }}cross-pack{{ end }}`)
	// Sibling shared dir also defines "info" — should win.
	f.Files["/city/prompts/shared/info.md.tmpl"] = []byte(
		`{{ define "info" }}sibling{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte(`{{ template "info" . }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard,
		[]string{"/extra"}, nil, nil)
	if got != "sibling" {
		t.Errorf("priority = %q, want %q (sibling wins)", got, "sibling")
	}
}

func TestRenderPromptInjectFragments(t *testing.T) {
	f := fsys.NewFake()
	// Shared dir has named fragments.
	f.Files["/city/prompts/shared/frag.md.tmpl"] = []byte(
		`{{ define "footer" }}--- footer ---{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Main body.")
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard,
		nil, []string{"footer"}, nil)
	want := "Main body.\n\n--- footer ---"
	if got != want {
		t.Errorf("inject = %q, want %q", got, want)
	}
}

func TestRenderPromptInjectMissing(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Main body.")
	var stderr strings.Builder
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", &stderr,
		nil, []string{"nonexistent"}, nil)
	// Should not crash, just warn.
	if got != "Main body." {
		t.Errorf("inject missing = %q, want %q", got, "Main body.")
	}
	if !strings.Contains(stderr.String(), "nonexistent") {
		t.Errorf("stderr = %q, want warning about nonexistent", stderr.String())
	}
}

func TestRenderPromptGlobalAndPerAgent(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/frag.md.tmpl"] = []byte(
		`{{ define "global-frag" }}GLOBAL{{ end }}{{ define "agent-frag" }}AGENT{{ end }}`)
	f.Files["/city/prompts/test.md.tmpl"] = []byte("Body.")
	// Global fragments come before per-agent.
	fragments := mergeFragmentLists([]string{"global-frag"}, []string{"agent-frag"})
	got := renderPrompt(f, "/city", "", "prompts/test.md.tmpl", PromptContext{}, "", io.Discard,
		nil, fragments, nil)
	want := "Body.\n\nGLOBAL\n\nAGENT"
	if got != want {
		t.Errorf("global+agent = %q, want %q", got, want)
	}
}

func TestRenderPromptGastownDogPromptHasRequiredSharedTemplates(t *testing.T) {
	gastownDir := materializeEmbeddedGastownPack(t)
	promptPath := filepath.Join(gastownDir, "agents", "dog", "prompt.template.md")

	raw, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatalf("os.ReadFile(gastown dog prompt): %v", err)
	}

	var stderr strings.Builder
	got := renderPrompt(fsys.OSFS{}, "/tmp/city", "", promptPath, PromptContext{
		CityRoot:  "/tmp/city",
		AgentName: "dog",
		WorkQuery: "bd ready",
	}, "", &stderr, []string{gastownDir}, nil, nil)

	if strings.Contains(stderr.String(), "template not defined") {
		t.Fatalf("renderPrompt emitted missing-template warning: %s", stderr.String())
	}
	if got == string(raw) {
		t.Fatalf("renderPrompt fell back to raw prompt; expected rendered gastown dog prompt")
	}
	if !strings.Contains(got, "Gas Town Architecture") {
		t.Fatalf("rendered prompt missing architecture context:\n%s", got)
	}
	if !strings.Contains(got, "Following Your Formula") {
		t.Fatalf("rendered prompt missing following-mol fragment:\n%s", got)
	}
	if !strings.Contains(got, formulaFilesystemSearchGuidance) {
		t.Fatalf("rendered prompt missing filesystem search guidance:\n%s", got)
	}
}

func TestFormulaFilesystemSearchGuidanceCoversPromptSources(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("filepath.Abs(repo root): %v", err)
	}
	gastownDir := materializeEmbeddedGastownPack(t)

	paths := map[string]string{
		"embedded gastown pack/template-fragments/following-mol.template.md": filepath.Join(
			gastownDir, "template-fragments", "following-mol.template.md"),
		"internal/bootstrap/packs/core/assets/prompts/pool-worker.md": filepath.Join(
			repoRoot, "internal", "bootstrap", "packs", "core", "assets", "prompts", "pool-worker.md"),
		"internal/bootstrap/packs/core/assets/prompts/graph-worker.md": filepath.Join(
			repoRoot, "internal", "bootstrap", "packs", "core", "assets", "prompts", "graph-worker.md"),
	}
	for rel, path := range paths {
		t.Run(rel, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("os.ReadFile(%s): %v", path, err)
			}
			text := string(data)
			for _, want := range []string{
				formulaFilesystemSearchGuidance,
				"`find /`",
				"`find ~`",
				"`find /Users`",
				"`find $HOME`",
				"`gc` / `bd`",
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("%s missing %q", rel, want)
				}
			}
		})
	}
}

func TestCoreWorkerPromptsUseHookClaimProtocol(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("filepath.Abs(repo root): %v", err)
	}

	for _, rel := range []string{
		"internal/bootstrap/packs/core/assets/prompts/pool-worker.md",
		"internal/bootstrap/packs/core/assets/prompts/graph-worker.md",
	} {
		t.Run(rel, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(repoRoot, rel))
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", rel, err)
			}
			text := string(data)
			if !strings.Contains(text, "gc hook --claim --drain-ack --json") {
				t.Fatalf("%s missing drain-aware hook claim startup protocol", rel)
			}
			if !strings.Contains(text, "gc hook --claim --json") {
				t.Fatalf("%s missing hook claim polling protocol", rel)
			}
			if strings.Contains(text, "{{ .AssignedReadyQuery }}") {
				t.Fatalf("%s still uses AssignedReadyQuery instead of hook claim protocol", rel)
			}
			if strings.Contains(text, "bd ready") {
				t.Fatalf("%s hardcodes bd ready instead of hook claim protocol", rel)
			}
			if strings.Contains(text, "bd ready --include-ephemeral --assignee") {
				t.Fatalf("%s hardcodes bd ready --include-ephemeral instead of hook claim protocol", rel)
			}
		})
	}

	gastownDir := materializeEmbeddedGastownPack(t)
	staticPrompts := map[string]string{
		"internal/bootstrap/packs/core/overlay/per-provider/kiro/AGENTS.md": filepath.Join(
			repoRoot, "internal", "bootstrap", "packs", "core", "overlay", "per-provider", "kiro", "AGENTS.md"),
		"internal/bootstrap/packs/core/skills/gc-work/SKILL.md": filepath.Join(
			repoRoot, "internal", "bootstrap", "packs", "core", "skills", "gc-work", "SKILL.md"),
		"embedded gastown pack/agents/mayor/prompt.template.md": filepath.Join(
			gastownDir, "agents", "mayor", "prompt.template.md"),
		"examples/hyperscale/packs/hyperscale/agents/worker/prompt.template.md": filepath.Join(
			repoRoot, "examples", "hyperscale", "packs", "hyperscale", "agents", "worker", "prompt.template.md"),
		"examples/swarm/packs/swarm/agents/coder/prompt.template.md": filepath.Join(
			repoRoot, "examples", "swarm", "packs", "swarm", "agents", "coder", "prompt.template.md"),
	}
	for rel, path := range staticPrompts {
		t.Run(rel, func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(%s): %v", rel, err)
			}
			if strings.Contains(string(data), "bd ready --include-ephemeral") {
				t.Fatalf("%s hardcodes bd ready --include-ephemeral in static prompt guidance", rel)
			}
		})
	}
}

func TestMergeFragmentLists(t *testing.T) {
	tests := []struct {
		name    string
		global  []string
		agent   []string
		want    []string
		wantNil bool
	}{
		{"both nil", nil, nil, nil, true},
		{"global only", []string{"a"}, nil, []string{"a"}, false},
		{"agent only", nil, []string{"b"}, []string{"b"}, false},
		{"both", []string{"a", "b"}, []string{"c"}, []string{"a", "b", "c"}, false},
		{"dedup preserves first occurrence", []string{"a", "b"}, []string{"b", "c"}, []string{"a", "b", "c"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeFragmentLists(tt.global, tt.agent)
			if tt.wantNil {
				if got != nil {
					t.Errorf("got %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestEffectivePromptFragments(t *testing.T) {
	got := effectivePromptFragments(
		[]string{"global"},
		[]string{"inject"},
		[]string{"append"},
		[]string{"inherited"},
		[]string{"default"},
	)
	want := []string{"global", "inject", "append", "inherited", "default"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if effectivePromptFragments(nil, nil, nil, nil, nil) != nil {
		t.Fatal("effectivePromptFragments(nil, nil, nil, nil, nil) = non-nil, want nil")
	}
}

func TestEffectivePromptFragmentsDedupsAcrossLayers(t *testing.T) {
	got := effectivePromptFragments(
		[]string{"shared"},
		[]string{"inject"},
		[]string{"inject", "agent"},
		[]string{"shared", "pack"},
		[]string{"pack", "city"},
	)
	want := []string{"shared", "inject", "agent", "pack", "city"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRenderPromptProviderContextVarsExposed(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.template.md"] = []byte(
		"key={{ .ProviderKey }} display={{ .ProviderDisplayName }}")
	ctx := PromptContext{ProviderKey: "claude", ProviderDisplayName: "Claude Code"}
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", ctx, "", io.Discard, nil, nil, nil)
	want := "key=claude display=Claude Code"
	if got != want {
		t.Errorf("provider vars = %q, want %q", got, want)
	}
}

func TestRenderPromptTemplateFirstPicksFirstRegistered(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/frags.template.md"] = []byte(
		`{{ define "note-claude" }}CLAUDE{{ end }}{{ define "note-default" }}DEFAULT{{ end }}`)
	f.Files["/city/prompts/test.template.md"] = []byte(
		`{{ templateFirst . "note-claude" "note-default" }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "CLAUDE" {
		t.Errorf("templateFirst first-wins = %q, want %q", got, "CLAUDE")
	}
}

func TestRenderPromptTemplateFirstFallsBackWhenFirstMissing(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/frags.template.md"] = []byte(
		`{{ define "note-default" }}DEFAULT{{ end }}`)
	f.Files["/city/prompts/test.template.md"] = []byte(
		`{{ templateFirst . "note-codex" "note-default" }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "DEFAULT" {
		t.Errorf("templateFirst fallback = %q, want %q", got, "DEFAULT")
	}
}

func TestRenderPromptTemplateFirstReturnsEmptyWhenNoneRegistered(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/test.template.md"] = []byte(
		`prefix:{{ templateFirst . "note-codex" "note-claude" }}:suffix`)
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	want := "prefix::suffix"
	if got != want {
		t.Errorf("templateFirst all-missing = %q, want %q", got, want)
	}
}

func TestRenderPromptTemplateFirstComposesWithProviderKey(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/frags.template.md"] = []byte(
		`{{ define "note-claude" }}slash commands{{ end }}` +
			`{{ define "note-codex" }}subcommands{{ end }}` +
			`{{ define "note-default" }}commands{{ end }}`)
	f.Files["/city/prompts/test.template.md"] = []byte(
		`{{ templateFirst . (printf "note-%s" .ProviderKey) "note-default" }}`)

	cases := []struct {
		key  string
		want string
	}{
		{"claude", "slash commands"},
		{"codex", "subcommands"},
		{"gemini", "commands"}, // falls through to default
		{"", "commands"},       // empty key skipped, falls through
	}
	for _, tc := range cases {
		got := renderPrompt(f, "/city", "", "prompts/test.template.md",
			PromptContext{ProviderKey: tc.key}, "", io.Discard, nil, nil, nil)
		if got != tc.want {
			t.Errorf("ProviderKey=%q: got %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestRenderPromptTemplateFirstSkipsEmptyName(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/frags.template.md"] = []byte(
		`{{ define "winner" }}WIN{{ end }}`)
	f.Files["/city/prompts/test.template.md"] = []byte(
		`{{ templateFirst . "" "winner" }}`)
	got := renderPrompt(f, "/city", "", "prompts/test.template.md", PromptContext{}, "", io.Discard, nil, nil, nil)
	if got != "WIN" {
		t.Errorf("templateFirst skip-empty = %q, want %q", got, "WIN")
	}
}

func TestProviderInfoForAgentResolvesAgentOverWorkspace(t *testing.T) {
	ws := &config.Workspace{Provider: "codex"}
	a := &config.Agent{Provider: "claude"}
	gotKey, gotName := providerInfoForAgent(a, ws, nil)
	if gotKey != "claude" {
		t.Errorf("key = %q, want %q (agent.Provider should win over workspace.Provider)", gotKey, "claude")
	}
	if gotName != "Claude Code" {
		t.Errorf("displayName = %q, want %q", gotName, "Claude Code")
	}
}

func TestProviderInfoForAgentFallsBackToWorkspace(t *testing.T) {
	ws := &config.Workspace{Provider: "codex"}
	a := &config.Agent{}
	gotKey, gotName := providerInfoForAgent(a, ws, nil)
	if gotKey != "codex" {
		t.Errorf("key = %q, want %q", gotKey, "codex")
	}
	if gotName != "Codex CLI" {
		t.Errorf("displayName = %q, want %q", gotName, "Codex CLI")
	}
}

func TestProviderInfoForAgentEmptyWhenNoneSet(t *testing.T) {
	gotKey, gotName := providerInfoForAgent(&config.Agent{}, &config.Workspace{}, nil)
	if gotKey != "" || gotName != "" {
		t.Errorf("got (%q, %q), want both empty", gotKey, gotName)
	}
}

func TestEmbeddedMayorPromptRendersProviderSpecificSlashNote(t *testing.T) {
	// Round-trip the embedded mayor.md source through the renderer the same
	// way `gc init` will (copy verbatim to agents/mayor/prompt.template.md,
	// then render). Verifies the {{ templateFirst }} mechanism resolves the
	// per-provider variant from the inline {{ define }} blocks at the top
	// of the file.
	source, err := defaultPrompts.ReadFile("prompts/mayor.md")
	if err != nil {
		t.Fatalf("read embedded mayor.md: %v", err)
	}

	cases := []struct {
		key            string
		wantContains   string
		wantNotContain string
	}{
		{
			key:            "claude",
			wantContains:   "Claude Code slash commands",
			wantNotContain: "provider's command",
		},
		{
			key:            "codex",
			wantContains:   "provider's command",
			wantNotContain: "Claude Code",
		},
		{
			key:            "", // no provider configured → fallback default
			wantContains:   "provider's command",
			wantNotContain: "Claude Code",
		},
	}

	for _, tc := range cases {
		f := fsys.NewFake()
		f.Files["/city/agents/mayor/prompt.template.md"] = source
		got := renderPrompt(f, "/city", "test-city",
			"agents/mayor/prompt.template.md",
			PromptContext{ProviderKey: tc.key},
			"", io.Discard, nil, nil, nil)
		if !strings.Contains(got, tc.wantContains) {
			t.Errorf("ProviderKey=%q: rendered prompt missing %q\n--- got ---\n%s",
				tc.key, tc.wantContains, got)
		}
		if tc.wantNotContain != "" && strings.Contains(got, tc.wantNotContain) {
			t.Errorf("ProviderKey=%q: rendered prompt should not contain %q\n--- got ---\n%s",
				tc.key, tc.wantNotContain, got)
		}
	}
}

func TestInstructionsFileForAgentClaudeReturnsCLAUDEMD(t *testing.T) {
	ws := &config.Workspace{Provider: "claude"}
	got := instructionsFileForAgent(&config.Agent{}, ws, nil)
	if got != "CLAUDE.md" {
		t.Errorf("InstructionsFile = %q, want %q", got, "CLAUDE.md")
	}
}

func TestInstructionsFileForAgentCodexReturnsAGENTSMD(t *testing.T) {
	ws := &config.Workspace{Provider: "codex"}
	got := instructionsFileForAgent(&config.Agent{}, ws, nil)
	if got != "AGENTS.md" {
		t.Errorf("InstructionsFile = %q, want %q", got, "AGENTS.md")
	}
}

func TestInstructionsFileForAgentDefaultsToAGENTSMDWhenUnset(t *testing.T) {
	got := instructionsFileForAgent(&config.Agent{}, &config.Workspace{}, nil)
	if got != "AGENTS.md" {
		t.Errorf("InstructionsFile = %q, want %q (default)", got, "AGENTS.md")
	}
}

func TestInstructionsFileForAgentResolvesAgentOverWorkspace(t *testing.T) {
	ws := &config.Workspace{Provider: "codex"}
	a := &config.Agent{Provider: "claude"}
	got := instructionsFileForAgent(a, ws, nil)
	if got != "CLAUDE.md" {
		t.Errorf("InstructionsFile = %q, want %q (agent.Provider beats workspace.Provider)", got, "CLAUDE.md")
	}
}

func TestInstructionsFileForAgentUsesCityOverride(t *testing.T) {
	// A custom provider declared in city.toml with InstructionsFile set
	// takes precedence over its builtin family default.
	cityProviders := map[string]config.ProviderSpec{
		"custom-claude": {
			Command:          "claude-fork",
			InstructionsFile: "INSTRUCTIONS.md",
		},
	}
	ws := &config.Workspace{Provider: "custom-claude"}
	got := instructionsFileForAgent(&config.Agent{}, ws, cityProviders)
	if got != "INSTRUCTIONS.md" {
		t.Errorf("InstructionsFile = %q, want %q (city override)", got, "INSTRUCTIONS.md")
	}
}

func TestInstructionsFileForAgentFallsBackToBuiltinFamily(t *testing.T) {
	// A custom provider with empty InstructionsFile but a builtin family
	// inherits the family's filename. `kiro` (a claude-family fork) is the
	// canonical case from internal/config/chain_test.go; here we mimic that
	// pattern with a synthetic provider whose Base points at "claude".
	base := "claude"
	cityProviders := map[string]config.ProviderSpec{
		"my-fork": {
			Base:    &base,
			Command: "my-fork",
		},
	}
	ws := &config.Workspace{Provider: "my-fork"}
	got := instructionsFileForAgent(&config.Agent{}, ws, cityProviders)
	if got != "CLAUDE.md" {
		t.Errorf("InstructionsFile = %q, want %q (inherited from claude family)", got, "CLAUDE.md")
	}
}

func TestRenderedCrewPromptShowsProviderSpecificInstructionsFile(t *testing.T) {
	// Regression test for Wasteland w-d4dba7b056: the Gastown pack's crew
	// prompt should reference the provider-specific instruction filename as
	// the fallback for missing/empty quality-gate guidance.
	//
	// Two assertions: (a) the shipped crew.template.md references the
	// {{ .InstructionsFile }} placeholder in the expected backtick pattern,
	// and (b) renderPrompt substitutes that placeholder to the right value
	// for each provider via buildTemplateData. Asserting (a)+(b)
	// independently keeps the test stable when crew.template.md gains new
	// fragment includes that would otherwise break a full-render assertion.
	crewPath := filepath.Join("..", "..", "examples", "gastown", "packs", "gastown", "assets", "prompts", "crew.template.md")
	source, err := os.ReadFile(crewPath)
	if err != nil {
		t.Skipf("crew.template.md not readable at %s: %v", crewPath, err)
	}
	if !strings.Contains(string(source), "`{{ .InstructionsFile }}`") {
		t.Fatalf("crew.template.md missing fallback marker `{{ .InstructionsFile }}` (w-d4dba7b056 regression)")
	}

	cases := []struct {
		providerKey string
		wantFile    string
	}{
		{"claude", "CLAUDE.md"},
		{"codex", "AGENTS.md"},
		{"", "AGENTS.md"},
	}

	const tmplBody = "fallback: (`{{ .InstructionsFile }}`)"
	for _, tc := range cases {
		f := fsys.NewFake()
		f.Files["/city/prompts/p.template.md"] = []byte(tmplBody)
		ws := &config.Workspace{Provider: tc.providerKey}
		got := renderPrompt(f, "/city", "test-city", "prompts/p.template.md",
			PromptContext{
				ProviderKey:      tc.providerKey,
				InstructionsFile: instructionsFileForAgent(&config.Agent{}, ws, nil),
			},
			"", io.Discard, nil, nil, nil)
		want := "fallback: (`" + tc.wantFile + "`)"
		if got != want {
			t.Errorf("ProviderKey=%q: rendered = %q, want %q", tc.providerKey, got, want)
		}
	}
}

func TestProviderDisplayNameFallsBackToKeyForUnknownProvider(t *testing.T) {
	ws := &config.Workspace{Provider: "totally-unknown"}
	a := &config.Agent{}
	gotKey, gotName := providerInfoForAgent(a, ws, nil)
	if gotKey != "totally-unknown" {
		t.Errorf("key = %q, want %q", gotKey, "totally-unknown")
	}
	if gotName != "totally-unknown" {
		t.Errorf("displayName = %q, want %q (unknown provider should fall back to key)", gotName, "totally-unknown")
	}
}

// TestRenderPromptCityRootTemplateFragments verifies that template-fragments/
// at the city root (the root pack itself) are loaded into the template set,
// not just template-fragments/ inside imported pack dirs. Regression test for
// rp-aew: a PackV2 root pack with no [imports.*] blocks (so cfg.PackDirs is
// empty) must still be able to use its own template-fragments/.
func TestRenderPromptCityRootTemplateFragments(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/template-fragments/recovery.template.md"] = []byte(
		`{{ define "recovery" }}recover-from-city-root{{ end }}`)
	f.Files["/city/agents/x/prompt.template.md"] = []byte(`{{ template "recovery" . }}`)
	got := renderPrompt(f, "/city", "", "agents/x/prompt.template.md", PromptContext{},
		"", io.Discard, nil, nil, nil)
	if got != "recover-from-city-root" {
		t.Errorf("renderPrompt(city-root template-fragments) = %q, want %q",
			got, "recover-from-city-root")
	}
}

// TestRenderPromptCityRootPromptsShared mirrors the test above for the
// prompts/shared/ subdirectory at the city root.
func TestRenderPromptCityRootPromptsShared(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/prompts/shared/greet.template.md"] = []byte(
		`{{ define "greet" }}hello-from-city-root{{ end }}`)
	f.Files["/city/agents/x/prompt.template.md"] = []byte(`{{ template "greet" . }}`)
	got := renderPrompt(f, "/city", "", "agents/x/prompt.template.md", PromptContext{},
		"", io.Discard, nil, nil, nil)
	if got != "hello-from-city-root" {
		t.Errorf("renderPrompt(city-root prompts/shared) = %q, want %q",
			got, "hello-from-city-root")
	}
}

// TestRenderPromptCityRootFragmentsPerAgentWins ensures the new city-root
// fragment load does not displace per-agent fragments (which must still win
// on name collision).
func TestRenderPromptCityRootFragmentsPerAgentWins(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/template-fragments/footer.template.md"] = []byte(
		`{{ define "footer" }}city-root{{ end }}`)
	f.Files["/city/agents/x/template-fragments/footer.template.md"] = []byte(
		`{{ define "footer" }}per-agent{{ end }}`)
	f.Files["/city/agents/x/prompt.template.md"] = []byte(`{{ template "footer" . }}`)
	got := renderPrompt(f, "/city", "", "agents/x/prompt.template.md", PromptContext{},
		"", io.Discard, nil, nil, nil)
	if got != "per-agent" {
		t.Errorf("renderPrompt(per-agent overrides city-root) = %q, want %q",
			got, "per-agent")
	}
}

// TestRenderPromptResolvesRigPackFragment is the renderer-level regression
// test for gascity#2676: a template fragment shipped in a rig-imported pack
// (i.e. in cfg.RigPackDirs[<rig>], not cfg.PackDirs) must resolve when the
// renderer is given that rig's scoped pack directories. Before the fix the
// two callers in agent_build_params.go / cmd_prime.go passed only cfg.PackDirs,
// so rig-pack fragments silently fell through and the renderer emitted the raw
// `{{ template ... }}` directive in error fallback.
func TestRenderPromptResolvesRigPackFragment(t *testing.T) {
	f := fsys.NewFake()
	// Rig-imported pack ships a template fragment.
	rigPackDir := "/city/.gc/cache/repos/abc123/packs/gastown"
	f.Files[rigPackDir+"/template-fragments/work-query.template.md"] = []byte(
		`{{ define "work-query" }}rig-pack-work-query{{ end }}`)
	// An imported agent's prompt references that fragment.
	f.Files["/city/agents/polecat/prompt.template.md"] = []byte(
		`{{ template "work-query" . }}`)

	cfg := &config.City{
		// PackDirs is intentionally empty: this city imports its pack
		// at the rig level, not the city level. Pre-fix callers passed
		// cfg.PackDirs here and the fragment was never registered.
		PackDirs: nil,
		RigPackDirs: map[string][]string{
			"gastown": {rigPackDir},
		},
	}

	// Post-fix call: cfg.PackDirsForRig includes the current rig dir without
	// exposing other rigs' pack fragments.
	got := renderPrompt(f, "/city", "", "agents/polecat/prompt.template.md",
		PromptContext{AgentName: "polecat"}, "", io.Discard, cfg.PackDirsForRig("gastown"), nil, nil)
	if got != "rig-pack-work-query" {
		t.Errorf("renderPrompt(cfg.PackDirsForRig()) = %q, want %q",
			got, "rig-pack-work-query")
	}

	// Sanity-check the pre-fix behavior: passing cfg.PackDirs alone (what
	// the two call sites used before gascity#2676) must NOT resolve the
	// fragment — otherwise this test wouldn't be guarding the fix. The
	// renderer's error path emits the raw body and logs to stderr; either
	// way the rendered output must not be the resolved fragment text.
	gotBuggy := renderPrompt(f, "/city", "", "agents/polecat/prompt.template.md",
		PromptContext{AgentName: "polecat"}, "", io.Discard, cfg.PackDirs, nil, nil)
	if gotBuggy == "rig-pack-work-query" {
		t.Errorf("pre-fix call with cfg.PackDirs unexpectedly resolved the rig-pack fragment; the regression guard is not actually guarding anything")
	}
}

// TestRenderPromptResolvesMultiRigPackFragments verifies that fragments from
// multiple rig-imported packs all reach the renderer when cfg.AllPackDirs()
// is passed. Guards against an off-by-one or single-rig assumption in the
// AllPackDirs union.
func TestRenderPromptResolvesMultiRigPackFragments(t *testing.T) {
	f := fsys.NewFake()
	alphaDir := "/city/.gc/cache/repos/aaa/packs/alpha"
	bravoDir := "/city/.gc/cache/repos/bbb/packs/bravo"
	f.Files[alphaDir+"/template-fragments/a.template.md"] = []byte(
		`{{ define "a" }}A{{ end }}`)
	f.Files[bravoDir+"/template-fragments/b.template.md"] = []byte(
		`{{ define "b" }}B{{ end }}`)
	f.Files["/city/agents/x/prompt.template.md"] = []byte(
		`{{ template "a" . }}-{{ template "b" . }}`)

	cfg := &config.City{
		RigPackDirs: map[string][]string{
			"alpha": {alphaDir},
			"bravo": {bravoDir},
		},
	}
	got := renderPrompt(f, "/city", "", "agents/x/prompt.template.md",
		PromptContext{}, "", io.Discard, cfg.AllPackDirs(), nil, nil)
	if got != "A-B" {
		t.Errorf("renderPrompt(multi-rig AllPackDirs) = %q, want %q", got, "A-B")
	}
}

// TestRenderPromptCityRootFragmentsAbsentNoEffect is the regression-safety
// check: when the city root has no template-fragments/ or prompts/shared/,
// rendered output is byte-identical to pre-fix behavior (i.e. the new
// directory probes silently no-op via the loadSharedTemplates ReadDir miss).
func TestRenderPromptCityRootFragmentsAbsentNoEffect(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/agents/x/prompt.template.md"] = []byte("plain body {{ .AgentName }}")
	got := renderPrompt(f, "/city", "", "agents/x/prompt.template.md",
		PromptContext{AgentName: "x"}, "", io.Discard, nil, nil, nil)
	want := "plain body x"
	if got != want {
		t.Errorf("renderPrompt(no city-root fragments) = %q, want %q", got, want)
	}
}
