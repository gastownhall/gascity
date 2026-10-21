//go:build integration

package ssh

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestConn_ExecOverRealLocalhost exercises the actual ssh client when
// passwordless localhost ssh is available; it skips otherwise (e.g. CI).
// Its outcome depends on the host's sshd and the passwd-entry user's keys,
// neither of which is a declared Bazel input, so it lives behind the
// integration build tag (engdocs/contributors/bazel-test-hermeticity-audit.md).
func TestConn_ExecOverRealLocalhost(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("no ssh client")
	}
	kh := filepath.Join(t.TempDir(), "known_hosts")
	probe := exec.Command("ssh", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile="+kh, "localhost", "true")
	if probe.Run() != nil {
		t.Skip("passwordless ssh to localhost unavailable")
	}
	c := New(Endpoint{Host: "localhost", KnownHostsPath: kh})
	out, code, err := c.Exec(context.Background(), "", []string{"printf", "%s", "ok"})
	if err != nil {
		t.Fatalf("Exec over localhost: %v", err)
	}
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if string(out) != "ok" {
		t.Errorf("out = %q, want %q", out, "ok")
	}
}
