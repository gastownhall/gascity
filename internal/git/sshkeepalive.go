package git

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

const (
	// SSHKeepaliveInterval is the ssh ServerAliveInterval (seconds) that
	// keeps an idle git-over-SSH transport alive during a long pre-push hook.
	// GitHub drops idle SSH sessions in well under the ~13 minute hook run
	// that produced ga-2i5 / SIGPIPE 141.
	SSHKeepaliveInterval = 20
	// SSHKeepaliveCountMax is ssh ServerAliveCountMax. 20s * 90 = 30 minutes
	// of unanswered keepalives before ssh gives up — long enough for the
	// heavy pre-push gate, short enough to notice a truly dead peer.
	SSHKeepaliveCountMax = 90
)

// SSHKeepaliveOptions returns the ssh -o flags that hold the transport open.
func SSHKeepaliveOptions() string {
	return fmt.Sprintf("-o ServerAliveInterval=%d -o ServerAliveCountMax=%d",
		SSHKeepaliveInterval, SSHKeepaliveCountMax)
}

// SSHKeepaliveCommand is the canonical core.sshCommand / GIT_SSH_COMMAND value
// for a clone that does not already wrap ssh.
func SSHKeepaliveCommand() string {
	return "ssh " + SSHKeepaliveOptions()
}

// HasSSHKeepalive reports whether cmd already sends ssh keepalives.
func HasSSHKeepalive(cmd string) bool {
	return strings.Contains(cmd, "ServerAliveInterval")
}

// EnsureSSHKeepaliveCommand returns cmd with keepalive flags. An empty or
// bare `ssh` command becomes SSHKeepaliveCommand. An ssh invocation that
// already has other -o flags keeps them and gains keepalive. A custom
// wrapper (anything that is not ssh) is left unchanged so a deploy-key
// helper is not silently replaced.
func EnsureSSHKeepaliveCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if HasSSHKeepalive(cmd) {
		return cmd
	}
	if cmd == "" || cmd == "ssh" {
		return SSHKeepaliveCommand()
	}
	if strings.HasPrefix(cmd, "ssh ") {
		return strings.TrimSpace(cmd + " " + SSHKeepaliveOptions())
	}
	return cmd
}

// ApplySSHKeepaliveEnv sets GIT_SSH_COMMAND on env so agent-shell git push
// (including polecat worktrees of a different clone) keeps the SSH transport
// alive. Existing GIT_SSH_COMMAND values are merged, not replaced.
func ApplySSHKeepaliveEnv(env map[string]string) map[string]string {
	out := make(map[string]string, len(env)+1)
	for k, v := range env {
		out[k] = v
	}
	out["GIT_SSH_COMMAND"] = EnsureSSHKeepaliveCommand(out["GIT_SSH_COMMAND"])
	return out
}

// ErrCustomSSHCommand is returned by ApplySSHKeepaliveConfig when
// core.sshCommand is a non-ssh wrapper we must not overwrite.
var ErrCustomSSHCommand = errors.New("core.sshCommand is a custom wrapper; add ServerAliveInterval by hand")

// ApplySSHKeepaliveConfig stamps core.sshCommand on the repo at dir so every
// worktree of that clone inherits the keepalive. It is a no-op when keepalive
// is already configured. A custom wrapper is left untouched and returns
// ErrCustomSSHCommand.
func ApplySSHKeepaliveConfig(dir string) error {
	current, err := repoSSHCommand(dir)
	if err != nil {
		return err
	}
	next := EnsureSSHKeepaliveCommand(current)
	if !HasSSHKeepalive(next) {
		return fmt.Errorf("%w: %s", ErrCustomSSHCommand, current)
	}
	if next == current {
		return nil
	}
	cmd := exec.Command("git", "config", "core.sshCommand", next)
	cmd.Dir = dir
	cmd.Env = SanitizedEnv()
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		return fmt.Errorf("setting core.sshCommand in %s: %s: %w", dir, strings.TrimSpace(string(out)), runErr)
	}
	return nil
}

func repoSSHCommand(dir string) (string, error) {
	cmd := exec.Command("git", "config", "--get", "core.sshCommand")
	cmd.Dir = dir
	cmd.Env = SanitizedEnv()
	out, err := cmd.Output()
	if err != nil {
		ee := &exec.ExitError{}
		if errors.As(err, &ee) {
			return "", nil
		}
		return "", fmt.Errorf("reading core.sshCommand in %s: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}
