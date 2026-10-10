//go:build unix

package testenv

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestCheckTemplateEntry pins the ownership and mode rule for one template
// entry. It takes the uid to expect so the owner half can be exercised
// without root: no test can chown a file to someone else, but it can ask
// whether a file it owns would pass for another user.
func TestCheckTemplateEntry(t *testing.T) {
	uid := os.Getuid()
	cases := []struct {
		name  string
		isDir bool
		mode  os.FileMode
		uid   int
		want  string // "" when the entry is acceptable, else a substring of the error
	}{
		{"file mine, 0644", false, 0o644, uid, ""},
		{"file mine, 0600", false, 0o600, uid, ""},
		{"directory mine, 0755", true, 0o755, uid, ""},
		{"directory mine, 0700", true, 0o700, uid, ""},
		{"file someone else's", false, 0o644, uid + 1, "owned by"},
		{"directory someone else's", true, 0o755, uid + 1, "owned by"},
		{"file group-writable", false, 0o664, uid, "writable"},
		{"file other-writable", false, 0o646, uid, "writable"},
		{"directory group-writable", true, 0o775, uid, "writable"},
		{"directory other-writable", true, 0o757, uid, "writable"},
		{"directory world-writable", true, 0o777, uid, "writable"},
	}
	dir := t.TempDir()
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "entry"+strconv.Itoa(i))
			if tc.isDir {
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatalf("Mkdir: %v", err)
				}
			} else {
				mustWrite(t, path, "")
			}
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatalf("Chmod: %v", err)
			}
			fi, err := os.Lstat(path)
			if err != nil {
				t.Fatalf("Lstat: %v", err)
			}

			err = checkTemplateEntry(path, fi, tc.uid)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("checkTemplateEntry rejected an acceptable entry: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), path)):
				t.Errorf("checkTemplateEntry = %v, want an error naming %q and containing %q", err, path, tc.want)
			}
		})
	}
}

// TestEnsureGitTemplateRefusesEntriesOthersCouldWrite changes real modes in a
// template this process owns, which is the half of the ownership rule a test
// can reach through ensureGitTemplate itself.
func TestEnsureGitTemplateRefusesEntriesOthersCouldWrite(t *testing.T) {
	cases := []struct {
		name string
		rel  string // the path the error must name, relative to the template; "" for the template itself
		mode os.FileMode
	}{
		{"template group-writable", "", 0o775},
		{"hooks other-writable", "hooks", 0o757},
		{"config group-writable", "config", 0o664},
		{"info/exclude other-writable", filepath.Join("info", "exclude"), 0o646},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, dir := newTestTemplate(t)
			if err := os.Chmod(filepath.Join(dir, tc.rel), tc.mode); err != nil {
				t.Fatalf("Chmod: %v", err)
			}
			assertTemplateRefused(t, root, dir, tc.rel)
		})
	}
}
