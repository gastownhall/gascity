package fsys

import (
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Fake is an in-memory [FS] for testing. It records all calls (spy) and
// simulates filesystem state (fake). Pre-populate Dirs, Files, Symlinks,
// and Errors before calling methods. ModTimes is optional unless a test needs
// exact timestamp control; Stat synthesizes and stores a mod time on demand.
type Fake struct {
	Dirs     map[string]bool   // pre-populated directories
	Files    map[string][]byte // pre-populated files
	Modes    map[string]os.FileMode
	Symlinks map[string]string    // pre-populated symlinks (path -> target)
	Errors   map[string]error     // path → injected error (checked first)
	ModTimes map[string]time.Time // file path → synthetic mod time
	Calls    []Call               // spy log

	clock time.Time
}

// Call records a single method invocation on [Fake].
type Call struct {
	Method string // "MkdirAll", "WriteFile", "ReadFile", "ReadRegularFile", "Stat", "ReadDir", "Rename", "Remove", or "Chmod"
	Path   string // path argument
}

// NewFake returns a ready-to-use [Fake] with empty maps.
func NewFake() *Fake {
	return &Fake{
		Dirs:     make(map[string]bool),
		Files:    make(map[string][]byte),
		Modes:    make(map[string]os.FileMode),
		Symlinks: make(map[string]string),
		Errors:   make(map[string]error),
		ModTimes: make(map[string]time.Time),
		clock:    time.Unix(0, 0).UTC(),
	}
}

func (f *Fake) nextModTime() time.Time {
	if f.ModTimes == nil {
		f.ModTimes = make(map[string]time.Time)
	}
	if f.clock.IsZero() {
		f.clock = time.Unix(0, 0).UTC()
	}
	f.clock = f.clock.Add(time.Second)
	return f.clock
}

// MkdirAll records the call and adds the directory (and parents) to Dirs.
func (f *Fake) MkdirAll(path string, perm os.FileMode) error {
	f.Calls = append(f.Calls, Call{Method: "MkdirAll", Path: path})
	if err, ok := f.Errors[path]; ok {
		return err
	}
	if f.Dirs == nil {
		f.Dirs = make(map[string]bool)
	}
	if f.Modes == nil {
		f.Modes = make(map[string]os.FileMode)
	}
	// Record this directory and all parents.
	for p := filepath.Clean(path); p != "." && p != "/" && p != string(filepath.Separator); p = filepath.Dir(p) {
		if !f.Dirs[p] {
			f.Modes[p] = perm.Perm()
		}
		f.Dirs[p] = true
	}
	return nil
}

// WriteFile records the call and stores the data in Files.
func (f *Fake) WriteFile(name string, data []byte, perm os.FileMode) error {
	f.Calls = append(f.Calls, Call{Method: "WriteFile", Path: name})
	if err, ok := f.Errors[name]; ok {
		return err
	}
	modTime := f.nextModTime()
	cp := make([]byte, len(data))
	copy(cp, data)
	if f.Files == nil {
		f.Files = make(map[string][]byte)
	}
	if f.Modes == nil {
		f.Modes = make(map[string]os.FileMode)
	}
	f.Files[name] = cp
	f.Modes[name] = perm.Perm()
	f.ModTimes[name] = modTime
	return nil
}

// ReadFile records the call and returns the file contents from Files.
func (f *Fake) ReadFile(name string) ([]byte, error) {
	f.Calls = append(f.Calls, Call{Method: "ReadFile", Path: name})
	if err, ok := f.Errors[name]; ok {
		return nil, err
	}
	if data, ok := f.Files[name]; ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		return cp, nil
	}
	return nil, &os.PathError{Op: "read", Path: name, Err: os.ErrNotExist}
}

// ReadRegularFile records the call and returns file contents without following
// symlinks or accepting directories.
func (f *Fake) ReadRegularFile(name string) ([]byte, error) {
	f.Calls = append(f.Calls, Call{Method: "ReadRegularFile", Path: name})
	if err, ok := f.Errors[name]; ok {
		return nil, err
	}
	if _, ok := f.Symlinks[name]; ok {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrInvalid}
	}
	if f.Dirs[name] {
		return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrInvalid}
	}
	if data, ok := f.Files[name]; ok {
		cp := make([]byte, len(data))
		copy(cp, data)
		return cp, nil
	}
	return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrNotExist}
}

// readRegularFileSnapshot returns regular file contents plus a stable fake
// identity for the path.
func (f *Fake) readRegularFileSnapshot(name string) (regularFileSnapshot, error) {
	data, err := f.ReadRegularFile(name)
	if err != nil {
		return regularFileSnapshot{}, err
	}
	return regularFileSnapshot{data: data, id: fakeIdentity(name), hasID: true}, nil
}

// Stat records the call and returns info based on Dirs/Files maps.
// Symlinks are followed — use Lstat to detect them without following.
func (f *Fake) Stat(name string) (os.FileInfo, error) {
	f.Calls = append(f.Calls, Call{Method: "Stat", Path: name})
	if err, ok := f.Errors[name]; ok {
		return nil, err
	}
	if target, ok := f.Symlinks[name]; ok {
		if f.Dirs[target] {
			return fakeFileInfo{name: filepath.Base(name), dir: true, mode: f.modeFor(target), id: fakeIdentity(target), hasID: true}, nil
		}
		if data, ok := f.Files[target]; ok {
			modTime := f.ModTimes[target]
			if modTime.IsZero() {
				modTime = f.nextModTime()
				f.ModTimes[target] = modTime
			}
			return fakeFileInfo{name: filepath.Base(name), size: int64(len(data)), mode: f.modeFor(target), id: fakeIdentity(target), hasID: true, modTime: modTime}, nil
		}
		return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
	}
	if f.Dirs[name] {
		return fakeFileInfo{name: filepath.Base(name), dir: true, mode: f.modeFor(name), id: fakeIdentity(name), hasID: true}, nil
	}
	if data, ok := f.Files[name]; ok {
		modTime := f.ModTimes[name]
		if modTime.IsZero() {
			modTime = f.nextModTime()
			f.ModTimes[name] = modTime
		}
		return fakeFileInfo{name: filepath.Base(name), size: int64(len(data)), mode: f.modeFor(name), id: fakeIdentity(name), hasID: true, modTime: modTime}, nil
	}
	return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
}

// Lstat records the call and reports the entry itself without following
// symlinks. Tests populate Symlinks to exercise the symlink-rejection path.
func (f *Fake) Lstat(name string) (os.FileInfo, error) {
	f.Calls = append(f.Calls, Call{Method: "Lstat", Path: name})
	if err, ok := f.Errors[name]; ok {
		return nil, err
	}
	if _, ok := f.Symlinks[name]; ok {
		return fakeFileInfo{name: filepath.Base(name), symlink: true, id: fakeIdentity(name), hasID: true}, nil
	}
	if f.Dirs[name] {
		return fakeFileInfo{name: filepath.Base(name), dir: true, mode: f.modeFor(name), id: fakeIdentity(name), hasID: true}, nil
	}
	if data, ok := f.Files[name]; ok {
		return fakeFileInfo{name: filepath.Base(name), size: int64(len(data)), mode: f.modeFor(name), id: fakeIdentity(name), hasID: true}, nil
	}
	return nil, &os.PathError{Op: "lstat", Path: name, Err: os.ErrNotExist}
}

// ReadDir records the call and returns entries from direct children.
func (f *Fake) ReadDir(name string) ([]os.DirEntry, error) {
	f.Calls = append(f.Calls, Call{Method: "ReadDir", Path: name})
	if err, ok := f.Errors[name]; ok {
		return nil, err
	}

	name = filepath.Clean(name)
	seen := make(map[string]bool)
	var entries []os.DirEntry

	// Collect direct child directories.
	for d := range f.Dirs {
		if filepath.Dir(d) == name && d != name {
			base := filepath.Base(d)
			if !seen[base] {
				seen[base] = true
				entries = append(entries, fakeDirEntry{name: base, dir: true, mode: f.modeFor(d), id: fakeIdentity(d), hasID: true})
			}
		}
	}
	// Collect direct child files.
	for p, data := range f.Files {
		if filepath.Dir(p) == name {
			base := filepath.Base(p)
			if !seen[base] {
				seen[base] = true
				entries = append(entries, fakeDirEntry{name: base, size: int64(len(data)), mode: f.modeFor(p), id: fakeIdentity(p), hasID: true})
			}
		}
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	return entries, nil
}

// Rename records the call and moves the file in the Files map.
func (f *Fake) Rename(oldpath, newpath string) error {
	f.Calls = append(f.Calls, Call{Method: "Rename", Path: oldpath})
	if err, ok := f.Errors[oldpath]; ok {
		return err
	}
	if target, ok := f.Symlinks[oldpath]; ok {
		f.Symlinks[newpath] = target
		delete(f.Symlinks, oldpath)
		return nil
	}
	if data, ok := f.Files[oldpath]; ok {
		f.Files[newpath] = data
		delete(f.Files, oldpath)
		if mode, ok := f.Modes[oldpath]; ok {
			f.Modes[newpath] = mode
		} else {
			delete(f.Modes, newpath)
		}
		delete(f.Modes, oldpath)
		delete(f.Symlinks, newpath)
		if modTime, ok := f.ModTimes[oldpath]; ok {
			f.ModTimes[newpath] = modTime
			delete(f.ModTimes, oldpath)
		} else {
			f.ModTimes[newpath] = f.nextModTime()
		}
		return nil
	}
	return &os.PathError{Op: "rename", Path: oldpath, Err: os.ErrNotExist}
}

// Remove records the call and deletes the file from the Files map.
func (f *Fake) Remove(name string) error {
	f.Calls = append(f.Calls, Call{Method: "Remove", Path: name})
	if err, ok := f.Errors[name]; ok {
		return err
	}
	if _, ok := f.Symlinks[name]; ok {
		delete(f.Symlinks, name)
		return nil
	}
	if _, ok := f.Files[name]; ok {
		delete(f.Files, name)
		delete(f.Modes, name)
		delete(f.ModTimes, name)
		return nil
	}
	if f.Dirs[name] {
		delete(f.Dirs, name)
		delete(f.Modes, name)
		return nil
	}
	return &os.PathError{Op: "remove", Path: name, Err: os.ErrNotExist}
}

// Chmod records the call and updates the stored mode.
func (f *Fake) Chmod(name string, mode os.FileMode) error {
	f.Calls = append(f.Calls, Call{Method: "Chmod", Path: name})
	if err, ok := f.Errors[name]; ok {
		return err
	}
	if _, ok := f.Symlinks[name]; ok {
		return nil
	}
	if f.Modes == nil {
		f.Modes = make(map[string]os.FileMode)
	}
	if _, ok := f.Files[name]; ok {
		f.Modes[name] = mode.Perm()
		return nil
	}
	if f.Dirs[name] {
		f.Modes[name] = mode.Perm()
		return nil
	}
	return &os.PathError{Op: "chmod", Path: name, Err: os.ErrNotExist}
}

func (f *Fake) modeFor(name string) os.FileMode {
	if mode, ok := f.Modes[name]; ok {
		return mode
	}
	return 0o755
}

// --- fake os.FileInfo ---

type fakeFileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	id      fileIdentity
	hasID   bool
	dir     bool
	modTime time.Time
	symlink bool
}

func (fi fakeFileInfo) Name() string { return fi.name }
func (fi fakeFileInfo) Size() int64  { return fi.size }
func (fi fakeFileInfo) Mode() os.FileMode {
	if fi.symlink {
		return 0o777 | os.ModeSymlink
	}
	if fi.dir {
		return fi.mode | os.ModeDir
	}
	return fi.mode
}
func (fi fakeFileInfo) ModTime() time.Time { return fi.modTime }
func (fi fakeFileInfo) IsDir() bool        { return fi.dir }
func (fi fakeFileInfo) Sys() any {
	if !fi.hasID {
		return nil
	}
	return struct{ Dev, Ino uint64 }{fi.id.dev, fi.id.ino}
}

// --- fake os.DirEntry ---

type fakeDirEntry struct {
	name  string
	size  int64
	mode  os.FileMode
	id    fileIdentity
	hasID bool
	dir   bool
}

func (de fakeDirEntry) Name() string { return de.name }
func (de fakeDirEntry) IsDir() bool  { return de.dir }
func (de fakeDirEntry) Type() fs.FileMode {
	if de.dir {
		return fs.ModeDir
	}
	return 0
}

func (de fakeDirEntry) Info() (fs.FileInfo, error) {
	return fakeFileInfo{name: de.name, size: de.size, mode: de.mode, id: de.id, hasID: de.hasID, dir: de.dir}, nil
}

func fakeIdentity(name string) fileIdentity {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return fileIdentity{dev: 1, ino: h.Sum64()}
}

var (
	_ FS = (*Fake)(nil)
	_ FS = OSFS{}
)

// Ensure fakeFileInfo implements os.FileInfo at compile time.
var _ os.FileInfo = fakeFileInfo{}

// Ensure fakeDirEntry implements os.DirEntry at compile time.
var _ os.DirEntry = fakeDirEntry{}
