// Package gastown_test validates the Gas Town example configuration.
//
// This test ensures the example stays valid as the SDK evolves:
// city.toml parses and validates, all formulas parse, and all
// prompt template files referenced by agents exist on disk.
package gastown_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func exampleDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

func runCmd(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	return runCmd(t, dir, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
}

// loadExpanded loads city.toml with full pack expansion.
func loadExpanded(t *testing.T) *config.City {
	t.Helper()
	dir := exampleDir()
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("config.LoadWithIncludes: %v", err)
	}
	return cfg
}

func TestCityTomlParses(t *testing.T) {
	dir := exampleDir()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Workspace.Name != "gastown" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "gastown")
	}
	if len(cfg.Workspace.Includes) != 1 || cfg.Workspace.Includes[0] != "packs/gastown" {
		t.Errorf("Workspace.Includes = %v, want [packs/gastown]", cfg.Workspace.Includes)
	}
}

func TestCityTomlValidates(t *testing.T) {
	cfg := loadExpanded(t)
	if err := config.ValidateAgents(cfg.Agents); err != nil {
		t.Errorf("ValidateAgents: %v", err)
	}
}

func TestPromptFilesExist(t *testing.T) {
	dir := exampleDir()
	cfg := loadExpanded(t)
	for _, a := range cfg.Agents {
		if a.PromptTemplate == "" || a.Implicit {
			continue
		}
		path := filepath.Join(dir, a.PromptTemplate)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("agent %q: prompt_template %q: %v", a.Name, a.PromptTemplate, err)
		}
	}
}

func TestOverlayDirsExist(t *testing.T) {
	dir := exampleDir()
	cfg := loadExpanded(t)
	for _, a := range cfg.Agents {
		if a.OverlayDir == "" {
			continue
		}
		path := filepath.Join(dir, a.OverlayDir)
		if info, err := os.Stat(path); err != nil {
			t.Errorf("agent %q: overlay_dir %q: %v", a.Name, a.OverlayDir, err)
		} else if !info.IsDir() {
			t.Errorf("agent %q: overlay_dir %q is not a directory", a.Name, a.OverlayDir)
		}
	}
}

func TestRefineryPromptSeedsTargetBranchVar(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "prompts", "refinery.md.tmpl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading refinery prompt: %v", err)
	}
	if !strings.Contains(string(data), "--var target_branch={{ .DefaultBranch }}") {
		t.Errorf("refinery prompt missing target_branch var injection:\n%s", data)
	}
}

func TestRefineryFormulaSupportsMergeStrategies(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-refinery-patrol.formula.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading refinery formula: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		".metadata.merge_strategy // \"direct\"",
		"gh pr create",
		"Pull request ready:",
		"merge_strategy=local",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("refinery formula missing %q", want)
		}
	}
}

func TestWorktreeSetupKeepsIgnoresLocal(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	city := filepath.Join(tmp, "city")
	script := filepath.Join(exampleDir(), "packs", "gastown", "scripts", "worktree-setup.sh")

	runCmd(t, tmp, "git", "init", repo)
	runCmd(t, repo, "git", "config", "user.email", "test@example.com")
	runCmd(t, repo, "git", "config", "user.name", "Gastown Test")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatalf("writing repo .gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing repo README: %v", err)
	}
	runCmd(t, repo, "git", "add", ".")
	runCmd(t, repo, "git", "commit", "-m", "init")

	worktree := filepath.Join(city, ".gc", "worktrees", filepath.Base(repo), "polecat-a")
	runCmd(t, tmp, "sh", script, repo, worktree, "polecat-a")

	gitignorePath := filepath.Join(worktree, ".gitignore")
	data, err := os.ReadFile(gitignorePath)
	if err != nil {
		t.Fatalf("reading worktree .gitignore: %v", err)
	}
	if got := string(data); got != "node_modules/\n" {
		t.Fatalf("worktree .gitignore = %q, want original repo content only", got)
	}

	excludePath := runCmd(t, tmp, "git", "-C", worktree, "rev-parse", "--git-path", "info/exclude")
	if !filepath.IsAbs(excludePath) {
		excludePath = filepath.Join(worktree, excludePath)
	}
	excludeData, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("reading local exclude: %v", err)
	}
	exclude := string(excludeData)
	for _, want := range []string{
		"# Gas City worktree infrastructure (local excludes)",
		".beads/redirect",
		".beads/hooks/",
		".beads/formulas/",
		".runtime/",
		".logs/",
		"worktrees/",
		"__pycache__/",
		".claude/",
		".codex/",
		".gemini/",
		".opencode/",
		".github/hooks/",
		".github/copilot-instructions.md",
		"state.json",
	} {
		if !strings.Contains(exclude, want) {
			t.Fatalf("local exclude missing %q:\n%s", want, exclude)
		}
	}

	runtimeFiles := map[string]string{
		filepath.Join(worktree, ".claude", "commands", "review.md"):        "review\n",
		filepath.Join(worktree, ".codex", "hooks.json"):                    "{}\n",
		filepath.Join(worktree, ".gemini", "settings.json"):                "{}\n",
		filepath.Join(worktree, ".opencode", "plugins", "gascity.js"):      "module.exports = {};\n",
		filepath.Join(worktree, ".github", "hooks", "gascity.json"):        "{}\n",
		filepath.Join(worktree, ".github", "copilot-instructions.md"):      "copilot\n",
		filepath.Join(worktree, ".runtime", "state.json"):                  "{}\n",
		filepath.Join(worktree, ".logs", "session.log"):                    "log\n",
		filepath.Join(worktree, "__pycache__", "module.cpython-313.pyc"):   "pyc\n",
		filepath.Join(worktree, "state.json"):                              "{}\n",
		filepath.Join(worktree, ".beads", "hooks", "post-applypatch.sh"):   "#!/bin/sh\n",
		filepath.Join(worktree, ".beads", "formulas", "sample.formula.sh"): "#!/bin/sh\n",
	}
	for path, contents := range runtimeFiles {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating runtime file dir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("writing runtime file %s: %v", path, err)
		}
	}
	if status := runCmd(t, tmp, "git", "-C", worktree, "status", "--porcelain"); status != "" {
		t.Fatalf("expected clean worktree after runtime files, got:\n%s", status)
	}

	before := exclude
	runCmd(t, tmp, "sh", script, repo, worktree, "polecat-a")
	afterData, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("reading local exclude after rerun: %v", err)
	}
	if got := string(afterData); got != before {
		t.Fatalf("local exclude changed on rerun:\nBEFORE:\n%s\nAFTER:\n%s", before, got)
	}
}

func TestWorktreeSetupBootstrapsPrepopulatedTargetDir(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	city := filepath.Join(tmp, "city")
	script := filepath.Join(exampleDir(), "packs", "gastown", "scripts", "worktree-setup.sh")

	runCmd(t, tmp, "git", "init", repo)
	runCmd(t, repo, "git", "config", "user.email", "test@example.com")
	runCmd(t, repo, "git", "config", "user.name", "Gastown Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing repo README: %v", err)
	}
	runCmd(t, repo, "git", "add", ".")
	runCmd(t, repo, "git", "commit", "-m", "init")

	worktree := filepath.Join(city, ".gc", "worktrees", filepath.Base(repo), "refinery")
	stagedPath := filepath.Join(worktree, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(stagedPath), 0o755); err != nil {
		t.Fatalf("creating staged dir: %v", err)
	}
	if err := os.WriteFile(stagedPath, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("writing staged file: %v", err)
	}

	runCmd(t, tmp, "sh", script, repo, worktree, "refinery")

	if got := runCmd(t, tmp, "git", "-C", worktree, "rev-parse", "--is-inside-work-tree"); got != "true" {
		t.Fatalf("worktree bootstrap did not produce a git worktree, got %q", got)
	}
	if _, err := os.Stat(stagedPath); err != nil {
		t.Fatalf("staged runtime file missing after bootstrap: %v", err)
	}
}

func TestWorktreeSetupBootstrapsPrepopulatedNestedRuntimeTree(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	city := filepath.Join(tmp, "city")
	script := filepath.Join(exampleDir(), "packs", "gastown", "scripts", "worktree-setup.sh")

	runCmd(t, tmp, "git", "init", repo)
	runCmd(t, repo, "git", "config", "user.email", "test@example.com")
	runCmd(t, repo, "git", "config", "user.name", "Gastown Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing repo README: %v", err)
	}
	runCmd(t, repo, "git", "add", ".")
	runCmd(t, repo, "git", "commit", "-m", "init")

	worktree := filepath.Join(city, ".gc", "worktrees", filepath.Base(repo), "polecat")
	stagedFiles := map[string]string{
		filepath.Join(worktree, ".gc", "scripts", "agent-menu.sh"): "#!/bin/sh\n",
		filepath.Join(worktree, ".gc", "scripts", "bind-key.sh"):   "#!/bin/sh\n",
		filepath.Join(worktree, ".gc", "settings.json"):            "{}\n",
	}
	for path, contents := range stagedFiles {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating staged dir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("writing staged file %s: %v", path, err)
		}
	}

	runCmd(t, tmp, "sh", script, repo, worktree, "polecat")

	if got := runCmd(t, tmp, "git", "-C", worktree, "rev-parse", "--is-inside-work-tree"); got != "true" {
		t.Fatalf("worktree bootstrap did not produce a git worktree, got %q", got)
	}
	for path := range stagedFiles {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("staged runtime file missing after bootstrap: %v", err)
		}
	}
	stageGlobs, err := filepath.Glob(filepath.Join(filepath.Dir(worktree), ".gascity-worktree-stage.*"))
	if err != nil {
		t.Fatalf("glob stage dirs: %v", err)
	}
	if len(stageGlobs) != 0 {
		t.Fatalf("unexpected leftover stage dirs: %v", stageGlobs)
	}
}

func TestWorktreeSetupPreservesTrackedFilesInPrepopulatedTargetDir(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	city := filepath.Join(tmp, "city")
	script := filepath.Join(exampleDir(), "packs", "gastown", "scripts", "worktree-setup.sh")

	runCmd(t, tmp, "git", "init", repo)
	runCmd(t, repo, "git", "config", "user.email", "test@example.com")
	runCmd(t, repo, "git", "config", "user.name", "Gastown Test")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("tracked/\n"), 0o644); err != nil {
		t.Fatalf("writing repo .gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing repo README: %v", err)
	}
	runCmd(t, repo, "git", "add", ".")
	runCmd(t, repo, "git", "commit", "-m", "init")

	worktree := filepath.Join(city, ".gc", "worktrees", filepath.Base(repo), "refinery")
	stagedRuntime := filepath.Join(worktree, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(stagedRuntime), 0o755); err != nil {
		t.Fatalf("creating staged runtime dir: %v", err)
	}
	if err := os.WriteFile(stagedRuntime, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("writing staged runtime file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".gitignore"), []byte("staged\n"), 0o644); err != nil {
		t.Fatalf("writing staged tracked file: %v", err)
	}

	runCmd(t, tmp, "sh", script, repo, worktree, "refinery")

	gitignoreData, err := os.ReadFile(filepath.Join(worktree, ".gitignore"))
	if err != nil {
		t.Fatalf("reading worktree .gitignore: %v", err)
	}
	if got := string(gitignoreData); got != "tracked/\n" {
		t.Fatalf("worktree .gitignore = %q, want tracked repo content", got)
	}
	if _, err := os.Stat(stagedRuntime); err != nil {
		t.Fatalf("staged runtime file missing after bootstrap: %v", err)
	}
	if status := runCmd(t, tmp, "git", "-C", worktree, "status", "--porcelain"); status != "" {
		t.Fatalf("expected clean worktree after preserving tracked files, got:\n%s", status)
	}
}

func TestWorktreeSetupSupportsLegacySignature(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	city := filepath.Join(tmp, "city")
	script := filepath.Join(exampleDir(), "packs", "gastown", "scripts", "worktree-setup.sh")

	runCmd(t, tmp, "git", "init", repo)
	runCmd(t, repo, "git", "config", "user.email", "test@example.com")
	runCmd(t, repo, "git", "config", "user.name", "Gastown Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing repo README: %v", err)
	}
	runCmd(t, repo, "git", "add", ".")
	runCmd(t, repo, "git", "commit", "-m", "init")

	runCmd(t, tmp, "sh", script, repo, "demo/refinery", city)

	worktree := filepath.Join(city, ".gc", "worktrees", filepath.Base(repo), "demo", "refinery")
	if got := runCmd(t, tmp, "git", "-C", worktree, "rev-parse", "--is-inside-work-tree"); got != "true" {
		t.Fatalf("legacy signature did not produce a git worktree, got %q", got)
	}
}

func TestWorktreeSetupReusesExistingAgentBranch(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	city := filepath.Join(tmp, "city")
	script := filepath.Join(exampleDir(), "packs", "gastown", "scripts", "worktree-setup.sh")

	runCmd(t, tmp, "git", "init", repo)
	runCmd(t, repo, "git", "config", "user.email", "test@example.com")
	runCmd(t, repo, "git", "config", "user.name", "Gastown Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing repo README: %v", err)
	}
	runCmd(t, repo, "git", "add", ".")
	runCmd(t, repo, "git", "commit", "-m", "init")

	worktree := filepath.Join(city, ".gc", "worktrees", filepath.Base(repo), "refinery")

	runCmd(t, tmp, "sh", script, repo, worktree, "refinery")
	runCmd(t, tmp, "git", "-C", repo, "worktree", "remove", worktree, "--force")
	runCmd(t, tmp, "sh", script, repo, worktree, "refinery")

	if got := currentBranch(t, worktree); !strings.HasPrefix(got, "gc-refinery-") {
		t.Fatalf("worktree reboot attached %q, want gc-refinery-*", got)
	}
}

func TestWorktreeSetupNamespacesAgentBranchesByWorktreePath(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	cityA := filepath.Join(tmp, "city-a")
	cityB := filepath.Join(tmp, "city-b")
	script := filepath.Join(exampleDir(), "packs", "gastown", "scripts", "worktree-setup.sh")

	runCmd(t, tmp, "git", "init", repo)
	runCmd(t, repo, "git", "config", "user.email", "test@example.com")
	runCmd(t, repo, "git", "config", "user.name", "Gastown Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing repo README: %v", err)
	}
	runCmd(t, repo, "git", "add", ".")
	runCmd(t, repo, "git", "commit", "-m", "init")

	worktreeA := filepath.Join(cityA, ".gc", "worktrees", filepath.Base(repo), "refinery")
	worktreeB := filepath.Join(cityB, ".gc", "worktrees", filepath.Base(repo), "refinery")

	runCmd(t, tmp, "sh", script, repo, worktreeA, "refinery")
	runCmd(t, tmp, "sh", script, repo, worktreeB, "refinery")

	branchA := currentBranch(t, worktreeA)
	branchB := currentBranch(t, worktreeB)
	if !strings.HasPrefix(branchA, "gc-refinery-") {
		t.Fatalf("branchA = %q, want gc-refinery-*", branchA)
	}
	if !strings.HasPrefix(branchB, "gc-refinery-") {
		t.Fatalf("branchB = %q, want gc-refinery-*", branchB)
	}
	if branchA == branchB {
		t.Fatalf("branch names should differ across worktree paths, got %q", branchA)
	}
}

func TestWorktreeSetupSyncSkipsMissingOrigin(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	city := filepath.Join(tmp, "city")
	script := filepath.Join(exampleDir(), "packs", "gastown", "scripts", "worktree-setup.sh")

	runCmd(t, tmp, "git", "init", repo)
	runCmd(t, repo, "git", "config", "user.email", "test@example.com")
	runCmd(t, repo, "git", "config", "user.name", "Gastown Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("writing repo README: %v", err)
	}
	runCmd(t, repo, "git", "add", ".")
	runCmd(t, repo, "git", "commit", "-m", "init")

	worktree := filepath.Join(city, ".gc", "worktrees", filepath.Base(repo), "polecat-a")
	runCmd(t, tmp, "sh", script, repo, worktree, "polecat-a", "--sync")
	runCmd(t, tmp, "sh", script, repo, worktree, "polecat-a", "--sync")

	if got := runCmd(t, tmp, "git", "-C", worktree, "rev-parse", "--is-inside-work-tree"); got != "true" {
		t.Fatalf("worktree sync did not preserve git worktree, got %q", got)
	}
}

func TestPromptGuidanceUsesConfiguredRigRootsAndNamespacedWorktrees(t *testing.T) {
	dir := exampleDir()

	mayorPrompt, err := os.ReadFile(filepath.Join(dir, "packs", "gastown", "prompts", "mayor.md.tmpl"))
	if err != nil {
		t.Fatalf("reading mayor prompt: %v", err)
	}
	if strings.Contains(string(mayorPrompt), "{{ .CityRoot }}/<rig>") {
		t.Fatalf("mayor prompt still hardcodes {{ .CityRoot }}/<rig>:\n%s", mayorPrompt)
	}
	if !strings.Contains(string(mayorPrompt), "{{ cmd }} rig status <rig>") {
		t.Fatalf("mayor prompt missing rig-status guidance:\n%s", mayorPrompt)
	}

	crewPrompt, err := os.ReadFile(filepath.Join(dir, "packs", "gastown", "prompts", "crew.md.tmpl"))
	if err != nil {
		t.Fatalf("reading crew prompt: %v", err)
	}
	if !strings.Contains(string(crewPrompt), "{{ .CityRoot }}/.gc/worktrees/$TARGET_RIG/crew/") {
		t.Fatalf("crew prompt missing namespaced worktree path:\n%s", crewPrompt)
	}

	polecatPrompt, err := os.ReadFile(filepath.Join(dir, "packs", "gastown", "prompts", "polecat.md.tmpl"))
	if err != nil {
		t.Fatalf("reading polecat prompt: %v", err)
	}
	if strings.Contains(string(polecatPrompt), "that's not a git working tree") {
		t.Fatalf("polecat prompt still claims rig root is not a git working tree:\n%s", polecatPrompt)
	}
}

func TestIdeaToPlanFormulaUsesSupportedPrimitives(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-idea-to-plan.formula.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading idea-to-plan formula: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		`formula = "mol-idea-to-plan"`,
		`gc sling "$REVIEW_TARGET" "$LEG_BEAD" --on {{review_formula}}`,
		`bd create`,
		`gc mail send`,
		`bd dep add`,
		`Do NOT use unsupported upstream shortcuts`,
		`This is the only required human gate.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("idea-to-plan formula missing %q", want)
		}
	}
}

func TestReviewLegFormulaPersistsReportAndNotifiesCoordinator(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-review-leg.formula.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading review-leg formula: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		`formula = "mol-review-leg"`,
		`coordinator`,
		`bd update {{issue}} --notes`,
		`gc mail send "$COORD"`,
		`bd update {{issue}} --status=closed`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("review-leg formula missing %q", want)
		}
	}
}

func TestAllFormulasExist(t *testing.T) {
	dir := exampleDir()
	formulaDir := filepath.Join(dir, "packs", "gastown", "formulas")

	entries, err := os.ReadDir(formulaDir)
	if err != nil {
		t.Fatalf("reading formulas dir: %v", err)
	}

	var count int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".formula.toml") {
			continue
		}
		count++
	}

	if count == 0 {
		t.Error("no formula files found")
	}
}

func TestAllPromptTemplatesExist(t *testing.T) {
	dir := exampleDir()
	promptDir := filepath.Join(dir, "packs", "gastown", "prompts")

	entries, err := os.ReadDir(promptDir)
	if err != nil {
		t.Fatalf("reading prompts dir: %v", err)
	}

	var count int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md.tmpl") {
			continue
		}
		count++
		t.Run(e.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(promptDir, e.Name()))
			if err != nil {
				t.Fatalf("reading %s: %v", e.Name(), err)
			}
			if len(data) == 0 {
				t.Errorf("%s is empty", e.Name())
			}
		})
	}

	if count != 7 {
		t.Errorf("found %d prompt template files, want 7", count)
	}
}

func TestAgentNudgeField(t *testing.T) {
	cfg := loadExpanded(t)

	// Verify nudge is populated for agents that have it.
	nudgeCounts := 0
	for _, a := range cfg.Agents {
		if a.Nudge != "" {
			nudgeCounts++
		}
	}
	if nudgeCounts == 0 {
		t.Error("no agents have nudge configured")
	}
}

func TestFormulasDir(t *testing.T) {
	cfg := loadExpanded(t)
	// Formulas come from packs, not from city.toml directly.
	// FormulaLayers.City should have formula dirs from both packs.
	// Note: bd/dolt formulas are auto-included at runtime by builtinPackIncludes,
	// not via pack.toml includes, so they won't appear in static expansion.
	if len(cfg.FormulaLayers.City) == 0 {
		t.Fatal("FormulaLayers.City is empty, want pack formulas layers")
	}
	wantSuffixes := []string{
		filepath.Join("packs", "maintenance", "formulas"),
		filepath.Join("packs", "gastown", "formulas"),
	}
	for _, suffix := range wantSuffixes {
		found := false
		for _, d := range cfg.FormulaLayers.City {
			if strings.HasSuffix(d, suffix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("FormulaLayers.City = %v, want entry ending with %s", cfg.FormulaLayers.City, suffix)
		}
	}
}

func TestPackDirsPopulated(t *testing.T) {
	cfg := loadExpanded(t)
	if len(cfg.PackDirs) == 0 {
		t.Fatal("PackDirs is empty after expansion")
	}
	// Should have pack dirs from maintenance and gastown packs.
	// Note: bd/dolt packs are auto-included at runtime by builtinPackIncludes,
	// not via pack.toml includes, so they won't appear in static expansion.
	var hasMaintenance, hasGastown bool
	for _, d := range cfg.PackDirs {
		if strings.HasSuffix(d, filepath.Join("packs", "maintenance")) {
			hasMaintenance = true
		}
		if strings.HasSuffix(d, filepath.Join("packs", "gastown")) {
			hasGastown = true
		}
	}
	if !hasMaintenance {
		t.Errorf("PackDirs missing maintenance: %v", cfg.PackDirs)
	}
	if !hasGastown {
		t.Errorf("PackDirs missing gastown: %v", cfg.PackDirs)
	}
}

func TestGlobalFragmentsParsed(t *testing.T) {
	dir := exampleDir()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.Workspace.GlobalFragments) == 0 {
		t.Fatal("Workspace.GlobalFragments is empty")
	}
	found := false
	for _, f := range cfg.Workspace.GlobalFragments {
		if f == "command-glossary" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("GlobalFragments = %v, want command-glossary", cfg.Workspace.GlobalFragments)
	}
}

func TestDaemonConfig(t *testing.T) {
	dir := exampleDir()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Daemon.PatrolInterval != "30s" {
		t.Errorf("Daemon.PatrolInterval = %q, want %q", cfg.Daemon.PatrolInterval, "30s")
	}
	if cfg.Daemon.MaxRestartsOrDefault() != 5 {
		t.Errorf("Daemon.MaxRestarts = %d, want 5", cfg.Daemon.MaxRestartsOrDefault())
	}
	if cfg.Daemon.RestartWindow != "1h" {
		t.Errorf("Daemon.RestartWindow = %q, want %q", cfg.Daemon.RestartWindow, "1h")
	}
	if cfg.Daemon.ShutdownTimeout != "5s" {
		t.Errorf("Daemon.ShutdownTimeout = %q, want %q", cfg.Daemon.ShutdownTimeout, "5s")
	}
}

// packFileConfig mirrors the pack.toml structure for test parsing.
type packFileConfig struct {
	Pack   config.PackMeta `toml:"pack"`
	Agents []config.Agent  `toml:"agent"`
}

func TestCombinedPackParses(t *testing.T) {
	dir := exampleDir()
	topoPath := filepath.Join(dir, "packs", "gastown", "pack.toml")

	data, err := os.ReadFile(topoPath)
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}

	var tc packFileConfig
	if _, err := toml.Decode(string(data), &tc); err != nil {
		t.Fatalf("parsing pack.toml: %v", err)
	}

	if tc.Pack.Name != "gastown" {
		t.Errorf("[pack] name = %q, want %q", tc.Pack.Name, "gastown")
	}
	if tc.Pack.Schema != 1 {
		t.Errorf("[pack] schema = %d, want 1", tc.Pack.Schema)
	}

	// Expect 7 agents: gastown's own 6 + themed dog (overrides maintenance fallback).
	want := map[string]bool{
		"mayor": false, "deacon": false, "boot": false,
		"witness": false, "refinery": false, "polecat": false,
		"dog": false,
	}
	for _, a := range tc.Agents {
		if _, ok := want[a.Name]; ok {
			want[a.Name] = true
		} else {
			t.Errorf("unexpected pack agent %q", a.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing pack agent %q", name)
		}
	}
	if len(tc.Agents) != 7 {
		t.Errorf("pack has %d agents, want 7", len(tc.Agents))
	}

	// Verify city-scoped agents have scope = "city".
	wantCity := map[string]bool{"mayor": true, "deacon": true, "boot": true, "dog": true}
	for _, a := range tc.Agents {
		if wantCity[a.Name] && a.Scope != "city" {
			t.Errorf("agent %q: scope = %q, want %q", a.Name, a.Scope, "city")
		}
	}
}

func TestPackUsesIsolatedWorkDirs(t *testing.T) {
	dir := exampleDir()
	topoPath := filepath.Join(dir, "packs", "gastown", "pack.toml")

	data, err := os.ReadFile(topoPath)
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}

	var tc packFileConfig
	if _, err := toml.Decode(string(data), &tc); err != nil {
		t.Fatalf("parsing pack.toml: %v", err)
	}

	want := map[string]string{
		"mayor":    ".gc/agents/mayor",
		"deacon":   ".gc/agents/deacon",
		"boot":     ".gc/agents/boot",
		"witness":  ".gc/agents/{{.Rig}}/witness",
		"refinery": ".gc/worktrees/{{.Rig}}/refinery",
		"polecat":  ".gc/worktrees/{{.Rig}}/polecats/{{.AgentBase}}",
		"dog":      ".gc/agents/dogs/{{.AgentBase}}",
	}
	for _, a := range tc.Agents {
		if expected, ok := want[a.Name]; ok && a.WorkDir != expected {
			t.Errorf("agent %q: work_dir = %q, want %q", a.Name, a.WorkDir, expected)
		}
	}
}

func TestPackPromptFilesExist(t *testing.T) {
	dir := exampleDir()
	topoDir := filepath.Join(dir, "packs", "gastown")
	topoPath := filepath.Join(topoDir, "pack.toml")

	data, err := os.ReadFile(topoPath)
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}

	var tc packFileConfig
	if _, err := toml.Decode(string(data), &tc); err != nil {
		t.Fatalf("parsing pack.toml: %v", err)
	}

	for _, a := range tc.Agents {
		if a.PromptTemplate == "" {
			continue
		}
		// Paths in pack are relative to pack dir.
		path := filepath.Join(topoDir, a.PromptTemplate)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("agent %q: prompt_template %q resolves to %q: %v",
				a.Name, a.PromptTemplate, path, err)
		}
	}
}

func TestCityAgentsFilter(t *testing.T) {
	// Verify config.LoadWithIncludes with both packs produces
	// only city-scoped agents when no rigs are registered.
	// Dog from maintenance + mayor/deacon/boot from gastown = 4.
	cfg := loadExpanded(t)

	cityAgents := map[string]bool{"mayor": true, "deacon": true, "boot": true, "dog": true}
	var explicit int
	for _, a := range cfg.Agents {
		if a.Implicit {
			continue
		}
		explicit++
		if !cityAgents[a.Name] {
			t.Errorf("unexpected agent %q — should be filtered out without rigs", a.Name)
		}
		if a.Dir != "" {
			t.Errorf("city agent %q: dir = %q, want empty", a.Name, a.Dir)
		}
	}
	if explicit != 4 {
		t.Errorf("got %d explicit agents, want 4 city-scoped agents", explicit)
	}
}

func TestMaintenancePackParses(t *testing.T) {
	dir := exampleDir()
	topoPath := filepath.Join(dir, "packs", "maintenance", "pack.toml")

	data, err := os.ReadFile(topoPath)
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}

	var tc packFileConfig
	if _, err := toml.Decode(string(data), &tc); err != nil {
		t.Fatalf("parsing pack.toml: %v", err)
	}

	if tc.Pack.Name != "maintenance" {
		t.Errorf("[pack] name = %q, want %q", tc.Pack.Name, "maintenance")
	}
	if tc.Pack.Schema != 1 {
		t.Errorf("[pack] schema = %d, want 1", tc.Pack.Schema)
	}

	// Maintenance has 1 agent: dog.
	if len(tc.Agents) != 1 {
		t.Errorf("pack has %d agents, want 1", len(tc.Agents))
	}
	if len(tc.Agents) > 0 && tc.Agents[0].Name != "dog" {
		t.Errorf("agent name = %q, want %q", tc.Agents[0].Name, "dog")
	}

	// Verify dog agent has scope = "city".
	if len(tc.Agents) > 0 && tc.Agents[0].Scope != "city" {
		t.Errorf("dog scope = %q, want %q", tc.Agents[0].Scope, "city")
	}

	// Verify prompt file exists.
	for _, a := range tc.Agents {
		if a.PromptTemplate == "" {
			continue
		}
		topoDir := filepath.Join(dir, "packs", "maintenance")
		path := filepath.Join(topoDir, a.PromptTemplate)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("agent %q: prompt_template %q resolves to %q: %v",
				a.Name, a.PromptTemplate, path, err)
		}
	}
}

func TestMaintenanceFormulasExist(t *testing.T) {
	dir := exampleDir()
	formulaDir := filepath.Join(dir, "packs", "maintenance", "formulas")

	entries, err := os.ReadDir(formulaDir)
	if err != nil {
		t.Fatalf("reading formulas dir: %v", err)
	}

	var count int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".formula.toml") {
			continue
		}
		count++
	}

	// 3 formulas: mol-shutdown-dance + mol-dog-jsonl + mol-dog-reaper
	if count != 3 {
		t.Errorf("found %d formula files, want 3", count)
	}
}

func TestDoltHealthFormulasExist(t *testing.T) {
	dir := exampleDir()
	formulaDir := filepath.Join(dir, "..", "dolt", "formulas")

	entries, err := os.ReadDir(formulaDir)
	if err != nil {
		t.Fatalf("reading dolt formulas dir: %v", err)
	}

	var count int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".formula.toml") {
			continue
		}
		count++
	}

	if count == 0 {
		t.Error("no formula files found")
	}
}
