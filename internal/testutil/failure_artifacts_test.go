package testutil

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type fakeFailureReporter struct {
	name   string
	failed bool
}

func (f fakeFailureReporter) Name() string { return f.name }
func (f fakeFailureReporter) Failed() bool { return f.failed }

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// listFiles returns every regular file under root, relative and sorted.
func listFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	sort.Strings(files)
	return files
}

// managedDoltTree lays out the Dolt, bd-proxy and supervisor logs a failing
// acceptance test leaves behind, plus files that must never be uploaded.
func managedDoltTree(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "gc-acceptance-123")
	for rel, content := range map[string]string{
		".gc/runtime/packs/dolt/dolt.log":         "gc-managed sql-server\n",
		".gc/runtime/packs/dolt/dolt-config.yaml": "listener: {}\n",
		"testrig/.beads/dolt-server.log":          "scope-local sql-server\n",
		"testrig/.beads/dolt/server.log":          "proxy child\n",
		"testrig/.beads/metadata.json":            "{}\n",
		"gc-home/supervisor.log":                  "supervisor + controller\n",
		"testrig/.beads/issues.jsonl":             "bead payload\n",
		"testrig/.beads/dolt/hq/.dolt/noms/LOCK":  "",
	} {
		writeTestFile(t, filepath.Join(dir, rel), content)
	}
	return dir
}

func TestSaveFailureDiagnosticsCopiesDoltAndSupervisorLogs(t *testing.T) {
	artifacts := t.TempDir()
	t.Setenv(FailureArtifactDirEnv, artifacts)
	dir := managedDoltTree(t)

	SaveFailureDiagnostics(fakeFailureReporter{name: "TestX/sub case", failed: true}, dir)

	got := listFiles(t, filepath.Join(artifacts, "TestX_sub_case", "gc-acceptance-123"))
	want := []string{
		".gc_runtime_packs_dolt_dolt-config.yaml",
		".gc_runtime_packs_dolt_dolt.log",
		"env.txt",
		"gc-home_supervisor.log",
		"testrig_.beads_dolt-server.log",
		"testrig_.beads_dolt_server.log",
		"testrig_.beads_metadata.json",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("preserved files:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	body, err := os.ReadFile(filepath.Join(artifacts, "TestX_sub_case", "gc-acceptance-123", "testrig_.beads_dolt-server.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "scope-local sql-server\n" {
		t.Fatalf("dolt-server.log = %q", body)
	}
}

func TestSaveFailureDiagnosticsSkipsPassingTests(t *testing.T) {
	artifacts := t.TempDir()
	t.Setenv(FailureArtifactDirEnv, artifacts)

	SaveFailureDiagnostics(fakeFailureReporter{name: "TestX", failed: false}, managedDoltTree(t))

	if got := listFiles(t, artifacts); len(got) != 0 {
		t.Fatalf("a passing test preserved %v", got)
	}
}

func TestSaveFailureDiagnosticsWithoutArtifactDirIsNoop(t *testing.T) {
	t.Setenv(FailureArtifactDirEnv, "")
	dir := managedDoltTree(t)
	before := listFiles(t, dir)

	SaveFailureDiagnostics(fakeFailureReporter{name: "TestX", failed: true}, dir)

	if after := listFiles(t, dir); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Fatalf("source tree changed:\nbefore %v\nafter  %v", before, after)
	}
}

func TestSaveDiagnosticsCopiesUnconditionally(t *testing.T) {
	artifacts := t.TempDir()
	t.Setenv(FailureArtifactDirEnv, artifacts)
	home := filepath.Join(t.TempDir(), "gc-home")
	writeTestFile(t, filepath.Join(home, "supervisor.log"), "controller exited\n")

	SaveDiagnostics("TestMain", home)

	got := listFiles(t, filepath.Join(artifacts, "TestMain", "gc-home"))
	if strings.Join(got, ",") != "env.txt,supervisor.log" {
		t.Fatalf("preserved %v, want env.txt and supervisor.log", got)
	}
}

func TestCopyBoundedFileKeepsHeadAndTail(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "server.log")
	var b bytes.Buffer
	b.WriteString("FIRST-LINE startup\n")
	for b.Len() < 4*maxFailureArtifactBytes {
		b.WriteString("retrying dolt init: usage block ...........................\n")
	}
	b.WriteString("LAST-LINE connection refused\n")
	if err := os.WriteFile(src, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "copy.log")

	if err := copyBoundedFile(src, dst); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > maxFailureArtifactBytes+len(truncatedArtifactMarker) {
		t.Fatalf("copy is %d bytes, cap is %d", len(got), maxFailureArtifactBytes)
	}
	for _, want := range []string{"FIRST-LINE startup", truncatedArtifactMarker, "LAST-LINE connection refused\n"} {
		if !bytes.Contains(got, []byte(want)) {
			t.Errorf("bounded copy lost %q", want)
		}
	}
}

func TestCopyBoundedFileCopiesSmallFilesVerbatim(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "dolt.log")
	writeTestFile(t, src, "short log\n")
	dst := filepath.Join(dir, "copy.log")

	if err := copyBoundedFile(src, dst); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "short log\n" {
		t.Fatalf("copy = %q", got)
	}
}

func TestCopyBoundedFileCapBoundary(t *testing.T) {
	for _, tc := range []struct {
		size    int
		wantLen int
	}{
		{size: maxFailureArtifactBytes, wantLen: maxFailureArtifactBytes},
		{size: maxFailureArtifactBytes + 1, wantLen: maxFailureArtifactBytes + len(truncatedArtifactMarker)},
	} {
		dir := t.TempDir()
		src := filepath.Join(dir, "server.log")
		content := bytes.Repeat([]byte("x"), tc.size)
		content[0], content[len(content)-1] = 'H', 'T'
		if err := os.WriteFile(src, content, 0o644); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, "copy.log")

		if err := copyBoundedFile(src, dst); err != nil {
			t.Fatal(err)
		}

		got, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != tc.wantLen || got[0] != 'H' || got[len(got)-1] != 'T' {
			t.Fatalf("size %d: copy is %d bytes (first %q last %q), want %d", tc.size, len(got), got[0], got[len(got)-1], tc.wantLen)
		}
	}
}

func TestCopyBoundedFileReportsUnwritableDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "dolt.log")
	writeTestFile(t, src, "short log\n")

	if err := copyBoundedFile(src, filepath.Join(dir, "missing", "copy.log")); err == nil {
		t.Fatal("copyBoundedFile into a missing directory returned nil")
	}
}

func TestSaveDiagnosticsReportsUnusableArtifactDir(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	writeTestFile(t, blocker, "")
	t.Setenv(FailureArtifactDirEnv, blocker)

	if err := saveDiagnostics("TestX", managedDoltTree(t)); err == nil {
		t.Fatal("saveDiagnostics into a path under a regular file returned nil")
	}
}

func TestSaveDiagnosticsReportsRemovedSourceDir(t *testing.T) {
	t.Setenv(FailureArtifactDirEnv, t.TempDir())
	gone := filepath.Join(t.TempDir(), "gc-acceptance-gone")

	if err := saveDiagnostics("TestX", gone); err == nil {
		t.Fatal("saveDiagnostics of a removed dir returned nil")
	}
}

func TestSaveDiagnosticsUnsetArtifactDirReturnsNil(t *testing.T) {
	t.Setenv(FailureArtifactDirEnv, "")

	if err := saveDiagnostics("TestX", filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatalf("saveDiagnostics with no artifact dir = %v, want nil", err)
	}
}

func TestSaveDiagnosticsHealthyTreeReturnsNil(t *testing.T) {
	t.Setenv(FailureArtifactDirEnv, t.TempDir())

	if err := saveDiagnostics("TestX", managedDoltTree(t)); err != nil {
		t.Fatalf("saveDiagnostics = %v", err)
	}
}
