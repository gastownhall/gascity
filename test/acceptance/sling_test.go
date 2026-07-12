//go:build acceptance_a

// Sling command acceptance tests.
//
// These exercise gc sling as a black box. Sling is the core dispatch
// mechanism that routes work (beads, formulas, inline text) to agents.
// Tests focus on argument validation, flag conflicts, and dry-run
// preview output since real dispatch requires a running city.
package acceptance_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/packregistry"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

func TestSlingCommands(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitNoStart("claude")

	// --- argument validation ---

	t.Run("NoArgs_ReturnsError", func(t *testing.T) {
		out, err := c.GC("sling")
		if err == nil {
			t.Fatal("expected error for bare 'gc sling', got success")
		}
		if !strings.Contains(out, "requires 1 or 2 arguments") {
			t.Errorf("expected argument count error, got:\n%s", out)
		}
	})

	t.Run("TooManyArgs_ReturnsError", func(t *testing.T) {
		out, err := c.GC("sling", "a", "b", "c")
		if err == nil {
			t.Fatal("expected error for too many args, got success")
		}
		if !strings.Contains(out, "requires 1 or 2 arguments") {
			t.Errorf("expected argument count error, got:\n%s", out)
		}
	})

	// --- flag validation ---

	t.Run("InvalidMergeStrategy_ReturnsError", func(t *testing.T) {
		out, err := c.GC("sling", "--merge", "squash", "agent", "text")
		if err == nil {
			t.Fatal("expected error for invalid merge strategy, got success")
		}
		if !strings.Contains(out, "must be direct, mr, or local") {
			t.Errorf("expected merge strategy error, got:\n%s", out)
		}
	})

	t.Run("OwnedWithNoConvoy_ReturnsError", func(t *testing.T) {
		out, err := c.GC("sling", "--owned", "--no-convoy", "agent", "text")
		if err == nil {
			t.Fatal("expected error for --owned with --no-convoy, got success")
		}
		if !strings.Contains(out, "cannot use with --no-convoy") {
			t.Errorf("expected conflict error, got:\n%s", out)
		}
	})

	t.Run("StdinRequiresOneArg_ReturnsError", func(t *testing.T) {
		out, err := c.GC("sling", "--stdin", "agent", "extra")
		if err == nil {
			t.Fatal("expected error for --stdin with 2 args, got success")
		}
		if !strings.Contains(out, "--stdin requires exactly 1 argument") {
			t.Errorf("expected --stdin argument error, got:\n%s", out)
		}
	})

	// --- target resolution errors ---

	t.Run("NonexistentAgent_ReturnsError", func(t *testing.T) {
		_, err := c.GC("sling", "nonexistent-agent-xyz", "some work")
		if err == nil {
			t.Fatal("expected error for nonexistent agent, got success")
		}
	})

	t.Run("InlineTextWithoutTarget_ReturnsError", func(t *testing.T) {
		// Single arg that looks like inline text (not a bead ID) needs a target.
		out, err := c.GC("sling", "write a README")
		if err == nil {
			t.Fatal("expected error for inline text without target, got success")
		}
		if !strings.Contains(out, "inline text requires explicit target") {
			t.Errorf("expected 'inline text requires explicit target' error, got:\n%s", out)
		}
	})
}

func TestSlingDryRun(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFromNoStart(filepath.Join(helpers.ExamplesDir(), "gastown"))

	agentName := findFirstAgent(t, c)
	if agentName == "" {
		t.Skip("no agents found in gastown city config")
	}

	t.Run("InlineText_ShowsPreview", func(t *testing.T) {
		out, err := c.GC("sling", "--dry-run", agentName, "write unit tests")
		if err != nil {
			t.Fatalf("gc sling --dry-run: %v\n%s", err, out)
		}
		if !strings.Contains(out, "No side effects executed") {
			t.Errorf("dry-run output should contain 'No side effects executed', got:\n%s", out)
		}
		if !strings.Contains(out, "Target:") {
			t.Errorf("dry-run output should contain 'Target:' section, got:\n%s", out)
		}
	})

	t.Run("Formula_ShowsPreview", func(t *testing.T) {
		// Use --formula with dry-run. Formula may not exist but we're testing
		// that the dry-run path handles the attempt gracefully.
		out, err := c.GC("sling", "--dry-run", "-f", agentName, "mol-polecat-work")
		if err != nil {
			// Formula might not exist; that's OK as long as the error is about
			// the formula, not a crash.
			if strings.Contains(out, "No side effects executed") {
				// Dry-run succeeded despite formula issues — fine.
				return
			}
			// If it's a formula-not-found error, that's expected.
			if strings.Contains(out, "formula") || strings.Contains(out, "not found") {
				t.Log("formula not found (expected in some configs)")
				return
			}
			t.Fatalf("gc sling --dry-run -f: %v\n%s", err, out)
		}
		if !strings.Contains(out, "No side effects executed") {
			t.Errorf("dry-run output should contain 'No side effects executed', got:\n%s", out)
		}
	})
}

func TestSlingBuildBasicFromGascityRig(t *testing.T) {
	pluginEnv := testEnv.Clone().WithConfiguredBeadsBackend()
	configureLocalBdGCDLRegistry(t, pluginEnv)

	c := helpers.NewCity(t, pluginEnv)
	out, err := helpers.RunGC(
		pluginEnv,
		"",
		"init",
		"--template", "gascity",
		"--default-provider", "claude",
		"--beads-backend", "doltlite",
		"--skip-provider-readiness",
		"--no-start",
		c.Dir,
	)
	if err != nil {
		t.Fatalf("gc init --template gascity failed: %v\n%s", err, out)
	}
	installGCRolesRigTemplateShim(t, c)

	rigDir := createGitRig(t)
	out, err = c.GC("rig", "add", rigDir)
	if err != nil {
		t.Fatalf("gc rig add failed: %v\n%s", err, out)
	}

	out, err = helpers.RunGC(
		c.Env,
		rigDir,
		"--city", c.Dir,
		"--rig", "testrig",
		"sling",
		"testrig/gc.run-operator",
		"Build a tiny greeting helper",
		"--on", "build-basic",
		"--var", "artifact_root=.gc/acceptance/build-basic",
	)
	if err != nil {
		t.Fatalf("gc sling testrig/gc.run-operator --on build-basic: %v\n%s", err, out)
	}
	lowerOut := strings.ToLower(out)
	if !strings.Contains(lowerOut, "started workflow") && !strings.Contains(lowerOut, "attached workflow") && !strings.Contains(lowerOut, "slung") {
		t.Fatalf("build-basic sling output did not report dispatch:\n%s", out)
	}
	if !strings.Contains(out, "build-basic") {
		t.Fatalf("build-basic sling output missing formula name:\n%s", out)
	}
}

func configureLocalBdGCDLRegistry(t *testing.T, env *helpers.Env) {
	t.Helper()

	packRepo := "/home/ubuntu/.gc/cache/repos/e65dec680efb221706c51d5d75893d7bc152f8c054b269cb8b02544b91ceccc9"
	packDir := filepath.Join(packRepo, "bd-gc-dl")
	if _, err := os.Stat(filepath.Join(packDir, "pack.toml")); err != nil {
		t.Fatalf("bd-gc-dl pack source is not available at %s: %v", packDir, err)
	}
	commitOut, err := exec.Command("git", "-C", packRepo, "rev-parse", "HEAD").CombinedOutput()
	if err != nil {
		t.Fatalf("resolving bd-gc-dl pack source commit: %v\n%s", err, string(commitOut))
	}
	commit := strings.TrimSpace(string(commitOut))
	hash, err := packregistry.PackContentHash(packRepo, commit, "bd-gc-dl")
	if err != nil {
		t.Fatalf("hashing bd-gc-dl pack source: %v", err)
	}

	registryDir := t.TempDir()
	registryBody := `schema = 1

[[pack]]
name = "bd-gc-dl"
description = "Plugin-backed DoltLite beads backend."
source = "file://` + filepath.ToSlash(packRepo) + `//bd-gc-dl"
source_kind = "git"

[[pack.plugin]]
kind = "beads-backend"
backend = "doltlite"
display_name = "DoltLite"
capabilities = ["setup", "provider", "metadata", "fastpath", "store-health"]

[[pack.release]]
version = "0.0.1"
ref = "HEAD"
commit = "` + commit + `"
hash = "` + hash + `"
description = "Local acceptance fixture"
`
	if err := os.WriteFile(filepath.Join(registryDir, "registry.toml"), []byte(registryBody), 0o644); err != nil {
		t.Fatalf("writing local bd-gc-dl registry: %v", err)
	}
	if err := packregistry.SaveConfig(env.Get("GC_HOME"), packregistry.Config{Registries: []packregistry.Registry{{
		Name:   "local",
		Source: registryDir,
	}}}); err != nil {
		t.Fatalf("saving local pack registry config: %v", err)
	}
}

func installGCRolesRigTemplateShim(t *testing.T, c *helpers.City) {
	t.Helper()

	shimDir := filepath.Join(c.Dir, ".gc", "local-packs", "gc-roles-city")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("creating role shim pack: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shimDir, "pack.toml"), []byte("[pack]\nname = \"gc-roles-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("writing role shim pack.toml: %v", err)
	}
	for _, role := range []string{
		"design-author",
		"design-implementation-reviewer",
		"gap-analyst",
		"implementation-reviewer",
		"implementation-worker",
		"publisher",
		"requirements-planner",
		"review-synthesizer",
		"run-operator",
		"task-decomposer",
	} {
		agentDir := filepath.Join(shimDir, "agents", role)
		if err := os.MkdirAll(agentDir, 0o755); err != nil {
			t.Fatalf("creating role shim %s: %v", role, err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte("scope = \"rig\"\nfallback = true\n"), 0o644); err != nil {
			t.Fatalf("writing role shim agent.toml for %s: %v", role, err)
		}
		if err := os.WriteFile(filepath.Join(agentDir, "prompt.template.md"), []byte("# "+role+"\n\nAcceptance role shim.\n"), 0o644); err != nil {
			t.Fatalf("writing role shim prompt for %s: %v", role, err)
		}
	}

	packToml := filepath.Join(c.Dir, "pack.toml")
	f, err := os.OpenFile(packToml, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("opening pack.toml: %v", err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := f.WriteString("\n[imports.gc]\nsource = \".gc/local-packs/gc-roles-city\"\n"); err != nil {
		t.Fatalf("appending gc role shim import: %v", err)
	}
}

// --- helpers ---

// findFirstAgent parses gc config explain to find the first agent name.
func findFirstAgent(t *testing.T, c *helpers.City) string {
	t.Helper()
	out, err := c.GC("config", "explain")
	if err != nil {
		t.Fatalf("gc config explain: %v\n%s", err, out)
	}
	// Look for agent lines in config output.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "agent:") || strings.HasPrefix(line, "Agent:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				return parts[1]
			}
		}
	}
	// Try gc agent list if config explain didn't work.
	listOut, err := c.GC("agent", "list")
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(listOut), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 0 && !strings.EqualFold(fields[0], "NAME") && !strings.HasPrefix(fields[0], "-") {
			return fields[0]
		}
	}
	return ""
}
