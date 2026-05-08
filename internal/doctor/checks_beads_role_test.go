package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// setupFakeGitConfig returns a HOME override that points to an empty temp dir,
// so git config --global reads/writes go there without touching the real user config.
func setupFakeGitConfig(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Windows / macOS also respect GIT_CONFIG_GLOBAL:
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	return home
}

func TestBeadsRoleCheck_NotSet(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	setupFakeGitConfig(t)

	c := &BeadsRoleCheck{}
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %v, want StatusError (beads.role unset)", r.Status)
	}
	if !strings.Contains(r.Message, "beads.role") {
		t.Errorf("message %q should mention beads.role", r.Message)
	}
	if r.FixHint == "" {
		t.Error("FixHint should be set when beads.role is missing")
	}
}

func TestBeadsRoleCheck_Set(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := setupFakeGitConfig(t)

	cfg := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(cfg, []byte("[beads]\n\trole = maintainer\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &BeadsRoleCheck{}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %v, want StatusOK (beads.role = maintainer)", r.Status)
	}
	if !strings.Contains(r.Message, "maintainer") {
		t.Errorf("message %q should include the role value", r.Message)
	}
}

func TestBeadsRoleCheck_CanFix(t *testing.T) {
	c := &BeadsRoleCheck{}
	if !c.CanFix() {
		t.Fatal("CanFix should return true")
	}
}

func TestBeadsRoleCheck_Fix_SetsRole(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	setupFakeGitConfig(t)

	c := &BeadsRoleCheck{}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	// After Fix, Run should pass.
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("after Fix: status = %v, want StatusOK", r.Status)
	}
	if !strings.Contains(r.Message, "maintainer") {
		t.Errorf("after Fix: message %q should contain 'maintainer'", r.Message)
	}
}

func TestBeadsRoleCheck_Fix_PreservesExistingRole(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := setupFakeGitConfig(t)

	cfg := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(cfg, []byte("[beads]\n\trole = contributor\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &BeadsRoleCheck{}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	// Should not have overwritten the existing "contributor" value.
	out, err := exec.Command("git", "config", "--global", "beads.role").Output()
	if err != nil {
		t.Fatalf("git config --global beads.role: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "contributor" {
		t.Errorf("beads.role = %q, want %q (Fix should preserve existing value)", got, "contributor")
	}
}

func TestBeadsRoleCheck_Fix_PreservesReadFailureContext(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home := setupFakeGitConfig(t)
	cfg := filepath.Join(home, ".gitconfig")
	if err := os.WriteFile(cfg, []byte("not valid\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &BeadsRoleCheck{}
	err := c.Fix(&CheckContext{})
	if err == nil {
		t.Fatal("Fix error = nil, want read failure context")
	}
	if !strings.Contains(err.Error(), "bad config line 1") {
		t.Fatalf("Fix error = %q, want preserved git read failure", err)
	}
}
