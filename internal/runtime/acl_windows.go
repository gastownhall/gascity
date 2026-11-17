//go:build windows

package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"syscall"
	"unsafe"
)

// Access rights and the security descriptor, ACL and SID layouts, from winnt.h.
const (
	accessDelete        = 0x00010000
	accessWriteOwner    = 0x00080000
	accessGenericAll    = 0x10000000
	accessGenericWrite  = 0x40000000
	fileWriteData       = 0x00000002 // FILE_ADD_FILE on a directory
	fileAppendData      = 0x00000004 // FILE_ADD_SUBDIRECTORY on a directory
	fileWriteEA         = 0x00000010
	fileDeleteChild     = 0x00000040
	fileWriteAttributes = 0x00000100

	// SECURITY_DESCRIPTOR_RELATIVE: Revision, Sbz1, Control, then the offsets
	// of the owner, group, SACL and DACL.
	securityDescriptorHeaderSize = 20
	seDACLPresent                = 0x0004
	seSelfRelative               = 0x8000
	aclHeaderSize                = 8 // AclRevision, Sbz1, AclSize, AceCount, Sbz2
	aceHeaderSize                = 4 // AceType, AceFlags, AceSize
	aceMaskSize                  = 4
	sidHeaderSize                = 8 // Revision, SubAuthorityCount, IdentifierAuthority
	sidRevision                  = 1
	sidMaxSubAuthorities         = 15

	aceTypeAccessAllowed              = 0x00
	aceTypeAccessDenied               = 0x01
	aceTypeAccessDeniedObject         = 0x06
	aceTypeAccessAllowedCallback      = 0x09
	aceTypeAccessDeniedCallback       = 0x0A
	aceTypeAccessDeniedCallbackObject = 0x0C
	aceFlagInheritOnly                = 0x08

	// ownerRightsSID stands for whoever owns the object the entry is on.
	ownerRightsSID = "S-1-3-4"
)

// procGetKernelObjectSecurity reads a descriptor straight off a handle.
// GetSecurityInfo answers the same question through the access-control
// provider layer at over forty times the cost per call, which is felt when a
// check runs over every file in a tree.
var procGetKernelObjectSecurity = advapi32.NewProc("GetKernelObjectSecurity")

// redirectRights are the rights over a directory on a path that let their
// holder put something else at its name. DELETE removes or renames the
// directory itself and FILE_DELETE_CHILD any entry in it; WRITE_DAC and
// WRITE_OWNER hand the holder either of those whenever they like; GENERIC_ALL
// maps onto all four.
//
// The right to add entries is left out. Something another account creates
// where a component is missing is theirs, and every component's owner is held
// to the trusted set, so a squatter is refused there instead.
//
// So are the rights to write a directory's data or attributes, although on an
// empty directory they are enough to make it a junction. An ancestor of an
// existing path is never empty. The deepest existing ancestor of a path still
// to be created can be, and between this check and the creation of its first
// missing entry another account holding one of those rights on it could
// redirect what gets created. No directory a private path is rooted in by
// default grants another account such a right.
const redirectRights = accessDelete | fileDeleteChild | writeDAC | accessWriteOwner | accessGenericAll

// modifyRights are the rights over a file that let their holder change what it
// holds or who may change it: writing or appending data, writing attributes or
// extended attributes, deleting it, and rewriting its DACL or owner.
// GENERIC_WRITE and GENERIC_ALL map onto them.
const modifyRights = fileWriteData | fileAppendData | fileWriteEA | fileWriteAttributes |
	accessDelete | writeDAC | accessWriteOwner | accessGenericWrite | accessGenericAll

// handleSecurity is an object's owner and the allow entries of its DACL that
// apply to the object itself, read in one call so both describe one descriptor.
type handleSecurity struct {
	owner  string
	allows []allowEntry
}

// allowEntry is one allow entry: the principal, and the access mask it grants.
type allowEntry struct {
	sid  string
	mask uint32
}

func readHandleSecurity(handle syscall.Handle) (handleSecurity, error) {
	descriptor := make([]byte, 512)
	for {
		var needed uint32
		r1, _, e1 := procGetKernelObjectSecurity.Call(
			uintptr(handle),
			uintptr(ownerSecurityInformation|daclSecurityInformation),
			uintptr(unsafe.Pointer(&descriptor[0])),
			uintptr(len(descriptor)),
			uintptr(unsafe.Pointer(&needed)),
		)
		if r1 != 0 {
			if needed > 0 && int(needed) <= len(descriptor) {
				descriptor = descriptor[:needed]
			}
			return parseSecurityDescriptor(descriptor)
		}
		if !errors.Is(e1, syscall.ERROR_INSUFFICIENT_BUFFER) || int(needed) <= len(descriptor) {
			return handleSecurity{}, e1
		}
		descriptor = make([]byte, needed)
	}
}

// parseSecurityDescriptor reads the owner and the object's own allow entries
// from a self-relative security descriptor.
//
// A descriptor without a DACL and one whose DACL is NULL both grant every
// account full access, so neither is read as "nothing granted".
func parseSecurityDescriptor(descriptor []byte) (handleSecurity, error) {
	if len(descriptor) < securityDescriptorHeaderSize {
		return handleSecurity{}, fmt.Errorf("the security descriptor is %d bytes, shorter than its own header", len(descriptor))
	}
	control := binary.LittleEndian.Uint16(descriptor[2:4])
	if control&seSelfRelative == 0 {
		return handleSecurity{}, errors.New("the security descriptor is not in self-relative form")
	}
	owner, err := parseSID(descriptor, int(binary.LittleEndian.Uint32(descriptor[4:8])))
	if err != nil {
		return handleSecurity{}, fmt.Errorf("reading the owner: %w", err)
	}
	daclOffset := int(binary.LittleEndian.Uint32(descriptor[16:20]))
	if control&seDACLPresent == 0 || daclOffset == 0 {
		return handleSecurity{}, errors.New("the security descriptor has no DACL or a NULL one, which grants every account full access")
	}
	if daclOffset < securityDescriptorHeaderSize || daclOffset > len(descriptor)-aclHeaderSize {
		return handleSecurity{}, fmt.Errorf("the DACL offset %d lies outside the %d-byte security descriptor", daclOffset, len(descriptor))
	}
	daclSize := int(binary.LittleEndian.Uint16(descriptor[daclOffset+2 : daclOffset+4]))
	if daclSize > len(descriptor)-daclOffset {
		return handleSecurity{}, fmt.Errorf("the %d-byte DACL runs past the end of the security descriptor", daclSize)
	}
	allows, err := parseAllowEntries(descriptor[daclOffset : daclOffset+daclSize])
	if err != nil {
		return handleSecurity{}, err
	}
	return handleSecurity{owner: owner, allows: allows}, nil
}

// parseSID returns the string form of the SID at offset in buf, refusing one
// that is absent or does not fit.
func parseSID(buf []byte, offset int) (string, error) {
	if offset <= 0 {
		return "", errors.New("no SID is present")
	}
	if offset > len(buf)-sidHeaderSize {
		return "", fmt.Errorf("the SID at offset %d lies outside the %d-byte buffer", offset, len(buf))
	}
	sid := buf[offset:]
	subAuthorities := int(sid[1])
	if sid[0] != sidRevision || subAuthorities > sidMaxSubAuthorities || len(sid) < sidHeaderSize+4*subAuthorities {
		return "", errors.New("the SID is malformed")
	}
	return sidToString((*syscall.SID)(unsafe.Pointer(&sid[0])))
}

// parseAllowEntries returns the allow entries of acl that apply to the object
// the ACL is attached to.
//
// Deny entries are skipped: they can only take access away, so passing over
// one can only make a grant look wider than it is. Inherit-only entries are
// skipped because they govern what is created inside the object, not the object.
// Anything it cannot read with certainty — a type it does not know, a length
// that runs off the end, a malformed SID — is an error, never a pass.
func parseAllowEntries(acl []byte) ([]allowEntry, error) {
	if len(acl) < aclHeaderSize {
		return nil, fmt.Errorf("the DACL is %d bytes, shorter than its own header", len(acl))
	}
	count := int(binary.LittleEndian.Uint16(acl[4:6]))
	var allows []allowEntry
	offset := aclHeaderSize
	for i := range count {
		if len(acl)-offset < aceHeaderSize {
			return nil, fmt.Errorf("DACL entry %d of %d runs past the end of the DACL", i, count)
		}
		aceType, aceFlags := acl[offset], acl[offset+1]
		aceSize := int(binary.LittleEndian.Uint16(acl[offset+2 : offset+4]))
		if aceSize < aceHeaderSize || aceSize > len(acl)-offset {
			return nil, fmt.Errorf("DACL entry %d of %d declares %d bytes, which do not fit the DACL", i, count, aceSize)
		}
		ace := acl[offset : offset+aceSize]
		offset += aceSize

		switch aceType {
		case aceTypeAccessDenied, aceTypeAccessDeniedObject, aceTypeAccessDeniedCallback, aceTypeAccessDeniedCallbackObject:
			continue
		case aceTypeAccessAllowed, aceTypeAccessAllowedCallback:
		default:
			return nil, fmt.Errorf("DACL entry %d has type %#x, which this check does not interpret", i, aceType)
		}
		if aceFlags&aceFlagInheritOnly != 0 {
			continue
		}
		if len(ace) < aceHeaderSize+aceMaskSize+sidHeaderSize {
			return nil, fmt.Errorf("DACL entry %d is too short to hold an access mask and a SID", i)
		}
		mask := binary.LittleEndian.Uint32(ace[aceHeaderSize:])
		name, err := parseSID(ace, aceHeaderSize+aceMaskSize)
		if err != nil {
			return nil, fmt.Errorf("DACL entry %d: %w", i, err)
		}
		allows = append(allows, allowEntry{sid: name, mask: mask})
	}
	return allows, nil
}

// isTrustedPrincipal reports whether sid is this user or one of the system
// principals in trustedSystemSIDs.
func isTrustedPrincipal(sid, userSID string) bool {
	return sid == userSID || slices.Contains(trustedSystemSIDs, sid)
}

// foreignGrant returns the first allow entry that gives any of rights to a
// principal other than this user and the system principals.
//
// OWNER RIGHTS is passed over because it stands for the object's owner. That
// is only sound once the owner has been held to the same set, so callers check
// the owner first.
func (s handleSecurity) foreignGrant(userSID string, rights uint32) (allowEntry, bool) {
	for _, entry := range s.allows {
		if entry.mask&rights == 0 || entry.sid == ownerRightsSID || isTrustedPrincipal(entry.sid, userSID) {
			continue
		}
		return entry, true
	}
	return allowEntry{}, false
}

// describeSID names sid for a message, with the account it resolves to when
// the lookup succeeds.
func describeSID(sid string) string {
	parsed, err := syscall.StringToSid(sid)
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

// VerifyNotWritableByOthers returns nil when no account other than this user
// and the system principals can change the regular file at path: one of them
// owns it, and its DACL gives nobody else a right to write it, delete it, or
// rewrite its security.
//
// It is the Windows form of a Unix mode with the group and other write bits
// clear, which os.FileMode cannot express here. A question it cannot answer is
// an error rather than a pass: a NULL DACL grants every account everything, and
// an entry it cannot interpret could grant anything.
func VerifyNotWritableByOthers(path string) error {
	userSID, err := currentUserSIDString()
	if err != nil {
		return fmt.Errorf("reading current user SID: %w", err)
	}
	handle, err := openVerifiedPath(path, false, readControl|fileReadAttributes, "file")
	if err != nil {
		return fmt.Errorf("opening %q: %w", path, err)
	}
	defer syscall.CloseHandle(handle) //nolint:errcheck

	security, err := readHandleSecurity(handle)
	if err != nil {
		return fmt.Errorf("reading the security of %q: %w", path, err)
	}
	if !isTrustedPrincipal(security.owner, userSID) {
		return fmt.Errorf("%q is owned by %s, which is neither this user nor a system principal", path, describeSID(security.owner))
	}
	if grant, ok := security.foreignGrant(userSID, modifyRights); ok {
		return fmt.Errorf("%q grants %s access %#x, which lets an account other than this user or a system principal change it", path, describeSID(grant.sid), grant.mask)
	}
	return nil
}
