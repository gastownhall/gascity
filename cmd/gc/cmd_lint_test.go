package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestLintValidPackPasses(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "valid", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "Agent {{.AgentName}} work {{.WorkQuery}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "ok") {
		t.Fatalf("stdout missing ok status: %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestLintUsesRuntimeMissingKeyPolicyForUnknownVariables(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "typo", "witness", "prompts/witness.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "witness.template.md"), "runtime-compatible {{.CommitSha}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestLintPromptContextDoesNotInjectAlias(t *testing.T) {
	packDir := t.TempDir()
	data := buildTemplateData(lintPromptContext(packDir, config.Agent{Name: "worker"}, nil))
	if _, ok := data["Alias"]; ok {
		t.Fatalf("lint prompt data injected Alias = %q, want no lint-only Alias key", data["Alias"])
	}

	data = buildTemplateData(lintPromptContext(packDir, config.Agent{
		Name: "worker",
		Env:  map[string]string{"Alias": "configured"},
	}, nil))
	if got := data["Alias"]; got != "configured" {
		t.Fatalf("Alias = %q, want configured env value", got)
	}
}

func TestLintReportsMalformedTemplateActionWithLine(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "bad-template", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "broken {{if .AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint succeeded; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "worker.template.md:") || !strings.Contains(errText, "unexpected EOF") {
		t.Fatalf("stderr missing line-numbered malformed template path:\n%s", errText)
	}
}

func TestLintReportsMalformedPackTOMLWithLine(t *testing.T) {
	packDir := t.TempDir()
	writeFile(t, filepath.Join(packDir, "pack.toml"), "[pack]\nname = \"broken\"\nschema =\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint succeeded; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "pack.toml:3:") {
		t.Fatalf("stderr missing line-numbered pack.toml error:\n%s", errText)
	}
}

func TestLintDotWalksPackTOMLDirectories(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	first := filepath.Join(root, "packs", "first")
	writeLintPack(t, first, "first", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(first, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	second := filepath.Join(root, "packs", "second")
	writeLintPack(t, second, "second", "reviewer", "prompts/reviewer.template.md")
	writeLintFile(t, filepath.Join(second, "prompts", "reviewer.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", "."}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint . = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "2 pack(s) ok") {
		t.Fatalf("stdout missing recursive pack count: %q", stdout.String())
	}
}

func TestLintDotHandlesCityRootPackDefaults(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	writeLintFile(t, filepath.Join(root, "city.toml"), `[defaults.rig.imports.worker]
source = "packs/worker"
`)
	writeLintFile(t, filepath.Join(root, "pack.toml"), `[pack]
name = "city-root"
version = "0.1.0"
schema = 2

[imports.worker]
source = "packs/worker"
`)
	writeLintPack(t, filepath.Join(root, "packs", "worker"), "worker", "builder", "prompts/builder.template.md")
	writeLintFile(t, filepath.Join(root, "packs", "worker", "prompts", "builder.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", "."}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint . = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
}

func TestLintEmitsLoaderWarnings(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "warns", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")
	appendLintFile(t, filepath.Join(packDir, "pack.toml"), "\n[agents]\nwake_mode = \"resume\"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint succeeded, want pack authoring error\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "[agents] is a city.toml compatibility alias for [agent_defaults], not a pack.toml field") {
		t.Fatalf("stderr missing pack authoring error:\n%s", errText)
	}
}

func TestLintRejectsNamedSessionBackedByPoolControlledAgent(t *testing.T) {
	packDir := t.TempDir()
	writeLintFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "bad-pool-named"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
min_active_sessions = 0
max_active_sessions = 3

[[named_session]]
template = "worker"
scope = "rig"
mode = "on_demand"
`)
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint succeeded, want named-session pool conflict\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, `named_session "worker" targets pool-controlled agent "worker"`) {
		t.Fatalf("stderr missing named-session pool conflict:\n%s", errText)
	}
}

// TestLintAllowsNamedSessionOnExplicitlyDisabledPoolAgent is a regression
// guard for #4184 problem 2: min_active_sessions=0 + max_active_sessions=0
// is documented (TestValidateAgentsPoolMaxZeroIsValid) as the intentional
// way to disable an agent's pool — it is not a pool. The lint rule must not
// re-flag it as "pool-controlled" the same way it flags a real pool
// (e.g. min=0/max=3 in TestLintRejectsNamedSessionBackedByPoolControlledAgent
// above).
func TestLintAllowsNamedSessionOnExplicitlyDisabledPoolAgent(t *testing.T) {
	packDir := t.TempDir()
	writeLintFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "disabled-pool-named"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
min_active_sessions = 0
max_active_sessions = 0

[[named_session]]
template = "worker"
scope = "rig"
mode = "on_demand"
`)
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint failed on an explicitly disabled pool agent, want pass\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "pool-controlled agent") {
		t.Fatalf("stderr wrongly flagged the documented max=0 disable form as pool-controlled:\n%s", stderr.String())
	}
}

// TestLintAllowsNamedSessionSingletonAgent is a regression guard for the
// max_active_sessions=1 named-session flavor documented by
// (*config.Agent).SupportsInstanceExpansion: max=1 with no min/scale_check/
// namepool is a singleton with a stable canonical identity, not a pool.
// A [[named_session]] targeting that shape is the supported way to declare a
// persistent seat and must lint clean.
func TestLintAllowsNamedSessionSingletonAgent(t *testing.T) {
	packDir := t.TempDir()
	writeLintFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "singleton-named"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
max_active_sessions = 1

[[named_session]]
template = "worker"
scope = "rig"
mode = "on_demand"
`)
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint failed on a named-session singleton agent, want pass\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "pool-controlled agent") {
		t.Fatalf("stderr wrongly flagged the max=1 named-session flavor as pool-controlled:\n%s", stderr.String())
	}
}

// TestLintStillRejectsSingletonPoolWithMin pins the boundary of the max=1
// exemption: an explicit min_active_sessions keeps pool semantics
// (SupportsInstanceExpansion's pool flavor), so a named_session targeting
// min=1/max=1 remains a conflict.
func TestLintStillRejectsSingletonPoolWithMin(t *testing.T) {
	packDir := t.TempDir()
	writeLintFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "singleton-pool-named"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
min_active_sessions = 1
max_active_sessions = 1

[[named_session]]
template = "worker"
scope = "rig"
mode = "on_demand"
`)
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint succeeded, want pool conflict for min=1/max=1\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `named_session "worker" targets pool-controlled agent "worker"`) {
		t.Fatalf("stderr missing named-session pool conflict:\n%s", stderr.String())
	}
}

func TestLintPromptDiscoverySkipsIgnoredDirs(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "skip-dirs", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")
	writeLintFile(t, filepath.Join(packDir, ".gc", "bad.template.md"), "broken {{if .AgentName}}\n")
	writeLintFile(t, filepath.Join(packDir, "node_modules", "dep", "bad.template.md"), "broken {{if .AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want ignored dirs skipped\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
}

func TestLintReportsMissingInjectFragment(t *testing.T) {
	packDir := t.TempDir()
	writeLintFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "missing-frag"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
inject_fragments = ["missing-footer"]
`)
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint succeeded; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `inject_fragment "missing-footer"`) {
		t.Fatalf("stderr missing inject fragment diagnostic:\n%s", stderr.String())
	}
}

// TestLintResolvesOwnPackFragmentCleanly pins the baseline case Ask #3 of
// ga-as6dhb asked for: a pack that injects a fragment it defines itself
// must lint clean, so a future regression in the fragment-search-directory
// logic doesn't silently start flagging valid packs again.
func TestLintResolvesOwnPackFragmentCleanly(t *testing.T) {
	packDir := t.TempDir()
	writeLintFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "own-fragment"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
inject_fragments = ["footer"]
`)
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")
	writeLintFile(t, filepath.Join(packDir, "template-fragments", "footer.template.md"), `{{ define "footer" }}FOOTER{{ end }}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0 (own-pack fragment should resolve)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "inject_fragment") {
		t.Fatalf("stderr unexpectedly reports inject_fragment diagnostic for a fragment defined in the pack's own template-fragments/:\n%s", stderr.String())
	}
}

// TestLintResolvesFragmentComposedViaCityRigIncludes guards ga-as6dhb: a
// pack can depend on a fragment shipped by a sibling pack that is composed
// in purely at the city.toml rig level (rigs[].includes), never declared by
// the dependent pack's own pack.toml — exactly how packs/cairn-loop-orders
// depends on packs/actual/all in production. The runtime resolves this fine
// because resolveTemplate (template_resolve.go) renders against
// city.PackDirsForRig(rigName), but config.LoadPackForLint only ever sees
// the target pack's own recursive includes (empty here), so lint reported
// the fragment "not found" even though real sessions rendered it correctly.
// See lintCityResolver.searchDirs in cmd_lint.go.
func TestLintResolvesFragmentComposedViaCityRigIncludes(t *testing.T) {
	root := t.TempDir()
	writeLintFile(t, filepath.Join(root, "city.toml"), `[[rigs]]
name = "myrig"
includes = ["packs/actual", "packs/uses-fragment"]
`)

	actualDir := filepath.Join(root, "packs", "actual")
	writeLintPack(t, actualDir, "actual", "helper", "prompts/helper.template.md")
	writeLintFile(t, filepath.Join(actualDir, "prompts", "helper.template.md"), "hello {{.AgentName}}\n")
	writeLintFile(t, filepath.Join(actualDir, "template-fragments", "footer.template.md"), `{{ define "footer" }}FOOTER{{ end }}`)

	packDir := filepath.Join(root, "packs", "uses-fragment")
	writeLintFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "uses-fragment"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
inject_fragments = ["footer"]
`)
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0 (fragment composed in via city.toml rigs[].includes should resolve)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "inject_fragment") {
		t.Fatalf("stderr unexpectedly reports inject_fragment diagnostic for a fragment composed in via city.toml rigs[].includes:\n%s", stderr.String())
	}
}

// TestLintDoesNotResolveFragmentFromUnrelatedRig is the other half of
// ga-as6dhb: widening lint's fragment search with city-composed pack dirs must
// not widen it past what the runtime would actually resolve. A fragment defined
// only by a pack in rigb is not on the search path of an agent in riga's pack
// (resolveTemplate renders against city.PackDirsForRig("riga")), so lint must
// still report it missing. Unioning every rig would turn that real error into a
// silent pass. See lintCityComposedPackDirs in cmd_lint.go.
func TestLintDoesNotResolveFragmentFromUnrelatedRig(t *testing.T) {
	root := t.TempDir()
	writeLintFile(t, filepath.Join(root, "city.toml"), `[[rigs]]
name = "riga"
includes = ["packs/riga-pack"]

[[rigs]]
name = "rigb"
includes = ["packs/rigb-pack"]
`)

	rigbPack := filepath.Join(root, "packs", "rigb-pack")
	writeLintPack(t, rigbPack, "rigb-pack", "helper", "prompts/helper.template.md")
	writeLintFile(t, filepath.Join(rigbPack, "prompts", "helper.template.md"), "hello {{.AgentName}}\n")
	writeLintFile(t, filepath.Join(rigbPack, "template-fragments", "footer.template.md"), `{{ define "footer" }}FOOTER{{ end }}`)

	rigaPack := filepath.Join(root, "packs", "riga-pack")
	writeLintFile(t, filepath.Join(rigaPack, "pack.toml"), `[pack]
name = "riga-pack"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
inject_fragments = ["footer"]
`)
	writeLintFile(t, filepath.Join(rigaPack, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", rigaPack}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint = 0, want non-zero (fragment lives only in an unrelated rig's pack)\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `inject_fragment "footer"`) {
		t.Fatalf("stderr does not report the missing fragment from the unrelated rig:\n%s", stderr.String())
	}
}

// TestLintIgnoresBrokenFragmentInCityComposedSiblingPack pins the ownership
// boundary: gc lint <pack> reports on <pack>. A sibling pack composed in by the
// city supplies fragments to the linted pack, but its own malformed fragment is
// that sibling's lint failure, not this one's — so the parse error is recorded
// at warning severity and the linted pack still passes. See
// lintLoadAdvisorySharedTemplates in cmd_lint.go.
func TestLintIgnoresBrokenFragmentInCityComposedSiblingPack(t *testing.T) {
	root := t.TempDir()
	writeLintFile(t, filepath.Join(root, "city.toml"), `[[rigs]]
name = "myrig"
includes = ["packs/actual", "packs/uses-fragment"]
`)

	actualDir := filepath.Join(root, "packs", "actual")
	writeLintPack(t, actualDir, "actual", "helper", "prompts/helper.template.md")
	writeLintFile(t, filepath.Join(actualDir, "prompts", "helper.template.md"), "hello {{.AgentName}}\n")
	writeLintFile(t, filepath.Join(actualDir, "template-fragments", "footer.template.md"), `{{ define "footer" }}FOOTER{{ end }}`)
	writeLintFile(t, filepath.Join(actualDir, "template-fragments", "broken.template.md"), `{{ define "broken" }}{{ if .AgentName }}`)

	packDir := filepath.Join(root, "packs", "uses-fragment")
	writeLintFile(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "uses-fragment"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
inject_fragments = ["footer"]
`)
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0 (a broken fragment in a city-composed sibling must not fail this pack)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	for _, line := range strings.Split(stderr.String(), "\n") {
		if !strings.Contains(line, "broken.template.md") {
			continue
		}
		if strings.Contains(line, "error") {
			t.Fatalf("stderr reports a sibling pack's broken fragment as an error for this pack:\n%s", stderr.String())
		}
	}
}

func TestLintCleanBdInvocationsProduceNoFindings(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "bd-flag-clean", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"),
		"Agent {{.AgentName}}\n`gc bd update <id> --claim`\n`gc bd ready --unassigned --json`\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestLintReportsUnknownBdFlag(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "bd-flag-typo", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"),
		"Agent {{.AgentName}}\n`gc bd update <id> --asignee bob`\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint succeeded; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	errText := stderr.String()
	if !strings.Contains(errText, "bd-unknown-flag") || !strings.Contains(errText, `"--asignee"`) {
		t.Fatalf("stderr missing bd-unknown-flag diagnostic:\n%s", errText)
	}
	if !strings.Contains(errText, "worker.template.md:2:") {
		t.Fatalf("stderr missing correct line number for bd-unknown-flag diagnostic:\n%s", errText)
	}
}

func TestLintSkipsOutOfScopeBdSubcommand(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "bd-flag-out-of-scope", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"),
		"Agent {{.AgentName}}\n`gc bd formula show some-formula --made-up-flag`\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0 (out-of-scope subcommand silently skipped)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
}

func TestLintJSONReportsDiagnostics(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "json-bad", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "broken {{if .AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir, "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint --json succeeded; stdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want diagnostics in JSON stdout only", stderr.String())
	}

	var report struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		Passed        bool   `json:"passed"`
		ErrorCount    int    `json:"error_count"`
		Packs         []struct {
			Path        string `json:"path"`
			OK          bool   `json:"ok"`
			Diagnostics []struct {
				Path    string `json:"path"`
				Line    int    `json:"line"`
				Message string `json:"message"`
			} `json:"diagnostics"`
		} `json:"packs"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	validateLintJSONSchema(t, stdout.Bytes())
	if report.SchemaVersion != "2" {
		t.Fatalf("schema_version = %q, want 2", report.SchemaVersion)
	}
	if !report.OK {
		t.Fatalf("report.OK = false, want true transport discriminator: %+v", report)
	}
	if report.Passed {
		t.Fatalf("report.Passed = true, want false: %+v", report)
	}
	if report.ErrorCount != 1 {
		t.Fatalf("error_count = %d, want 1: %+v", report.ErrorCount, report)
	}
	if len(report.Packs) != 1 || len(report.Packs[0].Diagnostics) != 1 {
		t.Fatalf("unexpected JSON diagnostics: %+v", report)
	}
	diag := report.Packs[0].Diagnostics[0]
	if diag.Line == 0 || !strings.Contains(diag.Message, "unexpected EOF") {
		t.Fatalf("diagnostic = %+v, want line-numbered template error", diag)
	}
}

func TestLintRecursiveJSONReportsSchemaBackedPassAndFail(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)

	passing := filepath.Join(root, "packs", "passing")
	writeLintPack(t, passing, "passing", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(passing, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", ".", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint . --json = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	validateLintJSONSchema(t, stdout.Bytes())

	failing := filepath.Join(root, "packs", "failing")
	writeLintPack(t, failing, "failing", "reviewer", "prompts/reviewer.template.md")
	writeLintFile(t, filepath.Join(failing, "prompts", "reviewer.template.md"), "broken {{if .AgentName}}\n")

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"lint", ".", "--json"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint . --json succeeded, want failing report\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	validateLintJSONSchema(t, stdout.Bytes())
}

func TestLintHelpDocumentsJSONFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint --help = %d; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "gc lint <pack>") || !strings.Contains(out, "--json") {
		t.Fatalf("help missing lint usage or --json flag:\n%s", out)
	}
}

// lintNestedCityFixture is a root city that composes a pack tree through a rig
// include, with a nested city.toml sitting between the leaf packs and the root:
//
//	root/city.toml            rig "myrig" includes packs/comp
//	root/packs/comp           defines fragment "footer"; includes both leaves
//	root/sub/nested/city.toml composes nothing
//	root/sub/nested/leaf*     inject fragment "footer"
type lintNestedCityFixture struct {
	root, nested, comp, leaf, leaf2 string
}

func writeLintNestedCityFixture(t *testing.T, nestedCityToml string) lintNestedCityFixture {
	t.Helper()
	root := pathutil.NormalizePathForCompare(t.TempDir())
	f := lintNestedCityFixture{
		root:   root,
		nested: filepath.Join(root, "sub", "nested"),
		comp:   filepath.Join(root, "packs", "comp"),
		leaf:   filepath.Join(root, "sub", "nested", "leaf"),
		leaf2:  filepath.Join(root, "sub", "nested", "leaf2"),
	}
	writeLintFile(t, filepath.Join(root, "city.toml"), `[[rigs]]
name = "myrig"
includes = ["packs/comp"]
`)
	writeLintFile(t, filepath.Join(f.nested, "city.toml"), nestedCityToml)
	writeLintFile(t, filepath.Join(f.comp, "pack.toml"), `[pack]
name = "comp"
version = "0.1.0"
schema = 2
includes = ["../../sub/nested/leaf", "../../sub/nested/leaf2"]

[[agent]]
name = "helper"
prompt_template = "prompts/helper.template.md"
`)
	writeLintFile(t, filepath.Join(f.comp, "prompts", "helper.template.md"), "hello {{.AgentName}}\n")
	writeLintFile(t, filepath.Join(f.comp, "template-fragments", "footer.template.md"), `{{ define "footer" }}FOOTER{{ end }}`)
	for _, leaf := range []string{f.leaf, f.leaf2} {
		name := filepath.Base(leaf)
		writeLintFile(t, filepath.Join(leaf, "pack.toml"), `[pack]
name = "`+name+`"
version = "0.1.0"
schema = 2

[[agent]]
name = "`+name+`-worker"
prompt_template = "prompts/worker.template.md"
inject_fragments = ["footer"]
`)
		writeLintFile(t, filepath.Join(leaf, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")
	}
	return f
}

const lintNestedCityToml = "[workspace]\nname = \"nested\"\n"

// countLintCityLoads swaps the city loader for one that counts loads per
// normalized city dir.
func countLintCityLoads(t *testing.T) map[string]int {
	t.Helper()
	loads := make(map[string]int)
	orig := lintLoadCityConfig
	lintLoadCityConfig = func(cityPath string) (*config.City, error) {
		loads[pathutil.NormalizePathForCompare(cityPath)]++
		return orig(cityPath)
	}
	t.Cleanup(func() { lintLoadCityConfig = orig })
	return loads
}

func lintWarningLines(stderr, needle string) []string {
	var out []string
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, ": warning: ") && strings.Contains(line, needle) {
			out = append(out, line)
		}
	}
	return out
}

// TestLintNestedCityDoesNotShadowComposingCity is the nested-city regression:
// the nearest enclosing city.toml composes nothing, so it must not decide the
// fragment namespace. The root city composes the pack tree, and both
// `gc lint .` (walk-root city) and `gc lint <leaf>` (ancestor chain) find it.
func TestLintNestedCityDoesNotShadowComposingCity(t *testing.T) {
	f := writeLintNestedCityFixture(t, lintNestedCityToml)
	if _, err := loadCityConfigAdvisory(f.nested); err != nil {
		t.Fatalf("fixture nested city must load: %v", err)
	}
	if _, err := loadCityConfigAdvisory(f.root); err != nil {
		t.Fatalf("fixture root city must load: %v", err)
	}

	t.Run("walk root", func(t *testing.T) {
		t.Chdir(f.root)
		var stdout, stderr bytes.Buffer
		code := run([]string{"lint", "."}, &stdout, &stderr)
		if code != 0 || strings.Contains(stderr.String(), "inject_fragment") {
			t.Fatalf("gc lint . = %d, want 0 and no fragment findings\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
		}
		if lines := lintWarningLines(stderr.String(), ""); len(lines) != 0 {
			t.Fatalf("composed packs must lint without warnings, got:\n%s", strings.Join(lines, "\n"))
		}
	})
	t.Run("leaf only", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"lint", f.leaf}, &stdout, &stderr)
		if code != 0 || strings.Contains(stderr.String(), "inject_fragment") {
			t.Fatalf("gc lint <leaf> = %d, want 0 and no fragment findings\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
		}
		if lines := lintWarningLines(stderr.String(), ""); len(lines) != 0 {
			t.Fatalf("composed pack must lint without warnings, got:\n%s", strings.Join(lines, "\n"))
		}
	})
}

// TestLintLoadsEachCandidateCityOncePerInvocation pins the memoization: two
// leaves share two candidate cities, and each city is loaded exactly once for
// the whole invocation.
func TestLintLoadsEachCandidateCityOncePerInvocation(t *testing.T) {
	f := writeLintNestedCityFixture(t, lintNestedCityToml)
	loads := countLintCityLoads(t)

	// Walk root is the nested city: it is the first candidate for both leaves,
	// and the root city is the second.
	t.Chdir(f.nested)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"lint", "."}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc lint . = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	for _, city := range []string{f.root, f.nested} {
		if loads[city] != 1 {
			t.Errorf("city %s loaded %d times, want exactly 1 (loads: %v)", city, loads[city], loads)
		}
	}
}

// TestLintWarnsOncePerCityThatFailsToLoad: a candidate city that cannot load is
// reported once per invocation, naming the city and the error, and the next
// candidate is tried; the warning does not fail the run.
func TestLintWarnsOncePerCityThatFailsToLoad(t *testing.T) {
	f := writeLintNestedCityFixture(t, "[workspace\nname = \n")
	loads := countLintCityLoads(t)

	t.Chdir(f.nested)
	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", "."}, &stdout, &stderr)
	if code != 0 || strings.Contains(stderr.String(), "inject_fragment") {
		t.Fatalf("gc lint . = %d, want 0 (root city still composes the packs)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	lines := lintWarningLines(stderr.String(), f.nested)
	if len(lines) != 1 {
		t.Fatalf("want exactly one load warning naming %s for two packs under it, got %d:\n%s\nfull stderr:\n%s", f.nested, len(lines), strings.Join(lines, "\n"), stderr.String())
	}
	if !strings.Contains(lines[0], "failed to load") {
		t.Errorf("load warning does not say the city failed to load: %s", lines[0])
	}
	if loads[f.nested] != 1 {
		t.Errorf("failing city loaded %d times, want 1", loads[f.nested])
	}
}

// TestLintCityLoadWarningCarriesTheLoadError pins the half of the W1 warning
// that says why: besides the city, the message carries the loader's own error,
// collapsed to one line, so the operator can see what to fix.
func TestLintCityLoadWarningCarriesTheLoadError(t *testing.T) {
	f := writeLintNestedCityFixture(t, "[workspace\nname = \n")
	_, loadErr := loadCityConfigAdvisory(f.nested)
	if loadErr == nil {
		t.Fatal("fixture nested city must fail to load")
	}
	want := strings.Join(strings.Fields(loadErr.Error()), " ")
	if want == "" {
		t.Fatal("fixture load error has no text to look for")
	}

	t.Chdir(f.nested)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"lint", "."}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc lint . = %d, want 0 (root city still composes the packs)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	lines := lintWarningLines(stderr.String(), f.nested)
	if len(lines) != 1 {
		t.Fatalf("want exactly one load warning naming %s, got %d:\n%s\nfull stderr:\n%s", f.nested, len(lines), strings.Join(lines, "\n"), stderr.String())
	}
	if !strings.Contains(lines[0], want) {
		t.Errorf("load warning does not carry the load error\nwant it to contain: %s\ngot: %s", want, lines[0])
	}
}

// TestLintWarnsWhenNoCityComposesPack: a loadable city that does not compose
// the pack keeps the #6256 fallback (the city's whole pack set) and says so.
func TestLintWarnsWhenNoCityComposesPack(t *testing.T) {
	root := pathutil.NormalizePathForCompare(t.TempDir())
	writeLintFile(t, filepath.Join(root, "city.toml"), `[[rigs]]
name = "myrig"
includes = ["packs/other"]
`)
	other := filepath.Join(root, "packs", "other")
	writeLintPack(t, other, "other", "helper", "prompts/helper.template.md")
	writeLintFile(t, filepath.Join(other, "prompts", "helper.template.md"), "hello {{.AgentName}}\n")
	writeLintFile(t, filepath.Join(other, "template-fragments", "footer.template.md"), `{{ define "footer" }}FOOTER{{ end }}`)
	orphan := filepath.Join(root, "packs", "orphan")
	writeLintFile(t, filepath.Join(orphan, "pack.toml"), `[pack]
name = "orphan"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
inject_fragments = ["footer"]
`)
	writeLintFile(t, filepath.Join(orphan, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", orphan}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0 (fallback keeps resolving; the warning never fails the run)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	lines := lintWarningLines(stderr.String(), "not composed")
	if len(lines) != 1 || !strings.Contains(lines[0], "orphan") || !strings.Contains(lines[0], root) {
		t.Fatalf("want one warning naming the pack %q and city %s, got:\n%s\nfull stderr:\n%s", "orphan", root, strings.Join(lines, "\n"), stderr.String())
	}
}

// TestLintNotComposedWarningNamesPackAndCity pins the text of the W2 warning.
// The warning is located at the pack's pack.toml, and that path lies under the
// city directory, so the location alone already contains the city and the
// directory name; here the pack's name differs from its directory and only the
// text after "warning: " is read.
func TestLintNotComposedWarningNamesPackAndCity(t *testing.T) {
	root := pathutil.NormalizePathForCompare(t.TempDir())
	writeLintFile(t, filepath.Join(root, "city.toml"), `[[rigs]]
name = "myrig"
includes = ["packs/other"]
`)
	other := filepath.Join(root, "packs", "other")
	writeLintPack(t, other, "other", "helper", "prompts/helper.template.md")
	writeLintFile(t, filepath.Join(other, "prompts", "helper.template.md"), "hello {{.AgentName}}\n")
	stray := filepath.Join(root, "packs", "stray-dir")
	writeLintPack(t, stray, "stray-name", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(stray, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	if code := run([]string{"lint", stray}, &stdout, &stderr); code != 0 {
		t.Fatalf("gc lint = %d, want 0 (the warning never fails the run)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	lines := lintWarningLines(stderr.String(), "not composed")
	if len(lines) != 1 {
		t.Fatalf("want exactly one not-composed warning, got %d:\n%s\nfull stderr:\n%s", len(lines), strings.Join(lines, "\n"), stderr.String())
	}
	_, message, _ := strings.Cut(lines[0], ": warning: ")
	if !strings.Contains(message, `"stray-name"`) {
		t.Errorf("warning text does not name the pack %q: %s", "stray-name", message)
	}
	if !strings.Contains(message, root) {
		t.Errorf("warning text does not name the city %s: %s", root, message)
	}
}

// TestLintNoCandidateLoadsKeepsOwnDirs: when every candidate city fails to load
// lint falls back to the pack's own dirs, with the load warning, and a fragment
// only the city could have supplied is still reported missing.
func TestLintNoCandidateLoadsKeepsOwnDirs(t *testing.T) {
	root := pathutil.NormalizePathForCompare(t.TempDir())
	writeLintFile(t, filepath.Join(root, "city.toml"), "[[rigs\n")
	pack := filepath.Join(root, "packs", "lonely")
	writeLintFile(t, filepath.Join(pack, "pack.toml"), `[pack]
name = "lonely"
version = "0.1.0"
schema = 2

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
inject_fragments = ["footer"]
`)
	writeLintFile(t, filepath.Join(pack, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", pack}, &stdout, &stderr)
	if code == 0 || !strings.Contains(stderr.String(), `inject_fragment "footer"`) {
		t.Fatalf("gc lint = %d, want the missing fragment reported\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if lines := lintWarningLines(stderr.String(), root); len(lines) != 1 || !strings.Contains(lines[0], "failed to load") {
		t.Fatalf("want one load warning naming %s, got:\n%s", root, strings.Join(lines, "\n"))
	}
}

// TestLintStandaloneAndRootPackEmitNoCityWarnings: no city on the chain stays
// standalone and silent, and a pack at the city root counts as composed.
func TestLintStandaloneAndRootPackEmitNoCityWarnings(t *testing.T) {
	t.Run("no city", func(t *testing.T) {
		dir := t.TempDir()
		writeLintPack(t, dir, "solo", "worker", "prompts/worker.template.md")
		writeLintFile(t, filepath.Join(dir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")
		var stdout, stderr bytes.Buffer
		if code := run([]string{"lint", dir}, &stdout, &stderr); code != 0 {
			t.Fatalf("gc lint = %d\nstderr:\n%s", code, stderr.String())
		}
		if lines := lintWarningLines(stderr.String(), ""); len(lines) != 0 {
			t.Fatalf("standalone pack must not warn, got:\n%s", strings.Join(lines, "\n"))
		}
	})
	t.Run("city root pack", func(t *testing.T) {
		root := pathutil.NormalizePathForCompare(t.TempDir())
		writeLintFile(t, filepath.Join(root, "city.toml"), lintNestedCityToml)
		writeLintPack(t, root, "rootpack", "worker", "prompts/worker.template.md")
		writeLintFile(t, filepath.Join(root, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")
		var stdout, stderr bytes.Buffer
		if code := run([]string{"lint", root}, &stdout, &stderr); code != 0 {
			t.Fatalf("gc lint = %d\nstderr:\n%s", code, stderr.String())
		}
		if lines := lintWarningLines(stderr.String(), "not composed"); len(lines) != 0 {
			t.Fatalf("the city's own root pack is composed, got:\n%s", strings.Join(lines, "\n"))
		}
	})
}

func writeLintPack(t *testing.T, dir, packName, agentName, promptTemplate string) {
	t.Helper()
	writeLintFile(t, filepath.Join(dir, "pack.toml"), `[pack]
name = "`+packName+`"
version = "0.1.0"
schema = 2

[[agent]]
name = "`+agentName+`"
prompt_template = "`+promptTemplate+`"
`)
}

func writeLintFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLintFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLintFormulaOutputJSONWarning(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "mypack", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	formulaDir := filepath.Join(packDir, "formulas")
	writeLintFile(t, filepath.Join(formulaDir, "legacy.formula.toml"), strings.TrimSpace(`
formula = "legacy-fanout"
version = 1
contract = "graph.v2"
[[steps]]
id = "worker"
prompt = "do work"
[steps.metadata]
"gc.output_json_required" = "true"
`)+"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0 (warnings-only exits 0)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc.output_json is deprecated; use drain in v2 formulas") {
		t.Errorf("stderr = %q, want gc.output_json warning", stderr.String())
	}
}

func TestLintFormulaNoWarningForDrain(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "mypack", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	formulaDir := filepath.Join(packDir, "formulas")
	writeLintFile(t, filepath.Join(formulaDir, "drain.formula.toml"), strings.TrimSpace(`
formula = "drain-fanout"
version = 1
contract = "graph.v2"
[[steps]]
id = "worker"
prompt = "do work"
[steps.drain]
context = "separate"
formula = "mol-do-work"
member_access = "exclusive"
`)+"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "gc.output_json") {
		t.Errorf("stderr = %q, must not warn about gc.output_json for drain formula", stderr.String())
	}
}

func TestLintFormulaNoWarningForGraphV1(t *testing.T) {
	packDir := t.TempDir()
	writeLintPack(t, packDir, "mypack", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")

	formulaDir := filepath.Join(packDir, "formulas")
	writeLintFile(t, filepath.Join(formulaDir, "v1.formula.toml"), strings.TrimSpace(`
formula = "v1-fanout"
version = 1
[[steps]]
id = "worker"
prompt = "do work"
[steps.metadata]
"gc.output_json_required" = "true"
`)+"\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stderr.String(), "gc.output_json") {
		t.Errorf("stderr = %q, must not warn about gc.output_json for graph.v1 formula", stderr.String())
	}
}

// writeLintHookSettings writes settingsJSON as a .claude/settings.json overlay
// inside an otherwise valid pack and returns the pack directory.
func writeLintHookSettings(t *testing.T, settingsJSON string) string {
	t.Helper()
	packDir := t.TempDir()
	writeLintPack(t, packDir, "matchers", "worker", "prompts/worker.template.md")
	writeLintFile(t, filepath.Join(packDir, "prompts", "worker.template.md"), "hello {{.AgentName}}\n")
	writeLintFile(t, filepath.Join(packDir, "overlays", "default", ".claude", "settings.json"), settingsJSON)
	return packDir
}

func TestLintRejectsPermissionSyntaxHookMatcher(t *testing.T) {
	packDir := writeLintHookSettings(t, `{"hooks":{"PreToolUse":[{"matcher":"Bash(*bd mol pour*patrol*)","hooks":[{"type":"command","command":"true"}]}]}}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc lint = 0, want a permission-syntax hook matcher to fail the pack\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
	wantLoc := filepath.Join(".claude", "settings.json") + ": hooks.PreToolUse[0] matcher"
	if !strings.Contains(stderr.String(), wantLoc) {
		t.Errorf("stderr missing %q:\n%s", wantLoc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "permission-rule syntax") {
		t.Errorf("stderr missing the permission-syntax message:\n%s", stderr.String())
	}
}

func TestLintWarnsOnUnknownToolHookMatcherWithoutFailing(t *testing.T) {
	packDir := writeLintHookSettings(t, `{"hooks":{"PreToolUse":[{"matcher":"^Frobnicate$","hooks":[{"type":"command","command":"true"}]}]}}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0 (a matcher naming no known tool only warns)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	wantLoc := filepath.Join(".claude", "settings.json") + ": warning: hooks.PreToolUse[0] matcher"
	if !strings.Contains(stderr.String(), wantLoc) {
		t.Errorf("stderr missing %q:\n%s", wantLoc, stderr.String())
	}
}

// A grouped regex over unknown tools with a suffix after the group is a regex,
// not a permission rule; lint must warn about the unknown tool, not fail.
func TestLintWarnsOnGroupedRegexOverUnknownToolWithoutFailing(t *testing.T) {
	packDir := writeLintHookSettings(t, `{"hooks":{"PreToolUse":[{"matcher":"Browser(Open|Close).*","hooks":[{"type":"command","command":"true"}]}]}}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0 (a grouped regex over unknown tools only warns)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	wantLoc := filepath.Join(".claude", "settings.json") + ": warning: hooks.PreToolUse[0] matcher"
	if !strings.Contains(stderr.String(), wantLoc) {
		t.Errorf("stderr missing %q:\n%s", wantLoc, stderr.String())
	}
}

// A JavaScript-only escape such as \u0042 is valid in Claude Code but does not
// compile under Go RE2; lint must warn, not fail the pack.
func TestLintWarnsOnJavaScriptOnlyHookMatcherWithoutFailing(t *testing.T) {
	packDir := writeLintHookSettings(t, `{"hooks":{"PreToolUse":[{"matcher":"^\\u0042ash$","hooks":[{"type":"command","command":"true"}]}]}}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0 (a JavaScript-only matcher only warns)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	wantLoc := filepath.Join(".claude", "settings.json") + ": warning: hooks.PreToolUse[0] matcher"
	if !strings.Contains(stderr.String(), wantLoc) {
		t.Errorf("stderr missing %q:\n%s", wantLoc, stderr.String())
	}
}

// A JavaScript identity escape such as \R reads as the literal letter in Claude
// Code but is an invalid escape under Go RE2; lint must warn, not fail the pack.
func TestLintWarnsOnJavaScriptIdentityEscapeHookMatcherWithoutFailing(t *testing.T) {
	packDir := writeLintHookSettings(t, `{"hooks":{"PreToolUse":[{"matcher":"^\\Read$","hooks":[{"type":"command","command":"true"}]}]}}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0 (a JavaScript identity escape only warns)\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	wantLoc := filepath.Join(".claude", "settings.json") + ": warning: hooks.PreToolUse[0] matcher"
	if !strings.Contains(stderr.String(), wantLoc) {
		t.Errorf("stderr missing %q:\n%s", wantLoc, stderr.String())
	}
}

// The matchers this city and its packs ship today must keep linting clean: the
// anchored ^Bash$ guard, the empty all-tools matcher, and SessionStart's
// "startup" source matcher.
func TestLintAcceptsShippedHookMatchers(t *testing.T) {
	packDir := writeLintHookSettings(t, `{"hooks":{
		"SessionStart":[
			{"matcher":"startup","hooks":[{"type":"command","command":"true"}]},
			{"matcher":"","hooks":[{"type":"command","command":"true"}]}
		],
		"PreToolUse":[{"matcher":"^Bash$","hooks":[{"type":"command","command":"true"}]}],
		"PreCompact":[{"matcher":"","hooks":[{"type":"command","command":"true"}]}],
		"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"true"}]}]
	}}`)

	var stdout, stderr bytes.Buffer
	code := run([]string{"lint", packDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc lint = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func validateLintJSONSchema(t *testing.T, data []byte) {
	t.Helper()
	rawSchema, err := readBuiltinSchema([]string{"lint"}, jsonSchemaResultRole)
	if err != nil {
		t.Fatalf("read lint schema: %v", err)
	}
	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(rawSchema))
	if err != nil {
		t.Fatalf("parse lint schema: %v", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse lint payload: %v\n%s", err, string(data))
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("lint/result.schema.json", schemaDoc); err != nil {
		t.Fatalf("add lint schema: %v", err)
	}
	compiled, err := compiler.Compile("lint/result.schema.json")
	if err != nil {
		t.Fatalf("compile lint schema: %v", err)
	}
	if err := compiled.Validate(instance); err != nil {
		t.Fatalf("lint payload does not validate: %v\n%s", err, string(data))
	}
}
