package tokenrender

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/connect"
)

// newFileMode is the mode of a token file the renderer creates.
const newFileMode os.FileMode = 0o600

// LockOutputs takes an exclusive flock on the parent directory of every
// output's token file, each directory once and in path order, so renderers
// in this process and in others serialize without creating a lock file. The
// returned unlock releases every lock; it is safe to call more than once.
// The call blocks until every lock is held.
func LockOutputs(outputs []Output) (unlock func(), err error) {
	dirs := make([]string, 0, len(outputs))
	for _, o := range outputs {
		dirs = append(dirs, filepath.Dir(o.TokenFile))
	}
	slices.Sort(dirs)
	dirs = slices.Compact(dirs)

	held := make([]*os.File, 0, len(dirs))
	release := func() {
		for i := len(held) - 1; i >= 0; i-- {
			syscall.Flock(int(held[i].Fd()), syscall.LOCK_UN) //nolint:errcheck // closing releases it anyway
			held[i].Close()                                   //nolint:errcheck // read-only directory handle
		}
		held = nil
	}
	for _, dir := range dirs {
		f, err := os.Open(dir)
		if err != nil {
			release()
			return nil, fmt.Errorf("locking token-file directory %s: %w", dir, err)
		}
		if err := flockExclusive(f); err != nil {
			f.Close() //nolint:errcheck // read-only directory handle
			release()
			return nil, fmt.Errorf("locking token-file directory %s: %w", dir, err)
		}
		held = append(held, f)
	}
	var once sync.Once
	return func() { once.Do(release) }, nil
}

func flockExclusive(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// WriteTokenFile replaces the token file at path with content, following
// DES-CN D5's write rules. Call it with LockOutputs held. Equal bytes write
// nothing and leave the mtime alone. Otherwise the content goes to a temp
// file in the same directory with the current file's mode (0600 when new),
// is fsynced, gets an mtime strictly after the previous one (so bd serve's
// (mtime, size) reload check sees a same-size rotation), is renamed over the
// path, and the directory is fsynced. Empty, unterminated and oversized
// content is refused, and so is a path that exists but is not a regular file
// (a symlink or a directory). It reports whether the file changed.
func WriteTokenFile(path string, content []byte) (changed bool, err error) {
	return writeTokenFile(path, content, time.Now)
}

func writeTokenFile(path string, content []byte, now func() time.Time) (bool, error) {
	switch {
	case len(content) == 0:
		return false, fmt.Errorf("writing token file %s: content is empty", path)
	case content[len(content)-1] != '\n':
		return false, fmt.Errorf("writing token file %s: content is not newline-terminated", path)
	case len(content) > connect.MaxTokenFileBytes:
		return false, fmt.Errorf("writing token file %s: %w", path, connect.ErrTokenFileTooLarge)
	}
	current, info, err := readTokenFile(path)
	if err != nil {
		return false, err
	}
	mode := newFileMode
	var prev time.Time
	if info != nil {
		if bytes.Equal(current, content) {
			return false, nil
		}
		mode = info.Mode().Perm()
		prev = info.ModTime()
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("writing token file %s: %w", path, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			os.Remove(tmpName) //nolint:errcheck // best-effort cleanup of our own temp file
		}
	}()
	if err := writeAndSync(tmp, content, mode); err != nil {
		return false, fmt.Errorf("writing token file %s: %w", path, err)
	}
	if err := advanceMtime(tmpName, prev, now()); err != nil {
		return false, fmt.Errorf("writing token file %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("writing token file %s: %w", path, err)
	}
	committed = true
	if err := syncDir(dir); err != nil {
		return true, fmt.Errorf("writing token file %s: syncing directory: %w", path, err)
	}
	return true, nil
}

// readTokenFile returns the current bytes and file info, or (nil, nil, nil)
// when the path does not exist. A non-regular file is an error. At most one
// byte beyond the cap is read: such content never equals a render.
func readTokenFile(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading token file %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("token file %s is not a regular file (%s)", path, info.Mode().Type())
	}
	data, err := readCapped(path, connect.MaxTokenFileBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("reading token file %s: %w", path, err)
	}
	return data, info, nil
}

// readCapped reads at most limit+1 bytes of path.
func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	return io.ReadAll(io.LimitReader(f, limit+1))
}

func writeAndSync(f *os.File, content []byte, mode os.FileMode) error {
	if err := f.Chmod(mode); err != nil {
		f.Close() //nolint:errcheck // the chmod error wins
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close() //nolint:errcheck // the write error wins
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close() //nolint:errcheck // the sync error wins
		return err
	}
	return f.Close()
}

// advanceMtime sets name's mtime to max(now, prev + 1µs). If the filesystem
// stores it no later than prev (a coarse timestamp granule), it moves on to
// the next whole second after prev.
func advanceMtime(name string, prev, now time.Time) error {
	mtime := now
	if !prev.IsZero() && !mtime.After(prev) {
		mtime = prev.Add(time.Microsecond)
	}
	if err := os.Chtimes(name, mtime, mtime); err != nil {
		return err
	}
	if prev.IsZero() {
		return nil
	}
	st, err := os.Stat(name)
	if err != nil {
		return err
	}
	if st.ModTime().After(prev) {
		return nil
	}
	mtime = prev.Truncate(time.Second).Add(time.Second)
	return os.Chtimes(name, mtime, mtime)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close() //nolint:errcheck // the sync error wins
		return err
	}
	return d.Close()
}
