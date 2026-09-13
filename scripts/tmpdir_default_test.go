package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wantTestTMPDirDefault is the fallback TMPDIR the test-running wrappers
// (Makefile TEST_ENV, and the shard scripts below) must use when the calling
// shell has not already set TMPDIR itself. It must stay off the shared,
// size-capped /tmp tmpfs (see AGENTS.md "Build Cache Conventions") and it
// must stay short: internal/testutil.ShortTempDir roots test-owned socket
// directories at os.TempDir() (== $TMPDIR on Linux), and Unix socket paths
// built under it must stay under the sun_path limit (104 bytes on macOS, 108
// on Linux; see internal/runtime/acp and internal/runtime/subprocess).
const wantTestTMPDirDefault = "/var/tmp"

// TestMakefileTestEnvDefaultsTMPDirOffSharedTmpTmpfs guards ga-ntbpyb.4: make
// test-fast-parallel (and every other $(TEST_ENV)-wrapped target) must not
// fall back to the shared /tmp tmpfs when the caller leaves TMPDIR unset.
func TestMakefileTestEnvDefaultsTMPDirOffSharedTmpTmpfs(t *testing.T) {
	got := runMakefileTestEnvTMPDirPrintTarget(t, nil)
	if got == "/tmp" || strings.HasPrefix(got, "/tmp/") {
		t.Fatalf("TEST_ENV TMPDIR = %q, still rooted under the shared /tmp tmpfs", got)
	}
	if got != wantTestTMPDirDefault {
		t.Fatalf("TEST_ENV TMPDIR = %q, want %q", got, wantTestTMPDirDefault)
	}
}

// TestMakefileTestEnvRespectsCallerSuppliedTMPDir guards the other half of
// the same fallback expression: a caller (CI, a developer's shell, a deploy
// gate) that already exports TMPDIR to somewhere sane must still have that
// value win, not get silently overridden by the new default.
func TestMakefileTestEnvRespectsCallerSuppliedTMPDir(t *testing.T) {
	custom := filepath.Join(t.TempDir(), "caller-tmpdir")
	if err := os.MkdirAll(custom, 0o755); err != nil {
		t.Fatalf("mkdir custom TMPDIR: %v", err)
	}
	got := runMakefileTestEnvTMPDirPrintTarget(t, []string{"TMPDIR=" + custom})
	if got != custom {
		t.Fatalf("TEST_ENV TMPDIR = %q, want caller-supplied %q", got, custom)
	}
}

// TestMakefileTestEnvTMPDirDefaultLeavesSocketPathHeadroom proves the actual
// resolved default (not just an assumed literal) leaves enough room for a
// realistic Unix socket path. Mirrors the "socks/<hashed-key>.sock" shape
// built by internal/runtime/subprocess.Provider.sockPath and
// internal/runtime/acp.Provider.sockPath: a short prefix directory (per
// internal/testutil.ShortTempDir) holding a "socks" dir and a 9-byte hashed
// key ("s" + 8 hex chars) plus ".sock".
func TestMakefileTestEnvTMPDirDefaultLeavesSocketPathHeadroom(t *testing.T) {
	root := runMakefileTestEnvTMPDirPrintTarget(t, nil)
	shortDir := filepath.Join(root, "gc-t-123456789")
	sockPath := filepath.Join(shortDir, "socks", "s01234567.sock")
	const sunPathLimit = 104 // stricter of macOS(104)/Linux(108) sun_path limits
	const wantHeadroom = 20  // arbitrary but generous safety margin in bytes
	if margin := sunPathLimit - len(sockPath); margin < wantHeadroom {
		t.Fatalf("socket path %q (%d bytes) leaves only %d bytes of headroom under the sun_path limit %d; want >= %d",
			sockPath, len(sockPath), margin, sunPathLimit, wantHeadroom)
	}
}

func runMakefileTestEnvTMPDirPrintTarget(t *testing.T, extraEnv []string) string {
	t.Helper()
	repoRoot := repoRoot(t)
	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	tmp := t.TempDir()
	testMakefile := filepath.Join(tmp, "Makefile")
	content := string(makefile) + `
.PHONY: print-test-env-tmpdir
print-test-env-tmpdir:
	@$(TEST_ENV) sh -c 'echo TMPDIR=$$TMPDIR'
`
	if err := os.WriteFile(testMakefile, []byte(content), 0o644); err != nil {
		t.Fatalf("write test Makefile: %v", err)
	}

	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"USER=" + os.Getenv("USER"),
		"SHELL=/bin/sh",
	}
	env = append(env, extraEnv...)

	cmd := makeCommand("--no-print-directory", "-f", testMakefile, "print-test-env-tmpdir")
	cmd.Dir = repoRoot
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make print-test-env-tmpdir failed: %v\n%s", err, out)
	}
	line := strings.TrimSpace(string(out))
	const prefix = "TMPDIR="
	if !strings.HasPrefix(line, prefix) {
		t.Fatalf("unexpected output from print-test-env-tmpdir: %q", line)
	}
	return strings.TrimPrefix(line, prefix)
}

// shardScriptTMPDirDefaults documents every sharded/parallel test-runner
// script that constructs its own env -i wrapper around go test (mirroring
// the Makefile's TEST_ENV) and must therefore apply the same off-tmpfs
// TMPDIR default. Each count is the exact number of "${TMPDIR:-...}"
// fallback sites in that file today; a changed count means a site was added
// or removed and this ledger must be updated deliberately, not silently.
var shardScriptTMPDirDefaults = map[string]int{
	"scripts/test-local-parallel":    2, // log_dir mktemp + per-job env
	"scripts/go-test-observable":     1, // per-run log file mktemp
	"scripts/test-go-test-shard":     1, // per-shard env
	"scripts/test-integration-shard": 1, // per-shard env
}

// TestShardScriptsDefaultTMPDirOffSharedTmpTmpfs is the sibling-targets half
// of ga-ntbpyb.4: test-cmd-gc-process-parallel, test-integration-shards-parallel,
// and test-local-full-parallel all fan out through these scripts directly
// (not through the Makefile's TEST_ENV), so each script's own TMPDIR fallback
// must independently stay off /tmp.
func TestShardScriptsDefaultTMPDirOffSharedTmpTmpfs(t *testing.T) {
	repoRoot := repoRoot(t)
	oldPattern := "${TMPDIR:-/tmp}"
	newPattern := "${TMPDIR:-" + wantTestTMPDirDefault + "}"
	for relPath, wantCount := range shardScriptTMPDirDefaults {
		t.Run(relPath, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(repoRoot, relPath))
			if err != nil {
				t.Fatalf("read %s: %v", relPath, err)
			}
			content := string(data)
			if strings.Contains(content, oldPattern) {
				t.Fatalf("%s still falls back to the shared /tmp tmpfs via %q", relPath, oldPattern)
			}
			if got := strings.Count(content, newPattern); got != wantCount {
				t.Fatalf("%s has %d occurrences of %q, want %d", relPath, got, newPattern, wantCount)
			}
		})
	}
}
