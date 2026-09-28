package testutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FailureArtifactDirEnv names the directory a failing test copies its Dolt and
// beads-proxy diagnostics into before its temp dir is removed.
//
// The lifecycle errors this exists for name a log file and nothing else — bd's
// proxied-server failure is "proxy child exited before publishing its
// OS-assigned port (see <root>/.beads/dolt/server.log)". The child writes its
// stderr only to that file, so without it a CI failure carries no evidence at
// all: t.Cleanup deletes the temp dir before any workflow step could collect
// it. Set this to a path outside the temp root and the workflow can upload it.
const FailureArtifactDirEnv = "GC_TEST_FAILURE_ARTIFACT_DIR"

// failureArtifactNames are the diagnostic files worth keeping, relative to any
// directory found while walking a failed test's temp root. Kept to an explicit
// allowlist so a failure never uploads bead payloads or a whole Dolt database.
var failureArtifactNames = map[string]bool{
	"server.log":       true, // bd db-proxy-child stderr + the dolt sql-server it spawns
	"proxy.log":        true, // bd proxy listener
	"dolt.log":         true, // gc-managed dolt sql-server
	"config.yaml":      true, // the config bd generated for its child
	"dolt-config.yaml": true, // the config gc-beads-bd generated for the managed server
	"metadata.json":    true, // dolt_mode / dolt_database the scope actually got
}

// maxFailureArtifactBytes caps each copied file. A proxy child that retries
// dolt init writes its whole usage block per attempt, so server.log reaches
// hundreds of KB while only the first and last few lines carry the error.
const maxFailureArtifactBytes = 256 << 10

// saveFailureDiagnostics copies the allowlisted diagnostics under dir into the
// directory named by FailureArtifactDirEnv. It is best-effort by design: this
// runs while a test is already failing, and a collection error must never
// replace the real failure.
func saveFailureDiagnostics(t *testing.T, dir string) {
	if !t.Failed() {
		return
	}
	dest := strings.TrimSpace(os.Getenv(FailureArtifactDirEnv))
	if dest == "" {
		return
	}
	dest = filepath.Join(dest, sanitizeArtifactPathSegment(t.Name()), filepath.Base(dir))
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return
	}
	writeFailureEnvSnapshot(dest, dir)

	_ = filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !failureArtifactNames[entry.Name()] {
			return nil //nolint:nilerr // a walk error must not mask the test's own failure
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return nil
		}
		copyBoundedFile(path, filepath.Join(dest, sanitizeArtifactPathSegment(rel)))
		return nil
	})
}

// writeFailureEnvSnapshot records the environment the test projected into the
// bd and dolt child processes. Every plausible cause of a child that dies
// before it publishes a port — no dolt on PATH, an unset or unwritable HOME,
// a TMPDIR the child cannot use — is visible here and nowhere else once the
// temp dir is gone.
func writeFailureEnvSnapshot(dest, dir string) {
	var b strings.Builder
	fmt.Fprintf(&b, "temp_root\t%s\n", dir)
	for _, key := range []string{"HOME", "PATH", "TMPDIR", "USER", "LOGNAME", "DOLT_ROOT_PATH", "GC_BEADS", "GC_DOLT", "GC_CITY_PATH", "GC_FAST_UNIT"} {
		value, ok := os.LookupEnv(key)
		if !ok {
			fmt.Fprintf(&b, "%s\t<unset>\n", key)
			continue
		}
		fmt.Fprintf(&b, "%s\t%s\n", key, value)
	}
	_ = os.WriteFile(filepath.Join(dest, "env.txt"), []byte(b.String()), 0o644)
}

func copyBoundedFile(src, dst string) {
	in, err := os.Open(src) //nolint:gosec // G304: src comes from walking the test's own temp dir
	if err != nil {
		return
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst) //nolint:gosec // G304: dst is under the workflow-provided artifact dir
	if err != nil {
		return
	}
	defer func() { _ = out.Close() }()
	_, _ = io.Copy(out, io.LimitReader(in, maxFailureArtifactBytes))
}

// sanitizeArtifactPathSegment flattens a relative path or test name into one
// filename-safe segment so nested scopes and subtests cannot collide or escape
// the artifact directory.
func sanitizeArtifactPathSegment(segment string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, segment)
}
