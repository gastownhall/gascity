package herdr

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The reconciler's pending-create ownership check reads GC_SESSION_ID /
// GC_INSTANCE_TOKEN via GetMeta while Start is still delivering the startup
// nudge. tmux satisfies that read from the session environment (seeded from
// cfg.Env at creation); herdr's sidecar must be seeded explicitly or the
// fresh runtime is reaped as "live runtime belongs to another session".
func TestSeedMetaFromEnvMakesIdentityKeysReadable(t *testing.T) {
	p := New("gctest-seedmeta", t.TempDir(), t.TempDir(), time.Second, 0)
	env := map[string]string{
		"GC_SESSION_ID":     "az-wisp-abc12",
		"GC_INSTANCE_TOKEN": "tok-1",
		"GC_RUNTIME_EPOCH":  "3",
	}
	if err := p.seedMetaFromEnv("polecat-az-wisp-abc12", env); err != nil {
		t.Fatalf("seedMetaFromEnv: %v", err)
	}
	for k, want := range env {
		got, err := p.GetMeta("polecat-az-wisp-abc12", k)
		if err != nil || got != want {
			t.Errorf("GetMeta(%q) = %q, %v; want %q", k, got, err, want)
		}
	}
	// Unset keys still read as absent, not an error.
	if got, err := p.GetMeta("polecat-az-wisp-abc12", "GC_DRAIN"); err != nil || got != "" {
		t.Errorf("GetMeta(unset) = %q, %v; want \"\", nil", got, err)
	}
}

// Later SetMeta calls override seeded values, matching tmux setenv semantics.
func TestSeedMetaFromEnvIsOverridableBySetMeta(t *testing.T) {
	p := New("gctest-seedmeta2", t.TempDir(), t.TempDir(), time.Second, 0)
	if err := p.seedMetaFromEnv("s1", map[string]string{"GC_INSTANCE_TOKEN": "old"}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMeta("s1", "GC_INSTANCE_TOKEN", "new"); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.GetMeta("s1", "GC_INSTANCE_TOKEN"); got != "new" {
		t.Errorf("GetMeta after override = %q, want %q", got, "new")
	}
}

// Empty env is a no-op.
func TestSeedMetaFromEnvEmptyIsNoOp(t *testing.T) {
	p := New("gctest-seedmeta3", t.TempDir(), t.TempDir(), time.Second, 0)
	if err := p.seedMetaFromEnv("s1", nil); err != nil {
		t.Fatalf("seedMetaFromEnv(nil) = %v, want nil", err)
	}
}

// The sidecar is a durable on-disk store, so seeding it from the whole session
// environment writes every API key the agent was launched with to a file that
// outlives the session. Only the keys a GetMeta consumer actually reads belong
// there.
func TestSeedMetaFromEnvWithholdsCredentials(t *testing.T) {
	const secret = "sk-ant-NOT-A-REAL-KEY-0123456789"
	metaDir := t.TempDir()
	p := New("gctest-seedmeta4", metaDir, t.TempDir(), time.Second, 0)
	env := map[string]string{
		"GC_SESSION_ID":        "az-wisp-abc12",
		"GC_INSTANCE_TOKEN":    "tok-1",
		"ANTHROPIC_API_KEY":    secret,
		"ANTHROPIC_AUTH_TOKEN": secret,
		"BEADS_HOLDER_TOKEN":   secret,
	}
	if err := p.seedMetaFromEnv("s1", env); err != nil {
		t.Fatalf("seedMetaFromEnv: %v", err)
	}

	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "BEADS_HOLDER_TOKEN"} {
		if got, _ := p.GetMeta("s1", key); got != "" {
			t.Errorf("GetMeta(%q) returned a value; credentials must not reach the sidecar", key)
		}
	}
	// The read-back check alone would pass if the value landed under a
	// different filename, so scan the whole tree for the byte sequence.
	if err := filepath.WalkDir(metaDir, func(path string, d fs.DirEntry, err error) error {
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

	// Control: withholding must be selective. The fence token has no argv-safe
	// substitute and every consumer treats its absence as permission to
	// proceed, so a filter that dropped it would disable incarnation fencing
	// silently while satisfying every assertion above.
	if got, _ := p.GetMeta("s1", "GC_INSTANCE_TOKEN"); got != "tok-1" {
		t.Errorf("GetMeta(GC_INSTANCE_TOKEN) = %q, want %q", got, "tok-1")
	}
	if got, _ := p.GetMeta("s1", "GC_SESSION_ID"); got != "az-wisp-abc12" {
		t.Errorf("GetMeta(GC_SESSION_ID) = %q, want %q", got, "az-wisp-abc12")
	}
}

// The fence token still reaches disk, so the file holding it must be readable
// only by the user running the city.
func TestSetMetaWritesOwnerOnlyFiles(t *testing.T) {
	metaDir := t.TempDir()
	p := New("gctest-seedmeta5", metaDir, t.TempDir(), time.Second, 0)
	if err := p.SetMeta("s1", "GC_INSTANCE_TOKEN", "tok-1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	sessionDir := filepath.Join(metaDir, sanitize("s1"))
	dirInfo, err := os.Lstat(sessionDir)
	if err != nil {
		t.Fatalf("Lstat session dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("session meta dir mode = %04o, want 0700", got)
	}
	fileInfo, err := os.Lstat(filepath.Join(sessionDir, sanitize("GC_INSTANCE_TOKEN")))
	if err != nil {
		t.Fatalf("Lstat meta file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("meta file mode = %04o, want 0600", got)
	}
}

// The upgrade path, and the reason the fix cannot be a wider perm argument to
// MkdirAll/WriteFile: those apply only at create, so a host that already ran an
// older binary keeps its 0755 directory and 0644 files forever while the new
// code reports success.
func TestSetMetaNarrowsPreExistingWideModes(t *testing.T) {
	metaDir := t.TempDir()
	sessionDir := filepath.Join(metaDir, sanitize("s1"))
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chmod(sessionDir, 0o755); err != nil {
		t.Fatalf("Chmod dir: %v", err)
	}
	metaFile := filepath.Join(sessionDir, sanitize("GC_INSTANCE_TOKEN"))
	if err := os.WriteFile(metaFile, []byte("stale"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	p := New("gctest-seedmeta6", metaDir, t.TempDir(), time.Second, 0)
	if err := p.SetMeta("s1", "GC_INSTANCE_TOKEN", "tok-2"); err != nil {
		t.Fatalf("SetMeta over pre-existing wide state must succeed, not fail: %v", err)
	}

	dirInfo, err := os.Lstat(sessionDir)
	if err != nil {
		t.Fatalf("Lstat dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("pre-existing dir mode = %04o, want 0700 after narrowing", got)
	}
	fileInfo, err := os.Lstat(metaFile)
	if err != nil {
		t.Fatalf("Lstat file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("pre-existing file mode = %04o, want 0600 after narrowing", got)
	}
	// Control: narrowing must not cost the write itself.
	if got, _ := p.GetMeta("s1", "GC_INSTANCE_TOKEN"); got != "tok-2" {
		t.Errorf("GetMeta = %q, want %q", got, "tok-2")
	}
}

// City-less construction falls back to a path under the shared temp dir. Without
// the euid in the name every user on the host races for one predictable
// directory, and whoever creates it first owns everything written into it —
// MkdirAll returns nil on an existing directory whatever its owner.
func TestNewMetaDirFallbackIsPerEUID(t *testing.T) {
	p := New("gctest-fallback", "", "", time.Second, 0)
	if p.metaDir == "" {
		t.Fatal("city-less construction must still pick a metaDir")
	}
	// Equality, not a substring: under euid 0 or 1 — root CI, most containers —
	// any stray digit anywhere in the temp path would satisfy a Contains check
	// and the assertion would pass on an un-namespaced directory. The session
	// leaf is part of the expected path, so this also pins that two herdr
	// sessions for the same user do not share one sidecar.
	want := filepath.Join(
		os.TempDir(),
		fmt.Sprintf("gc-herdr-meta-%d", os.Geteuid()),
		sanitize("gctest-fallback"),
	)
	if p.metaDir != want {
		t.Errorf("fallback metaDir = %q, want %q", p.metaDir, want)
	}
}

// The sidecar root is one level above the per-session directory, and MkdirAll
// walks through an existing ancestor without inspecting it. A root left wide
// lets its owner replace the session directory underneath a validated leaf.
func TestSetMetaNarrowsTheSidecarRootNotJustTheLeaf(t *testing.T) {
	root := filepath.Join(t.TempDir(), "meta")
	if err := os.MkdirAll(root, 0o777); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	p := New("gctest-root", root, "", time.Second, 0)
	if err := p.SetMeta("s1", "GC_INSTANCE_TOKEN", "tok-1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	info, err := os.Lstat(root)
	if err != nil {
		t.Fatalf("Lstat root: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("sidecar root mode = %04o, want 0700", got)
	}
	// Control: the leaf is narrowed too, so a pass here is not the root being
	// checked at the leaf's expense.
	leaf, err := os.Lstat(filepath.Join(root, sanitize("s1")))
	if err != nil {
		t.Fatalf("Lstat leaf: %v", err)
	}
	if got := leaf.Mode().Perm(); got != 0o700 {
		t.Errorf("session dir mode = %04o, want 0700", got)
	}
	// Control: tightening must not cost the write.
	if got, _ := p.GetMeta("s1", "GC_INSTANCE_TOKEN"); got != "tok-1" {
		t.Errorf("GetMeta = %q, want %q", got, "tok-1")
	}
}
