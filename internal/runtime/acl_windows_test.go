//go:build windows

package runtime

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

// icacls runs icacls.exe against path. Principals are named by SID so a case
// does not depend on the machine's display language, and no elevation is
// needed: the owner of an object may rewrite its DACL.
func icacls(t *testing.T, path string, args ...string) {
	t.Helper()
	out, err := exec.Command("icacls.exe", append([]string{path}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("icacls %s %s: %v\n%s", path, strings.Join(args, " "), err, out)
	}
}

// grantEveryone adds an allow entry giving Everyone rights, in icacls notation.
func grantEveryone(t *testing.T, path, rights string) {
	t.Helper()
	icacls(t, path, "/grant", "*S-1-1-0:"+rights)
}

// setNullDACL gives path a NULL DACL, which grants every account full access.
func setNullDACL(t *testing.T, path string, directory bool) {
	t.Helper()
	handle, err := openPrivatePath(path, directory, readControl|writeDAC|fileReadAttributes)
	if err != nil {
		t.Fatalf("opening %q to replace its DACL: %v", path, err)
	}
	defer syscall.CloseHandle(handle) //nolint:errcheck
	if r1, _, _ := procSetSecurityInfo.Call(uintptr(handle), uintptr(seFileObject), uintptr(daclSecurityInformation), 0, 0, 0, 0); r1 != 0 {
		t.Fatalf("setting a NULL DACL on %q: %v", path, syscall.Errno(r1))
	}
	if got := pathDACL(t, path, directory); !strings.Contains(got, "NO_ACCESS_CONTROL") {
		t.Fatalf("precondition: %q has DACL %q, want a NULL DACL", path, got)
	}
}

// setExplicitDACL replaces the explicit entries on path with those in the SDDL
// DACL string. Entries inherited from the parent still apply, so the test keeps
// its own access.
//
// icacls cannot build the deny cases: it adds SYNCHRONIZE to every deny entry,
// and CreateFile asks for SYNCHRONIZE on every synchronous open, so such an
// entry refuses the check's own open instead of exercising how it reads denials.
func setExplicitDACL(t *testing.T, path string, directory bool, sddl string) {
	t.Helper()
	sddlPtr, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		t.Fatalf("encoding %q: %v", sddl, err)
	}
	var descriptor uintptr
	if r1, _, e1 := procConvertStringSecurityDescriptorToSecurityDescriptorW.Call(uintptr(unsafe.Pointer(sddlPtr)), uintptr(sddlRevision1), uintptr(unsafe.Pointer(&descriptor)), 0); r1 == 0 {
		t.Fatalf("parsing %q: %v", sddl, e1)
	}
	defer syscall.LocalFree(syscall.Handle(descriptor)) //nolint:errcheck
	var present, defaulted int32
	var dacl uintptr
	if r1, _, e1 := procGetSecurityDescriptorDACL.Call(descriptor, uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted))); r1 == 0 || present == 0 {
		t.Fatalf("reading the DACL of %q: %v", sddl, e1)
	}
	handle, err := openPrivatePath(path, directory, readControl|writeDAC|fileReadAttributes)
	if err != nil {
		t.Fatalf("opening %q to replace its DACL: %v", path, err)
	}
	defer syscall.CloseHandle(handle) //nolint:errcheck
	if r1, _, _ := procSetSecurityInfo.Call(uintptr(handle), uintptr(seFileObject), uintptr(daclSecurityInformation), 0, 0, dacl, 0); r1 != 0 {
		t.Fatalf("setting DACL %q on %q: %v", sddl, path, syscall.Errno(r1))
	}
	t.Cleanup(func() { _ = exec.Command("icacls.exe", path, "/reset").Run() })
	got := pathDACL(t, path, directory)
	if !strings.Contains(got, "(D;") || !strings.Contains(got, currentSID(t)) {
		t.Fatalf("precondition: %q has DACL %q, want the explicit entries alongside this user's inherited access", path, got)
	}
}

func mkdirAncestor(t *testing.T) string {
	t.Helper()
	ancestor := filepath.Join(t.TempDir(), "ancestor")
	if err := os.Mkdir(ancestor, 0o700); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	return ancestor
}

// TestPrivateAncestryRejectsAGrantThatCanRedirectThePath is the gap an owner
// check leaves: an ancestor the right account owns is still replaceable when
// its DACL lets another account delete, rename or re-permission it. Each such
// right is exercised on its own, against a private directory still to be
// created and against one that already exists.
func TestPrivateAncestryRejectsAGrantThatCanRedirectThePath(t *testing.T) {
	for _, rights := range []string{"(D)", "(DC)", "(WDAC)", "(WO)", "(GA)", "(M)", "(F)"} {
		t.Run(rights, func(t *testing.T) {
			ancestor := mkdirAncestor(t)
			existing := filepath.Join(ancestor, "existing")
			if err := EnsurePrivateDir(existing); err != nil {
				t.Fatalf("EnsurePrivateDir before the grant: %v", err)
			}
			grantEveryone(t, ancestor, rights)

			created := filepath.Join(ancestor, "created")
			for _, target := range []string{created, existing} {
				err := EnsurePrivateDir(target)
				if err == nil {
					t.Fatalf("EnsurePrivateDir(%q) accepted an ancestor that grants Everyone %s", target, rights)
				}
				if !strings.Contains(err.Error(), "S-1-1-0") || !strings.Contains(err.Error(), strconv.Quote(ancestor)) {
					t.Fatalf("EnsurePrivateDir(%q) = %v; the refusal must name the ancestor and the principal", target, err)
				}
			}
			if _, err := os.Lstat(created); !os.IsNotExist(err) {
				t.Fatalf("a refused path was created anyway (Lstat: %v)", err)
			}
		})
	}
}

// TestPrivateAncestryAcceptsGrantsThatCannotRedirectThePath pins the other
// side of the line, so the check cannot drift into refusing ordinary machines.
// Reading, traversing and adding entries are what a volume root grants every
// signed-in account; an inherit-only entry does not apply to the ancestor
// itself; and a deny entry only ever takes access away.
//
// Writing an ancestor's data or attributes is accepted as well. On an empty
// directory that is enough to make it a junction; redirectRights records why
// that residual is accepted.
func TestPrivateAncestryAcceptsGrantsThatCannotRedirectThePath(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, path string){
		"read and traverse":         func(t *testing.T, path string) { grantEveryone(t, path, "(RX)") },
		"add subdirectory":          func(t *testing.T, path string) { grantEveryone(t, path, "(AD)") },
		"write":                     func(t *testing.T, path string) { grantEveryone(t, path, "(W)") },
		"inherit-only full control": func(t *testing.T, path string) { grantEveryone(t, path, "(OI)(CI)(IO)(F)") },
		"deny delete":               func(t *testing.T, path string) { setExplicitDACL(t, path, true, "D:(D;;SD;;;WD)") },
	} {
		t.Run(name, func(t *testing.T) {
			ancestor := mkdirAncestor(t)
			prepare(t, ancestor)
			if err := EnsurePrivateDir(filepath.Join(ancestor, "state")); err != nil {
				t.Fatalf("EnsurePrivateDir under an ancestor with %s: %v", name, err)
			}
		})
	}
}

// TestEnsurePrivateDirNarrowsAnExistingDirectoryWithAForeignGrant keeps
// adoption working under the ancestor check. The directory at the path itself
// is about to be narrowed to owner-only, so a grant on it is repaired, as chmod
// repairs a wide mode on Unix, rather than refused as it is one level up.
func TestEnsurePrivateDirNarrowsAnExistingDirectoryWithAForeignGrant(t *testing.T) {
	sid := currentSID(t)
	dir := mkdirAncestor(t)
	grantEveryone(t, dir, "(F)")

	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir must narrow the directory it is asked to make private, not refuse it: %v", err)
	}
	assertOwnerOnly(t, "adopted directory", dirDACL(t, dir), sid, true)
}

// TestPrivateAncestryRejectsANullDACL covers the descriptor that grants the
// most while listing nothing. Read as "no entries, so nothing granted", a NULL
// DACL would pass the widest ancestor there is.
func TestPrivateAncestryRejectsANullDACL(t *testing.T) {
	ancestor := mkdirAncestor(t)
	setNullDACL(t, ancestor, true)

	err := EnsurePrivateDir(filepath.Join(ancestor, "state"))
	if err == nil {
		t.Fatal("EnsurePrivateDir accepted an ancestor with a NULL DACL")
	}
	if !strings.Contains(err.Error(), "grants every account full access") {
		t.Fatalf("refusal was %v; it must say the DACL grants everyone everything", err)
	}
}

func TestVerifyNotWritableByOthersAcceptsAFileCreatedHere(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := VerifyNotWritableByOthers(path); err != nil {
		t.Fatalf("a file this user just created, in a directory under its profile, was refused: %v", err)
	}
}

// TestVerifyNotWritableByOthersRejectsAWriteGrant exercises each right that
// lets another account change a file, including the generic form of write.
func TestVerifyNotWritableByOthersRejectsAWriteGrant(t *testing.T) {
	for _, rights := range []string{"(W)", "(WD)", "(AD)", "(WA)", "(WEA)", "(D)", "(WDAC)", "(WO)", "(GW)", "(GA)", "(M)"} {
		t.Run(rights, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			grantEveryone(t, path, rights)

			err := VerifyNotWritableByOthers(path)
			if err == nil {
				t.Fatalf("a file granting Everyone %s was accepted", rights)
			}
			if !strings.Contains(err.Error(), "S-1-1-0") {
				t.Fatalf("refusal was %v; it must name the principal", err)
			}
		})
	}
}

func TestVerifyNotWritableByOthersAcceptsReadingAndDenials(t *testing.T) {
	for name, prepare := range map[string]func(t *testing.T, path string){
		"read and execute": func(t *testing.T, path string) { grantEveryone(t, path, "(RX)") },
		// 0x116 is write data, append data, write extended attributes and
		// write attributes: FILE_GENERIC_WRITE without SYNCHRONIZE.
		"deny write": func(t *testing.T, path string) { setExplicitDACL(t, path, false, "D:(D;;0x116;;;WD)") },
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			prepare(t, path)
			if err := VerifyNotWritableByOthers(path); err != nil {
				t.Fatalf("a file with %s was refused: %v", name, err)
			}
		})
	}
}

func TestVerifyNotWritableByOthersRejectsANullDACL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	setNullDACL(t, path, false)

	err := VerifyNotWritableByOthers(path)
	if err == nil || !strings.Contains(err.Error(), "grants every account full access") {
		t.Fatalf("VerifyNotWritableByOthers on a NULL DACL = %v, want a refusal saying it grants everyone everything", err)
	}
}

func TestVerifyNotWritableByOthersRejectsADirectory(t *testing.T) {
	if err := VerifyNotWritableByOthers(t.TempDir()); err == nil {
		t.Fatal("VerifyNotWritableByOthers accepted a directory; it answers only for regular files")
	}
}

func sidBytes(authority byte, subAuthorities ...uint32) []byte {
	sid := []byte{sidRevision, byte(len(subAuthorities)), 0, 0, 0, 0, 0, authority}
	for _, sub := range subAuthorities {
		sid = binary.LittleEndian.AppendUint32(sid, sub)
	}
	return sid
}

func aceBytes(aceType, flags byte, mask uint32, sid []byte, trailing ...byte) []byte {
	ace := []byte{aceType, flags}
	ace = binary.LittleEndian.AppendUint16(ace, uint16(aceHeaderSize+aceMaskSize+len(sid)+len(trailing)))
	ace = binary.LittleEndian.AppendUint32(ace, mask)
	ace = append(ace, sid...)
	return append(ace, trailing...)
}

func aclBytes(aces ...[]byte) []byte {
	size := aclHeaderSize
	for _, ace := range aces {
		size += len(ace)
	}
	acl := []byte{2, 0}
	acl = binary.LittleEndian.AppendUint16(acl, uint16(size))
	acl = binary.LittleEndian.AppendUint16(acl, uint16(len(aces)))
	acl = append(acl, 0, 0)
	for _, ace := range aces {
		acl = append(acl, ace...)
	}
	return acl
}

func TestParseAllowEntriesReadsTheEntriesThatApplyToTheObject(t *testing.T) {
	everyone := sidBytes(1, 0)
	acl := aclBytes(
		aceBytes(aceTypeAccessAllowed, 0, 0x001F01FF, everyone),
		aceBytes(aceTypeAccessAllowed, aceFlagInheritOnly|0x03, 0x001F01FF, sidBytes(5, 18)),
		aceBytes(aceTypeAccessDenied, 0, accessDelete, everyone),
		aceBytes(aceTypeAccessAllowedCallback, 0, accessDelete, sidBytes(5, 11), 'a', 'r', 't', 'x'),
	)
	got, err := parseAllowEntries(acl)
	if err != nil {
		t.Fatalf("parseAllowEntries: %v", err)
	}
	want := []allowEntry{{sid: "S-1-1-0", mask: 0x001F01FF}, {sid: "S-1-5-11", mask: accessDelete}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAllowEntries = %+v, want %+v (inherit-only and deny entries skipped, a callback entry read like any allow)", got, want)
	}
}

// TestParseAllowEntriesRefusesWhatItCannotRead holds the parser to failing
// closed: every layout it cannot read with certainty is an error, because
// an entry skipped for being unreadable could be the one granting access.
func TestParseAllowEntriesRefusesWhatItCannotRead(t *testing.T) {
	everyone := sidBytes(1, 0)
	valid := aclBytes(aceBytes(aceTypeAccessAllowed, 0, accessDelete, everyone))
	withByte := func(offset int, value byte) []byte {
		acl := append([]byte(nil), valid...)
		acl[offset] = value
		return acl
	}
	truncatedSID := sidBytes(5, 21, 1)[:sidHeaderSize+4]
	cases := map[string][]byte{
		"header shorter than an ACL header":   valid[:aclHeaderSize-1],
		"entry count past the end":            withByte(4, 2),
		"entry size below an entry header":    withByte(aclHeaderSize+2, aceHeaderSize-1),
		"entry size past the end":             withByte(aclHeaderSize+2, byte(len(valid))),
		"allowed-object entry":                aclBytes(aceBytes(0x05, 0, accessDelete, everyone)),
		"system-audit entry":                  aclBytes(aceBytes(0x02, 0, accessDelete, everyone)),
		"entry too short for a mask and SID":  aclBytes([]byte{aceTypeAccessAllowed, 0, 8, 0, 0, 0, 1, 0}),
		"SID with an unknown revision":        withByte(aclHeaderSize+aceHeaderSize+aceMaskSize, 2),
		"SID with too many subauthorities":    withByte(aclHeaderSize+aceHeaderSize+aceMaskSize+1, sidMaxSubAuthorities+1),
		"SID shorter than its subauthorities": aclBytes(aceBytes(aceTypeAccessAllowed, 0, accessDelete, truncatedSID)),
	}
	for name, acl := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := parseAllowEntries(acl); err == nil {
				t.Fatalf("parseAllowEntries accepted it and returned %+v", got)
			}
		})
	}
}

func TestForeignGrantPassesOverTrustedPrincipalsOnly(t *testing.T) {
	const user = "S-1-5-21-1-2-3-1001"
	const readExecute = 0x001200A9
	security := handleSecurity{owner: user, allows: []allowEntry{
		{sid: user, mask: accessGenericAll},
		{sid: "S-1-5-18", mask: accessGenericAll},
		{sid: "S-1-5-32-544", mask: accessGenericAll},
		{sid: "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464", mask: accessGenericAll},
		{sid: ownerRightsSID, mask: accessGenericAll},
		{sid: "S-1-1-0", mask: readExecute},
		{sid: "S-1-5-11", mask: fileAppendData},
	}}
	if grant, ok := security.foreignGrant(user, redirectRights); ok {
		t.Fatalf("foreignGrant(redirectRights) = %+v; nothing here can redirect a path", grant)
	}
	grant, ok := security.foreignGrant(user, modifyRights)
	if !ok || grant.sid != "S-1-5-11" {
		t.Fatalf("foreignGrant(modifyRights) = %+v, %v; want the append grant to Authenticated Users", grant, ok)
	}
}

// descriptorBytes lays out a self-relative security descriptor: the header,
// then the owner SID, then the DACL. A zero offset is written for an absent
// part, as Windows does.
func descriptorBytes(control uint16, owner, dacl []byte) []byte {
	descriptor := []byte{1, 0}
	descriptor = binary.LittleEndian.AppendUint16(descriptor, control)
	ownerOffset, daclOffset := 0, 0
	if owner != nil {
		ownerOffset = securityDescriptorHeaderSize
	}
	if dacl != nil {
		daclOffset = securityDescriptorHeaderSize + len(owner)
	}
	descriptor = binary.LittleEndian.AppendUint32(descriptor, uint32(ownerOffset))
	descriptor = binary.LittleEndian.AppendUint32(descriptor, 0) // group
	descriptor = binary.LittleEndian.AppendUint32(descriptor, 0) // SACL
	descriptor = binary.LittleEndian.AppendUint32(descriptor, uint32(daclOffset))
	descriptor = append(descriptor, owner...)
	return append(descriptor, dacl...)
}

func TestParseSecurityDescriptorReadsTheOwnerAndTheObjectsEntries(t *testing.T) {
	dacl := aclBytes(
		aceBytes(aceTypeAccessAllowed, 0, accessDelete, sidBytes(1, 0)),
		aceBytes(aceTypeAccessAllowed, aceFlagInheritOnly, accessDelete, sidBytes(5, 11)),
	)
	got, err := parseSecurityDescriptor(descriptorBytes(seSelfRelative|seDACLPresent, sidBytes(5, 18), dacl))
	if err != nil {
		t.Fatalf("parseSecurityDescriptor: %v", err)
	}
	want := handleSecurity{owner: "S-1-5-18", allows: []allowEntry{{sid: "S-1-1-0", mask: accessDelete}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSecurityDescriptor = %+v, want %+v", got, want)
	}
}

// TestParseSecurityDescriptorRefusesWhatItCannotRead extends the parser's
// fail-closed rule to the descriptor around the DACL. The two DACL cases are
// the ones that matter most: no DACL and a NULL DACL each grant every account
// full access, and both look like "no entries" to a reader that only counts.
func TestParseSecurityDescriptorRefusesWhatItCannotRead(t *testing.T) {
	owner := sidBytes(5, 18)
	dacl := aclBytes(aceBytes(aceTypeAccessAllowed, 0, accessDelete, sidBytes(1, 0)))
	valid := descriptorBytes(seSelfRelative|seDACLPresent, owner, dacl)
	withUint32 := func(offset int, value uint32) []byte {
		descriptor := append([]byte(nil), valid...)
		binary.LittleEndian.PutUint32(descriptor[offset:], value)
		return descriptor
	}
	daclOffset := securityDescriptorHeaderSize + len(owner)
	oversizedDACL := append([]byte(nil), valid...)
	binary.LittleEndian.PutUint16(oversizedDACL[daclOffset+2:], uint16(len(dacl)+1))
	cases := map[string]struct {
		descriptor []byte
		want       string
	}{
		"shorter than its header":  {valid[:securityDescriptorHeaderSize-1], "shorter than its own header"},
		"absolute form":            {descriptorBytes(seDACLPresent, owner, dacl), "self-relative"},
		"no owner":                 {descriptorBytes(seSelfRelative|seDACLPresent, nil, dacl), "owner"},
		"owner past the end":       {withUint32(4, uint32(len(valid))), "owner"},
		"no DACL":                  {descriptorBytes(seSelfRelative, owner, dacl), "grants every account full access"},
		"NULL DACL":                {descriptorBytes(seSelfRelative|seDACLPresent, owner, nil), "grants every account full access"},
		"DACL offset past the end": {withUint32(16, uint32(len(valid))), "outside"},
		"DACL inside the header":   {withUint32(16, 4), "outside"},
		"DACL size past the end":   {oversizedDACL, "runs past the end"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parseSecurityDescriptor(tc.descriptor)
			if err == nil {
				t.Fatalf("parseSecurityDescriptor accepted it and returned %+v", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseSecurityDescriptor error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}
