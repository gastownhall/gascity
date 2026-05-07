package contract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

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
}
