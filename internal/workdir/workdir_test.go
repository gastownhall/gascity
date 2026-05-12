package workdir

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func intPtr(n int) *int { return &n }

func TestResolveWorkDirPathUsesWorkDirTemplate(t *testing.T) {
	cityPath := t.TempDir()
	cityName := "gastown"
	cfg := &config.City{
		Workspace: config.Workspace{Name: cityName},
		Rigs:      []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}},
	}
	agent := config.Agent{
		Name:    "refinery",
		Dir:     "demo",
		WorkDir: ".gc/worktrees/{{.Rig}}/{{.AgentBase}}",
	}

	got := ResolveWorkDirPath(cityPath, cityName, "demo/refinery", agent, cfg.Rigs)
	want := filepath.Join(cityPath, ".gc", "worktrees", "demo", "refinery")
	if got != want {
		t.Fatalf("ResolveWorkDirPath() = %q, want %q", got, want)
	}
}

func TestResolveWorkDirPathDefaultsRigScopedAgentsToRigRoot(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(t.TempDir(), "demo-repo")
	got := ResolveWorkDirPath(cityPath, "gastown", "demo/refinery", config.Agent{
		Name: "refinery",
		Dir:  "demo",
	}, []config.Rig{{Name: "demo", Path: rigRoot}})
	if got != rigRoot {
		t.Fatalf("ResolveWorkDirPath() = %q, want %q", got, rigRoot)
	}
}

func TestResolveWorkDirPathUsesPoolInstanceBase(t *testing.T) {
	cityPath := t.TempDir()
	got := ResolveWorkDirPath(cityPath, "gastown", "demo/polecat-2", config.Agent{
		Name:              "polecat",
		Dir:               "demo",
		WorkDir:           ".gc/worktrees/{{.Rig}}/polecats/{{.AgentBase}}",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
	}, []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}})
	want := filepath.Join(cityPath, ".gc", "worktrees", "demo", "polecats", "polecat-2")
	if got != want {
		t.Fatalf("ResolveWorkDirPath() = %q, want %q", got, want)
	}
}

// TestResolveWorkDirPathGivesEachPoolSlotUniqueWorktree is the #774 regression
// guard: N pool workers sharing one template must each resolve to a distinct
// worktree path derived from their namepool slot, not the template base.
func TestResolveWorkDirPathGivesEachPoolSlotUniqueWorktree(t *testing.T) {
	cityPath := t.TempDir()
	rigs := []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}
	agent := config.Agent{
		Name:              "ant",
		Dir:               "demo",
		WorkDir:           ".gc/worktrees/{{.Rig}}/ants/{{.AgentBase}}",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(4),
	}

	cases := []struct {
		slot string
		want string
	}{
		{slot: "demo/ant-fenrir", want: filepath.Join(cityPath, ".gc", "worktrees", "demo", "ants", "ant-fenrir")},
		{slot: "demo/ant-grendel", want: filepath.Join(cityPath, ".gc", "worktrees", "demo", "ants", "ant-grendel")},
		{slot: "demo/ant-hati", want: filepath.Join(cityPath, ".gc", "worktrees", "demo", "ants", "ant-hati")},
		{slot: "demo/ant-skoll", want: filepath.Join(cityPath, ".gc", "worktrees", "demo", "ants", "ant-skoll")},
	}

	seen := make(map[string]string, len(cases))
	for _, tc := range cases {
		t.Run(tc.slot, func(t *testing.T) {
			got := ResolveWorkDirPath(cityPath, "gastown", tc.slot, agent, rigs)
			if got != tc.want {
				t.Fatalf("ResolveWorkDirPath(%q) = %q, want %q", tc.slot, got, tc.want)
			}
			if prev, dup := seen[got]; dup {
				t.Fatalf("slot %q collided with %q on path %q", tc.slot, prev, got)
			}
			seen[got] = tc.slot
		})
	}

	if len(seen) != len(cases) {
		t.Fatalf("unique paths = %d, want %d", len(seen), len(cases))
	}
}

func TestSessionQualifiedNameCanonicalizesBareAndQualifiedPoolAliases(t *testing.T) {
	cityPath := t.TempDir()
	rigs := []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}
	agent := config.Agent{
		Name:              "polecat",
		Dir:               "demo",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
	}

	bare := SessionQualifiedName(cityPath, agent, rigs, "polecat-fenrir", "")
	qualified := SessionQualifiedName(cityPath, agent, rigs, "demo/polecat-fenrir", "")
	if bare != "demo/polecat-fenrir" {
		t.Fatalf("SessionQualifiedName(bare) = %q, want %q", bare, "demo/polecat-fenrir")
	}
	if qualified != bare {
		t.Fatalf("SessionQualifiedName(qualified) = %q, want %q", qualified, bare)
	}
}

func TestSessionQualifiedNameKeepsSingletonTemplateIdentity(t *testing.T) {
	cityPath := t.TempDir()
	rigs := []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}
	agent := config.Agent{Name: "witness", Dir: "demo", MaxActiveSessions: intPtr(1)}

	if got := SessionQualifiedName(cityPath, agent, rigs, "demo/boot", ""); got != "demo/witness" {
		t.Fatalf("SessionQualifiedName() = %q, want %q", got, "demo/witness")
	}
}

func TestSessionQualifiedNamePreservesRigQualifiedBindingIdentity(t *testing.T) {
	cityPath := t.TempDir()
	rigs := []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}
	agent := config.Agent{
		Name:              "worker",
		Dir:               "demo",
		BindingName:       "ops",
		MinActiveSessions: intPtr(0),
		MaxActiveSessions: intPtr(2),
	}

	if got := SessionQualifiedName(cityPath, agent, rigs, "ops.worker-1", ""); got != "demo/ops.worker-1" {
		t.Fatalf("SessionQualifiedName(bare binding) = %q, want %q", got, "demo/ops.worker-1")
	}
	if got := SessionQualifiedName(cityPath, agent, rigs, "demo/ops.worker-1", ""); got != "demo/ops.worker-1" {
		t.Fatalf("SessionQualifiedName(rig-qualified binding) = %q, want %q", got, "demo/ops.worker-1")
	}
}

func TestCityNameFallsBackToCityDirBase(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city-root")
	got := CityName(cityPath, &config.City{})
	if got != "city-root" {
		t.Fatalf("CityName() = %q, want %q", got, "city-root")
	}
}

func TestResolveWorkDirPathStrictRejectsInvalidTemplate(t *testing.T) {
	cityPath := t.TempDir()
	_, err := ResolveWorkDirPathStrict(cityPath, "gastown", "demo/refinery", config.Agent{
		Name:    "refinery",
		Dir:     "demo",
		WorkDir: ".gc/worktrees/{{.RigName}}/refinery",
	}, []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}})
	if err == nil {
		t.Fatal("ResolveWorkDirPathStrict() error = nil, want invalid template error")
	}
}

func TestExpandCommandTemplateFallsBackToCityDirBase(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "demo-city")
	agent := config.Agent{Name: "worker"}

	got, err := ExpandCommandTemplate("echo {{.CityName}}", cityPath, "", agent, nil)
	if err != nil {
		t.Fatalf("ExpandCommandTemplate() error = %v, want nil", err)
	}
	if got != "echo demo-city" {
		t.Fatalf("ExpandCommandTemplate() = %q, want %q", got, "echo demo-city")
	}
}

func TestConfiguredRigNameMatchesSymlinkAliasPath(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	rigPath := filepath.Join(realRoot, "demo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(root, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlink setup unavailable: %v", err)
	}

	aliasRigPath := filepath.Join(aliasRoot, "demo")
	got := ConfiguredRigName(t.TempDir(), config.Agent{
		Name: "worker",
		Dir:  aliasRigPath,
	}, []config.Rig{{Name: "demo", Path: rigPath}})
	if got != "demo" {
		t.Fatalf("ConfiguredRigName() = %q, want %q", got, "demo")
	}
}

func TestSamePathUsesSharedPathNormalization(t *testing.T) {
	a := "/private/tmp/gc-home"
	b := "/tmp/gc-home"
	got := samePath(a, b)
	want := runtime.GOOS == "darwin"
	if got != want {
		t.Fatalf("samePath(%q, %q) = %v, want %v", a, b, got, want)
	}
}

// writeStaleGitPointer creates a stale worktree marker file at parent/.git
// whose gitdir target does not exist on disk. Mirrors the bug shape from
// gascity#1556.
func writeStaleGitPointer(t *testing.T, parent, missingTarget string) {
	t.Helper()
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(parent, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: "+missingTarget+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeValidGitPointer creates a worktree marker file at parent/.git whose
// gitdir target is an existing directory.
func writeValidGitPointer(t *testing.T, parent, target string) {
	t.Helper()
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(parent, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: "+target+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestValidateAncestorWorktreesNotStale_NoMarkersInAncestry(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "c", "polecat-1")
	if err := ValidateAncestorWorktreesNotStale(target); err != nil {
		t.Fatalf("ValidateAncestorWorktreesNotStale() = %v, want nil (ancestry has no .git markers)", err)
	}
}

func TestValidateAncestorWorktreesNotStale_AncestorHasRealGitDir(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repo, ".gc", "worktrees", "demo", "polecats", "polecat-1")
	if err := ValidateAncestorWorktreesNotStale(target); err != nil {
		t.Fatalf("ValidateAncestorWorktreesNotStale() = %v, want nil (ancestor has real .git directory)", err)
	}
}

func TestValidateAncestorWorktreesNotStale_AncestorHasValidWorktreePointer(t *testing.T) {
	root := t.TempDir()
	wtParent := filepath.Join(root, "repo", ".gc", "worktrees", "demo", "polecats", "furiosa")
	gitdirTarget := filepath.Join(root, "repo", ".git", "worktrees", "furiosa")
	writeValidGitPointer(t, wtParent, gitdirTarget)
	target := filepath.Join(wtParent, "child-spawn")
	if err := ValidateAncestorWorktreesNotStale(target); err != nil {
		t.Fatalf("ValidateAncestorWorktreesNotStale() = %v, want nil (ancestor has valid worktree pointer)", err)
	}
}

func TestValidateAncestorWorktreesNotStale_AncestorHasStalePointer(t *testing.T) {
	root := t.TempDir()
	staleParent := filepath.Join(root, "repo", ".gc", "worktrees", "demo", "polecats", "furiosa")
	missingTarget := filepath.Join(root, "repo", ".git", "worktrees", "furiosa-was-removed")
	writeStaleGitPointer(t, staleParent, missingTarget)
	target := filepath.Join(staleParent, "worktrees", "ta-2j4p")
	err := ValidateAncestorWorktreesNotStale(target)
	if err == nil {
		t.Fatal("ValidateAncestorWorktreesNotStale() = nil, want error (ancestor has stale worktree pointer)")
	}
	for _, want := range []string{staleParent, missingTarget} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing reference %q", err.Error(), want)
		}
	}
}

func TestValidateAncestorWorktreesNotStale_PathItselfMissingButAncestryClean(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repo, "does", "not", "exist", "yet")
	if err := ValidateAncestorWorktreesNotStale(target); err != nil {
		t.Fatalf("ValidateAncestorWorktreesNotStale() = %v, want nil (path missing but ancestry clean)", err)
	}
}

func TestValidateAncestorWorktreesNotStale_StopsAtFirstValidMarker(t *testing.T) {
	// Two levels of worktree markers in the chain. The closer one (a valid
	// worktree pointer) should stop the walk; the further-up stale marker
	// must NOT be reached and thus must NOT trigger rejection. This mirrors
	// git's own resolution: once a valid worktree root is found, deeper
	// ancestors are not consulted.
	root := t.TempDir()
	deepStaleParent := filepath.Join(root, "outer")
	deepStaleTarget := filepath.Join(root, "outer", ".git", "worktrees", "gone")
	writeStaleGitPointer(t, deepStaleParent, deepStaleTarget)

	innerWtParent := filepath.Join(deepStaleParent, "inner", "polecats", "furiosa")
	innerWtTarget := filepath.Join(root, "real-repo", ".git", "worktrees", "furiosa")
	writeValidGitPointer(t, innerWtParent, innerWtTarget)

	target := filepath.Join(innerWtParent, "child-spawn")
	if err := ValidateAncestorWorktreesNotStale(target); err != nil {
		t.Fatalf("ValidateAncestorWorktreesNotStale() = %v, want nil (closer valid marker should stop the walk)", err)
	}
}

func TestValidateAncestorWorktreesNotStale_WalkTerminatesAtFilesystemRoot(t *testing.T) {
	// Tests that the loop terminates on systems where the temp tree has no
	// .git marker anywhere from path up to /. The validator must not loop
	// or stat-thrash.
	root := t.TempDir()
	target := filepath.Join(root, "no", "git", "anywhere", "in", "ancestry")
	if err := ValidateAncestorWorktreesNotStale(target); err != nil {
		t.Fatalf("ValidateAncestorWorktreesNotStale() = %v, want nil (no markers up to FS root)", err)
	}
}

// PR #2033 review: a gitdir: target written as a relative path must be
// resolved against the directory containing the .git file (Git's gitfile
// format), not against the process working directory.
func TestValidateAncestorWorktreesNotStale_RelativeGitdirTarget(t *testing.T) {
	root := t.TempDir()
	wtParent := filepath.Join(root, "repo", ".gc", "worktrees", "demo", "polecats", "furiosa")
	// Absolute target on disk lives alongside the pointer parent, so a
	// relative pointer "../../../.git/worktrees/furiosa" resolves to it.
	absTarget := filepath.Join(root, "repo", ".git", "worktrees", "furiosa")
	if err := os.MkdirAll(absTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtParent, 0o755); err != nil {
		t.Fatal(err)
	}
	relTarget, err := filepath.Rel(wtParent, absTarget)
	if err != nil {
		t.Fatalf("filepath.Rel: %v", err)
	}
	gitFile := filepath.Join(wtParent, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: "+relTarget+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(wtParent, "child-spawn")
	if err := ValidateAncestorWorktreesNotStale(target); err != nil {
		t.Fatalf("ValidateAncestorWorktreesNotStale() = %v, want nil (relative gitdir should resolve against .git parent)", err)
	}
}

// PR #2033 review: a gitdir target that exists on disk but is not a
// directory (regular file, broken symlink, etc.) must be rejected with
// the same shape as a missing target — the pointer is non-worktree-
// capable and spawning into a descendant produces dangling content.
func TestValidateAncestorWorktreesNotStale_GitdirTargetNotDirectory(t *testing.T) {
	root := t.TempDir()
	staleParent := filepath.Join(root, "repo", ".gc", "worktrees", "demo", "polecats", "furiosa")
	if err := os.MkdirAll(staleParent, 0o755); err != nil {
		t.Fatal(err)
	}
	// Target exists but is a regular file rather than a worktree admin dir.
	fileTarget := filepath.Join(root, "repo", ".git", "worktrees", "furiosa-file")
	if err := os.MkdirAll(filepath.Dir(fileTarget), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileTarget, []byte("not a directory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitFile := filepath.Join(staleParent, ".git")
	if err := os.WriteFile(gitFile, []byte("gitdir: "+fileTarget+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(staleParent, "child-spawn")
	err := ValidateAncestorWorktreesNotStale(target)
	if err == nil {
		t.Fatal("ValidateAncestorWorktreesNotStale() = nil, want error (gitdir target is a file, not a directory)")
	}
	for _, want := range []string{staleParent, fileTarget, "not a directory"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing reference %q", err.Error(), want)
		}
	}
}
