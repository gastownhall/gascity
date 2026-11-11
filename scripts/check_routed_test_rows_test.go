package scripts_test

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

var routedMatrixRows = []string{
	"api-happy-path",
	"api-cache-not-live",
	"api-500-fallback",
	"api-404-error",
	"controller-down",
	"escape-hatch",
}

// runRoutedRowsGuard executes the real guard, including its exit status.
func runRoutedRowsGuard(t *testing.T, root string) (string, int) {
	t.Helper()
	out, err := exec.Command("bash", filepath.Join(root, "scripts", "check-routed-test-rows.sh")).CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("execute routed-row guard: %v\n%s", err, out)
	}
	return string(out), exit.ExitCode()
}

// routedRowsFixture copies the unchanged checker into an isolated corpus.
func routedRowsFixture(t *testing.T, manifest *string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "scripts", "check-routed-test-rows.sh"),
		readFile(t, repoRoot(t), "scripts/check-routed-test-rows.sh"))
	if manifest != nil {
		writeTestFile(t, filepath.Join(root, "scripts", "routed-test-rows.manifest"), *manifest)
	}
	for path, body := range files {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(path)), body)
	}
	return root
}

// TestCheckRoutedTestRowsPassesOnThisRepo keeps fixture success tied to the
// real corpus. Ordinary service reason/refusal values must not force service
// tests to invent read-path matrix rows or join the matrix manifest.
func TestCheckRoutedTestRowsPassesOnThisRepo(t *testing.T) {
	manifest := readFile(t, repoRoot(t), "scripts/routed-test-rows.manifest")
	for _, line := range strings.Split(manifest, "\n") {
		entry, _, _ := strings.Cut(line, "#")
		if strings.TrimSpace(entry) == "cmd/gc/cmd_service_test.go" {
			t.Fatal("ordinary service reason/refusal tests must not be enrolled as a read-path matrix")
		}
	}
	out, code := runRoutedRowsGuard(t, repoRoot(t))
	if code != 0 {
		t.Fatalf("routed-row guard rejects the checked-in corpus (exit %d):\n%s", code, out)
	}
}

// TestCheckRoutedTestRowsAcceptsListedMatrix exercises the guard against an
// existing real six-row suite and manifest formatting used by contributors.
func TestCheckRoutedTestRowsAcceptsListedMatrix(t *testing.T) {
	manifest := "# required read-path suite\n\n  cmd/gc/cmd_status_test.go  # six rows\n"
	root := routedRowsFixture(t, &manifest, map[string]string{
		"cmd/gc/cmd_status_test.go": readFile(t, repoRoot(t), "cmd/gc/cmd_status_test.go"),
		"cmd/gc/cmd_ordinary_test.go": "package main\n" +
			"import \"testing\"\nfunc TestOrdinary(t *testing.T) { t.Log(\"ordinary case\") }\n",
	})
	if out, code := runRoutedRowsGuard(t, root); code != 0 {
		t.Fatalf("complete listed matrix rejected (exit %d):\n%s", code, out)
	}
}

// TestCheckRoutedTestRowsRejectsBrokenManifest preserves the nonempty,
// present-on-disk denominator independently of discovery.
func TestCheckRoutedTestRowsRejectsBrokenManifest(t *testing.T) {
	empty := ""
	comments := "# no files\n  \n\t# still no files\n"
	missingFile := "cmd/gc/cmd_absent_test.go\n"
	for _, tc := range []struct {
		name     string
		manifest *string
		want     string
	}{
		{name: "missing_manifest", want: "manifest missing"},
		{name: "empty_manifest", manifest: &empty, want: "lists no files"},
		{name: "comments_only_manifest", manifest: &comments, want: "lists no files"},
		{name: "missing_listed_file", manifest: &missingFile, want: "MANIFEST FILE MISSING: cmd/gc/cmd_absent_test.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := routedRowsFixture(t, tc.manifest, nil)
			out, code := runRoutedRowsGuard(t, root)
			if code == 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("broken manifest: exit=%d, want nonzero and %q:\n%s", code, tc.want, out)
			}
		})
	}
}

// TestCheckRoutedTestRowsRejectsMissingRows verifies every row is required,
// even if a rename removes all references or duplicates another valid row.
func TestCheckRoutedTestRowsRejectsMissingRows(t *testing.T) {
	const path = "cmd/gc/cmd_status_test.go"
	manifest := path + "\n"
	source := readFile(t, repoRoot(t), path)
	for _, row := range routedMatrixRows {
		for _, replacement := range []string{"", "renamed-row", "api-happy-path"} {
			if replacement == row {
				continue
			}
			t.Run(row+"/replace_with_"+replacement, func(t *testing.T) {
				root := routedRowsFixture(t, &manifest, map[string]string{
					path: strings.ReplaceAll(source, row, replacement),
				})
				out, code := runRoutedRowsGuard(t, root)
				if code == 0 || !strings.Contains(out, "MANIFEST FILE UNDER-COVERED: "+path) {
					t.Fatalf("missing row %q: exit=%d, want under-covered failure:\n%s", row, code, out)
				}
			})
		}
	}
	t.Run("all_rows_renamed", func(t *testing.T) {
		for _, row := range routedMatrixRows {
			source = strings.ReplaceAll(source, row, "renamed-row")
		}
		root := routedRowsFixture(t, &manifest, map[string]string{path: source})
		out, code := runRoutedRowsGuard(t, root)
		if code == 0 || !strings.Contains(out, path+" has 0/6 rows") {
			t.Fatalf("empty listed matrix: exit=%d, want 0/6 failure:\n%s", code, out)
		}
	})
}

// TestCheckRoutedTestRowsRejectsUnlistedMatrix preserves discovery of real
// matrices: one through five distinct rows are partial, six require enrollment.
func TestCheckRoutedTestRowsRejectsUnlistedMatrix(t *testing.T) {
	const listed = "cmd/gc/cmd_status_test.go"
	const unlisted = "cmd/gc/cmd_unlisted_test.go"
	manifest := listed + "\n"
	source := readFile(t, repoRoot(t), listed)
	for count := 1; count <= len(routedMatrixRows); count++ {
		t.Run(strings.Join(routedMatrixRows[:count], "+"), func(t *testing.T) {
			partial := source
			for _, row := range routedMatrixRows[count:] {
				partial = strings.ReplaceAll(partial, row, "removed-row")
			}
			root := routedRowsFixture(t, &manifest, map[string]string{
				listed: source, unlisted: partial,
			})
			want := "INCOMPLETE: " + unlisted
			if count == len(routedMatrixRows) {
				want = "ADD TO MANIFEST: " + unlisted
			}
			out, code := runRoutedRowsGuard(t, root)
			if code == 0 || !strings.Contains(out, want) {
				t.Fatalf("unlisted matrix with %d rows: exit=%d, want %q:\n%s", count, code, want, out)
			}
		})
	}
}
