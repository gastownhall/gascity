//go:build acceptance_a

// Rig management acceptance tests.
//
// These exercise gc rig list, status, suspend, resume, and remove as
// a black box. Tests add a real git repo as a rig, then walk through
// the full lifecycle. Error paths for missing names and nonexistent
// rigs are also covered.
package acceptance_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

// createGitRig creates a minimal git repo suitable for gc rig add.
func createGitRig(t *testing.T) string {
	t.Helper()
	rigDir := filepath.Join(t.TempDir(), "testrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	for _, cmd := range [][]string{
		{"git", "init", rigDir},
		{"git", "-C", rigDir, "config", "user.email", "test@test.com"},
		{"git", "-C", rigDir, "config", "user.name", "Test"},
	} {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("git setup %v: %v\n%s", cmd, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(rigDir, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatalf("writing README: %v", err)
	}
	for _, cmd := range [][]string{
		{"git", "-C", rigDir, "add", "."},
		{"git", "-C", rigDir, "commit", "-m", "init"},
	} {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err != nil {
			t.Fatalf("git setup %v: %v\n%s", cmd, err, out)
		}
	}
	return rigDir
}

func TestRigListGastownCity(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	t.Run("ListSucceeds", func(t *testing.T) {
		out, err := c.GC("rig", "list")
		if err != nil {
			t.Fatalf("gc rig list failed: %v\n%s", err, out)
		}
		// Fresh gastown city should at least show HQ.
		if strings.TrimSpace(out) == "" {
			t.Fatal("gc rig list produced empty output")
		}
	})

	t.Run("ListJSON", func(t *testing.T) {
		out, err := c.GC("rig", "list", "--json")
		if err != nil {
			t.Fatalf("gc rig list --json failed: %v\n%s", err, out)
		}
		// JSON output should contain array brackets.
		if !strings.Contains(out, "[") {
			t.Errorf("expected JSON array in output, got:\n%s", out)
		}
	})
}

func TestRigLifecycle(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.InitFrom(filepath.Join(helpers.ExamplesDir(), "gastown"))

	rigDir := createGitRig(t)
	rigName := "testrig"

	// Add the rig.
	out, err := c.GC("rig", "add", rigDir, "--include", "packs/gastown")
	if err != nil {
		t.Fatalf("gc rig add failed: %v\n%s", err, out)
	}

	t.Run("ListShowsAddedRig", func(t *testing.T) {
		out, err := c.GC("rig", "list")
		if err != nil {
			t.Fatalf("gc rig list: %v\n%s", err, out)
		}
		if !strings.Contains(out, rigName) {
			t.Errorf("rig list should contain %q, got:\n%s", rigName, out)
		}
	})

	t.Run("StatusShowsRig", func(t *testing.T) {
		out, err := c.GC("rig", "status", rigName)
		if err != nil {
			t.Fatalf("gc rig status %s: %v\n%s", rigName, err, out)
		}
		if !strings.Contains(out, rigName) {
			t.Errorf("rig status should mention %q, got:\n%s", rigName, out)
		}
	})

	t.Run("SuspendThenResume", func(t *testing.T) {
		out, err := c.GC("rig", "suspend", rigName)
		if err != nil {
			t.Fatalf("gc rig suspend %s: %v\n%s", rigName, err, out)
		}

		// Verify suspended state in config.
		toml := c.ReadFile("city.toml")
		if !strings.Contains(toml, "suspended") {
			t.Error("city.toml should contain 'suspended' after rig suspend")
		}

		// Resume.
		out, err = c.GC("rig", "resume", rigName)
		if err != nil {
			t.Fatalf("gc rig resume %s: %v\n%s", rigName, err, out)
		}
	})

	t.Run("Remove", func(t *testing.T) {
		out, err := c.GC("rig", "remove", rigName)
		if err != nil {
			t.Fatalf("gc rig remove %s: %v\n%s", rigName, err, out)
		}

		// After removal, rig should not appear in list.
		listOut, err := c.GC("rig", "list")
		if err != nil {
			t.Fatalf("gc rig list after remove: %v\n%s", err, listOut)
		}
		if strings.Contains(listOut, rigName) {
			t.Errorf("rig list should not contain %q after remove, got:\n%s", rigName, listOut)
		}
	})
}

func TestRigErrors(t *testing.T) {
	c := helpers.NewCity(t, testEnv)
	c.Init("claude")

	t.Run("StatusMissingName", func(t *testing.T) {
		_, err := c.GC("rig", "status")
		if err == nil {
			t.Fatal("expected error for rig status without name")
		}
	})

	t.Run("StatusNonexistent", func(t *testing.T) {
		_, err := c.GC("rig", "status", "no-such-rig-xyz")
		if err == nil {
			t.Fatal("expected error for nonexistent rig status")
		}
	})

	t.Run("RemoveMissingName", func(t *testing.T) {
		_, err := c.GC("rig", "remove")
		if err == nil {
			t.Fatal("expected error for rig remove without name")
		}
	})

	t.Run("RemoveNonexistent", func(t *testing.T) {
		_, err := c.GC("rig", "remove", "no-such-rig-xyz")
		if err == nil {
			t.Fatal("expected error for removing nonexistent rig")
		}
	})

	t.Run("SuspendMissingName", func(t *testing.T) {
		_, err := c.GC("rig", "suspend")
		if err == nil {
			t.Fatal("expected error for rig suspend without name")
		}
	})

	t.Run("ResumeMissingName", func(t *testing.T) {
		_, err := c.GC("rig", "resume")
		if err == nil {
			t.Fatal("expected error for rig resume without name")
		}
	})

	t.Run("ListFromNonCity", func(t *testing.T) {
		emptyDir := t.TempDir()
		_, err := helpers.RunGC(testEnv, emptyDir, "rig", "list")
		if err == nil {
			t.Fatal("expected error for rig list from non-city directory")
		}
	})
}
