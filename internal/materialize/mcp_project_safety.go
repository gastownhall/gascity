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
	path, err := symlinkedPath(fs, p)
	if err != nil {
		return err
	}
	if path != "" {
		return symlinkError(path)
	}
	return nil
}

func symlinkError(path string) error {
	return fmt.Errorf(
		"refusing to project MCP through symlinked path %s: "+
			"managed targets must be regular files or directories",
		path,
	)
}

// symlinkedPath returns the first path ensureNotSymlink would reject: the
// target, or the provider directory holding it. It returns "" when neither
// is a symlink.
func symlinkedPath(fs fsys.FS, p MCPProjection) (string, error) {
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
			return "", fmt.Errorf("inspecting %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return path, nil
		}
	}
	return "", nil
}

// snapshotAdoption copies data, the existing provider-native MCP content, to
// an adoption backup before the first managed write. Gas City promises
// non-destructive adoption: once we've taken ownership of a target we will
// clobber it on every reconcile, but on the very first adoption the user's
// pre-existing content is preserved under .gc/mcp-adopted/<provider>/ and a
// one-time warning is emitted so operators can recover or diff.
//
// Apply calls it only for an existing target it does not manage yet, under
// the target lock. Errors writing the backup are fatal: silent destructive
// adoption is exactly the failure mode this function exists to prevent.
func snapshotAdoption(fs fsys.FS, p MCPProjection, data []byte, stderr io.Writer) error {
	timestamp := time.Now().UTC().Format("20060102T150405Z")
	backupDir := p.adoptionSnapshotDir()
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

// adoptionSnapshotDir is where Apply saves a target's content before it
// adopts the target: .gc/mcp-adopted/<provider>/ under the projection root.
func (p MCPProjection) adoptionSnapshotDir() string {
	return filepath.Join(p.Root, ".gc", "mcp-adopted", p.Provider)
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
