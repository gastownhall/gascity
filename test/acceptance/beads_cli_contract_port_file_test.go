//go:build acceptance_bd_contract

// Port-file precedence coverage for agent sessions. Kept in its own file so
// beads_cli_contract_test.go's test list stays the focused external contract
// manifest pinned by TestAcceptanceTargetsSeparateTierAFromExternalBdContracts
// (scripts/ci_critical_path_test.go).
package acceptance_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/beadstest"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
	"github.com/gastownhall/gascity/test/toolhome"
)

// TestBdReadsScopePortFileUnderWithheldSessionEnv pins the bd precedence gc
// relies on when it withholds the Dolt port from an agent session's env: with
// the port keys present but empty and shared-server mode pinned off, bd dials
// the port in the scope's .beads/dolt-server.port mirror, even under a HOME
// whose user-level config enables dolt.shared-server. Without the opt-out the
// same HOME takes bd away from the mirror, which is why the session pins it.
func TestBdReadsScopePortFileUnderWithheldSessionEnv(t *testing.T) {
	bdPath := helpers.RequireBD(t)

	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".beads", "config.yaml"), []byte("dolt:\n    shared-server: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	port := closedLoopbackPort(t)
	dir := beadstest.GuardedTempDir(t)
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"metadata.json":    `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"ct"}`,
		"config.yaml":      "issue_prefix: ct\ndolt.auto-start: false\n",
		"dolt-server.port": port + "\n",
	} {
		if err := os.WriteFile(filepath.Join(beadsDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// No BEADS_TEST_MODE: under it bd dials port 1 whenever no env port is
	// set, skipping the port file this test pins. Agent sessions never carry
	// it, and toolhome already disables bd's metrics.
	sessionEnv := []string{
		"BEADS_DIR=" + beadsDir,
		"BEADS_DOLT_SERVER_PORT=",
		"BEADS_DOLT_PORT=",
		"BEADS_DOLT_AUTO_START=0",
	}
	sharedServerOptOut := []string{"BEADS_DOLT_SHARED_SERVER=0", toolhome.SharedServerConfigEnv + "=false"}
	// An empty BD_DOLT_SHARED_SERVER reads as unset, so the HOME config decides.
	sharedServerFromHome := []string{toolhome.SharedServerConfigEnv + "="}

	want := "Dolt server unreachable at 127.0.0.1:" + port
	withheld := runBDWithSessionEnv(t, bdPath, dir, home, append(sessionEnv, sharedServerOptOut...))
	if !strings.Contains(withheld, want) {
		t.Fatalf("bd list under the withheld session env did not dial the scope mirror's port; want %q in:\n%s", want, withheld)
	}

	unpinned := runBDWithSessionEnv(t, bdPath, dir, home, append(sessionEnv, sharedServerFromHome...))
	if strings.Contains(unpinned, ":"+port) {
		t.Fatalf("vacuous: bd list followed the scope mirror's port %s even without the shared-server opt-out:\n%s", port, unpinned)
	}
}

// runBDWithSessionEnv runs `bd list` in dir under home with exactly the given
// BEADS_*/BD_* session keys and returns its combined output; the command is
// expected to fail because nothing listens on the scope's port.
func runBDWithSessionEnv(t *testing.T, bdPath, dir, home string, session []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bdPath, "list", "--json") //nolint:gosec // resolved test binary
	cmd.Dir = dir
	cmd.Env = toolhome.Environ(append(os.Environ(), session...), home, toolhome.Explicit(session)...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bd list timed out:\n%s", out)
	}
	if err == nil {
		t.Fatalf("bd list succeeded with nothing listening on the scope's Dolt port:\n%s", out)
	}
	return string(out)
}

// closedLoopbackPort returns a loopback TCP port nothing listens on.
func closedLoopbackPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(port)
}
