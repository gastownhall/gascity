//go:build windows

package runtime

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

const (
	privateDirMode  os.FileMode = 0o700
	privateFileMode os.FileMode = 0o600

	seFileObject                        = 1
	ownerSecurityInformation            = 0x00000001
	daclSecurityInformation             = 0x00000004
	protectedDACLVisibilityInformation  = 0x80000000
	sddlRevision1                       = 1
	fileAttributeDirectory              = 0x00000010
	fileAttributeReparsePoint           = 0x00000400
	fileReadAttributes                  = 0x00000080
	fileTraverse                        = 0x00000020
	readControl                         = 0x00020000
	tokenQuery                          = 0x00000008
	writeDAC                            = 0x00040000
	privateFileRenameRetryInterval      = 10 * time.Millisecond
	privateFileRenameRetryTimeout       = 250 * time.Millisecond
	privateDirPinRetryInterval          = 10 * time.Millisecond
	privateDirPinRetryTimeout           = 250 * time.Millisecond
	privateTempFileCreationAttemptLimit = 100
	errorSharingViolation               = syscall.Errno(32)
	errorLockViolation                  = syscall.Errno(33)
)

var (
	advapi32                                                 = syscall.NewLazyDLL("advapi32.dll")
	kernel32                                                 = syscall.NewLazyDLL("kernel32.dll")
	procConvertSecurityDescriptorToStringSecurityDescriptorW = advapi32.NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")
	procConvertStringSecurityDescriptorToSecurityDescriptorW = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
	procGetSecurityDescriptorDACL                            = advapi32.NewProc("GetSecurityDescriptorDacl")
	procGetSecurityInfo                                      = advapi32.NewProc("GetSecurityInfo")
	procSetSecurityInfo                                      = advapi32.NewProc("SetSecurityInfo")
	procLstrlenW                                             = kernel32.NewProc("lstrlenW")
)

func EnsurePrivateDir(path string) error {
	if path == "" {
		return fmt.Errorf("creating private directory %q: empty path", path)
	}

	sid, err := currentUserSIDString()
	if err != nil {
		return fmt.Errorf("reading current user SID: %w", err)
	}

	clean := filepath.Clean(path)
	var missing []string
	for current := clean; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("private directory ancestor %q is a symlink", current)
			}
			if !info.IsDir() {
				return fmt.Errorf("private directory ancestor %q is not a directory", current)
			}
			// A directory already at path is about to be narrowed to owner-only,
			// with its owner held to this user, so its DACL as found is no verdict
			// on the path; what lies above it is.
			start := current
			if current == clean {
				start = filepath.Dir(current)
			}
			if err := verifyPrivateAncestry(start, sid); err != nil {
				return err
			}
			break
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("inspecting private directory %q: %w", current, err)
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("creating private directory %q: no existing ancestor", path)
		}
	}

	if len(missing) == 0 {
		return tightenPrivateDir(clean)
	}

	securityDescriptor, err := ownerOnlySecurityDescriptor(sid, true)
	if err != nil {
		return fmt.Errorf("building private directory security descriptor: %w", err)
	}
	defer syscall.LocalFree(syscall.Handle(securityDescriptor)) //nolint:errcheck

	attributes := syscall.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		SecurityDescriptor: securityDescriptor,
	}
	for i := len(missing) - 1; i >= 0; i-- {
		current := missing[i]
		name, err := syscall.UTF16PtrFromString(current)
		if err != nil {
			return fmt.Errorf("creating private directory %q: %w", current, err)
		}
		if err := syscall.CreateDirectory(name, &attributes); err != nil && !errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
			return fmt.Errorf("creating private directory %q: %w", current, err)
		}
		if err := tightenPrivateDir(current); err != nil {
			return err
		}
	}
	return nil
}

// VerifyPrivatePathAncestors checks every existing ancestor above path with the
// same replacement and reparse-point rules EnsurePrivateDir uses. The path
// itself is not checked; callers that validate or prepare the leaf separately
// use this to prove the already-secured leaf cannot be reached through a
// replaceable ancestor.
func VerifyPrivatePathAncestors(path string) error {
	if path == "" {
		return fmt.Errorf("verifying private path ancestors %q: empty path", path)
	}
	sid, err := currentUserSIDString()
	if err != nil {
		return fmt.Errorf("reading current user SID: %w", err)
	}
	return verifyPrivateAncestry(filepath.Dir(filepath.Clean(path)), sid)
}

func tightenPrivateDir(path string) error {
	handle, err := openPrivatePath(path, true, readControl|writeDAC|fileReadAttributes)
	if err != nil {
		return fmt.Errorf("opening private directory %q: %w", path, err)
	}
	defer syscall.CloseHandle(handle) //nolint:errcheck
	if testHookPrivateDirOpened != nil {
		testHookPrivateDirOpened(path)
	}

	sid, err := currentUserSIDString()
	if err != nil {
		return fmt.Errorf("reading current user SID: %w", err)
	}
	if err := verifyHandleOwner(handle, path, sid); err != nil {
		return err
	}
	if err := applyOwnerOnlyDACL(handle, sid, true); err != nil {
		return fmt.Errorf("tightening private directory %q: %w", path, err)
	}
	if err := verifyHandleDACL(handle, sid, true); err != nil {
		return fmt.Errorf("verifying private directory %q: %w", path, err)
	}
	return confirmPrivateDir(path, sid)
}

// testHookPrivateDirOpened, when set, is called with the path of each
// directory tightenPrivateDir has opened, before anything is read through it.
var testHookPrivateDirOpened func(path string)

// confirmPrivateDir establishes, through a handle that pins the directory, that
// path names this user's owner-only directory.
//
// Narrowing a DACL does not reach a handle opened before it: one holding
// delete access can still rename the directory away and let another take its
// name. The confirming handle asks for traverse access and shares no delete
// access, so it cannot be opened while any such handle exists, and no new one
// can be opened while it is held; once it is closed, the owner-only DACL keeps
// anyone else from opening one. Sharing is enforced only for a handle with a
// data right, which is what the traverse access is for. What path names is
// then checked against the handle, since a component above the directory could
// still have moved.
func confirmPrivateDir(path, sid string) error {
	handle, err := openPinnedPrivateDir(path, readControl|fileReadAttributes|fileTraverse)
	if err != nil {
		if errors.Is(err, errorSharingViolation) {
			return fmt.Errorf("confirming private directory %q: another handle to it holds delete access, so it could still be renamed away: %w", path, err)
		}
		return fmt.Errorf("confirming private directory %q: %w", path, err)
	}
	defer syscall.CloseHandle(handle) //nolint:errcheck
	if err := verifyHandleOwner(handle, path, sid); err != nil {
		return err
	}
	if err := verifyHandleDACL(handle, sid, true); err != nil {
		return fmt.Errorf("verifying private directory %q: %w", path, err)
	}
	return verifyPathStillNames(handle, path)
}

// openPinnedPrivateDir opens the directory at path as openPrivatePath does but
// shares no delete access. access must include a data right for that to bind.
// A handle someone else holds with delete access refuses the open; that is
// retried briefly, since such a handle is normally fleeting.
func openPinnedPrivateDir(path string, access uint32) (syscall.Handle, error) {
	deadline := time.Now().Add(privateDirPinRetryTimeout)
	for {
		handle, err := openVerifiedPathShared(path, true, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, "private path")
		if !errors.Is(err, errorSharingViolation) || time.Now().After(deadline) {
			return handle, err
		}
		time.Sleep(privateDirPinRetryInterval)
	}
}

// verifyPathStillNames confirms that path still names the directory behind
// handle: the same file on the same volume.
func verifyPathStillNames(handle syscall.Handle, path string) error {
	current, err := openPrivatePath(path, true, fileReadAttributes)
	if err != nil {
		return fmt.Errorf("re-opening private directory %q after tightening it: %w", path, err)
	}
	defer syscall.CloseHandle(current) //nolint:errcheck
	var tightened, found syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &tightened); err != nil {
		return fmt.Errorf("identifying private directory %q: %w", path, err)
	}
	if err := syscall.GetFileInformationByHandle(current, &found); err != nil {
		return fmt.Errorf("identifying private directory %q: %w", path, err)
	}
	if tightened.VolumeSerialNumber != found.VolumeSerialNumber || tightened.FileIndexHigh != found.FileIndexHigh || tightened.FileIndexLow != found.FileIndexLow {
		return fmt.Errorf("private directory %q is not the directory that was tightened: the path was redirected while it was being secured", path)
	}
	return nil
}

func WritePrivateFile(path string, data []byte) error {
	f, tmp, err := createPrivateTempFile(path)
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(tmp)
	}()

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("writing %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}

	// Windows cannot replace a destination while an existing handle omits
	// FILE_SHARE_DELETE. Retry the transient case, but a reader held beyond this
	// bound remains an explicit failure rather than weakening atomic replacement.
	if err := replacePrivateFile(tmp, path); err != nil {
		return fmt.Errorf("replacing %q: %w", path, err)
	}
	return nil
}

func createPrivateTempFile(path string) (*os.File, string, error) {
	sid, err := currentUserSIDString()
	if err != nil {
		return nil, "", fmt.Errorf("reading current user SID: %w", err)
	}
	securityDescriptor, err := ownerOnlySecurityDescriptor(sid, false)
	if err != nil {
		return nil, "", fmt.Errorf("building private file security descriptor: %w", err)
	}
	defer syscall.LocalFree(syscall.Handle(securityDescriptor)) //nolint:errcheck

	attributes := syscall.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(syscall.SecurityAttributes{})),
		SecurityDescriptor: securityDescriptor,
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	for range privateTempFileCreationAttemptLimit {
		var suffix [16]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", fmt.Errorf("creating temp file for %q: generating name: %w", path, err)
		}
		tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp%x", base, suffix))
		name, err := syscall.UTF16PtrFromString(tmp)
		if err != nil {
			return nil, "", fmt.Errorf("creating temp file for %q: %w", path, err)
		}
		handle, err := syscall.CreateFile(
			name,
			syscall.GENERIC_WRITE|readControl|fileReadAttributes,
			syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
			&attributes,
			syscall.CREATE_NEW,
			syscall.FILE_ATTRIBUTE_NORMAL|syscall.FILE_FLAG_OPEN_REPARSE_POINT,
			0,
		)
		if errors.Is(err, syscall.ERROR_FILE_EXISTS) || errors.Is(err, syscall.ERROR_ALREADY_EXISTS) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("creating temp file for %q: %w", path, err)
		}

		f := os.NewFile(uintptr(handle), tmp)
		if f == nil {
			_ = syscall.CloseHandle(handle)
			_ = os.Remove(tmp)
			return nil, "", fmt.Errorf("creating temp file for %q: invalid file handle", path)
		}
		if err := verifyPrivateFileHandle(handle, tmp, sid); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return nil, "", err
		}
		return f, tmp, nil
	}
	return nil, "", fmt.Errorf("creating temp file for %q: exhausted %d unique names", path, privateTempFileCreationAttemptLimit)
}

func verifyPrivateFileHandle(handle syscall.Handle, path, sid string) error {
	if err := verifyHandleType(handle, path, false, "private file"); err != nil {
		return err
	}
	if err := verifyHandleOwner(handle, path, sid); err != nil {
		return err
	}
	if err := verifyHandleDACL(handle, sid, false); err != nil {
		return fmt.Errorf("verifying private file %q: %w", path, err)
	}
	return nil
}

func verifyDirectoryHandle(path, label string) error {
	handle, err := openPrivatePath(path, true, fileReadAttributes)
	if err != nil {
		return fmt.Errorf("opening %s %q: %w", label, path, err)
	}
	return syscall.CloseHandle(handle)
}

// trustedSystemSIDs are the principals Windows itself runs as and gives
// ownership of the directories every path is reached through. With the current
// user, who is checked separately because their SID is not a constant, they are
// the only principals trusted to own what is checked here or to hold rights
// that change it.
var trustedSystemSIDs = []string{
	"S-1-5-18",     // Local System
	"S-1-5-32-544", // Administrators
	"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464", // TrustedInstaller, which owns the volume root
}

// verifyPrivateAncestry walks from path up to the volume root and refuses a
// chain another account could redirect or substitute.
//
// The DACL on a private directory governs what is inside it, not where it is.
// A junction anywhere above it sends every later open somewhere the attacker
// chose, and an ancestor another account owns, or may delete, rename or
// re-permission, can be moved aside so a replacement stands in its place —
// with the descriptor on the private directory still reading exactly as it
// should, because it is no longer the directory being opened.
//
// Only the deepest existing ancestor used to be examined, and when the private
// directory already existed that ancestor was the directory itself, so nothing
// above it was checked at all.
//
// An ancestor that cannot be opened or read is refused rather than allowed:
// "not ours" and "cannot tell" have opposite safe outcomes here, and treating
// the second as the first is what turns a check into a formality.
func verifyPrivateAncestry(path, userSID string) error {
	for current := path; ; {
		if err := verifyAncestorComponent(current, userSID); err != nil {
			return err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}
}

// verifyAncestorComponent checks one component: openPrivatePath rejects
// reparse points and non-directories, the owner is held to the trusted set, and
// so is every principal its DACL gives one of redirectRights.
func verifyAncestorComponent(path, userSID string) error {
	handle, err := openPrivatePath(path, true, readControl|fileReadAttributes)
	if err != nil {
		return fmt.Errorf("opening private directory ancestor %q: %w", path, err)
	}
	defer syscall.CloseHandle(handle) //nolint:errcheck

	security, err := readHandleSecurity(handle)
	if err != nil {
		return fmt.Errorf("reading security of private directory ancestor %q: %w", path, err)
	}
	if !isTrustedPrincipal(security.owner, userSID) {
		return fmt.Errorf("private directory ancestor %q is owned by SID %s, which is neither this user nor a system principal; a component of the path owned by another account can be replaced whatever permissions are set beneath it", path, security.owner)
	}
	if grant, ok := security.foreignGrant(userSID, redirectRights); ok {
		return fmt.Errorf("private directory ancestor %q grants %s access %#x, which lets an account other than this user or a system principal delete, rename or re-permission it; such a component of the path can be replaced whatever permissions are set beneath it", path, describeSID(grant.sid), grant.mask)
	}
	return nil
}

func openPrivatePath(path string, directory bool, access uint32) (syscall.Handle, error) {
	return openVerifiedPath(path, directory, access, "private path")
}

// openVerifiedPath opens the object at path itself, never what a reparse point
// there refers to, and returns the handle only if that object is a directory
// or a regular file as asked. label names the object in an error.
func openVerifiedPath(path string, directory bool, access uint32, label string) (syscall.Handle, error) {
	return openVerifiedPathShared(path, directory, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, label)
}

// openVerifiedPathShared is openVerifiedPath with the sharing the open allows
// others given as share.
func openVerifiedPathShared(path string, directory bool, access, share uint32, label string) (syscall.Handle, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	flags := uint32(syscall.FILE_FLAG_OPEN_REPARSE_POINT)
	if directory {
		flags |= syscall.FILE_FLAG_BACKUP_SEMANTICS
	}
	handle, err := syscall.CreateFile(
		name,
		access,
		share,
		nil,
		syscall.OPEN_EXISTING,
		flags,
		0,
	)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	if err := verifyHandleType(handle, path, directory, label); err != nil {
		_ = syscall.CloseHandle(handle)
		return syscall.InvalidHandle, err
	}
	return handle, nil
}

func verifyHandleType(handle syscall.Handle, path string, wantDirectory bool, label string) error {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		return fmt.Errorf("inspecting %s %q: %w", label, path, err)
	}
	if info.FileAttributes&fileAttributeReparsePoint != 0 {
		return fmt.Errorf("%s %q is a reparse point", label, path)
	}
	isDirectory := info.FileAttributes&fileAttributeDirectory != 0
	if wantDirectory && !isDirectory {
		return fmt.Errorf("%s %q is not a directory", label, path)
	}
	if !wantDirectory && isDirectory {
		return fmt.Errorf("%s %q is not a regular file", label, path)
	}
	return nil
}

func verifyHandleOwner(handle syscall.Handle, path, want string) error {
	got, err := handleOwnerSIDString(handle)
	if err != nil {
		return fmt.Errorf("reading owner for %q: %w", path, err)
	}
	if got != want {
		return fmt.Errorf("private path %q is owned by SID %s, want %s", path, got, want)
	}
	return nil
}

func currentUserSIDString() (string, error) {
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return "", err
	}
	var token syscall.Token
	if err := syscall.OpenProcessToken(process, tokenQuery, &token); err != nil {
		return "", err
	}
	defer token.Close() //nolint:errcheck
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	return sidToString(user.User.Sid)
}

func handleOwnerSIDString(handle syscall.Handle) (string, error) {
	var owner *syscall.SID
	securityDescriptor, err := getHandleSecurityDescriptor(handle, ownerSecurityInformation, &owner)
	if err != nil {
		return "", err
	}
	defer syscall.LocalFree(syscall.Handle(securityDescriptor)) //nolint:errcheck
	return sidToString(owner)
}

func getHandleSecurityDescriptor(handle syscall.Handle, securityInformation uintptr, owner **syscall.SID) (uintptr, error) {
	var securityDescriptor uintptr
	r1, _, _ := procGetSecurityInfo.Call(
		uintptr(handle),
		uintptr(seFileObject),
		securityInformation,
		uintptr(unsafe.Pointer(owner)),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&securityDescriptor)),
	)
	if securityDescriptor != 0 && r1 != 0 {
		_, _ = syscall.LocalFree(syscall.Handle(securityDescriptor))
	}
	if r1 != 0 {
		return 0, syscall.Errno(r1)
	}
	return securityDescriptor, nil
}

func sidToString(sid *syscall.SID) (string, error) {
	var raw *uint16
	if err := syscall.ConvertSidToStringSid(sid, &raw); err != nil {
		if raw != nil {
			_, _ = syscall.LocalFree(syscall.Handle(uintptr(unsafe.Pointer(raw))))
		}
		return "", err
	}
	defer syscall.LocalFree(syscall.Handle(uintptr(unsafe.Pointer(raw)))) //nolint:errcheck
	return utf16PtrToString(raw), nil
}

func ownerOnlySecurityDescriptor(sid string, directory bool) (uintptr, error) {
	sddl := ownerOnlySDDL(sid, directory)
	sddlPtr, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		return 0, err
	}
	var securityDescriptor uintptr
	r1, _, e1 := procConvertStringSecurityDescriptorToSecurityDescriptorW.Call(
		uintptr(unsafe.Pointer(sddlPtr)),
		uintptr(sddlRevision1),
		uintptr(unsafe.Pointer(&securityDescriptor)),
		0,
	)
	if r1 == 0 {
		if securityDescriptor != 0 {
			_, _ = syscall.LocalFree(syscall.Handle(securityDescriptor))
		}
		return 0, e1
	}
	return securityDescriptor, nil
}

func ownerOnlySDDL(sid string, directory bool) string {
	return "O:" + sid + ownerOnlyDACL(sid, directory)
}

func ownerOnlyDACL(sid string, directory bool) string {
	if directory {
		return "D:P(A;OICI;FA;;;" + sid + ")"
	}
	return "D:P(A;;FA;;;" + sid + ")"
}

func applyOwnerOnlyDACL(handle syscall.Handle, sid string, directory bool) error {
	securityDescriptor, err := ownerOnlySecurityDescriptor(sid, directory)
	if err != nil {
		return err
	}
	defer syscall.LocalFree(syscall.Handle(securityDescriptor)) //nolint:errcheck

	var present int32
	var dacl uintptr
	var defaulted int32
	r1, _, e1 := procGetSecurityDescriptorDACL.Call(
		securityDescriptor,
		uintptr(unsafe.Pointer(&present)),
		uintptr(unsafe.Pointer(&dacl)),
		uintptr(unsafe.Pointer(&defaulted)),
	)
	if r1 == 0 {
		return e1
	}
	if present == 0 || dacl == 0 {
		return fmt.Errorf("owner-only security descriptor has no DACL")
	}

	r1, _, _ = procSetSecurityInfo.Call(
		uintptr(handle),
		uintptr(seFileObject),
		uintptr(daclSecurityInformation|protectedDACLVisibilityInformation),
		0,
		0,
		dacl,
		0,
	)
	if r1 != 0 {
		return syscall.Errno(r1)
	}
	return nil
}

func verifyHandleDACL(handle syscall.Handle, sid string, directory bool) error {
	got, err := handleDACLString(handle)
	if err != nil {
		return err
	}
	want := ownerOnlyDACL(sid, directory)
	autoInherited := "D:PAI" + want[len("D:P"):]
	if got != want && got != autoInherited {
		return fmt.Errorf("DACL is %q, want %q or %q", got, want, autoInherited)
	}
	return nil
}

func handleDACLString(handle syscall.Handle) (string, error) {
	securityDescriptor, err := getHandleSecurityDescriptor(handle, daclSecurityInformation, nil)
	if err != nil {
		return "", err
	}
	defer syscall.LocalFree(syscall.Handle(securityDescriptor)) //nolint:errcheck

	var raw *uint16
	r1, _, e1 := procConvertSecurityDescriptorToStringSecurityDescriptorW.Call(
		securityDescriptor,
		uintptr(sddlRevision1),
		uintptr(daclSecurityInformation),
		uintptr(unsafe.Pointer(&raw)),
		0,
	)
	if r1 == 0 {
		if raw != nil {
			_, _ = syscall.LocalFree(syscall.Handle(uintptr(unsafe.Pointer(raw))))
		}
		return "", e1
	}
	defer syscall.LocalFree(syscall.Handle(uintptr(unsafe.Pointer(raw)))) //nolint:errcheck

	return utf16PtrToString(raw), nil
}

func utf16PtrToString(raw *uint16) string {
	if raw == nil {
		return ""
	}
	length, _, _ := procLstrlenW.Call(uintptr(unsafe.Pointer(raw)))
	if length == 0 {
		return ""
	}
	return syscall.UTF16ToString(unsafe.Slice(raw, int(length)))
}

func replacePrivateFile(source, target string) error {
	deadline := time.Now().Add(privateFileRenameRetryTimeout)
	for {
		err := os.Rename(source, target)
		if err == nil {
			return nil
		}
		if !isRenameSharingError(err) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(privateFileRenameRetryInterval)
	}
}

func isRenameSharingError(err error) bool {
	return errors.Is(err, syscall.ERROR_ACCESS_DENIED) ||
		errors.Is(err, errorSharingViolation) ||
		errors.Is(err, errorLockViolation)
}
