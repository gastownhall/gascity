package subprocess

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// persistStartMetadata runs on every Start with the session's whole
// environment. The sidecar files it writes outlive the process, so seeding it
// unfiltered leaves the agent's API keys on disk with no reader that ever wants
// them back.
func TestPersistStartMetadataWithholdsCredentials(t *testing.T) {
	const secret = "sk-ant-NOT-A-REAL-KEY-0123456789"
	dir := t.TempDir()
	p := newProvider(dir)
	env := map[string]string{
		"GC_SESSION_ID":        "az-wisp-abc12",
		"GC_INSTANCE_TOKEN":    "tok-1",
		"ANTHROPIC_API_KEY":    secret,
		"ANTHROPIC_AUTH_TOKEN": secret,
	}
	if err := p.persistStartMetadata("s1", env); err != nil {
		t.Fatalf("persistStartMetadata: %v", err)
	}

	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		if got, _ := p.GetMeta("s1", key); got != "" {
			t.Errorf("GetMeta(%q) returned a value; credentials must not reach the sidecar", key)
		}
	}
	// Meta filenames are content-independent hashes, so a read-back miss does
	// not prove the bytes are absent. Scan the directory.
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(b), secret) {
			t.Errorf("credential material found on disk at %s", path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}

	// Control: the filter must be selective. Every GC_INSTANCE_TOKEN consumer
	// guards with `actual != "" && actual != expected`, so dropping the fence
	// token would disable incarnation fencing without failing anything.
	if got, _ := p.GetMeta("s1", "GC_INSTANCE_TOKEN"); got != "tok-1" {
		t.Errorf("GetMeta(GC_INSTANCE_TOKEN) = %q, want %q", got, "tok-1")
	}
	if got, _ := p.GetMeta("s1", "GC_SESSION_ID"); got != "az-wisp-abc12" {
		t.Errorf("GetMeta(GC_SESSION_ID) = %q, want %q", got, "az-wisp-abc12")
	}
}

func TestSetMetaWritesOwnerOnlyFiles(t *testing.T) {
	dir := t.TempDir()
	p := newProvider(dir)
	if err := p.SetMeta("s1", "GC_INSTANCE_TOKEN", "tok-1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	info, err := os.Lstat(p.metaPath("s1", "GC_INSTANCE_TOKEN"))
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("meta file mode = %04o, want 0600", got)
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("Lstat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("provider dir mode = %04o, want 0700", got)
	}
}

// os.WriteFile's perm argument applies only when it creates the file, so a
// host that already ran an older binary keeps its 0644 meta files forever
// unless the mode is set explicitly.
func TestSetMetaNarrowsPreExistingWideModes(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("Chmod dir: %v", err)
	}
	p := newProvider(dir)
	path := p.metaPath("s1", "GC_INSTANCE_TOKEN")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := p.SetMeta("s1", "GC_INSTANCE_TOKEN", "tok-2"); err != nil {
		t.Fatalf("SetMeta over pre-existing wide state must succeed, not fail: %v", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("pre-existing meta file mode = %04o, want 0600 after narrowing", got)
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("Lstat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("pre-existing dir mode = %04o, want 0700 after narrowing", got)
	}
	// Control: narrowing must not cost the write itself.
	if got, _ := p.GetMeta("s1", "GC_INSTANCE_TOKEN"); got != "tok-2" {
		t.Errorf("GetMeta = %q, want %q", got, "tok-2")
	}
}

// The city-less default path is identical for every user on the host, and
// MkdirAll returns nil on a directory someone else owns. Namespacing by euid
// keeps two legitimate users off one path so the ownership check in
// EnsurePrivateDir is a real check rather than a permanent failure for whoever
// logs in second.
func TestDefaultProviderDirIsPerEUID(t *testing.T) {
	// Equality, not a substring: under euid 0 or 1 — root CI, most containers —
	// any stray digit anywhere in the temp path would satisfy a Contains check
	// and the assertion would pass on an un-namespaced directory.
	want := filepath.Join(os.TempDir(), fmt.Sprintf("gc-subprocess-%d", os.Geteuid()))
	if got := defaultProviderDir(); got != want {
		t.Errorf("default provider dir = %q, want %q", got, want)
	}
}
