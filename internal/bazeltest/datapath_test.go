package bazeltest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDataPathJoinsRootpathUnderTheMainRepositoryRunfiles(t *testing.T) {
	srcdir := t.TempDir()
	bin := filepath.Join(srcdir, "_main", "cmd", "tool", "tool_", "tool")
	ext := filepath.Join(srcdir, "+http_archive+bd_bin", "bd")
	for _, p := range []string{bin, ext} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, want := range map[string]string{
		"cmd/tool/tool_/tool":        bin,
		"../+http_archive+bd_bin/bd": ext,
	} {
		got, err := dataPath(rel, srcdir, "_main")
		if err != nil || got != want {
			t.Errorf("dataPath(%q) = %q, %v; want %q", rel, got, err, want)
		}
	}
}

func TestDataPathRejectsMissingRunfilesAndUndeclaredFiles(t *testing.T) {
	srcdir := t.TempDir()
	for _, tc := range []struct{ rel, srcdir, workspace, want string }{
		{"cmd/tool", "", "_main", "not under bazel test"},
		{"cmd/tool", srcdir, "", "not under bazel test"},
		{"cmd/tool", srcdir, "_main", "no such file"},
	} {
		_, err := dataPath(tc.rel, tc.srcdir, tc.workspace)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("dataPath(%q, %q, %q) error = %v, want it to mention %q", tc.rel, tc.srcdir, tc.workspace, err, tc.want)
		}
	}
}

func TestDataPathIsEmptyWhenTheTargetPassesNothing(t *testing.T) {
	t.Setenv("GC_BAZELTEST_PROBE_BIN", "")
	if got := DataPath(t, "GC_BAZELTEST_PROBE_BIN"); got != "" {
		t.Fatalf("DataPath with the variable unset = %q, want \"\"", got)
	}
}
