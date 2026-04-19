package materialize

import (
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

// ensureNotSymlink rejects paths that resolve through a symlink, including any
// ancestor directory inside the managed provider subtree. Refusing to follow
// the link is the security contract: Apply must never write to or read
// through an attacker-controlled path.
//
// For Claude the managed file sits directly under the workdir, so only the
// target itself is checked. For Gemini/Codex the provider directory
// (.gemini/ or .codex/) is also checked because the preserve-unrelated path
// reads from and writes into that directory.
func ensureNotSymlink(fs fsys.FS, p MCPProjection) error {
	toCheck := []string{p.Target}
	if dir := filepath.Dir(p.Target); dir != "" && dir != p.Root && dir != "." {
		toCheck = append(toCheck, dir)
	}
	for _, path := range toCheck {
		info, err := fs.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) || errors.Is(err, iofs.ErrNotExist) {
				continue
			}
			return fmt.Errorf("inspecting %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf(
				"refusing to project MCP through symlinked path %s: "+
					"managed targets must be regular files or directories",
				path,
			)
		}
	}
	return nil
}

// snapshotExistingIfUnmanaged copies the existing provider-native MCP content
// to an adoption backup before the first managed write. Gas City promises
// non-destructive adoption: once we've taken ownership of a target we will
// clobber it on every reconcile, but on the very first adoption the user's
// pre-existing content is preserved under .gc/mcp-adopted/<provider>/ and a
// one-time warning is emitted so operators can recover or diff.
//
// The snapshot is a no-op when the projection is already managed (marker
// exists) or the target does not exist yet. Errors reading/writing the
// backup are fatal: silent destructive adoption is exactly the failure
// mode this function exists to prevent.
func snapshotExistingIfUnmanaged(fs fsys.FS, p MCPProjection, now func() time.Time, stderr io.Writer) error {
	if p.isManaged(fs) {
		return nil
	}
	data, err := fs.ReadFile(p.Target)
	if err != nil {
		if errorsIsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s for adoption snapshot: %w", p.Target, err)
	}
	if now == nil {
		now = time.Now
	}
	timestamp := now().UTC().Format("20060102T150405Z")
	backupDir := filepath.Join(p.Root, ".gc", "mcp-adopted", p.Provider)
	ext := filepath.Ext(p.Target)
	if ext == "" {
		ext = ".bak"
	}
	backup := filepath.Join(backupDir, timestamp+ext)
	if err := fs.MkdirAll(backupDir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", backupDir, err)
	}
	if err := fsys.WriteFileAtomic(fs, backup, data, 0o600); err != nil {
		return fmt.Errorf("writing adoption snapshot %s: %w", backup, err)
	}
	if stderr != nil {
		_, _ = fmt.Fprintf(stderr,
			"gc: adopting provider-native MCP at %s; existing content snapshotted to %s\n",
			p.Target, backup,
		)
	}
	return nil
}

// adoptionStderr returns the stderr sink that Apply should use for adoption
// warnings. Tests can override via a build-time hook; default is os.Stderr.
var adoptionStderr io.Writer = os.Stderr

// SetAdoptionStderr overrides the destination for one-time adoption warnings.
// Tests use this to capture the stderr emission deterministically.
func SetAdoptionStderr(w io.Writer) (restore func()) {
	prev := adoptionStderr
	adoptionStderr = w
	return func() { adoptionStderr = prev }
}
