package tokenrender

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gastownhall/gascity/internal/connect"
)

// newFileMode is the mode of a token file the renderer creates.
const newFileMode os.FileMode = 0o600

// lockRetryInterval is how often a busy directory lock is retried while
// the caller's context is live.
const lockRetryInterval = 10 * time.Millisecond

// outputDir is one token-file directory a render locks, identified by its
// inode so that two spellings of one directory share one lock.
type outputDir struct {
	path    string // the first spelling seen, for errors
	dev     uint64
	ino     uint64
	f       *os.File
	err     error // opening or locking failed; the outputs fail alone
	locked  bool
	outputs []int // indexes into the outputs slice
}

// openOutputDirs opens the parent directory of every output's token file and
// groups the outputs by the directory's (device, inode), sorted by it: a
// global lock order whatever path each process spells. A directory that
// cannot be opened or stat'ed carries its error and is grouped by path.
func openOutputDirs(outputs []Output) []*outputDir {
	var dirs []*outputDir
	byID := map[[2]uint64]*outputDir{}
	byPath := map[string]*outputDir{}
	for i, o := range outputs {
		path := filepath.Dir(o.TokenFile)
		if d := byPath[path]; d != nil {
			d.outputs = append(d.outputs, i)
			continue
		}
		d := &outputDir{path: path}
		byPath[path] = d
		f, err := os.Open(path)
		if err == nil {
			var st unix.Stat_t
			if err = unix.Fstat(int(f.Fd()), &st); err == nil {
				d.dev, d.ino = uint64(st.Dev), st.Ino //nolint:unconvert // Dev is not uint64 on every unix
				id := [2]uint64{d.dev, d.ino}
				if same := byID[id]; same != nil {
					f.Close() //nolint:errcheck // read-only directory handle; the first one is kept
					same.outputs = append(same.outputs, i)
					byPath[path] = same
					continue
				}
				byID[id] = d
				d.f = f
			} else {
				f.Close() //nolint:errcheck // read-only directory handle
			}
		}
		if err != nil {
			d.err = fmt.Errorf("locking token-file directory %s: %w", path, err)
		}
		d.outputs = append(d.outputs, i)
		dirs = append(dirs, d)
	}
	slices.SortFunc(dirs, func(a, b *outputDir) int {
		if c := cmp.Compare(a.dev, b.dev); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ino, b.ino); c != 0 {
			return c
		}
		return strings.Compare(a.path, b.path)
	})
	return dirs
}

// lock takes d's exclusive flock, retrying a busy lock until ctx ends. onBusy,
// when set, runs each time the lock is found busy. A context error is
// returned wrapped; any other failure is recorded on d.
func (d *outputDir) lock(ctx context.Context, onBusy func()) error {
	if d.err != nil {
		return nil
	}
	var timer *time.Timer
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("locking token-file directory %s: %w", d.path, err)
		}
		err := unix.Flock(int(d.f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			d.locked = true
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			d.err = fmt.Errorf("locking token-file directory %s: %w", d.path, err)
			return nil
		}
		if onBusy != nil {
			onBusy()
		}
		if timer == nil {
			timer = time.NewTimer(lockRetryInterval)
			defer timer.Stop()
		} else {
			timer.Reset(lockRetryInterval)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("locking token-file directory %s: %w", d.path, ctx.Err())
		case <-timer.C:
		}
	}
}

// closeOutputDirs releases every lock and closes every handle.
func closeOutputDirs(dirs []*outputDir) {
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		if d.f == nil {
			continue
		}
		if d.locked {
			unix.Flock(int(d.f.Fd()), unix.LOCK_UN) //nolint:errcheck // closing releases it anyway
			d.locked = false
		}
		d.f.Close() //nolint:errcheck // read-only directory handle
		d.f = nil
	}
}

// lockOutputDirs locks every output's directory in (device, inode) order. A
// directory that cannot be opened or locked carries its error and the others
// are still locked; only the context ending fails the call, with every lock
// released.
func lockOutputDirs(ctx context.Context, outputs []Output, onBusy func()) ([]*outputDir, error) {
	dirs := openOutputDirs(outputs)
	for _, d := range dirs {
		if err := d.lock(ctx, onBusy); err != nil {
			closeOutputDirs(dirs)
			return nil, err
		}
	}
	return dirs, nil
}

// LockOutputs takes an exclusive flock on the parent directory of every
// output's token file, each directory once (by device and inode, however
// its path is spelled) and in (device, inode) order, so renderers in this
// process and in others serialize without creating a lock file. A busy lock
// is retried until ctx ends; then the call returns ctx's error, wrapped. Any
// directory that cannot be locked fails the whole call, with nothing held.
// The returned unlock releases every lock; it is safe to call more than once.
func LockOutputs(ctx context.Context, outputs []Output) (unlock func(), err error) {
	dirs, err := lockOutputDirs(ctx, outputs, nil)
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		if d.err != nil {
			closeOutputDirs(dirs)
			return nil, d.err
		}
	}
	var once sync.Once
	return func() { once.Do(func() { closeOutputDirs(dirs) }) }, nil
}

// WriteTokenFile replaces the token file at path with content, following
// DES-CN D5's write rules. Call it with LockOutputs held. Equal bytes write
// nothing and leave the mtime alone. Otherwise the content goes to a temp
// file in the same directory with the current file's mode and group (0600
// and the caller's group when new), is fsynced, gets an mtime strictly after
// the previous one (so bd serve's (mtime, size) reload check sees a
// same-size rotation), is renamed over the path, and the directory is
// fsynced. Empty, unterminated and oversized content is refused, and so is a
// path that exists but is not a regular file (a symlink, a directory or a
// FIFO). It reports whether the file changed.
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
	gid := -1
	if info != nil {
		if bytes.Equal(current, content) {
			return false, nil
		}
		mode = info.Mode().Perm()
		prev = info.ModTime()
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			gid = int(st.Gid)
		}
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
	if err := keepGroup(tmp, gid); err != nil {
		return false, fmt.Errorf("writing token file %s: keeping group %d: %w", path, gid, err)
	}
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

// keepGroup gives f the group gid when it has another; gid -1 keeps f's. A
// failure is returned, never ignored: a replacement in another group could
// silently lose a reader that reads through its group.
func keepGroup(f *os.File, gid int) error {
	if gid < 0 {
		return nil
	}
	info, err := f.Stat()
	if err != nil {
		f.Close() //nolint:errcheck // the stat error wins
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Gid) == gid {
		return nil
	}
	if err := f.Chown(-1, gid); err != nil {
		f.Close() //nolint:errcheck // the chown error wins
		return err
	}
	return nil
}

// readTokenFile returns the current bytes and file info, or (nil, nil, nil)
// when the path does not exist. A path that is not a regular file (a
// symlink, a directory, a FIFO) is an error. At most one byte beyond the cap
// is read: such content never equals a render.
func readTokenFile(path string) ([]byte, os.FileInfo, error) {
	data, info, err := readRegular(path, connect.MaxTokenFileBytes, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading token file %s: %w", path, err)
	}
	return data, info, nil
}

// errNotRegular is wrapped by readRegular's refusal of anything but a
// regular file.
var errNotRegular = errors.New("not a regular file")

// readRegular opens path without blocking (a FIFO never stalls the caller),
// checks on the open descriptor that it is a regular file, and reads at most
// limit+1 bytes of it. followSymlinks false refuses a symlink at path.
func readRegular(path string, limit int64, followSymlinks bool) ([]byte, os.FileInfo, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK
	if !followSymlinks {
		flags |= syscall.O_NOFOLLOW
	}
	f, err := os.OpenFile(path, flags, 0)
	if err != nil {
		if !followSymlinks && errors.Is(err, syscall.ELOOP) {
			return nil, nil, fmt.Errorf("%s is a symlink: %w", path, errNotRegular)
		}
		return nil, nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	info, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s (%s): %w", path, info.Mode().Type(), errNotRegular)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	return data, info, nil
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
