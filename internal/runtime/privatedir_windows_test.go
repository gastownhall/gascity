//go:build windows

package runtime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The private-state trust boundary on Windows is a DACL, not a mode. These
// cases hold it to the same promise chmod 0700 makes on Unix: nobody but the
// owner can read the contents, inherited access from above does not apply, and
// a secret is never briefly readable on its way to disk.

// dirDACL reads the DACL of a directory as SDDL.
func dirDACL(t *testing.T, path string) string {
	t.Helper()
	return pathDACL(t, path, true)
}

// fileDACL reads the DACL of a regular file as SDDL.
func fileDACL(t *testing.T, path string) string {
	t.Helper()
	return pathDACL(t, path, false)
}

func pathDACL(t *testing.T, path string, directory bool) string {
	t.Helper()
	handle, err := openPrivatePath(path, directory, readControl|fileReadAttributes)
	if err != nil {
		t.Fatalf("opening %q to read its DACL: %v", path, err)
	}
	defer syscall.CloseHandle(handle) //nolint:errcheck
	dacl, err := handleDACLString(handle)
	if err != nil {
		t.Fatalf("reading DACL of %q: %v", path, err)
	}
	return dacl
}

func currentSID(t *testing.T) string {
	t.Helper()
	sid, err := currentUserSIDString()
	if err != nil {
		t.Fatalf("reading current user SID: %v", err)
	}
	return sid
}

// assertOwnerOnly holds a DACL to every part of the contract at once: it is
// protected (so nothing is inherited from the parent), it carries exactly one
// access-allowed ACE, that ACE grants full access to the owner and to nobody
// else, and there are no deny ACEs hiding in it.
func assertOwnerOnly(t *testing.T, label, dacl, sid string, directory bool) {
	t.Helper()
	if !strings.HasPrefix(dacl, "D:P") {
		t.Fatalf("%s DACL %q is not protected; access granted on an ancestor would still apply here", label, dacl)
	}
	if n := strings.Count(dacl, "(A;"); n != 1 {
		t.Fatalf("%s DACL %q has %d access-allowed ACEs, want exactly one for the owner", label, dacl, n)
	}
	if strings.Contains(dacl, "(D;") {
		t.Fatalf("%s DACL %q contains a deny ACE", label, dacl)
	}
	if !strings.Contains(dacl, sid) {
		t.Fatalf("%s DACL %q does not name the owner SID %s", label, dacl, sid)
	}
	// Anything other than the owner's SID in an ACE is another trustee.
	for _, wellKnown := range []string{"WD", "BA", "SY", "BU", "AU", "IU", "WR"} {
		if strings.Contains(dacl, ";"+wellKnown+")") {
			t.Fatalf("%s DACL %q grants access to %s; private state must be owner-only", label, dacl, wellKnown)
		}
	}
	flags := aceFlags(t, dacl)
	if directory {
		if !strings.Contains(flags, "OI") || !strings.Contains(flags, "CI") {
			t.Fatalf("%s DACL %q has ACE flags %q, want object and container inheritance (OI and CI) so children are owner-only too", label, dacl, flags)
		}
	} else if flags != "" {
		t.Fatalf("%s DACL %q has ACE flags %q, want none on a leaf file", label, dacl, flags)
	}
}

// aceFlags returns the flags field of the first ACE in an SDDL DACL, which is
// the second semicolon-separated field inside the parentheses.
func aceFlags(t *testing.T, dacl string) string {
	t.Helper()
	open := strings.Index(dacl, "(")
	close := strings.Index(dacl, ")")
	if open < 0 || close < open {
		t.Fatalf("DACL %q has no ACE to read flags from", dacl)
	}
	fields := strings.Split(dacl[open+1:close], ";")
	if len(fields) < 2 {
		t.Fatalf("ACE %q in DACL %q has too few fields", dacl[open+1:close], dacl)
	}
	return fields[1]
}

// assertInheritedOwnerOnly holds a child's DACL to the inherited form of the
// same promise. An inherited DACL is marked auto-inherited rather than
// protected -- protection describes an explicitly set descriptor, and a child
// that carried it would not be inheriting at all -- so what matters here is
// that exactly one ACE arrived and it grants the owner and nobody else.
func assertInheritedOwnerOnly(t *testing.T, label, dacl, sid string) {
	t.Helper()
	if !strings.Contains(dacl, "AI") {
		t.Fatalf("%s DACL %q is not marked auto-inherited; it did not come from the private parent", label, dacl)
	}
	if n := strings.Count(dacl, "(A;"); n != 1 {
		t.Fatalf("%s DACL %q has %d access-allowed ACEs, want exactly one inherited from the owner-only parent", label, dacl, n)
	}
	if strings.Contains(dacl, "(D;") {
		t.Fatalf("%s DACL %q contains a deny ACE", label, dacl)
	}
	if !strings.Contains(dacl, sid) {
		t.Fatalf("%s DACL %q does not name the owner SID %s", label, dacl, sid)
	}
	for _, wellKnown := range []string{"WD", "BA", "SY", "BU", "AU", "IU", "WR"} {
		if strings.Contains(dacl, ";"+wellKnown+")") {
			t.Fatalf("%s DACL %q grants access to %s; a child of a private directory must be owner-only too", label, dacl, wellKnown)
		}
	}
	if !strings.Contains(aceFlags(t, dacl), "ID") {
		t.Fatalf("%s ACE flags %q do not mark the ACE inherited", label, aceFlags(t, dacl))
	}
}

func TestEnsurePrivateDirProducesAProtectedOwnerOnlyDACL(t *testing.T) {
	sid := currentSID(t)
	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	assertOwnerOnly(t, "private directory", dirDACL(t, dir), sid, true)
}

// TestEnsurePrivateDirAppliesToEveryCreatedAncestor pins that an intermediate
// directory is not left wide open. A readable parent is enough to enumerate
// what lives beneath it.
func TestEnsurePrivateDirAppliesToEveryCreatedAncestor(t *testing.T) {
	sid := currentSID(t)
	root := t.TempDir()
	leaf := filepath.Join(root, "a", "b", "c")
	if err := EnsurePrivateDir(leaf); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	for _, dir := range []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b"), leaf} {
		assertOwnerOnly(t, "created ancestor "+dir, dirDACL(t, dir), sid, true)
	}
}

// TestPrivateDirChildInheritsOwnerOnlyAccess is the point of the OI|CI flags.
// Without inheritance a child gets the token's default DACL, and with Bypass
// Traverse Checking enabled the parent's ACL does not gate reads of it the way
// a 0700 parent would on Unix.
func TestPrivateDirChildInheritsOwnerOnlyAccess(t *testing.T) {
	sid := currentSID(t)
	dir := filepath.Join(t.TempDir(), "state")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}

	child := filepath.Join(dir, "child.txt")
	if err := os.WriteFile(child, []byte("inherited"), 0o600); err != nil {
		t.Fatalf("WriteFile in private dir: %v", err)
	}
	assertInheritedOwnerOnly(t, "inherited child file", fileDACL(t, child), sid)

	childDir := filepath.Join(dir, "sub")
	if err := os.Mkdir(childDir, 0o700); err != nil {
		t.Fatalf("Mkdir in private dir: %v", err)
	}
	assertInheritedOwnerOnly(t, "inherited child directory", dirDACL(t, childDir), sid)
}

// TestEnsurePrivateDirTightensAPreExistingWideDirectory covers adoption: an
// existing directory must be narrowed, not accepted as-is, because creation
// succeeding says nothing about who could already read it.
func TestEnsurePrivateDirTightensAPreExistingWideDirectory(t *testing.T) {
	sid := currentSID(t)
	dir := filepath.Join(t.TempDir(), "adopted")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	// A freshly created directory inherits from its parent, so it starts
	// unprotected; that is the state being adopted.
	if before := dirDACL(t, dir); strings.HasPrefix(before, "D:P") {
		t.Fatalf("precondition: %q is already protected (%q); the adoption path is not being exercised", dir, before)
	}

	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir on an existing wide dir must tighten, not fail: %v", err)
	}
	assertOwnerOnly(t, "tightened directory", dirDACL(t, dir), sid, true)
}

// TestEnsurePrivateDirNeverReportsAWideDirectoryAtThePathSecured is the leaf
// that moved while it was tightened: renamed away once opened, with a wide
// directory put at its name, so the call narrowed the renamed one and returned
// nil over the replacement. Whatever stops it, a nil return
// must mean the directory at the path is owner-only.
func TestEnsurePrivateDirNeverReportsAWideDirectoryAtThePathSecured(t *testing.T) {
	sid := currentSID(t)
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	hookRan := false
	var moveErr error
	testHookPrivateDirOpened = func(path string) {
		if path != dir || hookRan {
			return
		}
		hookRan = true
		if moveErr = os.Rename(dir, dir+".moved"); moveErr != nil {
			return
		}
		if err := os.Mkdir(dir, 0o777); err != nil {
			t.Errorf("putting a replacement at %q: %v", dir, err)
			return
		}
		grantEveryone(t, dir, "(OI)(CI)F")
	}
	t.Cleanup(func() { testHookPrivateDirOpened = nil })

	err := EnsurePrivateDir(dir)
	if !hookRan {
		t.Fatal("the directory was never opened for tightening")
	}
	if err != nil {
		if moveErr != nil {
			t.Fatalf("EnsurePrivateDir = %v with the directory unmoved", err)
		}
		return
	}
	assertOwnerOnly(t, "the directory at the path EnsurePrivateDir secured", dirDACL(t, dir), sid, true)
}

// TestEnsurePrivateDirRefusesWhileAnotherHandleCanRenameIt is the handle a
// narrowed DACL does not reach: one opened with delete access before the
// directory was tightened can rename it away afterwards and let a wide
// directory take its name. While such a handle is open, the directory is not
// confirmed private.
func TestEnsurePrivateDirRefusesWhileAnotherHandleCanRenameIt(t *testing.T) {
	sid := currentSID(t)
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	name, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		t.Fatalf("encoding %q: %v", dir, err)
	}
	const deleteAccess = 0x00010000
	held, err := syscall.CreateFile(name, deleteAccess, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatalf("opening %q with delete access: %v", dir, err)
	}
	closeHeld := func() {
		if held != syscall.InvalidHandle {
			_ = syscall.CloseHandle(held)
			held = syscall.InvalidHandle
		}
	}
	t.Cleanup(closeHeld)

	if err := EnsurePrivateDir(dir); err == nil {
		t.Fatal("EnsurePrivateDir reported the directory private while another handle could still rename it away")
	}
	closeHeld()
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir once that handle was closed: %v", err)
	}
	assertOwnerOnly(t, "confirmed directory", dirDACL(t, dir), sid, true)
}

func TestVerifyPathStillNamesTellsTwoDirectoriesApart(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	handle, err := openPrivatePath(first, true, fileReadAttributes)
	if err != nil {
		t.Fatalf("opening %q: %v", first, err)
	}
	defer syscall.CloseHandle(handle) //nolint:errcheck

	if err := verifyPathStillNames(handle, first); err != nil {
		t.Errorf("verifyPathStillNames(handle to %q, %q) = %v, want nil", first, first, err)
	}
	if err := verifyPathStillNames(handle, second); err == nil {
		t.Errorf("verifyPathStillNames(handle to %q, %q) = nil; they are different directories", first, second)
	}
}

func TestEnsurePrivateDirRejectsANonDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := EnsurePrivateDir(path); err == nil {
		t.Fatal("EnsurePrivateDir on a regular file returned nil")
	}
}

// makeJunction creates a directory junction, the reparse point an unprivileged
// user can make on Windows without developer mode.
func makeJunction(t *testing.T, link, target string) {
	t.Helper()
	cmd := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot create a junction here (%v): %s", err, out)
	}
}

// TestEnsurePrivateDirRejectsAJunction is the hostile-precreation case. An
// attacker who wins the race to create the path can point it at a directory
// they control; following it would apply the owner-only DACL to their
// directory and then write secrets into it.
func TestEnsurePrivateDirRejectsAJunction(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir target: %v", err)
	}
	link := filepath.Join(root, "link")
	makeJunction(t, link, target)

	err := EnsurePrivateDir(link)
	if err == nil {
		t.Fatal("EnsurePrivateDir followed a junction; a reparse point must be refused, not adopted")
	}
	// Go's Lstat reports a junction as an irregular non-directory rather than a
	// symlink, so the refusal arrives through the type check rather than the
	// symlink check. Either way it must be a refusal, and the attacker's
	// directory must be left exactly as it was.
	targetDACLBefore := dirDACL(t, target)
	if err := EnsurePrivateDir(link); err == nil {
		t.Fatal("EnsurePrivateDir accepted the junction on a second call")
	}
	if got := dirDACL(t, target); got != targetDACLBefore {
		t.Fatalf("the junction's target had its DACL rewritten to %q; the refusal must not have touched it", got)
	}

	// The real directory behind the junction must still be usable by name.
	if err := EnsurePrivateDir(target); err != nil {
		t.Fatalf("EnsurePrivateDir on the junction's target by its real path: %v", err)
	}
}

// TestEnsurePrivateDirRejectsAJunctionedAncestor covers the same attack one
// level up: the final component is innocent, but an ancestor redirects the
// whole subtree.
func TestEnsurePrivateDirRejectsAJunctionedAncestor(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir target: %v", err)
	}
	link := filepath.Join(root, "link")
	makeJunction(t, link, target)

	if err := EnsurePrivateDir(filepath.Join(link, "state")); err == nil {
		t.Fatal("EnsurePrivateDir created state under a junctioned ancestor")
	}
}

// TestEnsurePrivateDirRejectsAJunctionedAncestorOfAnExistingDirectory is the
// case a check that stops at the deepest existing ancestor cannot see.
//
// When the private directory already exists, that deepest existing ancestor is
// the directory itself — so the walk ended immediately and nothing above it was
// ever examined. Reached through a junction the directory is real, is not a
// reparse point, and carries a perfectly correct owner-only DACL. Everything
// the check looked at was right; the location was not.
func TestEnsurePrivateDirRejectsAJunctionedAncestorOfAnExistingDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir target: %v", err)
	}
	state := filepath.Join(target, "state")
	if err := EnsurePrivateDir(state); err != nil {
		t.Fatalf("EnsurePrivateDir on the real path: %v", err)
	}
	link := filepath.Join(root, "link")
	makeJunction(t, link, target)

	err := EnsurePrivateDir(filepath.Join(link, "state"))
	if err == nil {
		t.Fatal("EnsurePrivateDir accepted an existing private directory reached through a junctioned ancestor")
	}
	if !strings.Contains(err.Error(), "reparse point") {
		t.Fatalf("refusal was %v; it must name the reparse point in the ancestry", err)
	}
}

// TestPrivateAncestryAcceptsAStockSystemChain guards the trusted-owner list
// against rotting into something no real machine satisfies.
//
// The directories a private path is reached through are owned by Windows, not
// by the user: on a stock install the chain up from the per-user temp
// directory runs through Administrators, SYSTEM and TrustedInstaller. If any of
// those were dropped, no private directory could be created anywhere.
func TestPrivateAncestryAcceptsAStockSystemChain(t *testing.T) {
	sid := currentSID(t)
	if err := verifyPrivateAncestry(t.TempDir(), sid); err != nil {
		t.Fatalf("the chain above a temp directory was refused: %v", err)
	}
	volume := filepath.VolumeName(os.TempDir()) + `\`
	if err := verifyAncestorComponent(volume, sid); err != nil {
		t.Fatalf("the volume root %q was refused: %v", volume, err)
	}
}

// TestPrivateAncestryRejectsAnUntrustedOwner exercises the refusal itself.
//
// An ancestor owned by another ordinary account cannot be manufactured from a
// test without a second account, so the trusted set is emptied instead: that
// makes the system-owned ancestors stand in for the untrusted one, which is the
// same code path and the same message.
func TestPrivateAncestryRejectsAnUntrustedOwner(t *testing.T) {
	original := trustedSystemSIDs
	t.Cleanup(func() { trustedSystemSIDs = original })
	trustedSystemSIDs = nil

	// A SID that is not this user, so no ancestor can match on identity.
	const stranger = "S-1-5-21-0-0-0-1000"
	err := verifyPrivateAncestry(t.TempDir(), stranger)
	if err == nil {
		t.Fatal("an ancestry owned by nobody trusted was accepted")
	}
	if !strings.Contains(err.Error(), "owned by SID") {
		t.Fatalf("refusal was %v; it must name the owner that failed the check", err)
	}
}

func TestWritePrivateFileProducesAProtectedOwnerOnlyDACL(t *testing.T) {
	sid := currentSID(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := WritePrivateFile(path, []byte("tok-1")); err != nil {
		t.Fatalf("WritePrivateFile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "tok-1" {
		t.Fatalf("ReadFile = %q, want %q", data, "tok-1")
	}
	assertOwnerOnly(t, "private file", fileDACL(t, path), sid, false)
}

// TestPrivateFileIsOwnerOnlyBeforeAnySecretIsWritten is the ordering
// guarantee, and it is the one an end-to-end check cannot see. If the
// descriptor were applied after the bytes, the secret would exist on disk
// under an inherited DACL for the length of the write — brief, but a window
// chmod 0700 never opens because the file is created private.
//
// createPrivateTempFile is called directly and nothing is written through the
// returned handle, so the DACL observed here is the one the file was born
// with.
func TestPrivateFileIsOwnerOnlyBeforeAnySecretIsWritten(t *testing.T) {
	sid := currentSID(t)
	dir := t.TempDir()

	f, tmp, err := createPrivateTempFile(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatalf("createPrivateTempFile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.Close()
		_ = os.Remove(tmp)
	})

	info, err := os.Stat(tmp)
	if err != nil {
		t.Fatalf("Stat temp file: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("temp file already holds %d bytes; the DACL check below would not prove the ordering", info.Size())
	}
	assertOwnerOnly(t, "unwritten private temp file", fileDACL(t, tmp), sid, false)
}

// TestWritePrivateFileNarrowsAPreExistingWideFile pins that an existing
// readable file is not adopted as-is.
func TestWritePrivateFileNarrowsAPreExistingWideFile(t *testing.T) {
	sid := currentSID(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("old"), 0o666); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if before := fileDACL(t, path); strings.HasPrefix(before, "D:P") {
		t.Fatalf("precondition: %q is already protected (%q)", path, before)
	}

	if err := WritePrivateFile(path, []byte("tok-2")); err != nil {
		t.Fatalf("WritePrivateFile: %v", err)
	}
	assertOwnerOnly(t, "narrowed private file", fileDACL(t, path), sid, false)
}

// openWithDeleteSharing opens path for reading with full sharing, including
// FILE_SHARE_DELETE. That is what a Windows reader must request for a
// concurrent atomic replacement of the file to remain possible; os.Open does
// not ask for it.
func openWithDeleteSharing(t *testing.T, path string) *os.File {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatalf("UTF16PtrFromString(%q): %v", path, err)
	}
	handle, err := syscall.CreateFile(
		name,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		t.Fatalf("opening %q with delete sharing: %v", path, err)
	}
	f := os.NewFile(uintptr(handle), path)
	if f == nil {
		_ = syscall.CloseHandle(handle)
		t.Fatalf("os.NewFile returned nil for %q", path)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestWritePrivateFileNeverWritesTheDestinationInPlace pins the atomic
// replacement at the point it is decided: the secret is written to a fresh
// temp file and the destination is swapped to it, so a reader either sees the
// whole old value or the whole new one and never a truncated or torn file.
//
// The two steps are driven separately so the intermediate state is
// observable. Holding the destination open across a real WritePrivateFile
// cannot be used to show this on Windows -- see
// TestWritePrivateFileCannotReplaceADestinationHeldOpen, which pins why.
func TestWritePrivateFileNeverWritesTheDestinationInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := WritePrivateFile(path, []byte("first-secret")); err != nil {
		t.Fatalf("first WritePrivateFile: %v", err)
	}

	f, tmp, err := createPrivateTempFile(path)
	if err != nil {
		t.Fatalf("createPrivateTempFile: %v", err)
	}
	if tmp == path {
		t.Fatal("createPrivateTempFile handed back the destination itself; the write would land in place")
	}
	if _, err := f.Write([]byte("second")); err != nil {
		t.Fatalf("writing the new secret to the temp file: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing the temp file: %v", err)
	}

	// The whole new secret is on disk, and the destination still holds the
	// whole old one. That is the window an in-place rewrite would corrupt.
	mid, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile before the swap: %v", err)
	}
	if string(mid) != "first-secret" {
		t.Fatalf("destination = %q after the new secret was written, want the original %q untouched until the swap", mid, "first-secret")
	}

	if err := replacePrivateFile(tmp, path); err != nil {
		t.Fatalf("replacePrivateFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile after the swap: %v", err)
	}
	if string(data) != "second" {
		t.Fatalf("destination = %q after the swap, want %q", data, "second")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("the temp file survived the swap (stat err = %v); a second copy of the secret must not remain", err)
	}
}

// TestWritePrivateFileCannotReplaceADestinationHeldOpen records a real
// Windows-only limitation and the direction it fails in.
//
// On Unix a name and an open file are independent, so renaming over a file
// somebody is reading always succeeds. Windows has no equivalent: os.Rename
// goes through MoveFileEx, and its replace path cannot swap a destination that
// any handle holds open -- not even one opened with FILE_SHARE_DELETE, which
// is the sharing mode that makes deletion possible. Both cases are pinned here
// so a future change to ReplaceFile or FileRenameInfoEx shows up as this test
// changing rather than as silent new behaviour.
//
// What matters for safety is the direction: WritePrivateFile retries briefly
// and then reports the failure, leaving the destination exactly as it was. The
// caller is told the secret was not written rather than handed a success over
// a stale value.
func TestWritePrivateFileCannotReplaceADestinationHeldOpen(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(t *testing.T, path string) *os.File
	}{
		{
			name: "reader without delete sharing",
			open: func(t *testing.T, path string) *os.File {
				t.Helper()
				f, err := os.Open(path)
				if err != nil {
					t.Fatalf("os.Open: %v", err)
				}
				t.Cleanup(func() { _ = f.Close() })
				return f
			},
		},
		{
			name: "reader with delete sharing",
			open: openWithDeleteSharing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "token")
			if err := WritePrivateFile(path, []byte("first-secret")); err != nil {
				t.Fatalf("first WritePrivateFile: %v", err)
			}
			tc.open(t, path)

			if err := WritePrivateFile(path, []byte("second")); err == nil {
				t.Skip("this filesystem allowed the replacement while the destination was open; the limitation does not apply here")
			}

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile after the failed write: %v", err)
			}
			if string(data) != "first-secret" {
				t.Fatalf("destination = %q after a failed write, want the original %q left intact", data, "first-secret")
			}

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("ReadDir: %v", err)
			}
			if len(entries) != 1 || entries[0].Name() != "token" {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Fatalf("directory holds %v after a failed write, want only the untouched destination", names)
			}
		})
	}
}

// TestWritePrivateFileLeavesNoTempBehind pins that the secret does not survive
// in a second, unreferenced copy.
func TestWritePrivateFileLeavesNoTempBehind(t *testing.T) {
	dir := t.TempDir()
	if err := WritePrivateFile(filepath.Join(dir, "token"), []byte("tok-3")); err != nil {
		t.Fatalf("WritePrivateFile: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "token" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("directory holds %v, want only the destination file", names)
	}
}

// TestWritePrivateFileDoesNotValidateAncestors records where the protection
// stops, rather than asserting one that was never offered.
//
// WritePrivateFile protects the file it creates: owner-only from birth, and
// refused outright if the file itself turns out to be a reparse point. It does
// not walk the directories above it, and the Unix build does not either --
// O_NOFOLLOW constrains only the final component. Validating ancestors is
// EnsurePrivateDir's job, and the documented flow calls it first.
//
// So a caller that writes into a redirected directory it never validated gets
// no ancestor protection here. What still holds, and is what keeps the secret
// from being disclosed, is that the file is owner-only wherever it lands: the
// attacker who owns the redirected directory cannot read it.
func TestWritePrivateFileDoesNotValidateAncestors(t *testing.T) {
	sid := currentSID(t)
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir target: %v", err)
	}
	link := filepath.Join(root, "link")
	makeJunction(t, link, target)

	// The gate the documented flow relies on must refuse this parent.
	if err := EnsurePrivateDir(filepath.Join(link, "state")); err == nil {
		t.Fatal("EnsurePrivateDir accepted a junctioned ancestor; the ancestor gate is what protects this path")
	}

	// Skipping that gate writes through the junction, which is the boundary
	// being recorded. The file must still be owner-only where it lands.
	if err := WritePrivateFile(filepath.Join(link, "token"), []byte("secret")); err != nil {
		t.Skipf("WritePrivateFile through a junction failed with %v; the landed-file guarantee cannot be checked here", err)
	}
	landed := filepath.Join(target, "token")
	if _, err := os.Stat(landed); err != nil {
		t.Fatalf("the secret did not land in the redirected directory as expected: %v", err)
	}
	assertOwnerOnly(t, "secret written through a junction", fileDACL(t, landed), sid, false)
}

// TestOwnerOnlySDDLShapes pins the descriptor text the rest of this file
// asserts against, so a change to the SDDL is a deliberate edit rather than a
// silent loosening.
func TestOwnerOnlySDDLShapes(t *testing.T) {
	const sid = "S-1-5-21-0-0-0-1000"
	if got, want := ownerOnlyDACL(sid, true), "D:P(A;OICI;FA;;;"+sid+")"; got != want {
		t.Fatalf("directory DACL = %q, want %q", got, want)
	}
	if got, want := ownerOnlyDACL(sid, false), "D:P(A;;FA;;;"+sid+")"; got != want {
		t.Fatalf("file DACL = %q, want %q", got, want)
	}
	if got, want := ownerOnlySDDL(sid, true), "O:"+sid+"D:P(A;OICI;FA;;;"+sid+")"; got != want {
		t.Fatalf("directory SDDL = %q, want %q", got, want)
	}
}

// TestEnsurePrivateDirRejectsAnEmptyPath pins the degenerate input rather than
// letting it resolve to the working directory.
func TestEnsurePrivateDirRejectsAnEmptyPath(t *testing.T) {
	if err := EnsurePrivateDir(""); err == nil {
		t.Fatal("EnsurePrivateDir(\"\") returned nil")
	}
}

// TestWritePrivateFileFailsWhenTheDirectoryIsMissing pins that the write fails
// closed rather than creating an unprotected parent on the way.
func TestWritePrivateFileFailsWhenTheDirectoryIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "token")
	err := WritePrivateFile(path, []byte("secret"))
	if err == nil {
		t.Fatal("WritePrivateFile created a secret under a directory that did not exist")
	}
	if errors.Is(err, os.ErrExist) {
		t.Fatalf("unexpected error kind: %v", err)
	}
}
