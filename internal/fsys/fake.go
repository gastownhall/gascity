package fsys

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Fake is an in-memory [FS] for testing. It records all calls (spy) and
// simulates filesystem state (fake). Pre-populate Dirs, Files, and Errors
// before calling methods. ModTimes is optional unless a test needs exact
// timestamp control; Stat synthesizes and stores a mod time on demand.
type Fake struct {
	Dirs     map[string]bool      // pre-populated directories
	Files    map[string][]byte    // pre-populated files
	Errors   map[string]error     // path → injected error (checked first)
	ModTimes map[string]time.Time // file path → synthetic mod time
	Calls    []Call               // spy log

	clock time.Time
}

// Call records a single method invocation on [Fake].
type Call struct {
	Method string // "MkdirAll", "WriteFile", "ReadFile", "Stat", "ReadDir", "Rename", "Remove", or "Chmod"
	Path   string // path argument
}

// NewFake returns a ready-to-use [Fake] with empty maps.
func NewFake() *Fake {
	return &Fake{
		Dirs:     make(map[string]bool),
		Files:    make(map[string][]byte),
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
func (f *Fake) MkdirAll(path string, _ os.FileMode) error {
	f.Calls = append(f.Calls, Call{Method: "MkdirAll", Path: path})
	if err, ok := f.Errors[path]; ok {
		return err
	}
	// Record this directory and all parents.
	for p := filepath.Clean(path); p != "." && p != "/" && p != string(filepath.Separator); p = filepath.Dir(p) {
		f.Dirs[p] = true
	}
	return nil
}

// WriteFile records the call and stores the data in Files.
func (f *Fake) WriteFile(name string, data []byte, _ os.FileMode) error {
	f.Calls = append(f.Calls, Call{Method: "WriteFile", Path: name})
	if err, ok := f.Errors[name]; ok {
		return err
	}
	if f.Files == nil {
		f.Files = make(map[string][]byte)
	}
	modTime := f.nextModTime()
	cp := make([]byte, len(data))
	copy(cp, data)
	f.Files[name] = cp
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

// Stat records the call and returns info based on Dirs/Files maps.
func (f *Fake) Stat(name string) (os.FileInfo, error) {
	f.Calls = append(f.Calls, Call{Method: "Stat", Path: name})
	if err, ok := f.Errors[name]; ok {
		return nil, err
	}
	if f.Dirs[name] {
		return fakeFileInfo{name: filepath.Base(name), dir: true}, nil
	}
	if data, ok := f.Files[name]; ok {
		modTime := f.ModTimes[name]
		if modTime.IsZero() {
			modTime = f.nextModTime()
			f.ModTimes[name] = modTime
		}
		return fakeFileInfo{name: filepath.Base(name), size: int64(len(data)), modTime: modTime}, nil
	}
	return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
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
				entries = append(entries, fakeDirEntry{name: base, dir: true})
			}
		}
	}
	// Collect direct child files.
	for p, data := range f.Files {
		if filepath.Dir(p) == name {
			base := filepath.Base(p)
			if !seen[base] {
				seen[base] = true
				entries = append(entries, fakeDirEntry{name: base, size: int64(len(data))})
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
	if data, ok := f.Files[oldpath]; ok {
		f.Files[newpath] = data
		delete(f.Files, oldpath)
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
	if _, ok := f.Files[name]; ok {
		delete(f.Files, name)
		delete(f.ModTimes, name)
		return nil
	}
	if f.Dirs[name] {
		delete(f.Dirs, name)
		return nil
	}
	return &os.PathError{Op: "remove", Path: name, Err: os.ErrNotExist}
}

// Chmod records the call. Mode is not tracked — the spy log is sufficient
// for tests that care about which paths were chmodded.
func (f *Fake) Chmod(name string, _ os.FileMode) error {
	f.Calls = append(f.Calls, Call{Method: "Chmod", Path: name})
	if err, ok := f.Errors[name]; ok {
		return err
	}
	if _, ok := f.Files[name]; ok {
		return nil
	}
	if f.Dirs[name] {
		return nil
	}
	return &os.PathError{Op: "chmod", Path: name, Err: os.ErrNotExist}
}

// --- fake os.FileInfo ---

type fakeFileInfo struct {
	name    string
	size    int64
	dir     bool
	modTime time.Time
}

func (fi fakeFileInfo) Name() string       { return fi.name }
func (fi fakeFileInfo) Size() int64        { return fi.size }
func (fi fakeFileInfo) Mode() os.FileMode  { return 0o755 }
func (fi fakeFileInfo) ModTime() time.Time { return fi.modTime }
func (fi fakeFileInfo) IsDir() bool        { return fi.dir }
func (fi fakeFileInfo) Sys() any           { return nil }

// --- fake os.DirEntry ---

type fakeDirEntry struct {
	name string
	size int64
	dir  bool
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
	return fakeFileInfo{name: de.name, size: de.size, dir: de.dir}, nil
}

var (
	_ FS = (*Fake)(nil)
	_ FS = OSFS{}
)

// Ensure fakeFileInfo implements os.FileInfo at compile time.
var _ os.FileInfo = fakeFileInfo{}

// Ensure fakeDirEntry implements os.DirEntry at compile time.
var _ os.DirEntry = fakeDirEntry{}
