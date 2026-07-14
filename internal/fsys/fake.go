package fsys

import (
	"hash/fnv"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Fake is an in-memory [FS] for testing. It records all calls (spy) and
// simulates filesystem state (fake). Pre-populate Dirs, Files, Symlinks,
// and Errors before calling methods. ModTimes is optional unless a test needs
// exact timestamp control; Stat synthesizes and stores a mod time on demand.
// A directly seeded descendant implies its parent directories for fixture
// compatibility, while mutating operations still reject truly missing parents.
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
	// Method is the invoked filesystem method name.
	Method string
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

type fakeEntryKind uint8

const (
	fakeEntryMissing fakeEntryKind = iota
	fakeEntryDirectory
	fakeEntryFile
	fakeEntrySymlink
)

func (f *Fake) entryKind(path string) fakeEntryKind {
	if _, ok := f.Symlinks[path]; ok {
		return fakeEntrySymlink
	}
	if f.Dirs[path] {
		return fakeEntryDirectory
	}
	if _, ok := f.Files[path]; ok {
		return fakeEntryFile
	}
	if f.directoryExists(path) {
		return fakeEntryDirectory
	}
	return fakeEntryMissing
}

func (f *Fake) directoryExists(path string) bool {
	if f.Dirs[path] {
		return true
	}
	clean := filepath.Clean(path)
	if clean == "." || filepath.Dir(clean) == clean {
		return true
	}
	return f.hasDescendant(path)
}

func (f *Fake) hasDescendant(path string) bool {
	for candidate := range f.Dirs {
		if isDescendant(candidate, path) {
			return true
		}
	}
	for candidate := range f.Files {
		if isDescendant(candidate, path) {
			return true
		}
	}
	for candidate := range f.Symlinks {
		if isDescendant(candidate, path) {
			return true
		}
	}
	return false
}

func (f *Fake) materializeParentDirectories(path string) {
	var parents []string
	for parent := filepath.Dir(filepath.Clean(path)); parent != "." && filepath.Dir(parent) != parent; parent = filepath.Dir(parent) {
		parents = append(parents, parent)
	}
	if f.Dirs == nil {
		f.Dirs = make(map[string]bool)
	}
	for i := len(parents) - 1; i >= 0; i-- {
		parent := parents[i]
		if f.entryKind(parent) != fakeEntryDirectory {
			return
		}
		f.Dirs[parent] = true
	}
}

func isDescendant(candidate, parent string) bool {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(candidate))
	if err != nil || relative == "." || filepath.IsAbs(relative) {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func immediateChild(parent, candidate string) (path string, direct bool, ok bool) {
	relative, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(candidate))
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false, false
	}
	first := relative
	if separator := strings.IndexRune(relative, filepath.Separator); separator >= 0 {
		first = relative[:separator]
	}
	return filepath.Join(parent, first), first == relative, true
}

func fakePathError(operation, path string, err error) error {
	return &os.PathError{Op: operation, Path: path, Err: err}
}

func rebasedPath(candidate, oldRoot, newRoot string) (string, bool) {
	if filepath.Clean(candidate) == filepath.Clean(oldRoot) {
		return newRoot, true
	}
	if !isDescendant(candidate, oldRoot) {
		return "", false
	}
	relative, err := filepath.Rel(oldRoot, candidate)
	if err != nil {
		return "", false
	}
	return filepath.Join(newRoot, relative), true
}

func moveMapEntries[V any](entries map[string]V, oldRoot, newRoot string) {
	type move struct {
		oldPath string
		newPath string
		value   V
	}
	var moves []move
	for path, value := range entries {
		if destination, ok := rebasedPath(path, oldRoot, newRoot); ok {
			moves = append(moves, move{oldPath: path, newPath: destination, value: value})
		}
	}
	for _, item := range moves {
		delete(entries, item.oldPath)
	}
	for _, item := range moves {
		entries[item.newPath] = item.value
	}
}

func (f *Fake) clearEntry(path string) {
	delete(f.Dirs, path)
	delete(f.Files, path)
	delete(f.Symlinks, path)
	delete(f.Modes, path)
	delete(f.ModTimes, path)
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

	var missing []string
	for p := filepath.Clean(path); p != "." && filepath.Dir(p) != p; p = filepath.Dir(p) {
		switch f.entryKind(p) {
		case fakeEntryDirectory:
			continue
		case fakeEntryMissing:
			missing = append(missing, p)
		default:
			return fakePathError("mkdir", path, fs.ErrExist)
		}
	}
	for _, p := range missing {
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
	if f.entryKind(filepath.Dir(name)) != fakeEntryDirectory {
		return fakePathError("open", name, fs.ErrNotExist)
	}
	if f.entryKind(name) == fakeEntryDirectory {
		return fakePathError("open", name, fs.ErrInvalid)
	}
	_, existed := f.Files[name]
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
	if !existed {
		f.Modes[name] = perm.Perm()
	}
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
	if f.directoryExists(name) {
		return fakeFileInfo{name: filepath.Base(name), dir: true, mode: f.modeFor(name), id: fakeIdentity(name), hasID: true}, nil
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
	if f.directoryExists(name) {
		return fakeFileInfo{name: filepath.Base(name), dir: true, mode: f.modeFor(name), id: fakeIdentity(name), hasID: true}, nil
	}
	return nil, &os.PathError{Op: "lstat", Path: name, Err: os.ErrNotExist}
}

// Readlink records the call and returns the symlink target without following it.
func (f *Fake) Readlink(name string) (string, error) {
	f.Calls = append(f.Calls, Call{Method: "Readlink", Path: name})
	if err, ok := f.Errors[name]; ok {
		return "", err
	}
	if target, ok := f.Symlinks[name]; ok {
		return target, nil
	}
	return "", &os.PathError{Op: "readlink", Path: name, Err: os.ErrInvalid}
}

// Symlink records the call and creates a symlink entry.
func (f *Fake) Symlink(oldname, newname string) error {
	f.Calls = append(f.Calls, Call{Method: "Symlink", Path: newname})
	if err, ok := f.Errors[newname]; ok {
		return err
	}
	if f.Symlinks == nil {
		f.Symlinks = make(map[string]string)
	}
	f.Symlinks[newname] = oldname
	delete(f.Files, newname)
	delete(f.Dirs, newname)
	delete(f.Modes, newname)
	delete(f.ModTimes, newname)
	return nil
}

// ReadDir records the call and returns entries from direct children.
func (f *Fake) ReadDir(name string) ([]os.DirEntry, error) {
	f.Calls = append(f.Calls, Call{Method: "ReadDir", Path: name})
	if err, ok := f.Errors[name]; ok {
		return nil, err
	}

	switch f.entryKind(name) {
	case fakeEntryMissing:
		return nil, fakePathError("readdir", name, fs.ErrNotExist)
	case fakeEntryFile, fakeEntrySymlink:
		return nil, fakePathError("readdir", name, fs.ErrInvalid)
	}

	entriesByName := make(map[string]os.DirEntry)
	addImpliedDirectory := func(candidate string) {
		child, direct, ok := immediateChild(name, candidate)
		if !ok || direct {
			return
		}
		base := filepath.Base(child)
		if _, exists := entriesByName[base]; !exists {
			entriesByName[base] = fakeDirEntry{name: base, dir: true, mode: f.modeFor(child), id: fakeIdentity(child), hasID: true}
		}
	}
	for path := range f.Dirs {
		addImpliedDirectory(path)
	}
	for path := range f.Files {
		addImpliedDirectory(path)
	}
	for path := range f.Symlinks {
		addImpliedDirectory(path)
	}
	for path, data := range f.Files {
		if child, direct, ok := immediateChild(name, path); ok && direct {
			base := filepath.Base(child)
			entriesByName[base] = fakeDirEntry{name: base, size: int64(len(data)), mode: f.modeFor(path), id: fakeIdentity(path), hasID: true}
		}
	}
	for path := range f.Dirs {
		if child, direct, ok := immediateChild(name, path); ok && direct {
			base := filepath.Base(child)
			entriesByName[base] = fakeDirEntry{name: base, dir: true, mode: f.modeFor(path), id: fakeIdentity(path), hasID: true}
		}
	}
	for path := range f.Symlinks {
		if child, direct, ok := immediateChild(name, path); ok && direct {
			base := filepath.Base(child)
			entriesByName[base] = fakeDirEntry{name: base, symlink: true, id: fakeIdentity(path), hasID: true}
		}
	}

	entries := make([]os.DirEntry, 0, len(entriesByName))
	for _, entry := range entriesByName {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

// Rename records the call and moves a file, symlink, or directory tree.
func (f *Fake) Rename(oldpath, newpath string) error {
	f.Calls = append(f.Calls, Call{Method: "Rename", Path: oldpath})
	if err, ok := f.Errors[oldpath]; ok {
		return err
	}
	sourceKind := f.entryKind(oldpath)
	if sourceKind == fakeEntryMissing {
		return fakePathError("rename", oldpath, fs.ErrNotExist)
	}
	if oldpath == newpath {
		return nil
	}
	if f.entryKind(filepath.Dir(newpath)) != fakeEntryDirectory {
		return fakePathError("rename", newpath, fs.ErrNotExist)
	}
	destinationKind := f.entryKind(newpath)

	switch sourceKind {
	case fakeEntrySymlink:
		if destinationKind == fakeEntryDirectory {
			return fakePathError("rename", newpath, fs.ErrInvalid)
		}
		f.materializeParentDirectories(oldpath)
		target := f.Symlinks[oldpath]
		f.clearEntry(newpath)
		f.Symlinks[newpath] = target
		delete(f.Symlinks, oldpath)
		return nil

	case fakeEntryFile:
		if destinationKind == fakeEntryDirectory {
			return fakePathError("rename", newpath, fs.ErrInvalid)
		}
		f.materializeParentDirectories(oldpath)
		data := f.Files[oldpath]
		f.clearEntry(newpath)
		f.Files[newpath] = data
		delete(f.Files, oldpath)
		if mode, ok := f.Modes[oldpath]; ok {
			f.Modes[newpath] = mode
		} else {
			delete(f.Modes, newpath)
		}
		delete(f.Modes, oldpath)
		if modTime, ok := f.ModTimes[oldpath]; ok {
			f.ModTimes[newpath] = modTime
			delete(f.ModTimes, oldpath)
		} else {
			f.ModTimes[newpath] = f.nextModTime()
		}
		return nil

	case fakeEntryDirectory:
		if destinationKind != fakeEntryMissing || isDescendant(newpath, oldpath) {
			return fakePathError("rename", newpath, fs.ErrInvalid)
		}
		f.materializeParentDirectories(oldpath)
		moveMapEntries(f.Dirs, oldpath, newpath)
		moveMapEntries(f.Files, oldpath, newpath)
		moveMapEntries(f.Symlinks, oldpath, newpath)
		moveMapEntries(f.Modes, oldpath, newpath)
		moveMapEntries(f.ModTimes, oldpath, newpath)
		return nil
	}
	return fakePathError("rename", oldpath, fs.ErrInvalid)
}

// Remove records the call and deletes a file, symlink, or empty directory.
func (f *Fake) Remove(name string) error {
	f.Calls = append(f.Calls, Call{Method: "Remove", Path: name})
	if err, ok := f.Errors[name]; ok {
		return err
	}
	if _, ok := f.Symlinks[name]; ok {
		f.materializeParentDirectories(name)
		f.clearEntry(name)
		return nil
	}
	if _, ok := f.Files[name]; ok {
		f.materializeParentDirectories(name)
		f.clearEntry(name)
		return nil
	}
	if f.directoryExists(name) {
		if f.hasDescendant(name) {
			return fakePathError("remove", name, fs.ErrInvalid)
		}
		f.materializeParentDirectories(name)
		delete(f.Dirs, name)
		delete(f.Modes, name)
		delete(f.ModTimes, name)
		return nil
	}
	return fakePathError("remove", name, fs.ErrNotExist)
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
	if f.directoryExists(name) {
		f.materializeParentDirectories(name)
		f.Dirs[name] = true
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
	name    string
	size    int64
	mode    os.FileMode
	id      fileIdentity
	hasID   bool
	dir     bool
	symlink bool
}

func (de fakeDirEntry) Name() string { return de.name }
func (de fakeDirEntry) IsDir() bool  { return de.dir }
func (de fakeDirEntry) Type() fs.FileMode {
	if de.symlink {
		return fs.ModeSymlink
	}
	if de.dir {
		return fs.ModeDir
	}
	return 0
}

func (de fakeDirEntry) Info() (fs.FileInfo, error) {
	return fakeFileInfo{name: de.name, size: de.size, mode: de.mode, id: de.id, hasID: de.hasID, dir: de.dir, symlink: de.symlink}, nil
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
