package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// mkdirs creates each directory under root and returns the root.
func mkdirs(t *testing.T, root string, rel ...string) string {
	t.Helper()
	for _, r := range rel {
		if err := os.MkdirAll(filepath.Join(root, r), 0o755); err != nil {
			t.Fatalf("MkdirAll %s: %v", r, err)
		}
	}
	return root
}

func envFunc(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

// TestClassifyPrimeHookCwd_ForeignRigRefuses is the regression for the
// daemon identity leak: a session whose ambient environment says it is the
// flinders project lead, but whose hook payload reports a cwd inside the
// arnotts rig, must be classified foreign so no role prompt is rendered.
func TestClassifyPrimeHookCwd_ForeignRigRefuses(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "flinders", "arnotts", "city")
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "flinders", Path: filepath.Join(root, "flinders")},
		{Name: "arnotts", Path: filepath.Join(root, "arnotts")},
	}}
	roots := primeIdentityRootsFromEnv(cfg, filepath.Join(root, "city"), envFunc(map[string]string{
		"GC_RIG":      "flinders",
		"GC_DIR":      filepath.Join(root, "flinders"),
		"GC_RIG_ROOT": filepath.Join(root, "flinders"),
	}))

	verdict, owner := classifyPrimeHookCwd(filepath.Join(root, "arnotts"), roots)
	if verdict != primeIdentityForeign {
		t.Fatalf("cwd inside another rig: got verdict %q, want %q", verdict, primeIdentityForeign)
	}
	if owner != "arnotts" {
		t.Errorf("owner: got %q, want %q", owner, "arnotts")
	}
}

// TestClassifyPrimeHookCwd_OwnRigAndNestedWorktreeOK guards the common healthy
// cases: the agent's own rig root, and a polecat's per-bead worktree nested
// under its agent home. Neither may be flagged.
func TestClassifyPrimeHookCwd_OwnRigAndNestedWorktreeOK(t *testing.T) {
	root := mkdirs(t, t.TempDir(),
		"gas-city",
		"city/.gc/worktrees/gas-city/polecats/slit/worktrees/gci-cfki",
		"other")
	cityPath := filepath.Join(root, "city")
	agentHome := filepath.Join(cityPath, ".gc", "worktrees", "gas-city", "polecats", "slit")
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "gas-city", Path: filepath.Join(root, "gas-city")},
		{Name: "other", Path: filepath.Join(root, "other")},
	}}
	roots := primeIdentityRootsFromEnv(cfg, cityPath, envFunc(map[string]string{
		"GC_RIG":  "gas-city",
		"GC_DIR":  agentHome,
		"GC_CITY": cityPath,
	}))

	for _, cwd := range []string{
		filepath.Join(root, "gas-city"),
		agentHome,
		filepath.Join(agentHome, "worktrees", "gci-cfki"),
	} {
		if verdict, _ := classifyPrimeHookCwd(cwd, roots); verdict != primeIdentityOK {
			t.Errorf("cwd %s: got verdict %q, want %q", cwd, verdict, primeIdentityOK)
		}
	}
}

// TestClassifyPrimeHookCwd_CityRootDoesNotLaunderForeignWorktree is the
// longest-match rule. Every rig's worktrees live under the city directory, and
// city-scoped agents legitimately claim the city as a self root. Without
// longest-match, that self root would swallow another rig's worktree and
// silently pass the leak through.
func TestClassifyPrimeHookCwd_CityRootDoesNotLaunderForeignWorktree(t *testing.T) {
	root := mkdirs(t, t.TempDir(),
		"city/.gc/worktrees/detmold/polecats/x",
		"city/.gc/worktrees/gas-city/polecats/y",
		"detmold", "gas-city")
	cityPath := filepath.Join(root, "city")
	cfg := &config.City{Rigs: []config.Rig{
		{Name: "gas-city", Path: filepath.Join(root, "gas-city")},
		{Name: "detmold", Path: filepath.Join(root, "detmold")},
	}}
	roots := primeIdentityRootsFromEnv(cfg, cityPath, envFunc(map[string]string{
		"GC_RIG":  "gas-city",
		"GC_CITY": cityPath,
	}))

	foreign := filepath.Join(cityPath, ".gc", "worktrees", "detmold", "polecats", "x")
	verdict, owner := classifyPrimeHookCwd(foreign, roots)
	if verdict != primeIdentityForeign {
		t.Fatalf("foreign worktree under the city root: got %q, want %q", verdict, primeIdentityForeign)
	}
	if owner != "detmold" {
		t.Errorf("owner: got %q, want %q", owner, "detmold")
	}
}

// TestClassifyPrimeHookCwd_SymlinkedAgentHomeIsNotForeign is the false-positive
// guard that matters most in practice: gc agent homes are commonly reached
// through a symlinked prefix (a city whose worktrees live on another volume).
// The configured path and the path the provider reports then differ as strings
// while naming the same directory. A plain compare would mark every healthy
// polecat foreign — worse than the bug being fixed.
func TestClassifyPrimeHookCwd_SymlinkedAgentHomeIsNotForeign(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real-volume", "slit")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "city"), 0o755); err != nil {
		t.Fatalf("MkdirAll city: %v", err)
	}
	link := filepath.Join(root, "city", "agent-home")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "gas-city", Path: filepath.Join(root, "city")}}}
	// Environment names the agent home through the symlink...
	roots := primeIdentityRootsFromEnv(cfg, filepath.Join(root, "city"), envFunc(map[string]string{
		"GC_RIG": "gas-city",
		"GC_DIR": link,
	}))
	// ...while the provider reports the resolved real path.
	if verdict, _ := classifyPrimeHookCwd(realDir, roots); verdict != primeIdentityOK {
		t.Errorf("symlinked agent home: got verdict %q, want %q", verdict, primeIdentityOK)
	}
}

// TestClassifyPrimeHookCwd_NoEvidenceNeverRefuses pins the asymmetry: an empty
// cwd (older provider, non-Claude hook format) is OK, and an unrecognized cwd
// is unknown — never foreign. Refusal requires positive evidence of another
// owner.
func TestClassifyPrimeHookCwd_NoEvidenceNeverRefuses(t *testing.T) {
	root := mkdirs(t, t.TempDir(), "gas-city", "city", "elsewhere")
	cfg := &config.City{Rigs: []config.Rig{{Name: "gas-city", Path: filepath.Join(root, "gas-city")}}}
	roots := primeIdentityRootsFromEnv(cfg, filepath.Join(root, "city"), envFunc(map[string]string{
		"GC_RIG": "gas-city",
		"GC_DIR": filepath.Join(root, "gas-city"),
	}))

	if verdict, _ := classifyPrimeHookCwd("", roots); verdict != primeIdentityOK {
		t.Errorf("empty cwd: got %q, want %q", verdict, primeIdentityOK)
	}
	if verdict, _ := classifyPrimeHookCwd(filepath.Join(root, "elsewhere"), roots); verdict != primeIdentityUnknown {
		t.Errorf("unrecognized cwd: got %q, want %q", verdict, primeIdentityUnknown)
	}
}

// TestIdentityPathWithin_SiblingPrefixIsNotContained guards the containment
// helper against the classic prefix bug: "/a/bc" must not count as inside
// "/a/b".
func TestIdentityPathWithin_SiblingPrefixIsNotContained(t *testing.T) {
	if identityPathWithin("/a/bc", "/a/b") {
		t.Error("/a/bc must not be treated as inside /a/b")
	}
	if !identityPathWithin("/a/b/c", "/a/b") {
		t.Error("/a/b/c must be inside /a/b")
	}
	if !identityPathWithin("/a/b", "/a/b") {
		t.Error("a path must be inside itself")
	}
}

// TestPrimeIdentityMismatchPrompt_NamesBothSides checks the refusal actually
// tells the reader what to do: which identity the environment claimed, where
// the session really is, who owns it, and that gc/bd must not be run.
func TestPrimeIdentityMismatchPrompt_NamesBothSides(t *testing.T) {
	got := primeIdentityMismatchPrompt("flinders/oversight-rig.project-lead", "flinders", "/home/edward/the-arnotts-group", "arnotts")
	for _, want := range []string{
		"IDENTITY MISMATCH",
		"flinders/oversight-rig.project-lead",
		"/home/edward/the-arnotts-group",
		"arnotts",
		"Do not run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal prompt missing %q:\n%s", want, got)
		}
	}
}

// TestWarnPrimeIdentityUnknown_QuietOnEmptyCwd keeps the diagnostic from
// firing when there is nothing to report.
func TestWarnPrimeIdentityUnknown_QuietOnEmptyCwd(t *testing.T) {
	var buf bytes.Buffer
	warnPrimeIdentityUnknown(&buf, "gas-city/gastown.slit", "")
	if buf.Len() != 0 {
		t.Errorf("expected no warning for empty cwd, got %q", buf.String())
	}
	warnPrimeIdentityUnknown(&buf, "gas-city/gastown.slit", "/somewhere")
	if !strings.Contains(buf.String(), "/somewhere") {
		t.Errorf("expected warning naming the cwd, got %q", buf.String())
	}
}
