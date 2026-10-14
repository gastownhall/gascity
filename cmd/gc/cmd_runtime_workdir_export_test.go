package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

func writeWorkDirExportFixture(t *testing.T, rels ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, rel := range rels {
		full := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRuntimeWorkdirExportListsSurvivingEntries(t *testing.T) {
	dir := writeWorkDirExportFixture(t,
		".gc/controller.token",
		".gc/settings.json",
		".beads/doltlite/hq.db",
		".beads/metadata.json",
		"src/main.go",
	)
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"workdir-export", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workdir-export: %v; stderr: %s", err, stderr.String())
	}
	want := ".beads/metadata.json\n.gc/settings.json\nsrc/main.go\n"
	if stdout.String() != want {
		t.Fatalf("stdout =\n%s\nwant\n%s", stdout.String(), want)
	}
}

func TestRuntimeWorkdirExportNullSeparated(t *testing.T) {
	dir := writeWorkDirExportFixture(t, ".gc/events.jsonl", "a")
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"workdir-export", "--null", dir})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("workdir-export --null: %v; stderr: %s", err, stderr.String())
	}
	if got, want := stdout.String(), "a\x00"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
}

func TestRuntimeWorkdirExportMissingDirFails(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newRuntimeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"workdir-export", filepath.Join(t.TempDir(), "missing")})
	if err := cmd.Execute(); err == nil {
		t.Fatal("workdir-export of a missing dir succeeded")
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty on failure", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc runtime workdir-export") {
		t.Fatalf("stderr = %q, want a gc runtime workdir-export error", stderr.String())
	}
}

func runWorkdirExport(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := newRuntimeCmd(&out, &errOut)
	cmd.SetArgs(append([]string{"workdir-export"}, args...))
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

// TestRuntimeWorkdirExportNewlineModeFailsClosed covers names a line-based
// consumer would misread: an embedded newline that forges the line
// ".gc/controller.token", a carriage return, a leading dash tar's default
// file-list mode parses as an option, and backslash escapes GNU tar -T
// unquotes, which turn one file's name into the path of the real token or of
// a file above the work dir. Refusing them is a backstop for human use, not
// what makes the output safe for scripts: only --null does that, and only
// for a consumer that reads the list verbatim (GNU tar from 1.30, or from
// 1.29 with --verbatim-files-from --no-unquote; bsdtar always).
func TestRuntimeWorkdirExportNewlineModeFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name, rel string
	}{
		{"newline", "src\n.gc/controller.token"},
		{"nested newline", "rigs/app/x\n.gc/controller.token"},
		{"carriage return", "src\r.gc/controller.token"},
		{"leading dash", "-Cx/f"},
		{"escaped slash", `.gc\057controller.token`},
		{"escaped parent", `..\057..\057x`},
		{"nested backslash", `rigs/app/a\b`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeWorkDirExportFixture(t, ".gc/controller.token", "src/main.go", tc.rel)
			stdout, stderr, err := runWorkdirExport(t, dir)
			if err == nil {
				t.Fatalf("workdir-export listed %q one per line; stdout %q", tc.rel, stdout)
			}
			if stdout != "" {
				t.Fatalf("stdout = %q, want nothing when newline output would be ambiguous", stdout)
			}
			if !strings.Contains(stderr, "--null") {
				t.Fatalf("stderr = %q, want it to point at --null", stderr)
			}

			stdout, stderr, err = runWorkdirExport(t, "--null", dir)
			if err != nil {
				t.Fatalf("workdir-export --null: %v; stderr: %s", err, stderr)
			}
			got := strings.Split(strings.TrimSuffix(stdout, "\x00"), "\x00")
			sort.Strings(got)
			want := []string{tc.rel, "src/main.go"}
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Fatalf("--null entries = %q, want %q", got, want)
			}
		})
	}
}
