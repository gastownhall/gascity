package fsys

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// WriteFileAtomic writes data to path atomically using a temp file + rename.
// The temp file is created in the same directory as path to ensure the rename
// is on the same filesystem (required for atomic rename on POSIX). Permissions
// are enforced on the temp file before the rename so the final path is never
// visible with a wider mode (no write-then-chmod window).
func WriteFileAtomic(fs FS, path string, data []byte, perm os.FileMode) error {
	suffix := strconv.Itoa(os.Getpid()) + "." + strconv.FormatInt(time.Now().UnixNano(), 36)
	tmp := path + ".tmp." + suffix
	if err := fs.WriteFile(tmp, data, perm); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}
	// Chmod before rename so the final path never exists with a wider mode
	// even briefly. umask can relax `perm` on the initial WriteFile; an
	// explicit Chmod normalises it.
	if err := fs.Chmod(tmp, perm); err != nil {
		_ = fs.Remove(tmp)
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := fs.Rename(tmp, path); err != nil {
		_ = fs.Remove(tmp)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	sweepDeadAtomicOrphans(fs, path)
	return nil
}

// sweepDeadAtomicOrphans removes sibling temp files left behind by previous
// WriteFileAtomic callers that died (e.g., SIGTERM) between WriteFile and
// Rename. It is best-effort: any error during enumeration or removal is
// ignored so a stale-temp cleanup never fails an otherwise successful write.
//
// Only siblings of `target` matching the WriteFileAtomic suffix scheme
// (`<basename>.tmp.<pid>.<unixnano-base36>`) are considered. PIDs that are
// still alive — including in-progress writers from concurrent calls — are
// preserved.
func sweepDeadAtomicOrphans(fs FS, target string) {
	dir := filepath.Dir(target)
	prefix := filepath.Base(target) + ".tmp."
	entries, err := fs.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		pid, ok := parseAtomicTempPID(name[len(prefix):])
		if !ok {
			continue
		}
		if pidutil.Alive(pid) {
			continue
		}
		_ = fs.Remove(filepath.Join(dir, name))
	}
}

// parseAtomicTempPID parses the `<pid>.<unixnano-base36>` suffix produced by
// WriteFileAtomic and returns the PID. Returns ok=false when the input does
// not match the scheme (e.g., no dot, non-numeric PID).
func parseAtomicTempPID(suffix string) (int, bool) {
	dot := strings.IndexByte(suffix, '.')
	if dot <= 0 || dot == len(suffix)-1 {
		return 0, false
	}
	pid, err := strconv.Atoi(suffix[:dot])
	if err != nil || pid <= 0 {
		return 0, false
	}
	if _, err := strconv.ParseInt(suffix[dot+1:], 36, 64); err != nil {
		return 0, false
	}
	return pid, true
}

// WriteFileIfChangedAtomic writes data to path atomically only when the
// existing on-disk bytes differ. Returns nil with no write when the content
// already matches on a stable regular file. Read or stat errors are ignored
// and the write proceeds — this is a best-effort optimization to avoid
// churning mtime on no-op writes, not a safety check.
func WriteFileIfChangedAtomic(fs FS, path string, data []byte, perm os.FileMode) error {
	if info, err := fs.Lstat(path); err == nil && info.Mode().IsRegular() {
		if snapshot, err := readRegularFileSnapshot(fs, path); err == nil && bytes.Equal(snapshot.data, data) {
			if info, err := fs.Lstat(path); err == nil && info.Mode().IsRegular() {
				if !snapshot.hasID {
					return WriteFileAtomic(fs, path, data, perm)
				}
				currentID, ok := fileIdentityFromInfo(info)
				if !ok || currentID != snapshot.id {
					return WriteFileAtomic(fs, path, data, perm)
				}
				return nil
			}
		}
	}
	return WriteFileAtomic(fs, path, data, perm)
}

// WriteFileIfContentOrModeChangedAtomic writes data to path atomically when
// the existing on-disk bytes, file type, or permissions differ. Returns nil
// with no write when the path is already a regular file with matching content
// and mode. Symlinks and other non-regular entries are replaced without first
// reading through them. Read or stat errors are ignored and the write proceeds.
func WriteFileIfContentOrModeChangedAtomic(fs FS, path string, data []byte, perm os.FileMode) error {
	if info, err := fs.Lstat(path); err == nil && info.Mode().IsRegular() && ComparableMode(info.Mode()) == ComparableMode(perm) {
		if snapshot, err := readRegularFileSnapshot(fs, path); err == nil && bytes.Equal(snapshot.data, data) {
			if info, err := fs.Lstat(path); err == nil && info.Mode().IsRegular() && ComparableMode(info.Mode()) == ComparableMode(perm) {
				if !snapshot.hasID {
					return WriteFileAtomic(fs, path, data, perm)
				}
				currentID, ok := fileIdentityFromInfo(info)
				if !ok || currentID != snapshot.id {
					return WriteFileAtomic(fs, path, data, perm)
				}
				return nil
			}
		}
	}
	return WriteFileAtomic(fs, path, data, perm)
}

type regularFileSnapshotReader interface {
	readRegularFileSnapshot(name string) (regularFileSnapshot, error)
}

type regularFileSnapshot struct {
	data  []byte
	id    fileIdentity
	hasID bool
}

type fileIdentity struct {
	dev uint64
	ino uint64
}

func readRegularFileSnapshot(fs FS, path string) (regularFileSnapshot, error) {
	if reader, ok := fs.(regularFileSnapshotReader); ok {
		return reader.readRegularFileSnapshot(path)
	}
	return regularFileSnapshot{}, &os.PathError{Op: "open", Path: path, Err: os.ErrInvalid}
}

// ComparableMode returns the portion of a file mode that is significant when
// deciding whether an on-disk file already matches a desired mode: the
// permission bits plus the setuid, setgid, and sticky bits.
func ComparableMode(mode os.FileMode) os.FileMode {
	return mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

func fileIdentityFromInfo(info os.FileInfo) (fileIdentity, bool) {
	return fileIdentityFromSys(info.Sys())
}

func fileIdentityFromSys(sys any) (fileIdentity, bool) {
	// Signed stat fields follow Go's direct int-to-uint conversion so the
	// Fstat and Lstat paths agree on device identity across Unix variants.
	stat := reflect.Indirect(reflect.ValueOf(sys))
	if !stat.IsValid() {
		return fileIdentity{}, false
	}
	dev := stat.FieldByName("Dev")
	ino := stat.FieldByName("Ino")
	if !dev.IsValid() || !ino.IsValid() {
		return fileIdentity{}, false
	}
	devValue, ok := numericFieldToUint64(dev)
	if !ok {
		return fileIdentity{}, false
	}
	inoValue, ok := numericFieldToUint64(ino)
	if !ok {
		return fileIdentity{}, false
	}
	return fileIdentity{dev: devValue, ino: inoValue}, true
}

func numericFieldToUint64(v reflect.Value) (uint64, bool) {
	switch v.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return uint64(v.Int()), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint(), true
	default:
		return 0, false
	}
}
