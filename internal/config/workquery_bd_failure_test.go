package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Regression coverage for a silent standdown: a schema-v59 store with a v53
// bd on PATH made every bare-bd tier fail with stderr suppressed, so each generated hook-check script fell through all its
// tiers and answered `[]` — indistinguishable from a genuinely empty hook.
// Agents with assigned work quietly stood down across the city.
//
// These tests EXECUTE the generated shell against a fake broken `bd` on PATH
// and pin the loud-failure contract: an empty hook must be CERTIFIED by a
// working bd. When bd itself cannot serve a read, the script exits non-zero
// and surfaces bd's own stderr instead of reporting an empty hook.

const brokenBdStderr = `schema version mismatch: database is at v59, binary knows up to v53 (6 migrations ahead)`

// brokenBdScript simulates that skew: every subcommand fails with the
// mismatch error on stderr and a non-zero exit, exactly like a stale bd
// against a migrated store.
const brokenBdScript = `#!/bin/sh
echo "` + brokenBdStderr + `" >&2
exit 1
`

// healthyEmptyBdScript answers every read with an empty result, like a
// working bd over a city with no work on this hook.
const healthyEmptyBdScript = `#!/bin/sh
printf '[]'
`

// runQueryWithBd executes a generated query command with the given fake bd on
// PATH and reports the streams and exit status. Unlike runShellWithFakeBd it
// tolerates a non-zero exit, because non-zero IS the behavior under test.
func runQueryWithBd(t *testing.T, command string, env map[string]string, bdScript string) (stdout, stderr string, exit int) {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not available; the work-query shell requires it")
	}
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "bd"), []byte(bdScript), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	commandEnv := []string{"PATH=" + tmp + ":" + os.Getenv("PATH")}
	for k, v := range env {
		commandEnv = append(commandEnv, k+"="+v)
	}
	return runShellCommandCapture(t, command, commandEnv)
}

// agentQueryCommands enumerates every default hook-check query whose terminal
// empty answer must be certified by a working bd.
func agentQueryCommands(t *testing.T) map[string]string {
	t.Helper()
	a := Agent{Name: "worker"}
	return map[string]string{
		"Work":               a.EffectiveWorkQuery(),
		"AssignedInProgress": a.EffectiveAssignedInProgressQuery(),
		"AssignedReady":      a.EffectiveAssignedReadyQuery(),
		"RoutedPool":         a.EffectiveRoutedPoolQuery(),
	}
}

// TestHookQueriesFailLoudWhenBdIsBroken is the primary regression: with a
// broken bd, no default hook query may answer "[]"; each must exit non-zero
// and surface bd's stderr.
func TestHookQueriesFailLoudWhenBdIsBroken(t *testing.T) {
	for name, command := range agentQueryCommands(t) {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, exit := runQueryWithBd(t, command,
				map[string]string{"GC_SESSION_ID": "sess-1"}, brokenBdScript)
			if exit == 0 {
				t.Fatalf("query exited 0 with a broken bd (stdout %q); a broken bd must not certify an empty hook", stdout)
			}
			if strings.TrimSpace(stdout) == "[]" {
				t.Fatalf("query reported an empty hook with a broken bd; that is the silent standdown")
			}
			if !strings.Contains(stderr, brokenBdStderr) {
				t.Fatalf("bd's own error was swallowed; stderr = %q, want it to contain %q", stderr, brokenBdStderr)
			}
		})
	}
}

// TestHookQueriesStayQuietWhenBdIsHealthy pins the other half of the
// contract: a working bd with no work still answers a clean "[]" with exit 0,
// so the loud path fires only on real breakage.
func TestHookQueriesStayQuietWhenBdIsHealthy(t *testing.T) {
	for name, command := range agentQueryCommands(t) {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, exit := runQueryWithBd(t, command,
				map[string]string{"GC_SESSION_ID": "sess-1"}, healthyEmptyBdScript)
			if exit != 0 {
				t.Fatalf("healthy empty hook exited %d (stderr %q), want 0", exit, stderr)
			}
			if strings.TrimSpace(stdout) != "[]" {
				t.Fatalf("healthy empty hook stdout = %q, want []", stdout)
			}
		})
	}
}
