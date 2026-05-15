package contract

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// expectedIdentityBody is the exact byte sequence WriteProjectIdentity must
// produce for the given id. The template is fixed at v1 of the file format
// (designer §10) so diffs stay minimal and B1 / B9 can compare byte-for-byte.
func expectedIdentityBody(id string) string {
	return "# .beads/identity.toml — canonical, git-tracked.\n" +
		"# Edited only at scope creation or by deliberate human/`gc` migration.\n" +
		"\n" +
		"[project]\n" +
		"id = \"" + id + "\"\n"
}

// inodeOf returns the inode number for path on unix-like systems. The
// project does not target Windows (all production unix variants expose
// *syscall.Stat_t through FileInfo.Sys), matching the existing pattern in
// cmd/gc/beads_provider_lifecycle_test.go.
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Stat(%s) did not expose syscall.Stat_t", path)
	}
	return stat.Ino
}

// writeIdentity writes body to <scope>/.beads/identity.toml after creating
// the .beads directory. The contract package's read path must work whether
// or not WriteProjectIdentity exists (which is implemented in a sibling
// bead), so test setup uses os primitives directly.
func writeIdentity(t *testing.T, scope, body string) string {
	t.Helper()
	dir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", dir, err)
	}
	path := filepath.Join(dir, "identity.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

func TestProjectIdentity(t *testing.T) {
	fs := fsys.OSFS{}

	t.Run("A1_read_missing_returns_not_ok_no_error", func(t *testing.T) {
		scope := t.TempDir()
		id, ok, err := ReadProjectIdentity(fs, scope)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ok {
			t.Fatalf("ok = true, want false (file is absent)")
		}
		if id != "" {
			t.Fatalf("id = %q, want \"\"", id)
		}
	})

	t.Run("A2_read_present_valid", func(t *testing.T) {
		scope := t.TempDir()
		want := "gc-local-9c41a000"
		writeIdentity(t, scope, "[project]\nid = \""+want+"\"\n")
		id, ok, err := ReadProjectIdentity(fs, scope)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if id != want {
			t.Fatalf("id = %q, want %q", id, want)
		}
	})

	t.Run("A3_read_trims_whitespace", func(t *testing.T) {
		scope := t.TempDir()
		// TOML strings carry their whitespace literally; we must trim.
		writeIdentity(t, scope, "[project]\nid = \"   gc-local-pad   \"\n")
		id, ok, err := ReadProjectIdentity(fs, scope)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if id != "gc-local-pad" {
			t.Fatalf("id = %q, want %q (trimmed)", id, "gc-local-pad")
		}
	})

	t.Run("A4_read_empty_id_treated_as_not_ok", func(t *testing.T) {
		scope := t.TempDir()
		writeIdentity(t, scope, "[project]\nid = \"\"\n")
		id, ok, err := ReadProjectIdentity(fs, scope)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ok {
			t.Fatalf("ok = true, want false (empty id is not authoritative)")
		}
		if id != "" {
			t.Fatalf("id = %q, want \"\"", id)
		}
	})

	t.Run("A5_read_whitespace_only_id_treated_as_not_ok", func(t *testing.T) {
		scope := t.TempDir()
		writeIdentity(t, scope, "[project]\nid = \"   \"\n")
		id, ok, err := ReadProjectIdentity(fs, scope)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ok {
			t.Fatalf("ok = true, want false (whitespace-only id is not authoritative)")
		}
		if id != "" {
			t.Fatalf("id = %q, want \"\"", id)
		}
	})

	t.Run("A6_read_missing_project_section", func(t *testing.T) {
		scope := t.TempDir()
		// Comment-only file: parses as an empty TOML document (no project section).
		writeIdentity(t, scope, "# only a comment, no project section\n")
		id, ok, err := ReadProjectIdentity(fs, scope)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ok {
			t.Fatalf("ok = true, want false (no [project] section)")
		}
		if id != "" {
			t.Fatalf("id = %q, want \"\"", id)
		}
	})

	t.Run("A6b_read_bare_project_section_treated_as_not_ok", func(t *testing.T) {
		scope := t.TempDir()
		writeIdentity(t, scope, "[project]\n")
		id, ok, err := ReadProjectIdentity(fs, scope)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ok {
			t.Fatalf("ok = true, want false (project.id is absent)")
		}
		if id != "" {
			t.Fatalf("id = %q, want \"\"", id)
		}
	})

	t.Run("A7_read_malformed_toml_errors", func(t *testing.T) {
		scope := t.TempDir()
		// Truncated section header — invalid TOML.
		path := writeIdentity(t, scope, "[project\nid = \"x\"\n")
		id, ok, err := ReadProjectIdentity(fs, scope)
		if err == nil {
			t.Fatalf("err = nil, want non-nil for malformed TOML")
		}
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("err = %v, want message containing path %q", err, path)
		}
		if ok {
			t.Fatalf("ok = true, want false on parse error")
		}
		if id != "" {
			t.Fatalf("id = %q, want \"\" on parse error", id)
		}
	})

	t.Run("A8_read_extra_top_level_key_errors", func(t *testing.T) {
		scope := t.TempDir()
		writeIdentity(t, scope, "version = 1\n[project]\nid = \"gc-local-x\"\n")
		_, ok, err := ReadProjectIdentity(fs, scope)
		if err == nil {
			t.Fatalf("err = nil, want non-nil for extra top-level key")
		}
		if !strings.Contains(err.Error(), "version") {
			t.Fatalf("err = %v, want message naming the unknown key %q", err, "version")
		}
		if ok {
			t.Fatalf("ok = true, want false on parse error")
		}
	})

	t.Run("A9_read_extra_project_key_errors", func(t *testing.T) {
		scope := t.TempDir()
		writeIdentity(t, scope, "[project]\nid = \"gc-local-x\"\nname = \"unexpected\"\n")
		_, ok, err := ReadProjectIdentity(fs, scope)
		if err == nil {
			t.Fatalf("err = nil, want non-nil for extra project key")
		}
		if !strings.Contains(err.Error(), "name") {
			t.Fatalf("err = %v, want message naming the unknown key %q", err, "name")
		}
		if ok {
			t.Fatalf("ok = true, want false on parse error")
		}
	})

	t.Run("A9b_read_non_string_project_id_errors", func(t *testing.T) {
		scope := t.TempDir()
		path := writeIdentity(t, scope, "[project]\nid = 123\n")
		_, ok, err := ReadProjectIdentity(fs, scope)
		if err == nil {
			t.Fatalf("err = nil, want non-nil for non-string project.id")
		}
		if !strings.Contains(err.Error(), path) {
			t.Fatalf("err = %v, want message containing path %q", err, path)
		}
		if ok {
			t.Fatalf("ok = true, want false on parse error")
		}
	})

	t.Run("A9c_read_nested_unknown_project_table_errors", func(t *testing.T) {
		scope := t.TempDir()
		writeIdentity(t, scope, "[project]\nid = \"gc-local-x\"\n[project.extra]\nvalue = \"unexpected\"\n")
		_, ok, err := ReadProjectIdentity(fs, scope)
		if err == nil {
			t.Fatalf("err = nil, want non-nil for nested unknown project table")
		}
		if !strings.Contains(err.Error(), "project.extra") {
			t.Fatalf("err = %v, want message naming the unknown nested table %q", err, "project.extra")
		}
		if ok {
			t.Fatalf("ok = true, want false on parse error")
		}
	})

	t.Run("A10_read_permission_error_propagates", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses unix permission checks; cannot simulate read failure")
		}
		scope := t.TempDir()
		path := writeIdentity(t, scope, "[project]\nid = \"gc-local-x\"\n")
		if err := os.Chmod(path, 0); err != nil {
			t.Fatalf("Chmod(0): %v", err)
		}
		// Restore mode so t.TempDir() cleanup can remove the file.
		t.Cleanup(func() {
			_ = os.Chmod(path, 0o644)
		})
		id, ok, err := ReadProjectIdentity(fs, scope)
		if err == nil {
			t.Fatalf("err = nil, want non-nil for unreadable file")
		}
		if os.IsNotExist(err) {
			t.Fatalf("err = %v, want a permission/IO error (not ErrNotExist)", err)
		}
		if ok {
			t.Fatalf("ok = true, want false on read error")
		}
		if id != "" {
			t.Fatalf("id = %q, want \"\" on read error", id)
		}
	})

	t.Run("A11_read_with_comments_works", func(t *testing.T) {
		scope := t.TempDir()
		body := "# canonical identity for this scope\n# do not hand-edit\n\n[project]\nid = \"gc-local-cmt\"\n"
		writeIdentity(t, scope, body)
		id, ok, err := ReadProjectIdentity(fs, scope)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if id != "gc-local-cmt" {
			t.Fatalf("id = %q, want %q", id, "gc-local-cmt")
		}
	})

	t.Run("A12_read_utf8_id_round_trips", func(t *testing.T) {
		scope := t.TempDir()
		want := "gc-local-é"
		writeIdentity(t, scope, "[project]\nid = \""+want+"\"\n")
		id, ok, err := ReadProjectIdentity(fs, scope)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if !ok {
			t.Fatalf("ok = false, want true")
		}
		if id != want {
			t.Fatalf("id = %q, want %q", id, want)
		}
	})

	t.Run("C1_path_joins_scope_root", func(t *testing.T) {
		got := ProjectIdentityPath("/x/y")
		want := filepath.Join("/x/y", ".beads", "identity.toml")
		if got != want {
			t.Fatalf("ProjectIdentityPath(\"/x/y\") = %q, want %q", got, want)
		}
	})

	t.Run("C2_path_handles_trailing_slash", func(t *testing.T) {
		// filepath.Join canonicalizes the trailing slash; both inputs must
		// produce the same path.
		bare := ProjectIdentityPath("/x/y")
		slashed := ProjectIdentityPath("/x/y/")
		if bare != slashed {
			t.Fatalf("ProjectIdentityPath bare=%q vs slashed=%q (must canonicalize)", bare, slashed)
		}
	})

	t.Run("B1_write_creates_file_with_correct_body", func(t *testing.T) {
		scope := t.TempDir()
		id := "gc-local-write-b1"
		if err := WriteProjectIdentity(fs, scope, id); err != nil {
			t.Fatalf("WriteProjectIdentity: %v", err)
		}
		path := ProjectIdentityPath(scope)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		want := expectedIdentityBody(id)
		if string(got) != want {
			t.Fatalf("body mismatch\n got: %q\nwant: %q", string(got), want)
		}
		if !strings.HasSuffix(string(got), "\n") {
			t.Fatalf("body does not end with newline: %q", string(got))
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s): %v", path, err)
		}
		if mode := info.Mode().Perm(); mode != 0o644 {
			t.Fatalf("mode = %#o, want 0o644", mode)
		}
	})

	t.Run("B2_write_idempotent_no_inode_change", func(t *testing.T) {
		scope := t.TempDir()
		id := "gc-local-write-b2"
		if err := WriteProjectIdentity(fs, scope, id); err != nil {
			t.Fatalf("first WriteProjectIdentity: %v", err)
		}
		path := ProjectIdentityPath(scope)
		before := inodeOf(t, path)
		if err := WriteProjectIdentity(fs, scope, id); err != nil {
			t.Fatalf("second WriteProjectIdentity: %v", err)
		}
		after := inodeOf(t, path)
		if before != after {
			t.Fatalf("inode changed across idempotent write: before=%d after=%d", before, after)
		}
	})

	t.Run("B3_write_overwrite_changes_inode", func(t *testing.T) {
		scope := t.TempDir()
		if err := WriteProjectIdentity(fs, scope, "gc-local-write-b3-old"); err != nil {
			t.Fatalf("first WriteProjectIdentity: %v", err)
		}
		path := ProjectIdentityPath(scope)
		before := inodeOf(t, path)
		if err := WriteProjectIdentity(fs, scope, "gc-local-write-b3-new"); err != nil {
			t.Fatalf("second WriteProjectIdentity: %v", err)
		}
		after := inodeOf(t, path)
		if before == after {
			t.Fatalf("inode unchanged after overwrite with different id: inode=%d (atomic temp+rename should produce a new inode)", before)
		}
	})

	t.Run("B4_write_empty_id_returns_error", func(t *testing.T) {
		scope := t.TempDir()
		err := WriteProjectIdentity(fs, scope, "")
		if err == nil {
			t.Fatalf("err = nil, want non-nil for empty id")
		}
		if _, statErr := os.Stat(ProjectIdentityPath(scope)); !os.IsNotExist(statErr) {
			t.Fatalf("identity file exists after rejected write: stat err = %v (want IsNotExist)", statErr)
		}
	})

	t.Run("B5_write_whitespace_id_returns_error", func(t *testing.T) {
		scope := t.TempDir()
		err := WriteProjectIdentity(fs, scope, "   \n")
		if err == nil {
			t.Fatalf("err = nil, want non-nil for whitespace-only id")
		}
		if _, statErr := os.Stat(ProjectIdentityPath(scope)); !os.IsNotExist(statErr) {
			t.Fatalf("identity file exists after rejected write: stat err = %v (want IsNotExist)", statErr)
		}
	})

	t.Run("B6_write_id_with_newline_returns_error", func(t *testing.T) {
		scope := t.TempDir()
		bad := "gc-local-\nfoo"
		err := WriteProjectIdentity(fs, scope, bad)
		if err == nil {
			t.Fatalf("err = nil, want non-nil for id containing newline")
		}
		// The error message renders the id in Go-quoted form (%q) so a
		// newline shows as the literal escape sequence \n, which is what
		// surfaces in logs and CLI output.
		quoted := strconv.Quote(bad)
		if !strings.Contains(err.Error(), quoted) {
			t.Fatalf("err = %v, want message containing offending id %s", err, quoted)
		}
		if _, statErr := os.Stat(ProjectIdentityPath(scope)); !os.IsNotExist(statErr) {
			t.Fatalf("identity file exists after rejected write: stat err = %v (want IsNotExist)", statErr)
		}
	})

	t.Run("B7_write_id_with_quote_returns_error", func(t *testing.T) {
		scope := t.TempDir()
		bad := "gc-local-\"foo"
		err := WriteProjectIdentity(fs, scope, bad)
		if err == nil {
			t.Fatalf("err = nil, want non-nil for id containing quote")
		}
		quoted := strconv.Quote(bad)
		if !strings.Contains(err.Error(), quoted) {
			t.Fatalf("err = %v, want message containing offending id %s", err, quoted)
		}
		if _, statErr := os.Stat(ProjectIdentityPath(scope)); !os.IsNotExist(statErr) {
			t.Fatalf("identity file exists after rejected write: stat err = %v (want IsNotExist)", statErr)
		}
	})

	t.Run("B8_write_creates_dotbeads_dir_if_needed", func(t *testing.T) {
		scope := t.TempDir()
		dotBeads := filepath.Join(scope, ".beads")
		if _, err := os.Stat(dotBeads); !os.IsNotExist(err) {
			t.Fatalf("precondition: .beads must not exist; stat err = %v", err)
		}
		if err := WriteProjectIdentity(fs, scope, "gc-local-write-b8"); err != nil {
			t.Fatalf("WriteProjectIdentity: %v", err)
		}
		info, err := os.Stat(dotBeads)
		if err != nil {
			t.Fatalf("Stat(%s): %v", dotBeads, err)
		}
		if !info.IsDir() {
			t.Fatalf("%s exists but is not a directory", dotBeads)
		}
		if mode := info.Mode().Perm(); mode != 0o755 {
			t.Fatalf("mode = %#o, want 0o755", mode)
		}
	})

	t.Run("B9_write_round_trips_through_read", func(t *testing.T) {
		scope := t.TempDir()
		want := "gc-local-write-b9"
		if err := WriteProjectIdentity(fs, scope, want); err != nil {
			t.Fatalf("WriteProjectIdentity: %v", err)
		}
		got, ok, err := ReadProjectIdentity(fs, scope)
		if err != nil {
			t.Fatalf("ReadProjectIdentity: %v", err)
		}
		if !ok {
			t.Fatalf("ok = false after write, want true")
		}
		if got != want {
			t.Fatalf("id = %q, want %q", got, want)
		}
	})

	t.Run("B10_write_concurrent_same_id_safe", func(t *testing.T) {
		scope := t.TempDir()
		id := "gc-local-write-b10"

		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = WriteProjectIdentity(fs, scope, id)
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Fatalf("goroutine %d: WriteProjectIdentity: %v", i, err)
			}
		}

		path := ProjectIdentityPath(scope)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", path, err)
		}
		want := expectedIdentityBody(id)
		if string(got) != want {
			t.Fatalf("body mismatch after concurrent writes\n got: %q\nwant: %q", string(got), want)
		}
	})
}

// identityRepoRoot resolves the repository root from this test file's
// location. identity_test.go lives at <root>/internal/beads/contract/, so we
// walk up three directories. The result is sanity-checked to fail loudly if
// the file is ever moved without updating the offset.
func identityRepoRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) failed; cannot locate test source file")
	}
	root := filepath.Join(filepath.Dir(filename), "..", "..", "..")
	abs, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("filepath.Abs(%s): %v", root, err)
	}
	for _, marker := range []string{
		"go.mod",
		filepath.Join("internal", "beads", "contract", "identity.go"),
	} {
		if _, err := os.Stat(filepath.Join(abs, marker)); err != nil {
			t.Fatalf("resolved repo root %q is missing expected marker %q: %v", abs, marker, err)
		}
	}
	return abs
}

// TestNoExternalIdentityWriters enforces that .beads/identity.toml is only
// referenced by Go source in internal/beads/contract/. Any other production
// (.go, non-test) file mentioning the literal "identity.toml" is a candidate
// writer that must be moved into the contract package so all writers share the
// same atomic, validated, byte-equal template (designer §5 D1, ga-ich5z).
//
// V1 implementation is a coarse byte-level grep. False positives (an error
// message that mentions the file outside the contract package) should be rare;
// if any appear, add the offending path to identityWriterAllowlist with a
// comment explaining why it is benign. AST-level filtering is deferred to a
// future hardening bead per ga-ich5z out-of-scope notes.
func TestNoExternalIdentityWriters(t *testing.T) {
	root := identityRepoRoot(t)

	// contractRel is the package directory whose entire content (including
	// _test.go files) is exempt — the contract package owns identity.toml.
	contractRel := filepath.Join("internal", "beads", "contract") + string(filepath.Separator)

	// skipDirs are directory base names whose subtrees are not walked at all.
	// vendor/.git/node_modules are required by the bead spec; .gc/.claude and
	// nested git worktrees under worktrees/ are repo-local untracked trees
	// outside the production source surface.
	skipDirs := map[string]struct{}{
		".git":         {},
		"vendor":       {},
		"node_modules": {},
		".gc":          {},
		".claude":      {},
		"worktrees":    {},
	}

	// identityWriterAllowlist enumerates relative paths that may legitimately
	// contain the literal "identity.toml" outside internal/beads/contract/.
	// Add an entry only when the reference is not an identity file writer and
	// moving it through WriteProjectIdentity would misrepresent what it does.
	identityWriterAllowlist := map[string]string{
		// gitignore.go writes .gitignore negation patterns so identity.toml is
		// tracked; it never reads or writes the identity file itself.
		filepath.Join("cmd", "gc", "gitignore.go"): "gitignore pattern",
	}

	needle := []byte("identity.toml")
	var violations []string

	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if _, skip := skipDirs[d.Name()]; skip {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(rel, contractRel) {
			return nil
		}
		if _, ok := identityWriterAllowlist[rel]; ok {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, needle) {
			violations = append(violations, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking repo from %s: %v", root, err)
	}

	if len(violations) > 0 {
		sort.Strings(violations)
		t.Errorf("found %d Go file(s) outside internal/beads/contract/ that mention %q:", len(violations), "identity.toml")
		for _, v := range violations {
			t.Errorf("  %s", v)
		}
		t.Error("Move these references into internal/beads/contract/ so all writers share ProjectIdentityPath / WriteProjectIdentity.")
	}
}
