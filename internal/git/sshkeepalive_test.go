package git

import (
	"os/exec"
	"strings"
	"testing"
)

func TestEnsureSSHKeepaliveCommand(t *testing.T) {
	wantDefault := "ssh -o ServerAliveInterval=20 -o ServerAliveCountMax=90"
	if got := EnsureSSHKeepaliveCommand(""); got != wantDefault {
		t.Fatalf("empty = %q, want %q", got, wantDefault)
	}
	if got := EnsureSSHKeepaliveCommand("ssh"); got != wantDefault {
		t.Fatalf("bare ssh = %q, want %q", got, wantDefault)
	}

	already := "ssh -o ServerAliveInterval=30 -o IdentitiesOnly=yes"
	if got := EnsureSSHKeepaliveCommand(already); got != already {
		t.Fatalf("existing keepalive rewritten: %q", got)
	}

	withKey := "ssh -i /keys/id -o IdentitiesOnly=yes -o BatchMode=yes"
	got := EnsureSSHKeepaliveCommand(withKey)
	if !HasSSHKeepalive(got) {
		t.Fatalf("key command missing keepalive: %q", got)
	}
	if !strings.Contains(got, "-i /keys/id") || !strings.Contains(got, "IdentitiesOnly=yes") {
		t.Fatalf("key command dropped auth flags: %q", got)
	}

	wrapper := "/usr/local/bin/ssh-wrapper --policy"
	if got := EnsureSSHKeepaliveCommand(wrapper); got != wrapper {
		t.Fatalf("wrapper rewritten: %q", got)
	}
}

func TestApplySSHKeepaliveEnv(t *testing.T) {
	got := ApplySSHKeepaliveEnv(nil)
	if !HasSSHKeepalive(got["GIT_SSH_COMMAND"]) {
		t.Fatalf("nil env GIT_SSH_COMMAND = %q", got["GIT_SSH_COMMAND"])
	}

	env := map[string]string{"GIT_SSH_COMMAND": "ssh -i /k", "OTHER": "keep"}
	got = ApplySSHKeepaliveEnv(env)
	if got["OTHER"] != "keep" {
		t.Fatalf("unrelated env dropped: %#v", got)
	}
	if !HasSSHKeepalive(got["GIT_SSH_COMMAND"]) || !strings.Contains(got["GIT_SSH_COMMAND"], "-i /k") {
		t.Fatalf("merged GIT_SSH_COMMAND = %q", got["GIT_SSH_COMMAND"])
	}
	if env["GIT_SSH_COMMAND"] != "ssh -i /k" {
		t.Fatalf("input env mutated: %#v", env)
	}
}

func TestApplySSHKeepaliveConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir := initTestRepo(t)

	if err := ApplySSHKeepaliveConfig(dir); err != nil {
		t.Fatalf("ApplySSHKeepaliveConfig: %v", err)
	}
	got := strings.TrimSpace(runGit(t, dir, "config", "--get", "core.sshCommand"))
	if !HasSSHKeepalive(got) {
		t.Fatalf("core.sshCommand = %q, want keepalive", got)
	}

	runGit(t, dir, "config", "core.sshCommand", "ssh -i /k -o IdentitiesOnly=yes")
	if err := ApplySSHKeepaliveConfig(dir); err != nil {
		t.Fatalf("ApplySSHKeepaliveConfig merge: %v", err)
	}
	got = strings.TrimSpace(runGit(t, dir, "config", "--get", "core.sshCommand"))
	if !HasSSHKeepalive(got) || !strings.Contains(got, "-i /k") {
		t.Fatalf("merged core.sshCommand = %q", got)
	}
}

func TestApplySSHKeepaliveConfigLeavesCustomWrapper(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	dir := initTestRepo(t)
	runGit(t, dir, "config", "core.sshCommand", "/usr/local/bin/ssh-wrapper")
	if err := ApplySSHKeepaliveConfig(dir); err == nil {
		t.Fatal("expected error for custom wrapper")
	}
	got := strings.TrimSpace(runGit(t, dir, "config", "--get", "core.sshCommand"))
	if got != "/usr/local/bin/ssh-wrapper" {
		t.Fatalf("wrapper overwritten: %q", got)
	}
}
