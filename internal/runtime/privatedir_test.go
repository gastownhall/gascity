package runtime

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestEnsurePrivateDirCreatesRestricted(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "meta", "session")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}

	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("mode = %04o, want 0700", got)
	}
	// Control: a helper that returned nil without creating anything would pass
	// the mode check vacuously if Lstat were the only assertion.
	if !info.IsDir() {
		t.Error("path exists but is not a directory")
	}

	// Components this call creates come out private too, so a freshly built
	// tree does not leave the leaf's name readable behind a 0755 parent. This
	// is a property of MkdirAll's mode argument, not a guarantee the helper
	// enforces: only the leaf is inspected and tightened, so a parent that
	// already existed with a wide mode keeps it. The leaf's own 0700 is what
	// actually protects the contents.
	parent, err := os.Lstat(filepath.Dir(dir))
	if err != nil {
		t.Fatalf("Lstat parent: %v", err)
	}
	if got := parent.Mode().Perm(); got != 0o700 {
		t.Errorf("parent mode = %04o, want 0700", got)
	}
}

func TestEnsurePrivateDirTightensAnOwnedWideDirectory(t *testing.T) {
	// The upgrade path. Existing fleets carry 0755 directories from before this
	// fix; refusing them would turn a leak fix into an outage, so a directory we
	// already own is tightened in place rather than rejected.
	dir := filepath.Join(t.TempDir(), "meta")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	keep := filepath.Join(dir, "existing")
	if err := os.WriteFile(keep, []byte("payload"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir on an owned wide dir must tighten, not fail: %v", err)
	}

	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("mode = %04o, want 0700 after tightening", got)
	}
	// Control: tightening must preserve the directory, not replace it. A helper
	// that removed and recreated the dir would satisfy the mode assertion while
	// destroying live session state.
	got, err := os.ReadFile(keep)
	if err != nil {
		t.Fatalf("tightening destroyed existing content: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("existing file = %q, want %q", got, "payload")
	}
}

func TestEnsurePrivateDirRejectsForeignOwnership(t *testing.T) {
	// A pre-existing directory owned by another user is the squat case: MkdirAll
	// succeeds on it silently and every subsequent write is readable by its
	// owner, whatever mode we pass. Ownership is checked with an injected euid
	// because the test cannot create a foreign-owned directory unprivileged.
	dir := filepath.Join(t.TempDir(), "squatted")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	err := ensurePrivateDir(dir, os.Geteuid()+1)
	if err == nil {
		t.Fatal("a directory owned by another uid must fail closed")
	}
	if !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("error should name the ownership mismatch, got: %v", err)
	}

	// Control: the identical call with the real euid succeeds, so the test
	// distinguishes "rejects foreign ownership" from "always fails".
	if err := ensurePrivateDir(dir, os.Geteuid()); err != nil {
		t.Errorf("the same directory owned by us must be accepted: %v", err)
	}
}

func TestEnsurePrivateDirRejectsSymlink(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := EnsurePrivateDir(link)
	if err == nil {
		t.Fatal("a symlinked directory must fail closed: the link can be repointed after validation")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should name the symlink, got: %v", err)
	}

	// Control: the target itself is accepted, so this is not a blanket refusal.
	if err := EnsurePrivateDir(target); err != nil {
		t.Errorf("the symlink target must be accepted directly: %v", err)
	}
}

func TestEnsurePrivateDirRejectsNonDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := EnsurePrivateDir(path); err == nil {
		t.Fatal("a regular file where a directory belongs must fail closed")
	}
}

func TestEnsurePrivateDirClearsSpecialModeBits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sticky")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chmod(dir, 0o700|os.ModeSticky); err != nil {
		t.Skipf("cannot set sticky bit: %v", err)
	}
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if special := info.Mode() & (os.ModeSetuid | os.ModeSetgid | os.ModeSticky); special != 0 {
		t.Errorf("special mode bits survived: %v", special)
	}
}

func TestWritePrivateFileCreatesOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := WritePrivateFile(path, []byte("tok-1")); err != nil {
		t.Fatalf("WritePrivateFile: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}
	// Control: a helper that got the mode right by writing nothing at all would
	// pass the mode check on an empty file.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != "tok-1" {
		t.Errorf("content = %q, want %q", b, "tok-1")
	}
}

func TestWritePrivateFileNarrowsAPreExistingWideFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := WritePrivateFile(path, []byte("tok-2")); err != nil {
		t.Fatalf("WritePrivateFile: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600 — os.WriteFile's perm applies only at create, so an in-place write would leave this 0644", got)
	}
	// Control: narrowing must not cost the write.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(b) != "tok-2" {
		t.Errorf("content = %q, want %q", b, "tok-2")
	}
}

// The mode is only half the fix. A reader that opened the file while it was
// still 0644 keeps a working descriptor, and no later chmod revokes it — so
// writing in place would hand that reader the next secret. Replacing the file
// by rename swaps the inode instead, leaving the held descriptor on the old
// bytes.
func TestWritePrivateFileReplacesRatherThanRewritesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("old-secret"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Stands in for the reader who opened the world-readable file before the
	// upgrade narrowed it.
	held, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = held.Close() }()

	if err := WritePrivateFile(path, []byte("new-secret")); err != nil {
		t.Fatalf("WritePrivateFile: %v", err)
	}

	b, err := io.ReadAll(held)
	if err != nil {
		t.Fatalf("ReadAll on held descriptor: %v", err)
	}
	if strings.Contains(string(b), "new-secret") {
		t.Error("the pre-opened descriptor observed the new secret; the file was rewritten in place instead of replaced")
	}
	// Control: the descriptor is genuinely still usable, so the assertion above
	// is about which bytes it sees and not about the read having failed.
	if _, err := held.Stat(); err != nil {
		t.Errorf("held descriptor went unusable, making the check vacuous: %v", err)
	}
	// Control: the new content really did land at the path.
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(current) != "new-secret" {
		t.Errorf("path content = %q, want %q", current, "new-secret")
	}
}

// os.CreateTemp asks for 0600, but umask can only narrow what the kernel then
// grants, so under an unusual umask the sidecar would land at something like
// 0400 — no longer the mode the rest of this package asserts, and not writable
// by the owner that has to replace it. The explicit chmod normalises it.
//
// Umask is process-global. This test is not parallel and restores the previous
// value immediately, which is why it does no work between the two calls.
func TestWritePrivateFileNormalisesModeUnderARestrictiveUmask(t *testing.T) {
	// The directory comes first: t.TempDir under a restrictive umask would
	// create a directory the test framework can no longer clean up.
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	bare := filepath.Join(dir, "bare")

	// 0200 clears owner-write, which is the one bit of 0600 a umask can take
	// away; the more familiar 0077 and 0177 leave 0600 untouched.
	previous := syscall.Umask(0o200)
	err := WritePrivateFile(path, []byte("tok-4"))
	// Control: an ordinary 0600 write under the same umask, to show the umask
	// really was in force and the assertion below is about the chmod.
	bareErr := os.WriteFile(bare, []byte("x"), 0o600)
	syscall.Umask(previous)

	if err != nil {
		t.Fatalf("WritePrivateFile: %v", err)
	}
	if bareErr != nil {
		t.Fatalf("control write: %v", bareErr)
	}
	bareInfo, err := os.Lstat(bare)
	if err != nil {
		t.Fatalf("Lstat control: %v", err)
	}
	if bareInfo.Mode().Perm() == 0o600 {
		t.Skip("umask was not applied to a plain write here; the assertion would prove nothing")
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %04o, want 0600 — the umask was left to decide the sidecar's mode", got)
	}
}

// The scratch file lands in the same directory as the target, so a leak would
// accumulate copies of superseded secrets right next to the real sidecar.
func TestWritePrivateFileLeavesNoTempBehind(t *testing.T) {
	dir := t.TempDir()
	if err := WritePrivateFile(filepath.Join(dir, "token"), []byte("tok-3")); err != nil {
		t.Fatalf("WritePrivateFile: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 1 || names[0] != "token" {
		t.Errorf("directory holds %q, want only [token]", names)
	}
}
