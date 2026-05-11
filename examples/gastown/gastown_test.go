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
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
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

func assertContainsInOrder(t *testing.T, body string, wants ...string) {
	t.Helper()
	offset := 0
	for _, want := range wants {
		idx := strings.Index(body[offset:], want)
		if idx == -1 {
			t.Fatalf("missing %q after byte offset %d", want, offset)
		}
		offset += idx + len(want)
	}
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
	if len(cfg.Workspace.Includes) != 0 {
		t.Errorf("Workspace.Includes = %v, want empty (migrated to pack.toml)", cfg.Workspace.Includes)
	}
	// Imports live in pack.toml (portable definition), not city.toml (deployment).
	if len(cfg.Imports) != 0 {
		t.Errorf("cfg.Imports = %v, want empty (imports migrated to pack.toml)", cfg.Imports)
	}
}

func TestCityPackTomlParses(t *testing.T) {
	dir := exampleDir()
	data, err := os.ReadFile(filepath.Join(dir, "pack.toml"))
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
	if tc.Pack.Schema != 2 {
		t.Errorf("[pack] schema = %d, want 2", tc.Pack.Schema)
	}
	gastownImp, ok := tc.Imports["gastown"]
	if !ok {
		t.Fatalf("pack.toml imports = %v, want entry for \"gastown\"", tc.Imports)
	}
	if gastownImp.Source != "packs/gastown" {
		t.Errorf("pack.toml imports[\"gastown\"].Source = %q, want %q", gastownImp.Source, "packs/gastown")
	}
	gastownDefault, ok := tc.Defaults.Rig.Imports["gastown"]
	if !ok {
		t.Fatalf("pack.toml defaults.rig.imports = %v, want entry for \"gastown\"", tc.Defaults.Rig.Imports)
	}
	if gastownDefault.Source != "packs/gastown" {
		t.Errorf("pack.toml defaults.rig.imports[\"gastown\"].Source = %q, want %q", gastownDefault.Source, "packs/gastown")
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
		path := resolveExamplePath(dir, a.PromptTemplate)
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
		path := resolveExamplePath(dir, a.OverlayDir)
		if info, err := os.Stat(path); err != nil {
			t.Errorf("agent %q: overlay_dir %q: %v", a.Name, a.OverlayDir, err)
		} else if !info.IsDir() {
			t.Errorf("agent %q: overlay_dir %q is not a directory", a.Name, a.OverlayDir)
		}
	}
}

func TestRefineryPromptSeedsTargetBranchVar(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "agents", "refinery", "prompt.template.md")
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
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-refinery-patrol.toml")
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

// TestRefineryFormulaChainsMergeMetadataWithClose guards against the
// regression observed during concurrent fan-out (3 polecats, 3 work
// beads, one refinery): when the formula presented `gc bd update
// --set-metadata` and `gc bd close` as two separate commands in the
// same code block, the refinery agent skipped the metadata write and
// jumped straight to the close, leaving `merged_sha` and
// `merged_target` NULL on every closed bead. Forensic context tracing
// a closed bead to its merge commit is then lost.
//
// The fix chains both commands with `&&` so a refinery agent cannot
// honor `gc bd close` without also honoring the preceding metadata
// write. Both the direct-merge path and the mr/pr handoff path use
// the same chained shape.
func TestRefineryFormulaChainsMergeMetadataWithClose(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-refinery-patrol.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading refinery formula: %v", err)
	}
	body := string(data)

	// Direct-merge path: metadata write must be chained into the close.
	assertContainsInOrder(t, body,
		"--set-metadata merge_result=merged",
		"--set-metadata merged_sha=$MERGED_SHA",
		"--set-metadata merged_target=$TARGET &&",
		`gc bd close $WORK --reason "Merged to $TARGET at $MERGED_SHORT"`,
	)

	// mr/pr handoff path: same chained shape, different metadata fields.
	assertContainsInOrder(t, body,
		"--set-metadata merge_result=pull_request",
		`--set-metadata pr_url="$PR_URL"`,
		`--set-metadata pr_number="$PR_NUMBER"`,
		`--set-metadata merged_target="$TARGET" &&`,
		`gc bd close $WORK --reason "Pull request ready: $PR_URL"`,
	)
}

func TestPolecatFormulaTreatsMetadataBranchAsAuthoritative(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-polecat-work.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading polecat formula: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		`git fetch origin "+refs/heads/$BRANCH:refs/remotes/origin/$BRANCH"`,
		`Could not fetch metadata.branch=$BRANCH from origin`,
		`git merge --ff-only "origin/$BRANCH"`,
		`metadata.branch=$BRANCH was set but no local or origin branch exists`,
		`STOP. Do not create a different branch.`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("polecat formula missing metadata.branch authority guidance %q", want)
		}
	}
	assertContainsInOrder(t, body,
		`if git show-ref --verify --quiet "refs/remotes/origin/$BRANCH"; then`,
		`if git show-ref --verify --quiet "refs/heads/$BRANCH"; then`,
	)
}

func TestPolecatFormulaRecordsExistingPRMetadataOnSubmit(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-polecat-work.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading polecat formula: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		`metadata.existing_pr` + "`" + ` is preserved for refinery`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("polecat formula missing existing_pr submit handling %q", want)
		}
	}
	if strings.Contains(body, `--set-metadata pr_url="$EXISTING_PR"`) {
		t.Fatalf("polecat must not record caller-supplied existing_pr as canonical pr_url")
	}
	if strings.Contains(body, "gh pr create") {
		t.Fatalf("polecat submit flow must not create pull requests directly")
	}
}

func TestRefineryFormulaRespectsExistingPRMetadata(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-refinery-patrol.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading refinery formula: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		`EXISTING_PR=$(gc bd show $WORK --json | jq -r '.[0].metadata.existing_pr // empty')`,
		`ORIGIN_REPO=$(gh repo view --json nameWithOwner -q '.nameWithOwner')`,
		`metadata.existing_pr requires pull-request handoff; using merge_strategy=mr`,
		`block_existing_pr()`,
		`--assignee=""`,
		`--set-metadata gc.routed_to=human`,
		`--set-metadata blocked_reason="$reason"`,
		`gc mail send mayor/ -s "ESCALATION: invalid existing_pr for $WORK"`,
		`NEXT=$(gc bd mol wisp mol-refinery-patrol --root-only --var target_branch={{target_branch}} --var rig_name={{rig_name}} --var binding_prefix={{binding_prefix}} --json | jq -r '.new_epic_id')`,
		`gc bd update "$NEXT" --assignee=$GC_AGENT`,
		`CURRENT_WISP=${GC_BEAD_ID:-}`,
		`gc bd mol burn "$CURRENT_WISP" --force`,
		`pr_lookup_missing()`,
		`EXISTING_PR_ERR=$(mktemp)`,
		`EXISTING_PR_INFO=$(gh pr view --json url,number,state,headRefName,baseRefName,headRepositoryOwner,headRepository -- "$EXISTING_PR" 2>"$EXISTING_PR_ERR")`,
		`EXISTING_PR_STATUS=$?`,
		`if pr_lookup_missing "$EXISTING_PR_ERROR"; then`,
		`Existing PR $EXISTING_PR was not found or is not accessible.`,
		`Could not resolve existing PR $EXISTING_PR. STOP. Debug and retry without mutating bead state.`,
		`EXISTING_PR_STATE=$(printf '%s\n' "$EXISTING_PR_INFO" | jq -r '.state')`,
		`EXISTING_PR_HEAD=$(printf '%s\n' "$EXISTING_PR_INFO" | jq -r '.headRefName')`,
		`EXISTING_PR_BASE=$(printf '%s\n' "$EXISTING_PR_INFO" | jq -r '.baseRefName')`,
		`EXISTING_PR_REPO=$(printf '%s\n' "$EXISTING_PR_URL" | sed -E 's#^https://github.com/([^/]+/[^/]+)/pull/[0-9]+$#\\1#')`,
		`EXISTING_PR_HEAD_REPO=$(printf '%s\n' "$EXISTING_PR_INFO" | jq -r '.headRepositoryOwner.login + "/" + .headRepository.name')`,
		`metadata.existing_pr is set but metadata.branch is missing`,
		`Existing PR $EXISTING_PR is $EXISTING_PR_STATE, want OPEN`,
		`Existing PR $EXISTING_PR targets branch $EXISTING_PR_HEAD, want $BRANCH`,
		`Existing PR $EXISTING_PR targets base $EXISTING_PR_BASE, want $TARGET`,
		`Existing PR $EXISTING_PR belongs to repo $EXISTING_PR_REPO, want $ORIGIN_REPO`,
		`Existing PR $EXISTING_PR head repo $EXISTING_PR_HEAD_REPO, want $ORIGIN_REPO`,
		`PR_REF="$EXISTING_PR"`,
		`PR_STATUS=$?`,
		`if [ -n "$EXISTING_PR" ] && pr_lookup_missing "$PR_ERROR"; then`,
		`PR_REPO=$(printf '%s\n' "$PR_URL" | sed -E 's#^https://github.com/([^/]+/[^/]+)/pull/[0-9]+$#\\1#')`,
		`Existing PR $EXISTING_PR belongs to repo $PR_REPO, want $ORIGIN_REPO`,
		`if [ -n "$EXISTING_PR" ]; then`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("refinery formula missing existing_pr handling %q", want)
		}
	}
	assertContainsInOrder(t, body,
		`EXISTING_PR=$(gc bd show $WORK --json | jq -r '.[0].metadata.existing_pr // empty')`,
		`EXISTING_PR_INFO=$(gh pr view --json url,number,state,headRefName,baseRefName,headRepositoryOwner,headRepository -- "$EXISTING_PR" 2>"$EXISTING_PR_ERR")`,
		`git push origin HEAD:$BRANCH --force-with-lease`,
		`gh pr create`,
	)
}

func TestWorktreeSetupKeepsIgnoresLocal(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	city := filepath.Join(tmp, "city")
	script := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "worktree-setup.sh")

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
	script := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "worktree-setup.sh")

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
	script := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "worktree-setup.sh")

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
	script := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "worktree-setup.sh")

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
	script := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "worktree-setup.sh")

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
	script := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "worktree-setup.sh")

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
	script := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "worktree-setup.sh")

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
	script := filepath.Join(exampleDir(), "packs", "gastown", "assets", "scripts", "worktree-setup.sh")

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

	mayorPrompt, err := os.ReadFile(filepath.Join(dir, "packs", "gastown", "agents", "mayor", "prompt.template.md"))
	if err != nil {
		t.Fatalf("reading mayor prompt: %v", err)
	}
	if strings.Contains(string(mayorPrompt), "{{ .CityRoot }}/<rig>") {
		t.Fatalf("mayor prompt still hardcodes {{ .CityRoot }}/<rig>:\n%s", mayorPrompt)
	}
	if !strings.Contains(string(mayorPrompt), "{{ cmd }} rig status <rig>") {
		t.Fatalf("mayor prompt missing rig-status guidance:\n%s", mayorPrompt)
	}

	crewPrompt, err := os.ReadFile(filepath.Join(dir, "packs", "gastown", "assets", "prompts", "crew.template.md"))
	if err != nil {
		t.Fatalf("reading crew prompt: %v", err)
	}
	if !strings.Contains(string(crewPrompt), "{{ .CityRoot }}/.gc/worktrees/$TARGET_RIG/crew/") {
		t.Fatalf("crew prompt missing namespaced worktree path:\n%s", crewPrompt)
	}

	polecatPrompt, err := os.ReadFile(filepath.Join(dir, "packs", "gastown", "agents", "polecat", "prompt.template.md"))
	if err != nil {
		t.Fatalf("reading polecat prompt: %v", err)
	}
	if strings.Contains(string(polecatPrompt), "that's not a git working tree") {
		t.Fatalf("polecat prompt still claims rig root is not a git working tree:\n%s", polecatPrompt)
	}
}

func TestGastownRoutedToTargetsUseBindingPrefix(t *testing.T) {
	dir := exampleDir()
	checks := []struct {
		rel  string
		want string
	}{
		{"packs/gastown/formulas/mol-deacon-patrol.toml", `"gc.routed_to":"{{binding_prefix}}dog"`},
		{"packs/gastown/formulas/mol-witness-patrol.toml", `"gc.routed_to":"{{binding_prefix}}dog"`},
		{"packs/gastown/agents/boot/prompt.template.md", `"gc.routed_to":"{{ .BindingPrefix }}dog"`},
		{"packs/gastown/agents/deacon/prompt.template.md", `"gc.routed_to":"{{ .BindingPrefix }}dog"`},
		{"packs/gastown/agents/witness/prompt.template.md", `"gc.routed_to":"{{ .BindingPrefix }}dog"`},
		{"packs/gastown/formulas/mol-polecat-work.toml", `${GC_RIG:+$GC_RIG/}{{binding_prefix}}refinery`},
		{"packs/gastown/formulas/mol-refinery-patrol.toml", `${GC_RIG:+$GC_RIG/}{{binding_prefix}}polecat`},
		{"packs/gastown/formulas/mol-idea-to-plan.toml", "$GC_RIG/{{binding_prefix}}polecat"},
		{"packs/gastown/agents/mayor/prompt.template.md", `${TARGET_RIG:+$TARGET_RIG/}{{ .BindingPrefix }}polecat`},
		{"packs/gastown/agents/polecat/prompt.template.md", `${GC_RIG:+$GC_RIG/}{{ .BindingPrefix }}polecat`},
		{"packs/gastown/agents/polecat/prompt.template.md", `${GC_RIG:+$GC_RIG/}{{ .BindingPrefix }}refinery`},
		{"packs/gastown/template-fragments/approval-fallacy.template.md", `${GC_RIG:+$GC_RIG/}{{ .BindingPrefix }}refinery`},
	}
	for _, check := range checks {
		data, err := os.ReadFile(filepath.Join(dir, check.rel))
		if err != nil {
			t.Fatalf("reading %s: %v", check.rel, err)
		}
		body := string(data)
		if !strings.Contains(body, check.want) {
			t.Errorf("%s missing %q", check.rel, check.want)
		}
		for _, bad := range []string{
			"gc.routed_to=dog",
			"gc.routed_to=<rig>/polecat",
			"gc.routed_to=<rig>/refinery",
			"gc.routed_to={{ .RigName }}/refinery",
			"gc.routed_to={{rig_name}}/{{binding_prefix}}refinery",
			"gc.routed_to={{rig_name}}/{{binding_prefix}}polecat",
			"gc.routed_to={{ .RigName }}/{{ .BindingPrefix }}refinery",
			"{{ .RigName }}/{{ .BindingPrefix }}polecat",
		} {
			if strings.Contains(body, bad) {
				t.Errorf("%s still contains short-form route %q", check.rel, bad)
			}
		}
	}
}

func TestGastownWarrantCreateCommandsUseCreateMetadata(t *testing.T) {
	dir := exampleDir()
	files := []string{
		"packs/gastown/agents/boot/prompt.template.md",
		"packs/gastown/agents/deacon/prompt.template.md",
		"packs/gastown/agents/witness/prompt.template.md",
		"packs/gastown/formulas/mol-deacon-patrol.toml",
		"packs/gastown/formulas/mol-witness-patrol.toml",
		"packs/maintenance/formulas/mol-shutdown-dance.toml",
	}
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		inCreate := false
		for lineNo, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "bd create") {
				inCreate = true
			}
			if !inCreate {
				continue
			}
			if strings.Contains(line, "--set-metadata") {
				t.Errorf("%s:%d bd create command uses update-only --set-metadata:\n%s", rel, lineNo+1, line)
			}
			if !strings.HasSuffix(strings.TrimSpace(line), "\\") {
				inCreate = false
			}
		}
	}
}

func TestGastownRigTargetShellExpressionsRenderForRigAndHQ(t *testing.T) {
	tests := []struct {
		name      string
		expr      string
		gcRig     string
		targetRig string
		want      string
	}{
		{
			name: "refinery hq no binding",
			expr: `${GC_RIG:+$GC_RIG/}refinery`,
			want: "refinery",
		},
		{
			name:  "refinery rig with binding",
			expr:  `${GC_RIG:+$GC_RIG/}review.refinery`,
			gcRig: "gascity",
			want:  "gascity/review.refinery",
		},
		{
			name: "polecat hq with binding",
			expr: `${GC_RIG:+$GC_RIG/}review.polecat`,
			want: "review.polecat",
		},
		{
			name:  "polecat rig with binding",
			expr:  `${GC_RIG:+$GC_RIG/}review.polecat`,
			gcRig: "gascity",
			want:  "gascity/review.polecat",
		},
		{
			name: "mayor polecat hq with binding",
			expr: `${TARGET_RIG:+$TARGET_RIG/}review.polecat`,
			want: "review.polecat",
		},
		{
			name:      "mayor polecat rig with binding",
			expr:      `${TARGET_RIG:+$TARGET_RIG/}review.polecat`,
			targetRig: "gascity",
			want:      "gascity/review.polecat",
		},
		{
			name:      "gc rig expression ignores target rig",
			expr:      `${GC_RIG:+$GC_RIG/}review.refinery`,
			gcRig:     "gascity",
			targetRig: "othercity",
			want:      "gascity/review.refinery",
		},
		{
			name:      "target rig expression ignores gc rig",
			expr:      `${TARGET_RIG:+$TARGET_RIG/}review.polecat`,
			gcRig:     "gascity",
			targetRig: "othercity",
			want:      "othercity/review.polecat",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("sh", "-c", `printf '%s' "`+tt.expr+`"`)
			cmd.Env = append(os.Environ(), "GC_RIG="+tt.gcRig, "TARGET_RIG="+tt.targetRig)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("render target: %v", err)
			}
			if got := string(out); got != tt.want {
				t.Fatalf("target = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGastownRefineryPatrolRejectionCommandsReturnWorkToPolecatPool(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(exampleDir(), "packs/gastown/formulas/mol-refinery-patrol.toml"))
	if err != nil {
		t.Fatalf("reading mol-refinery-patrol.toml: %v", err)
	}
	body := string(data)

	checks := []struct {
		name      string
		startText string
		endText   string
	}{
		{
			name:      "rebase conflict rejection",
			startText: "If rebase FAILED (conflicts):",
			endText:   "A new polecat will pick up the bead",
		},
		{
			name:      "test failure rejection",
			startText: "If branch caused it:",
			endText:   "If pre-existing on target:",
		},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			start := strings.Index(body, check.startText)
			if start < 0 {
				t.Fatalf("missing section start %q", check.startText)
			}
			end := strings.Index(body[start:], check.endText)
			if end < 0 {
				t.Fatalf("missing section end %q after %q", check.endText, check.startText)
			}
			section := body[start : start+end]
			for _, want := range []string{
				"gc workflow delete-source $WORK --apply && gc workflow reopen-source $WORK",
				"gc bd update $WORK",
				"--status=open",
				`--assignee=""`,
				"--set-metadata rejection_reason=",
				`--set-metadata gc.routed_to="${GC_RIG:+$GC_RIG/}{{binding_prefix}}polecat"`,
			} {
				if !strings.Contains(section, want) {
					t.Errorf("%s missing %q:\n%s", check.name, want, section)
				}
			}
		})
	}
}

func TestGastownPatrolWispCommandsPropagateRoutingNamespace(t *testing.T) {
	dir := exampleDir()
	checks := []struct {
		rel     string
		formula string
		vars    []string
	}{
		{
			rel:     "packs/gastown/agents/deacon/prompt.template.md",
			formula: "mol-deacon-patrol",
			vars:    []string{"--var binding_prefix="},
		},
		{
			rel:     "packs/gastown/formulas/mol-deacon-patrol.toml",
			formula: "mol-deacon-patrol",
			vars:    []string{"--var binding_prefix="},
		},
		{
			rel:     "packs/gastown/agents/refinery/prompt.template.md",
			formula: "mol-refinery-patrol",
			vars:    []string{"--var target_branch=", "--var rig_name=", "--var binding_prefix="},
		},
		{
			rel:     "packs/gastown/formulas/mol-refinery-patrol.toml",
			formula: "mol-refinery-patrol",
			vars:    []string{"--var target_branch=", "--var rig_name=", "--var binding_prefix="},
		},
		{
			rel:     "packs/gastown/agents/witness/prompt.template.md",
			formula: "mol-witness-patrol",
			vars:    []string{"--var binding_prefix="},
		},
		{
			rel:     "packs/gastown/formulas/mol-witness-patrol.toml",
			formula: "mol-witness-patrol",
			vars:    []string{"--var binding_prefix="},
		},
	}
	for _, check := range checks {
		data, err := os.ReadFile(filepath.Join(dir, check.rel))
		if err != nil {
			t.Fatalf("reading %s: %v", check.rel, err)
		}
		for lineNo, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "gc bd mol wisp "+check.formula+" --root-only") {
				continue
			}
			for _, want := range check.vars {
				if !strings.Contains(line, want) {
					t.Errorf("%s:%d wisp command missing %q:\n%s", check.rel, lineNo+1, want, line)
				}
			}
		}
	}

	renderVars := map[string]string{
		"binding_prefix": "gastown.",
		"rig_name":       "gascity",
		"target_branch":  "main",
	}
	for _, rel := range []string{
		"packs/gastown/formulas/mol-deacon-patrol.toml",
		"packs/gastown/formulas/mol-refinery-patrol.toml",
		"packs/gastown/formulas/mol-witness-patrol.toml",
	} {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		rendered := formula.Substitute(string(data), renderVars)
		for _, bad := range []string{"{{binding_prefix}}", "{{rig_name}}"} {
			if strings.Contains(rendered, bad) {
				t.Errorf("%s rendered patrol formula still contains %q", rel, bad)
			}
		}
	}
}

func TestBootPromptMatchesNamedSessionLifecycle(t *testing.T) {
	cfg := loadExpanded(t)
	bootSession := config.FindNamedSession(cfg, "boot")
	if bootSession == nil {
		t.Fatal("boot named_session missing; prompt documents its lifecycle")
	}
	if got := bootSession.ModeOrDefault(); got != "always" {
		t.Fatalf("boot named_session mode = %q, want %q because prompt documents that lifecycle", got, "always")
	}
	bootAgent := config.FindAgent(cfg, bootSession.TemplateQualifiedName())
	if bootAgent == nil {
		t.Fatalf("boot agent template %q missing; named_session and prompt must refer to a real agent", bootSession.TemplateQualifiedName())
	}
	if got := bootAgent.EffectiveWakeMode(); got != "fresh" {
		t.Fatalf("boot agent wake_mode = %q, want %q because prompt documents fresh provider context", got, "fresh")
	}

	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "agents", "boot", "prompt.template.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading boot prompt: %v", err)
	}
	body := string(data)

	for _, stale := range []string{
		"{{ cmd }} agent peek",
		"Controller tick",
		"Spawn Boot (fresh session each time)",
		"on each tick",
		"Next Boot tick",
		"Narrow scope makes restarts cheap",
		"always fresh",
		"no persistent state",
	} {
		if strings.Contains(body, stale) {
			t.Fatalf("boot prompt still contains stale lifecycle or command guidance %q:\n%s", stale, body)
		}
	}

	for _, want := range []string{
		"{{ cmd }} session peek deacon --lines 1",
		"{{ cmd }} session peek deacon --lines 30",
		"configured `boot` named session",
		"`mode = \"always\"` keeps the `boot` identity present",
		"`wake_mode = \"fresh\"`",
		"gives each wake a new provider context",
		"Narrow scope keeps each wake cheap.",
		"Next Boot wake will re-evaluate.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("boot prompt missing current lifecycle or command guidance %q:\n%s", want, body)
		}
	}
}

func TestIdeaToPlanFormulaUsesSupportedPrimitives(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-idea-to-plan.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading idea-to-plan formula: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		`formula = "mol-idea-to-plan"`,
		`gc sling "$REVIEW_TARGET" "$LEG_BEAD" --on {{review_formula}}`,
		`gc bd create`,
		`gc mail send`,
		`gc bd dep add`,
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
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-review-leg.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading review-leg formula: %v", err)
	}
	body := string(data)
	for _, want := range []string{
		`formula = "mol-review-leg"`,
		`coordinator`,
		`gc bd update {{issue}} --notes`,
		`gc mail send "$COORD"`,
		`gc bd update {{issue}} --status=closed`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("review-leg formula missing %q", want)
		}
	}
}

type witnessSessionFixture struct {
	ID          string
	State       string
	Closed      bool
	SessionName string
	Alias       string
	AgentName   string
}

type witnessSessionBeadFixture struct {
	Status                  string
	State                   string
	ConfiguredNamedIdentity string
}

func resolveWitnessAssigneeForTest(
	assignee string,
	sessions []witnessSessionFixture,
	sessionBeads []witnessSessionBeadFixture,
) (string, bool) {
	index := make(map[string]string)
	add := func(key, state string, closed bool) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if closed {
			state = "closed"
		}
		index[key] = state
	}
	for _, s := range sessions {
		add(s.ID, s.State, s.Closed)
		add(s.SessionName, s.State, s.Closed)
		add(s.Alias, s.State, s.Closed)
		add(s.AgentName, s.State, s.Closed)
	}
	for _, b := range sessionBeads {
		add(b.ConfiguredNamedIdentity, b.State, b.Status == "closed")
	}
	state, ok := index[assignee]
	return state, ok
}

func witnessStateIsOrphanedForTest(state string) (bool, bool) {
	switch state {
	case string(session.StateActive),
		string(session.StateAwake),
		string(session.StateCreating),
		string(session.StateAsleep),
		string(session.StateDrained),
		string(session.StateSuspended),
		string(session.StateDraining),
		string(session.StateQuarantined):
		return false, true
	case string(session.StateArchived), "closed", "absent":
		return true, true
	default:
		return false, false
	}
}

func TestWitnessPatrolLivenessProcedureUsesExactSessionIdentity(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-witness-patrol.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading witness patrol formula: %v", err)
	}
	body := string(data)

	for _, forbidden := range []string{
		`grep -oE '(hq|sc|gc|de)-[a-z0-9]+'`,
		`(hq|sc|gc|de)-<id>`,
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("witness patrol still contains fixed-prefix extraction %q", forbidden)
		}
	}
	for _, want := range []string{
		`$s.ID`,
		`$s.SessionName`,
		`$s.Alias`,
		`$s.AgentName`,
		`configured_named_identity`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("witness patrol liveness procedure missing exact lookup key %q", want)
		}
	}

	sessions := []witnessSessionFixture{
		{
			ID:          "ga-n7iy6",
			State:       string(session.StateActive),
			SessionName: "polecats__sonnet-ga-n7iy6",
			Alias:       "gastown/polecat-slot-1",
			AgentName:   "gastown/sonnet",
		},
		{ID: "mp-7k4g", State: string(session.StateCreating)},
	}
	sessionBeads := []witnessSessionBeadFixture{
		{
			Status:                  "open",
			State:                   string(session.StateAsleep),
			ConfiguredNamedIdentity: "gastown/witness",
		},
	}
	for _, tc := range []struct {
		assignee string
		want     string
	}{
		{assignee: "ga-n7iy6", want: string(session.StateActive)},
		{assignee: "polecats__sonnet-ga-n7iy6", want: string(session.StateActive)},
		{assignee: "gastown/polecat-slot-1", want: string(session.StateActive)},
		{assignee: "gastown/sonnet", want: string(session.StateActive)},
		{assignee: "mp-7k4g", want: string(session.StateCreating)},
		{assignee: "gastown/witness", want: string(session.StateAsleep)},
	} {
		got, ok := resolveWitnessAssigneeForTest(tc.assignee, sessions, sessionBeads)
		if !ok || got != tc.want {
			t.Errorf("resolveWitnessAssigneeForTest(%q) = %q, %v; want %q, true", tc.assignee, got, ok, tc.want)
		}
	}
	if got, ok := resolveWitnessAssigneeForTest("polecat-hq-00ohd", sessions, sessionBeads); ok {
		t.Fatalf("embedded fixed-prefix assignee resolved to %q; want exact lookup miss", got)
	}
}

func TestWitnessPatrolStateClassificationCoversSessionStates(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-witness-patrol.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading witness patrol formula: %v", err)
	}
	body := string(data)

	notOrphaned := []session.State{
		session.StateActive,
		session.StateAwake,
		session.StateCreating,
		session.StateAsleep,
		session.StateDrained,
		session.StateSuspended,
		session.StateDraining,
		session.StateQuarantined,
	}
	for _, state := range notOrphaned {
		if !strings.Contains(body, "`"+string(state)+"`") {
			t.Errorf("witness patrol formula missing state %q", state)
		}
		got, ok := witnessStateIsOrphanedForTest(string(state))
		if !ok || got {
			t.Errorf("witnessStateIsOrphanedForTest(%q) = %v, %v; want false, true", state, got, ok)
		}
	}
	for _, state := range []string{string(session.StateArchived), "closed", "absent"} {
		if !strings.Contains(body, "`"+state+"`") {
			t.Errorf("witness patrol formula missing state %q", state)
		}
		got, ok := witnessStateIsOrphanedForTest(state)
		if !ok || !got {
			t.Errorf("witnessStateIsOrphanedForTest(%q) = %v, %v; want true, true", state, got, ok)
		}
	}
	if got, ok := witnessStateIsOrphanedForTest("future-state"); ok || got {
		t.Fatalf("witnessStateIsOrphanedForTest(future-state) = %v, %v; want false, false", got, ok)
	}
}

// TestWitnessPatrolAllStepsContinueNotExit guards against the regression
// in upstream #1884: every intermediate step in mol-witness-patrol must
// tell the agent to continue rather than exit the wisp. The burn
// primitive only lives in `next-iteration`, so any step that reads as
// terminal ("Exit criteria: no orphans found.") leaks wisps when an LLM
// treats the early-exit as a terminal instruction.
func TestWitnessPatrolAllStepsContinueNotExit(t *testing.T) {
	dir := exampleDir()
	path := filepath.Join(dir, "packs", "gastown", "formulas", "mol-witness-patrol.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading witness patrol formula: %v", err)
	}

	var parsed struct {
		Steps []struct {
			ID          string `toml:"id"`
			Description string `toml:"description"`
		} `toml:"steps"`
	}
	if _, err := toml.Decode(string(data), &parsed); err != nil {
		t.Fatalf("parsing witness patrol formula: %v", err)
	}

	byID := make(map[string]string, len(parsed.Steps))
	for _, s := range parsed.Steps {
		byID[s.ID] = s.Description
	}

	intermediate := []string{
		"check-inbox",
		"recover-orphaned-beads",
		"check-refinery",
		"check-polecat-health",
	}
	for _, id := range intermediate {
		desc, ok := byID[id]
		if !ok {
			t.Errorf("witness patrol formula missing step %q", id)
			continue
		}
		if !strings.Contains(desc, "do NOT exit") {
			t.Errorf("step %q missing continuation reminder 'do NOT exit' so an LLM can treat the step as terminal and leak the wisp:\n%s", id, desc)
		}
		if !strings.Contains(desc, "next-iteration") {
			t.Errorf("step %q missing reference to `next-iteration` as the sole burn site:\n%s", id, desc)
		}
	}

	if _, ok := byID["next-iteration"]; !ok {
		t.Fatal("witness patrol formula missing `next-iteration` step — continuation clauses point at a non-existent target")
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
		if e.IsDir() || !formula.IsTOMLFilename(e.Name()) {
			continue
		}
		count++
	}

	if count == 0 {
		t.Error("no formula files found")
	}
}

// TestAllPackTomlsParse decodes every *.toml file under
// examples/gastown/packs/ to catch malformed formulas, agent manifests,
// orders, and pack manifests at CI time. Without this, a description
// string with an invalid TOML escape (e.g. "\<space>" inside a """..."""
// block) silently fails formula registration at runtime — the agent
// running that formula falls back to memory and skips load-bearing
// steps. The remaining string-match formula tests in this file run
// AFTER parse, so adding this gate up front keeps their failure
// messages meaningful (they assume valid TOML).
//
// The walk includes pack.toml, formulas/*.toml, orders/*.toml, and
// agents/*/agent.toml uniformly — they all share the same TOML
// grammar, and we want any new TOML file added under packs/ to be
// covered automatically without remembering to update this test.
func TestAllPackTomlsParse(t *testing.T) {
	dir := exampleDir()
	packsRoot := filepath.Join(dir, "packs")

	var count int
	err := filepath.Walk(packsRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".toml") {
			return nil
		}
		count++
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Errorf("reading %s: %v", path, readErr)
			return nil
		}
		var into map[string]any
		if _, err := toml.Decode(string(data), &into); err != nil {
			t.Errorf("parsing %s: %v", path, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", packsRoot, err)
	}
	if count == 0 {
		t.Fatalf("no .toml files found under %s — directory layout changed?", packsRoot)
	}
}

func TestAllPromptTemplatesExist(t *testing.T) {
	var count int
	for _, a := range discoverPackAgents(t, filepath.Join("packs", "gastown")) {
		if a.PromptTemplate == "" {
			continue
		}
		count++
		data, err := os.ReadFile(a.PromptTemplate)
		if err != nil {
			t.Fatalf("reading %s prompt: %v", a.Name, err)
		}
		if len(data) == 0 {
			t.Errorf("%s prompt is empty", a.Name)
		}
	}

	if count != 6 {
		t.Errorf("found %d prompt templates, want 6", count)
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
	Pack     config.PackMeta          `toml:"pack"`
	Imports  map[string]config.Import `toml:"imports"`
	Defaults struct {
		Rig struct {
			Imports map[string]config.Import `toml:"imports"`
		} `toml:"rig"`
	} `toml:"defaults"`
}

func discoverPackAgents(t *testing.T, rel string) []config.Agent {
	t.Helper()
	packDir := filepath.Join(exampleDir(), rel)
	agents, err := config.DiscoverPackAgents(fsys.OSFS{}, packDir, filepath.Base(rel), nil)
	if err != nil {
		t.Fatalf("DiscoverPackAgents(%s): %v", rel, err)
	}
	return agents
}

func resolveExamplePath(base, candidate string) string {
	if filepath.IsAbs(candidate) {
		return candidate
	}
	return filepath.Join(base, candidate)
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
	if tc.Pack.Schema != 2 {
		t.Errorf("[pack] schema = %d, want 2", tc.Pack.Schema)
	}
	if len(tc.Pack.Includes) != 0 {
		t.Fatalf("pack includes = %v, want empty (migrated to [imports.maintenance])", tc.Pack.Includes)
	}
	maintImp, ok := tc.Imports["maintenance"]
	if !ok {
		t.Fatalf("pack imports = %v, want entry for \"maintenance\"", tc.Imports)
	}
	if maintImp.Source != "../maintenance" {
		t.Errorf("pack imports[\"maintenance\"].Source = %q, want %q", maintImp.Source, "../maintenance")
	}

	// Expect 6 locally-discovered agents. Dog comes from the maintenance import
	// and is themed via a pack patch, not a local agent file.
	agents := discoverPackAgents(t, filepath.Join("packs", "gastown"))
	want := map[string]bool{
		"mayor": false, "deacon": false, "boot": false,
		"witness": false, "refinery": false, "polecat": false,
	}
	for _, a := range agents {
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
	if len(agents) != 6 {
		t.Errorf("pack has %d locally-discovered agents, want 6", len(agents))
	}

	// Verify city-scoped agents have scope = "city".
	wantCity := map[string]bool{"mayor": true, "deacon": true, "boot": true}
	for _, a := range agents {
		if wantCity[a.Name] && a.Scope != "city" {
			t.Errorf("agent %q: scope = %q, want %q", a.Name, a.Scope, "city")
		}
	}
}

func TestPackUsesIsolatedWorkDirs(t *testing.T) {
	agents := discoverPackAgents(t, filepath.Join("packs", "gastown"))
	want := map[string]string{
		"mayor":    ".gc/agents/mayor",
		"deacon":   ".gc/agents/deacon",
		"boot":     ".gc/agents/boot",
		"witness":  ".gc/agents/{{.Rig}}/witness",
		"refinery": ".gc/worktrees/{{.Rig}}/refinery",
		"polecat":  ".gc/worktrees/{{.Rig}}/polecats/{{.AgentBase}}",
	}
	for _, a := range agents {
		if expected, ok := want[a.Name]; ok && a.WorkDir != expected {
			t.Errorf("agent %q: work_dir = %q, want %q", a.Name, a.WorkDir, expected)
		}
	}
}

func TestPackPromptFilesExist(t *testing.T) {
	for _, a := range discoverPackAgents(t, filepath.Join("packs", "gastown")) {
		if a.PromptTemplate == "" {
			continue
		}
		if _, err := os.Stat(a.PromptTemplate); err != nil {
			t.Errorf("agent %q: prompt_template %q: %v", a.Name, a.PromptTemplate, err)
		}
	}
}

func TestCityAgentsFilter(t *testing.T) {
	// Verify config.LoadWithIncludes with both packs produces
	// only city-scoped agents when no rigs are registered.
	// Effective dog from gastown override + mayor/deacon/boot = 4.
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

func TestExpandedCityUsesGastownDogOverride(t *testing.T) {
	cfg := loadExpanded(t)

	var dog *config.Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "dog" && !cfg.Agents[i].Implicit {
			dog = &cfg.Agents[i]
			break
		}
	}
	if dog == nil {
		t.Fatal("expected explicit dog agent in expanded gastown config")
	}
	if dog.WorkDir != ".gc/agents/dogs/{{.AgentBase}}" {
		t.Errorf("dog work_dir = %q, want gastown themed work dir", dog.WorkDir)
	}
	wantPromptSuffix := filepath.Join("packs", "maintenance", "agents", "dog", "prompt.template.md")
	if !strings.HasSuffix(dog.PromptTemplate, wantPromptSuffix) {
		t.Errorf("dog prompt_template = %q, want suffix %q", dog.PromptTemplate, wantPromptSuffix)
	}
	wantOverlaySuffix := filepath.Join("packs", "maintenance", "agents", "dog", "overlay")
	if !strings.HasSuffix(dog.OverlayDir, wantOverlaySuffix) {
		t.Errorf("dog overlay_dir = %q, want suffix %q", dog.OverlayDir, wantOverlaySuffix)
	}
	if len(dog.SessionLive) != 2 {
		t.Fatalf("dog session_live has %d entries, want 2 gastown theming commands", len(dog.SessionLive))
	}
	if !strings.Contains(dog.SessionLive[0], "tmux-theme.sh") {
		t.Errorf("dog session_live[0] = %q, want tmux-theme.sh", dog.SessionLive[0])
	}
	if !strings.Contains(dog.SessionLive[1], "tmux-keybindings.sh") {
		t.Errorf("dog session_live[1] = %q, want tmux-keybindings.sh", dog.SessionLive[1])
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
	if tc.Pack.Schema != 2 {
		t.Errorf("[pack] schema = %d, want 2", tc.Pack.Schema)
	}

	agents := discoverPackAgents(t, filepath.Join("packs", "maintenance"))
	// Maintenance has 1 agent: dog.
	if len(agents) != 1 {
		t.Errorf("pack has %d agents, want 1", len(agents))
	}
	if len(agents) > 0 && agents[0].Name != "dog" {
		t.Errorf("agent name = %q, want %q", agents[0].Name, "dog")
	}

	// Verify dog agent has scope = "city".
	if len(agents) > 0 && agents[0].Scope != "city" {
		t.Errorf("dog scope = %q, want %q", agents[0].Scope, "city")
	}

	// Verify prompt file exists.
	for _, a := range agents {
		if a.PromptTemplate == "" {
			continue
		}
		if _, err := os.Stat(a.PromptTemplate); err != nil {
			t.Errorf("agent %q: prompt_template %q: %v", a.Name, a.PromptTemplate, err)
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
		if e.IsDir() || !formula.IsTOMLFilename(e.Name()) {
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
		if e.IsDir() || !formula.IsTOMLFilename(e.Name()) {
			continue
		}
		count++
	}

	if count == 0 {
		t.Error("no formula files found")
	}
}
