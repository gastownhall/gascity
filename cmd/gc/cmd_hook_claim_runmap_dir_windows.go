//go:build windows

package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"syscall"
	"time"

	gcruntime "github.com/gastownhall/gascity/internal/runtime"
	"golang.org/x/sys/windows"
)

const (
	runMapDirFileDeleteChild windows.ACCESS_MASK = 0x00000040
	runMapDirFileTraverse    windows.ACCESS_MASK = 0x00000020

	runMapDirAceTypeAccessAllowedCallback      = 0x09
	runMapDirAceTypeAccessDeniedObject         = 0x06
	runMapDirAceTypeAccessDeniedCallback       = 0x0A
	runMapDirAceTypeAccessDeniedCallbackObject = 0x0C
	runMapDirAceFlagObjectInherit              = 0x01
	runMapDirAceFlagInheritOnly                = 0x08

	runMapDirOwnerRightsSID = "S-1-3-4"

	runMapDirPinRetryInterval = 10 * time.Millisecond
	runMapDirPinRetryTimeout  = 250 * time.Millisecond

	runMapDirErrorSharingViolation syscall.Errno = 32
)

var runMapDirWriteRights = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
	windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES | runMapDirFileDeleteChild |
	windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
	windows.GENERIC_WRITE | windows.GENERIC_ALL

var runMapDirEntryWriteRights windows.ACCESS_MASK = windows.FILE_WRITE_DATA | windows.FILE_APPEND_DATA |
	windows.FILE_WRITE_EA | windows.FILE_WRITE_ATTRIBUTES |
	windows.DELETE | windows.WRITE_DAC | windows.WRITE_OWNER |
	windows.GENERIC_WRITE | windows.GENERIC_ALL

var runMapDirTrustedSystemSIDs = map[string]bool{
	"S-1-5-18":     true, // Local System
	"S-1-5-32-544": true, // Administrators
	"S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464": true, // TrustedInstaller
}

// runMapDirSafeToPublish reports whether Windows names a directory whose owner
// and DACL keep write access to this user and the trusted system principals.
// Windows has no sticky directory handoff equivalent, and Go reports every
// writable directory as mode 0777, so POSIX mode bits cannot answer the
// publication-safety question here.
func runMapDirSafeToPublish(dir string) bool {
	return verifyRunMapDirNotWritableByOthers(dir) == nil
}

func runMapDirUnsafeReason() string {
	return "is writable by an account other than this user or a trusted system principal"
}

// runMapDirPrunable reports whether stale entries may be reaped in-process.
// With no sticky handoff on Windows, pruning is safe under the same condition as
// publishing: no untrusted principal can add, delete, replace, or re-permission
// entries in the directory.
func runMapDirPrunable(dir string) bool {
	return verifyRunMapDirNotWritableByOthers(dir) == nil
}

func verifyRunMapDirNotWritableByOthers(dir string) error {
	userSID, err := runMapCurrentUserSID()
	if err != nil {
		return fmt.Errorf("reading current user SID: %w", err)
	}
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolving run-map dir %q: %w", dir, err)
	}
	if err := gcruntime.VerifyPrivatePathAncestors(absoluteDir); err != nil {
		return fmt.Errorf("verifying run-map dir %q ancestry: %w", dir, err)
	}
	handle, err := runMapOpenDirectory(absoluteDir)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	security, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return fmt.Errorf("reading security of run-map dir %q: %w", absoluteDir, err)
	}
	if security == nil {
		return fmt.Errorf("run-map dir %q has no security descriptor", absoluteDir)
	}
	owner, _, err := security.Owner()
	if err != nil {
		return fmt.Errorf("reading owner of run-map dir %q: %w", absoluteDir, err)
	}
	if owner == nil {
		return fmt.Errorf("run-map dir %q has no owner", absoluteDir)
	}
	ownerSID := owner.String()
	if !runMapTrustedPrincipal(ownerSID, userSID) {
		return fmt.Errorf("run-map dir %q is owned by %s, which is neither this user nor a system principal", absoluteDir, runMapDescribeSID(ownerSID))
	}

	dacl, _, err := security.DACL()
	if err != nil {
		return fmt.Errorf("reading DACL of run-map dir %q: %w", absoluteDir, err)
	}
	if dacl == nil {
		return fmt.Errorf("run-map dir %q has a NULL DACL, which grants every account full access", absoluteDir)
	}
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			return fmt.Errorf("reading DACL entry %d of run-map dir %q: %w", i, absoluteDir, err)
		}
		inheritOnly := ace.Header.AceFlags&runMapDirAceFlagInheritOnly != 0
		if inheritOnly && ace.Header.AceFlags&runMapDirAceFlagObjectInherit == 0 {
			continue
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE, runMapDirAceTypeAccessDeniedObject, runMapDirAceTypeAccessDeniedCallback, runMapDirAceTypeAccessDeniedCallbackObject:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE, runMapDirAceTypeAccessAllowedCallback:
		default:
			return fmt.Errorf("DACL entry %d of run-map dir %q has type %#x, which this check does not interpret", i, absoluteDir, ace.Header.AceType)
		}
		rights := runMapDirWriteRights
		if inheritOnly {
			rights = runMapDirEntryWriteRights
		}
		if ace.Mask&rights == 0 {
			continue
		}
		sid := runMapAceSID(ace).String()
		if sid == runMapDirOwnerRightsSID || runMapTrustedPrincipal(sid, userSID) {
			continue
		}
		if inheritOnly {
			return fmt.Errorf("run-map dir %q grants %s inheritable access %#x, which lets an account other than this user or a system principal change entries created in it", absoluteDir, runMapDescribeSID(sid), ace.Mask)
		}
		return fmt.Errorf("run-map dir %q grants %s access %#x, which lets an account other than this user or a system principal change it", absoluteDir, runMapDescribeSID(sid), ace.Mask)
	}
	return nil
}

func runMapCurrentUserSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return user.User.Sid.String(), nil
}

func runMapTrustedPrincipal(sid, userSID string) bool {
	return sid == userSID || runMapDirTrustedSystemSIDs[sid]
}

func runMapDescribeSID(sid string) string {
	parsed, err := windows.StringToSid(sid)
	if err != nil {
		return "SID " + sid
	}
	account, domain, _, err := parsed.LookupAccount("")
	if err != nil || account == "" {
		return "SID " + sid
	}
	if domain != "" {
		account = domain + `\` + account
	}
	return fmt.Sprintf("SID %s (%s)", sid, account)
}

func runMapOpenDirectory(dir string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	deadline := time.Now().Add(runMapDirPinRetryTimeout)
	var handle windows.Handle
	for {
		handle, err = windows.CreateFile(
			name,
			uint32(windows.READ_CONTROL|windows.FILE_READ_ATTRIBUTES|runMapDirFileTraverse),
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil,
			windows.OPEN_EXISTING,
			windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
			0,
		)
		if !errors.Is(err, runMapDirErrorSharingViolation) || time.Now().After(deadline) {
			break
		}
		time.Sleep(runMapDirPinRetryInterval)
	}
	if err != nil {
		return 0, fmt.Errorf("opening run-map dir %q: %w", dir, err)
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("inspecting run-map dir %q: %w", dir, err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("run-map dir %q is a reparse point", dir)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("run-map dir %q is not a directory", dir)
	}
	return handle, nil
}
