package testutil

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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
	"dolt-server.log":  true, // scope-local dolt sql-server under <scope>/.beads
	"supervisor.log":   true, // gc supervisor and the city controllers it runs
	"config.yaml":      true, // the config bd generated for its child
	"dolt-config.yaml": true, // the config gc-beads-bd generated for the managed server
	"metadata.json":    true, // dolt_mode / dolt_database the scope actually got
}

// maxFailureArtifactBytes caps each copied file. A proxy child that retries
// dolt init writes its whole usage block per attempt, so server.log reaches
// hundreds of KB while only the first and last few lines carry the error;
// a longer file keeps its first quarter and its last three quarters.
const maxFailureArtifactBytes = 256 << 10

// truncatedArtifactMarker separates the head and tail of a capped copy.
const truncatedArtifactMarker = "\n... [truncated by testutil: middle of file omitted] ...\n"

// FailureReporter is the part of testing.TB that diagnostics collection reads.
type FailureReporter interface {
	Name() string
	Failed() bool
}

// SaveFailureDiagnostics copies the allowlisted diagnostics under dir into
// $GC_TEST_FAILURE_ARTIFACT_DIR/<test name>/<base of dir> when t has failed.
// Call it from a t.Cleanup registered before the one that removes dir. It is
// best-effort by design: this runs while a test is already failing, and a
// collection error must never replace the real failure.
func SaveFailureDiagnostics(t FailureReporter, dir string) {
	if !t.Failed() {
		return
	}
	SaveDiagnostics(t.Name(), dir)
}

// SaveDiagnostics copies the allowlisted diagnostics under dir into
// $GC_TEST_FAILURE_ARTIFACT_DIR/<name>/<base of dir> unconditionally. It is
// for state that outlives a single test, such as a TestMain's shared GC_HOME
// after a failed run. A no-op when the variable is unset. Collection errors
// are reported on stderr and never fail the caller.
func SaveDiagnostics(name, dir string) {
	if err := saveDiagnostics(name, dir); err != nil {
		fmt.Fprintf(os.Stderr, "testutil: saving diagnostics for %s from %s: %v\n", name, dir, err)
	}
}

// saveDiagnostics does SaveDiagnostics' work and returns every collection
// error it met. A file that vanishes mid-walk is not an error.
func saveDiagnostics(name, dir string) error {
	dest := strings.TrimSpace(os.Getenv(FailureArtifactDirEnv))
	if dest == "" {
		return nil
	}
	dest = filepath.Join(dest, sanitizeArtifactPathSegment(name), filepath.Base(dir))
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("creating artifact dir: %w", err)
	}
	errs := []error{writeFailureEnvSnapshot(dest, dir)}
	walkErr := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			if !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
			}
			return nil
		}
		if entry.IsDir() || !failureArtifactNames[entry.Name()] {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		errs = append(errs, copyBoundedFile(path, filepath.Join(dest, sanitizeArtifactPathSegment(rel))))
		return nil
	})
	errs = append(errs, walkErr)
	return errors.Join(errs...)
}

// writeFailureEnvSnapshot records the environment the test projected into the
// bd and dolt child processes. Every plausible cause of a child that dies
// before it publishes a port — no dolt on PATH, an unset or unwritable HOME,
// a TMPDIR the child cannot use — is visible here and nowhere else once the
// temp dir is gone.
func writeFailureEnvSnapshot(dest, dir string) error {
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
	return os.WriteFile(filepath.Join(dest, "env.txt"), []byte(b.String()), 0o644)
}

// copyBoundedFile copies src to dst, keeping the head and the tail of a file
// larger than maxFailureArtifactBytes: a server's startup lines and its last
// lines before it died are the ones a diagnosis needs. Both reads are bounded,
// so a log that is still being written cannot grow the copy past the cap.
func copyBoundedFile(src, dst string) (err error) {
	in, err := os.Open(src) //nolint:gosec // G304: src comes from walking the test's own temp dir
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(dst) //nolint:gosec // G304: dst is under the workflow-provided artifact dir
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
	}()
	if info.Size() <= maxFailureArtifactBytes {
		_, err = io.Copy(out, io.LimitReader(in, maxFailureArtifactBytes))
		return err
	}
	const head = maxFailureArtifactBytes / 4
	const tail = maxFailureArtifactBytes - head
	if _, err := io.CopyN(out, in, head); err != nil {
		return err
	}
	if _, err := io.WriteString(out, truncatedArtifactMarker); err != nil {
		return err
	}
	if _, err := in.Seek(-tail, io.SeekEnd); err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(in, tail))
	return err
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
