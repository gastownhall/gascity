package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
)

func TestRigSSHKeepaliveCheck_NoHookOK(t *testing.T) {
	rigPath := initGitRepoOnBranch(t, "main")
	c := NewRigSSHKeepaliveCheck(config.Rig{Name: "testrig", Path: rigPath})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want OK", r.Status, r.Message)
	}
	if c.CanFix() {
		t.Fatal("CanFix = true, want false when there is nothing to stamp")
	}
}

func TestRigSSHKeepaliveCheck_ShortHookOK(t *testing.T) {
	rigPath := initGitRepoOnBranch(t, "main")
	writePrePush(t, rigPath, "#!/bin/sh\necho ok\n")
	c := NewRigSSHKeepaliveCheck(config.Rig{Name: "testrig", Path: rigPath})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want OK for short hook", r.Status, r.Message)
	}
}

func TestRigSSHKeepaliveCheck_LongHookWithoutKeepaliveWarns(t *testing.T) {
	rigPath := initGitRepoOnBranch(t, "main")
	writePrePush(t, rigPath, "#!/bin/sh\nexec make test-fast-parallel\n")
	c := NewRigSSHKeepaliveCheck(config.Rig{Name: "testrig", Path: rigPath})
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want Warning", r.Status, r.Message)
	}
	if r.Severity != SeverityAdvisory {
		t.Fatalf("severity = %d, want Advisory", r.Severity)
	}
	if !strings.Contains(r.Message, "keepalive") {
		t.Errorf("message = %q, want keepalive", r.Message)
	}
	if !c.CanFix() {
		t.Fatal("CanFix = false, want true")
	}
	if r.FixHint == "" {
		t.Fatal("FixHint empty")
	}
}

func TestRigSSHKeepaliveCheck_LongHookWithKeepaliveOK(t *testing.T) {
	rigPath := initGitRepoOnBranch(t, "main")
	writePrePush(t, rigPath, "#!/bin/sh\nexec make test-fast-parallel\n")
	runGitForRigRootBranchTest(t, rigPath, "config", "core.sshCommand", git.SSHKeepaliveCommand())
	c := NewRigSSHKeepaliveCheck(config.Rig{Name: "testrig", Path: rigPath})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d (%s), want OK", r.Status, r.Message)
	}
}

func TestRigSSHKeepaliveCheck_FixStampsCoreSSHCommand(t *testing.T) {
	rigPath := initGitRepoOnBranch(t, "main")
	writePrePush(t, rigPath, "#!/bin/sh\nexec make test-fast-parallel\n")
	c := NewRigSSHKeepaliveCheck(config.Rig{Name: "testrig", Path: rigPath})
	if status := c.Run(&CheckContext{}); status.Status != StatusWarning {
		t.Fatalf("pre-fix status = %d (%s), want Warning", status.Status, status.Message)
	}
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("post-fix status = %d (%s), want OK", r.Status, r.Message)
	}
	got := strings.TrimSpace(runGitForRigRootBranchTest(t, rigPath, "config", "--get", "core.sshCommand"))
	if !git.HasSSHKeepalive(got) {
		t.Fatalf("core.sshCommand = %q", got)
	}
}

func TestRigSSHKeepaliveCheck_CustomWrapperNotAutoFixed(t *testing.T) {
	rigPath := initGitRepoOnBranch(t, "main")
	writePrePush(t, rigPath, "#!/bin/sh\nexec make test-fast-parallel\n")
	runGitForRigRootBranchTest(t, rigPath, "config", "core.sshCommand", "/usr/local/bin/ssh-wrapper")
	c := NewRigSSHKeepaliveCheck(config.Rig{Name: "testrig", Path: rigPath})
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d (%s), want Warning", r.Status, r.Message)
	}
	if c.CanFix() {
		t.Fatal("CanFix = true, want false for custom wrapper")
	}
}

func TestRigSSHKeepaliveCheck_WarmupEligible(t *testing.T) {
	c := NewRigSSHKeepaliveCheck(config.Rig{Name: "testrig", Path: t.TempDir()})
	if !c.WarmupEligible() {
		t.Fatal("WarmupEligible = false, want true")
	}
}

func writePrePush(t *testing.T, repoPath, content string) {
	t.Helper()
	dir := filepath.Join(repoPath, ".githooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	path := filepath.Join(dir, "pre-push")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write pre-push: %v", err)
	}
	runGitForRigRootBranchTest(t, repoPath, "config", "core.hooksPath", ".githooks")
}
