package testenv

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestBazelToolPATHPrependsEachToolDirectory(t *testing.T) {
	srcdir := filepath.FromSlash("/rf")
	got := bazelToolPATH("/usr/bin:/bin", srcdir, "_main",
		"../+http_archive+bd_bin_v1_3_1/bd ../+http_archive+dolt_bin_v2_2_0/dolt-linux-amd64/bin/dolt cmd/gc/gc_/gc")
	want := strings.Join([]string{
		filepath.Join(srcdir, "+http_archive+bd_bin_v1_3_1"),
		filepath.Join(srcdir, "+http_archive+dolt_bin_v2_2_0", "dolt-linux-amd64", "bin"),
		filepath.Join(srcdir, "_main", "cmd", "gc", "gc_"),
		"/usr/bin:/bin",
	}, string(filepath.ListSeparator))
	if got != want {
		t.Fatalf("bazelToolPATH = %q, want %q", got, want)
	}
}

func TestBazelToolPATHDedupesDirectories(t *testing.T) {
	got := bazelToolPATH("/bin", "/rf", "_main", "tools/a tools/b")
	want := filepath.Join("/rf", "_main", "tools") + string(filepath.ListSeparator) + "/bin"
	if got != want {
		t.Fatalf("bazelToolPATH = %q, want %q", got, want)
	}
}

// A re-executed test binary (helper processes, testscript commands) inherits
// the prepended PATH and runs init again; it must not prepend a second copy.
func TestBazelToolPATHIsIdempotent(t *testing.T) {
	once := bazelToolPATH("/bin", "/rf", "_main", "tools/bd other/dolt")
	if twice := bazelToolPATH(once, "/rf", "_main", "tools/bd other/dolt"); twice != once {
		t.Fatalf("second bazelToolPATH = %q, want unchanged %q", twice, once)
	}
}

func TestBazelToolPATHLeavesPATHAloneOutsideBazel(t *testing.T) {
	for _, tc := range []struct{ srcdir, workspace, tools string }{
		{"", "_main", "tools/bd"}, // not under bazel test
		{"/rf", "_main", ""},      // no tools declared
		{"/rf", "_main", "   "},   // blank list
		{"/rf", "", "tools/bd"},   // no workspace name
	} {
		if got := bazelToolPATH("/bin", tc.srcdir, tc.workspace, tc.tools); got != "/bin" {
			t.Errorf("bazelToolPATH(%q, %q, %q) = %q, want PATH unchanged", tc.srcdir, tc.workspace, tc.tools, got)
		}
	}
}

func TestBazelToolPATHWithEmptyPATH(t *testing.T) {
	got := bazelToolPATH("", "/rf", "_main", "tools/bd")
	if want := filepath.Join("/rf", "_main", "tools"); got != want {
		t.Fatalf("bazelToolPATH = %q, want %q", got, want)
	}
}
