package beads

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

const lifecycleMutationOwnerRecordMaxBytes = 4096

// lifecycleMutationFileLock owns one securely-opened lock object. Root leases
// flock the stable .beads directory inode and keep the private leaf as owner
// data only. Descendant leases flock generation-specific leaves opened relative
// to that retained directory, so replacing the owner-record pathname cannot
// fork the synchronization domain.
type lifecycleMutationFileLock struct {
	file              *os.File
	validationDir     *os.File
	ownsValidationDir bool
	validationName    string
	ownerRecordName   string
	locked            bool
}

func openLifecycleMutationScopeFileLock(scope, ownerRecordName string) (*lifecycleMutationFileLock, error) {
	if scope == "" {
		return nil, nil
	}
	if err := validateLifecycleMutationFilename(ownerRecordName); err != nil {
		return nil, err
	}
	scope = normalizeLifecycleMutationScope(scope)
	scopeFD, err := unix.Open(
		scope,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening lifecycle mutation scope %q: %w", scope, err)
	}
	closeScopeFD := true
	defer func() {
		if closeScopeFD {
			_ = unix.Close(scopeFD)
		}
	}()

	beadsDir := filepath.Join(scope, ".beads")
	beadsFD, err := unix.Openat(
		scopeFD,
		".beads",
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0,
	)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening lifecycle mutation directory %q: %w", beadsDir, err)
	}
	closeBeadsFD := true
	defer func() {
		if closeBeadsFD {
			_ = unix.Close(beadsFD)
		}
	}()

	var stat unix.Stat_t
	if err := unix.Fstat(beadsFD, &stat); err != nil {
		return nil, fmt.Errorf("checking lifecycle mutation directory %q: %w", beadsDir, err)
	}
	if err := validateLifecycleMutationDirectoryStat(beadsDir, &stat); err != nil {
		return nil, err
	}

	scopeFile := os.NewFile(uintptr(scopeFD), scope)
	if scopeFile == nil {
		return nil, fmt.Errorf("opening lifecycle mutation scope %q: invalid file descriptor", scope)
	}
	closeScopeFD = false
	beadsFile := os.NewFile(uintptr(beadsFD), beadsDir)
	if beadsFile == nil {
		_ = scopeFile.Close()
		return nil, fmt.Errorf("opening lifecycle mutation directory %q: invalid file descriptor", beadsDir)
	}
	closeBeadsFD = false
	return &lifecycleMutationFileLock{
		file:              beadsFile,
		validationDir:     scopeFile,
		ownsValidationDir: true,
		validationName:    ".beads",
		ownerRecordName:   ownerRecordName,
	}, nil
}

func openLifecycleMutationDelegationFileLock(
	owner *lifecycleMutationFileLock,
	filename string,
) (*lifecycleMutationFileLock, error) {
	return openLifecycleMutationChildFileLock(owner, filename, true, false)
}

func openLifecycleMutationChildFileLock(
	owner *lifecycleMutationFileLock,
	filename string,
	create bool,
	exclusive bool,
) (*lifecycleMutationFileLock, error) {
	if owner == nil || owner.file == nil || owner.ownerRecordName == "" {
		return nil, errors.New("opening lifecycle mutation delegation lock: owner directory is closed")
	}
	file, err := openLifecycleMutationLeaf(
		int(owner.file.Fd()),
		owner.file.Name(),
		filename,
		create,
		exclusive,
	)
	if err != nil {
		return nil, err
	}
	return &lifecycleMutationFileLock{
		file:           file,
		validationDir:  owner.file,
		validationName: filename,
	}, nil
}

func (fl *lifecycleMutationFileLock) TryLock() (bool, error) {
	if fl == nil || fl.file == nil {
		return false, errors.New("lifecycle mutation flock try-lock: closed")
	}
	if fl.locked {
		return false, errors.New("lifecycle mutation flock try-lock: already locked")
	}
	err := unix.Flock(int(fl.file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		if validateErr := fl.validateLockPath(); validateErr != nil {
			return false, validateErr
		}
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lifecycle mutation flock try-lock: %w", err)
	}
	if err := fl.validateLockPath(); err != nil {
		_ = unix.Flock(int(fl.file.Fd()), unix.LOCK_UN)
		return false, err
	}
	fl.locked = true
	return true, nil
}

func (fl *lifecycleMutationFileLock) Lock() error {
	if fl == nil || fl.file == nil {
		return errors.New("lifecycle mutation flock lock: closed")
	}
	if fl.locked {
		return errors.New("lifecycle mutation flock lock: already locked")
	}
	if err := unix.Flock(int(fl.file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lifecycle mutation flock lock: %w", err)
	}
	if err := fl.validateLockPath(); err != nil {
		_ = unix.Flock(int(fl.file.Fd()), unix.LOCK_UN)
		return err
	}
	fl.locked = true
	return nil
}

func (fl *lifecycleMutationFileLock) ReadExact() ([]byte, error) {
	if fl == nil || fl.file == nil {
		return nil, errors.New("lifecycle mutation flock read: closed")
	}
	if fl.ownerRecordName == "" {
		return nil, errors.New("lifecycle mutation flock read: no owner record")
	}
	record, err := openLifecycleMutationLeaf(
		int(fl.file.Fd()),
		fl.file.Name(),
		fl.ownerRecordName,
		false,
		false,
	)
	if err != nil {
		return nil, err
	}
	defer record.Close() //nolint:errcheck // read result is authoritative
	buf := make([]byte, lifecycleMutationOwnerRecordMaxBytes+1)
	n, err := record.ReadAt(buf, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("lifecycle mutation flock read: %w", err)
	}
	if n > lifecycleMutationOwnerRecordMaxBytes {
		return nil, fmt.Errorf("lifecycle mutation flock read: owner record exceeds %d bytes", lifecycleMutationOwnerRecordMaxBytes)
	}
	return buf[:n], nil
}

func (fl *lifecycleMutationFileLock) WriteLocked(data []byte) error {
	if fl == nil || fl.file == nil || !fl.locked {
		return errors.New("lifecycle mutation flock write: not locked")
	}
	if fl.ownerRecordName == "" {
		return errors.New("lifecycle mutation flock write: no owner record")
	}
	record, err := openLifecycleMutationLeaf(
		int(fl.file.Fd()),
		fl.file.Name(),
		fl.ownerRecordName,
		true,
		false,
	)
	if err != nil {
		return err
	}
	defer record.Close() //nolint:errcheck // write/sync errors take precedence
	if err := record.Truncate(0); err != nil {
		return fmt.Errorf("lifecycle mutation flock truncate: %w", err)
	}
	if _, err := record.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("lifecycle mutation flock seek: %w", err)
	}
	if len(data) > 0 {
		if _, err := record.Write(data); err != nil {
			return fmt.Errorf("lifecycle mutation flock write: %w", err)
		}
	}
	if err := record.Sync(); err != nil {
		return fmt.Errorf("lifecycle mutation flock sync: %w", err)
	}
	return nil
}

func (fl *lifecycleMutationFileLock) removeLocked() error {
	if fl == nil || fl.file == nil || !fl.locked {
		return errors.New("removing lifecycle mutation lock: not locked")
	}
	if fl.validationDir == nil || fl.validationName == "" {
		return errors.New("removing lifecycle mutation lock: missing parent directory")
	}
	if err := fl.validateLockPath(); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(fl.validationDir.Fd()), fl.validationName, 0); err != nil {
		return fmt.Errorf("removing lifecycle mutation lock %q: %w", fl.file.Name(), err)
	}
	return nil
}

func (fl *lifecycleMutationFileLock) Unlock() error {
	if fl == nil || fl.file == nil {
		return nil
	}
	var result error
	if fl.locked {
		if err := unix.Flock(int(fl.file.Fd()), unix.LOCK_UN); err != nil {
			result = errors.Join(result, fmt.Errorf("lifecycle mutation flock unlock: %w", err))
		}
		fl.locked = false
	}
	if err := fl.file.Close(); err != nil {
		result = errors.Join(result, err)
	}
	fl.file = nil
	if fl.ownsValidationDir && fl.validationDir != nil {
		if err := fl.validationDir.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	fl.validationDir = nil
	return result
}

func (fl *lifecycleMutationFileLock) Close() error { return fl.Unlock() }

func (fl *lifecycleMutationFileLock) validateLockPath() error {
	if fl == nil || fl.file == nil || fl.validationDir == nil {
		return errors.New("checking lifecycle mutation lock path: closed")
	}
	var opened, current unix.Stat_t
	if err := unix.Fstat(int(fl.file.Fd()), &opened); err != nil {
		return fmt.Errorf("checking lifecycle mutation lock %q: %w", fl.file.Name(), err)
	}
	if err := unix.Fstatat(
		int(fl.validationDir.Fd()),
		fl.validationName,
		&current,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return fmt.Errorf("checking lifecycle mutation lock path %q: %w", fl.file.Name(), err)
	}
	if fl.ownerRecordName != "" {
		if err := validateLifecycleMutationDirectoryStat(fl.file.Name(), &opened); err != nil {
			return err
		}
		if err := validateLifecycleMutationDirectoryStat(fl.file.Name(), &current); err != nil {
			return err
		}
	} else {
		if err := validateLifecycleMutationLeafStat(fl.file.Name(), &opened); err != nil {
			return err
		}
		if err := validateLifecycleMutationLeafStat(fl.file.Name(), &current); err != nil {
			return err
		}
	}
	if opened.Dev != current.Dev || opened.Ino != current.Ino {
		return fmt.Errorf("checking lifecycle mutation lock %q: pathname no longer names the opened inode", fl.file.Name())
	}
	return nil
}

func openLifecycleMutationLeaf(
	dirFD int,
	dirPath, filename string,
	create, exclusive bool,
) (*os.File, error) {
	if err := validateLifecycleMutationFilename(filename); err != nil {
		return nil, err
	}
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if create {
		flags |= unix.O_CREAT
	}
	if exclusive {
		flags |= unix.O_EXCL
	}
	path := filepath.Join(dirPath, filename)
	fd, err := unix.Openat(dirFD, filename, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening lifecycle mutation lock %q: %w", path, err)
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, fmt.Errorf("checking lifecycle mutation lock %q: %w", path, err)
	}
	if err := validateLifecycleMutationLeafStat(path, &stat); err != nil {
		return nil, err
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return nil, fmt.Errorf("securing lifecycle mutation lock %q: %w", path, err)
	}

	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		return nil, fmt.Errorf("opening lifecycle mutation lock %q: invalid file descriptor", path)
	}
	closeFD = false
	return file, nil
}

func validateLifecycleMutationFilename(filename string) error {
	if filename == "" || filename == "." || filename == ".." || filepath.Base(filename) != filename {
		return fmt.Errorf("opening lifecycle mutation lock: invalid filename %q", filename)
	}
	return nil
}

func validateLifecycleMutationDirectoryStat(path string, stat *unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf("checking lifecycle mutation directory %q: not a directory", path)
	}
	// A world-writable .beads directory is a genuine hijack vector: any local
	// account can swap the lock inode out from under a live lease, so it stays a
	// hard failure. Group writability (setgid shared-group deployments) and an
	// owner uid that differs from the effective uid (a controller service account
	// coordinating with a human's own gc) are legitimate shared-scope layouts —
	// warn once per scope and keep coordinating through flock rather than failing
	// every lifecycle mutation.
	if stat.Mode&0o002 != 0 {
		return fmt.Errorf(
			"checking lifecycle mutation directory %q: permissions are %04o, world writable",
			path,
			stat.Mode&0o777,
		)
	}
	if stat.Mode&0o020 != 0 {
		warnLifecycleMutationSharedScopeOnce(path, fmt.Sprintf(
			"lifecycle mutation directory %q is group-writable (%04o); coordinating through flock in a shared-group scope",
			path,
			stat.Mode&0o777,
		))
	}
	if stat.Uid != uint32(os.Geteuid()) {
		warnLifecycleMutationSharedScopeOnce(path, fmt.Sprintf(
			"lifecycle mutation directory %q is owned by uid %d, not effective uid %d; coordinating through flock across owners",
			path,
			stat.Uid,
			os.Geteuid(),
		))
	}
	return nil
}

func validateLifecycleMutationLeafStat(path string, stat *unix.Stat_t) error {
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("checking lifecycle mutation lock %q: not a regular file", path)
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("checking lifecycle mutation lock %q: link count is %d, want 1", path, stat.Nlink)
	}
	// A lock leaf owned by a different uid is the same legitimate multi-writer
	// case as the directory (controller service user vs a human's gc): warn once
	// per scope instead of failing the mutation. The hardlink and regular-file
	// checks above still fail closed for the genuinely dangerous layouts.
	if stat.Uid != uint32(os.Geteuid()) {
		warnLifecycleMutationSharedScopeOnce(path, fmt.Sprintf(
			"lifecycle mutation lock %q is owned by uid %d, not effective uid %d; coordinating through flock across owners",
			path,
			stat.Uid,
			os.Geteuid(),
		))
	}
	return nil
}

// lifecycleMutationSharedScopeWarned records lock paths that have already
// emitted a shared-scope advisory so a legitimately shared .beads directory does
// not spam the log on every lifecycle mutation.
var lifecycleMutationSharedScopeWarned sync.Map

// warnLifecycleMutationSharedScopeOnce logs a shared-scope advisory at most once
// per lock path. The mismatch is a downgraded warning, not a hard error, so
// shared-permission deployments keep mutating while operators still see the
// non-default ownership/permission layout once.
func warnLifecycleMutationSharedScopeOnce(path, message string) {
	if _, warned := lifecycleMutationSharedScopeWarned.LoadOrStore(path, struct{}{}); warned {
		return
	}
	log.Printf("beads lifecycle: %s", message)
}
