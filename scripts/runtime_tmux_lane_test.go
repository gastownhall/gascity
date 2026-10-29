package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// laneFixtureFile is one throwaway test file of a fixture package.
type laneFixtureFile struct {
	name string
	tags string // //go:build expression; empty for an untagged file
	pkg  string // defaults to "fixture"
	body string // top-level declarations after the testing import
}

// writeLaneFixture writes files into a package that always has one untagged
// non-test source file, so the package loads under any tag set.
func writeLaneFixture(t *testing.T, files ...laneFixtureFile) string {
	t.Helper()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "fixture.go"), "package fixture\n")
	for _, file := range files {
		pkg := file.pkg
		if pkg == "" {
			pkg = "fixture"
		}
		var content strings.Builder
		if file.tags != "" {
			content.WriteString("//go:build " + file.tags + "\n\n")
		}
		content.WriteString("package " + pkg + "\n\nimport \"testing\"\n\n" + file.body + "\n")
		writeTestFile(t, filepath.Join(dir, file.name), content.String())
	}
	return dir
}

func laneTests(names ...string) string {
	var body strings.Builder
	for _, name := range names {
		body.WriteString("func " + name + "(t *testing.T) {}\n")
	}
	return body.String()
}

// laneDriftForDir runs the lane check the way the real-tree test does, over a
// fixture package and an in-memory record.
func laneDriftForDir(t *testing.T, dir string, record []string) []string {
	t.Helper()
	integration := discoverRuntimeTmuxTestFiles(t, dir, canonicalRuntimeTmuxBuildContext("linux", true))
	untagged := discoverRuntimeTmuxTestFiles(t, dir, canonicalRuntimeTmuxBuildContext("linux", false))
	return runtimeTmuxLaneDrift(integration, untagged, record, "lane.manifest")
}

// guardProblems is everything the real-tree guard asserts: the manifest drift
// and the lane drift. A fixture that adds a test passes only if both are empty.
func guardProblems(t *testing.T, dir string, manifest, record []string) []string {
	t.Helper()
	declared := discoverRuntimeTmuxTests(t, dir, "linux", true)
	return append(runtimeTmuxManifestDrift(manifest, declared), laneDriftForDir(t, dir, record)...)
}

func TestRuntimeTmuxPerFileDiscoveryOrdersFilesThenTests(t *testing.T) {
	dir := writeLaneFixture(t,
		laneFixtureFile{name: "b_test.go", body: laneTests("TestB2", "TestB1")},
		laneFixtureFile{name: "a_test.go", body: laneTests("TestA1")},
		laneFixtureFile{name: "helper_test.go", tags: "integration", body: "func helper() {}\n"},
		laneFixtureFile{name: "x_test.go", pkg: "fixture_test", body: laneTests("TestX1")},
	)

	want := []runtimeTmuxTestFile{
		{Name: "a_test.go", Tests: []string{"TestA1"}},
		{Name: "b_test.go", Tests: []string{"TestB2", "TestB1"}},
		{Name: "helper_test.go"},
		{Name: "x_test.go", Tests: []string{"TestX1"}},
	}
	got := discoverRuntimeTmuxTestFiles(t, dir, canonicalRuntimeTmuxBuildContext("linux", true))
	if len(got) != len(want) {
		t.Fatalf("per-file view = %+v, want %+v", got, want)
	}
	for index := range want {
		if got[index].Name != want[index].Name || !slices.Equal(got[index].Tests, want[index].Tests) {
			t.Fatalf("per-file view entry %d = %+v, want %+v", index, got[index], want[index])
		}
	}

	var flat []string
	for _, file := range got {
		flat = append(flat, file.Tests...)
	}
	if all := discoverRuntimeTmuxTests(t, dir, "linux", true); !slices.Equal(flat, all) {
		t.Fatalf("discoverRuntimeTmuxTests = %q, want the per-file view concatenated %q", all, flat)
	}

	var untaggedFlat []string
	for _, file := range discoverRuntimeTmuxTestFiles(t, dir, canonicalRuntimeTmuxBuildContext("linux", false)) {
		untaggedFlat = append(untaggedFlat, file.Tests...)
	}
	if all := discoverRuntimeTmuxTests(t, dir, "linux", false); !slices.Equal(untaggedFlat, all) {
		t.Fatalf("untagged discoverRuntimeTmuxTests = %q, want the per-file view concatenated %q", all, untaggedFlat)
	}
}

func TestRuntimeTmuxLaneRecordParser(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		content := "# header\n\nB_test.go\na-b_test.go\na_test.go\n\n# trailing\nz_integration_test.go\n"
		want := []string{"B_test.go", "a-b_test.go", "a_test.go", "z_integration_test.go"}
		got, err := parseRuntimeTmuxLaneRecordText(content)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("parse = %q, %v; want %q in byte order", got, err, want)
		}
	})

	t.Run("empty record is allowed", func(t *testing.T) {
		for _, content := range []string{"", "# only a header\n\n"} {
			got, err := parseRuntimeTmuxLaneRecordText(content)
			if err != nil || len(got) != 0 {
				t.Fatalf("parse(%q) = %q, %v; want an empty record", content, got, err)
			}
		}
	})

	invalid := []struct {
		name     string
		content  string
		line     int
		rawLine  string
		contains string
	}{
		{"unsorted", "# header\n\nb_test.go\na_test.go\n", 4, "a_test.go", "ascending"},
		{"duplicate", "a_test.go\na_test.go\n", 2, "a_test.go", "duplicate"},
		{"path separator", "sub/a_test.go\n", 1, "sub/a_test.go", `"_test.go" basename`},
		{"not a test file", "a.go\n", 1, "a.go", `"_test.go" basename`},
		{"bare suffix", "_test.go\n", 1, "_test.go", `"_test.go" basename`},
		{"leading space", "a_test.go\n b_test.go\n", 2, " b_test.go", `"_test.go" basename`},
		{"trailing space", "a_test.go \n", 1, "a_test.go ", `"_test.go" basename`},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseRuntimeTmuxLaneRecordText(tc.content)
			if err == nil {
				t.Fatalf("parse(%q) = %q, want an error", tc.content, got)
			}
			if want := fmt.Sprintf("line %d: %q", tc.line, tc.rawLine); !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %q, want it to name %s", err, want)
			}
			if !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("error = %q, want it to say %q", err, tc.contains)
			}
		})
	}
}

func TestRuntimeTmuxLaneDriftDiagnostics(t *testing.T) {
	t.Run("unrecorded integration-only file", func(t *testing.T) {
		dir := writeLaneFixture(t,
			laneFixtureFile{name: "plain_test.go", body: laneTests("TestPlain")},
			laneFixtureFile{name: "x_integration_test.go", tags: "integration", body: laneTests("TestX1", "TestX2")},
		)
		want := []string{"x_integration_test.go declares 2 tests that compile only with -tags integration and is not in lane.manifest; add it in sorted position (if the file already existed, 2 tests just left the default lane: confirm that is intended)"}
		if got := laneDriftForDir(t, dir, nil); !slices.Equal(got, want) {
			t.Fatalf("lane drift = %q, want %q", got, want)
		}
	})

	t.Run("recorded file lost its tag", func(t *testing.T) {
		dir := writeLaneFixture(t,
			laneFixtureFile{name: "x_integration_test.go", body: laneTests("TestX1", "TestX2", "TestX3")},
		)
		want := []string{"x_integration_test.go is in lane.manifest but compiles without the integration tag; 3 tests move into the default lane; remove it from lane.manifest"}
		if got := laneDriftForDir(t, dir, []string{"x_integration_test.go"}); !slices.Equal(got, want) {
			t.Fatalf("lane drift = %q, want %q", got, want)
		}
	})

	const noIntegrationOnly = "is in lane.manifest but has no integration-only tests (file gone, tests gone, or excluded)"

	t.Run("recorded file is absent", func(t *testing.T) {
		dir := writeLaneFixture(t, laneFixtureFile{name: "plain_test.go", body: laneTests("TestPlain")})
		want := []string{"gone_test.go " + noIntegrationOnly}
		if got := laneDriftForDir(t, dir, []string{"gone_test.go"}); !slices.Equal(got, want) {
			t.Fatalf("lane drift = %q, want %q", got, want)
		}
	})

	t.Run("recorded file declares no tests", func(t *testing.T) {
		dir := writeLaneFixture(t, laneFixtureFile{name: "helper_test.go", tags: "integration", body: "func helper() {}\n"})
		want := []string{"helper_test.go " + noIntegrationOnly}
		if got := laneDriftForDir(t, dir, []string{"helper_test.go"}); !slices.Equal(got, want) {
			t.Fatalf("lane drift = %q, want %q", got, want)
		}
	})

	t.Run("file excluded when the integration tag is set", func(t *testing.T) {
		dir := writeLaneFixture(t,
			laneFixtureFile{name: "default_only_test.go", tags: "!integration", body: laneTests("TestA", "TestB")},
		)
		want := []string{"default_only_test.go is excluded when -tags integration is set; 2 tests would vanish from the integration lane; the lane model needs the integration set to contain the default set"}
		if got := laneDriftForDir(t, dir, nil); !slices.Equal(got, want) {
			t.Fatalf("lane drift = %q, want %q", got, want)
		}
	})

	t.Run("excluded file that is also recorded reports only the exclusion", func(t *testing.T) {
		dir := writeLaneFixture(t,
			laneFixtureFile{name: "default_only_test.go", tags: "!integration", body: laneTests("TestA")},
		)
		want := []string{"default_only_test.go is excluded when -tags integration is set; 1 tests would vanish from the integration lane; the lane model needs the integration set to contain the default set"}
		if got := laneDriftForDir(t, dir, []string{"default_only_test.go"}); !slices.Equal(got, want) {
			t.Fatalf("lane drift = %q, want %q", got, want)
		}
	})

	t.Run("failures are reported together sorted by file name", func(t *testing.T) {
		dir := writeLaneFixture(t,
			laneFixtureFile{name: "z_integration_test.go", tags: "integration", body: laneTests("TestZ")},
			laneFixtureFile{name: "m_test.go", body: laneTests("TestM")},
		)
		got := laneDriftForDir(t, dir, []string{"a_test.go", "m_test.go"})
		if len(got) != 3 ||
			!strings.HasPrefix(got[0], "a_test.go ") ||
			!strings.HasPrefix(got[1], "m_test.go ") ||
			!strings.HasPrefix(got[2], "z_integration_test.go ") {
			t.Fatalf("lane drift = %q, want three failures for a_test.go, m_test.go and z_integration_test.go in that order", got)
		}
	})

	t.Run("a tagged file with no tests and a default-set XTest file need no record", func(t *testing.T) {
		dir := writeLaneFixture(t,
			laneFixtureFile{name: "helper_test.go", tags: "integration", body: "func helper() {}\n"},
			laneFixtureFile{name: "x_test.go", pkg: "fixture_test", body: laneTests("TestX")},
			laneFixtureFile{name: "plain_test.go", body: laneTests("TestPlain")},
		)
		if got := laneDriftForDir(t, dir, nil); len(got) != 0 {
			t.Fatalf("lane drift = %q, want none", got)
		}
	})
}

func TestRuntimeTmuxGuardAddingATestCostsOnlyManifestLines(t *testing.T) {
	record := []string{"c_integration_test.go"}
	files := func(b, c string) []laneFixtureFile {
		return []laneFixtureFile{
			{name: "a_test.go", body: laneTests("TestA1")},
			{name: "b_test.go", body: b},
			{name: "c_integration_test.go", tags: "integration", body: c},
		}
	}

	t.Run("baseline", func(t *testing.T) {
		dir := writeLaneFixture(t, files(laneTests("TestB1"), laneTests("TestC1"))...)
		if got := guardProblems(t, dir, []string{"TestA1", "TestB1", "TestC1"}, record); len(got) != 0 {
			t.Fatalf("baseline guard problems = %q, want none", got)
		}
	})

	t.Run("an untagged test is one manifest line", func(t *testing.T) {
		dir := writeLaneFixture(t, files(laneTests("TestB1", "TestB2"), laneTests("TestC1"))...)
		if got := guardProblems(t, dir, []string{"TestA1", "TestB1", "TestB2", "TestC1"}, record); len(got) != 0 {
			t.Fatalf("guard problems after adding an untagged test = %q, want none with the record untouched", got)
		}
	})

	t.Run("a test inside a recorded integration-only file is one manifest line", func(t *testing.T) {
		dir := writeLaneFixture(t, files(laneTests("TestB1"), laneTests("TestC1", "TestC2"))...)
		if got := guardProblems(t, dir, []string{"TestA1", "TestB1", "TestC1", "TestC2"}, record); len(got) != 0 {
			t.Fatalf("guard problems after adding to a recorded file = %q, want none with the record untouched", got)
		}
	})

	t.Run("a new integration-only file needs its record line", func(t *testing.T) {
		dir := writeLaneFixture(t, append(files(laneTests("TestB1"), laneTests("TestC1")),
			laneFixtureFile{name: "d_integration_test.go", tags: "integration", body: laneTests("TestD1")})...)
		manifest := []string{"TestA1", "TestB1", "TestC1", "TestD1"}
		if got := guardProblems(t, dir, manifest, record); len(got) != 1 || !strings.HasPrefix(got[0], "d_integration_test.go declares 1 tests") {
			t.Fatalf("guard problems without the record line = %q, want only the d_integration_test.go lane failure", got)
		}
		if got := guardProblems(t, dir, manifest, []string{"c_integration_test.go", "d_integration_test.go"}); len(got) != 0 {
			t.Fatalf("guard problems with the record line = %q, want none", got)
		}
	})
}

func TestRuntimeTmuxLaneRecordMatchesTree(t *testing.T) {
	repo := repoRoot(t)
	dir := filepath.Join(repo, "internal", "runtime", "tmux")
	record := parseRuntimeTmuxLaneRecord(t, filepath.Join(repo, runtimeTmuxLaneRecordRelativePath))

	integration := discoverRuntimeTmuxTestFiles(t, dir, canonicalRuntimeTmuxBuildContext("linux", true))
	untagged := discoverRuntimeTmuxTestFiles(t, dir, canonicalRuntimeTmuxBuildContext("linux", false))
	if drift := runtimeTmuxLaneDrift(integration, untagged, record, runtimeTmuxLaneRecordRelativePath); len(drift) != 0 {
		t.Fatalf("runtime-tmux lane record drift:\n%s", strings.Join(drift, "\n"))
	}
}

const runtimeTmuxLaneRecordRelativePath = "scripts/runtime-tmux-integration-files.manifest"

// parseRuntimeTmuxLaneRecordText reads the lane record: one _test.go basename
// per line in strictly ascending byte order, with blank and # lines skipped.
func parseRuntimeTmuxLaneRecordText(content string) ([]string, error) {
	var record []string
	for lineNumber, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		reason := ""
		switch {
		case line != rawLine || !isSafeLaneRecordName(line):
			reason = `not a safe "_test.go" basename`
		case len(record) > 0 && line == record[len(record)-1]:
			reason = "duplicate"
		case len(record) > 0 && line < record[len(record)-1]:
			reason = "not in ascending byte order"
		}
		if reason != "" {
			return nil, fmt.Errorf("invalid runtime-tmux lane record entry on line %d: %q (%s)", lineNumber+1, rawLine, reason)
		}
		record = append(record, line)
	}
	return record, nil
}

func isSafeLaneRecordName(name string) bool {
	return len(name) > len("_test.go") &&
		strings.HasSuffix(name, "_test.go") &&
		filepath.Base(name) == name &&
		!strings.ContainsAny(name, "/\\ \t")
}

func parseRuntimeTmuxLaneRecord(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read runtime-tmux lane record: %v", err)
	}
	record, err := parseRuntimeTmuxLaneRecordText(string(content))
	if err != nil {
		t.Fatalf("runtime-tmux lane record %s: %v", path, err)
	}
	return record
}

// runtimeTmuxLaneDrift compares the lane record with the test files the
// integration and untagged builds see. Each file yields at most one failure and
// failures are returned sorted by file name.
func runtimeTmuxLaneDrift(integration, untagged []runtimeTmuxTestFile, record []string, recordPath string) []string {
	integrationFiles := make(map[string]runtimeTmuxTestFile, len(integration))
	for _, file := range integration {
		integrationFiles[file.Name] = file
	}
	untaggedFiles := make(map[string]runtimeTmuxTestFile, len(untagged))
	for _, file := range untagged {
		untaggedFiles[file.Name] = file
	}
	recorded := make(map[string]struct{}, len(record))
	for _, name := range record {
		recorded[name] = struct{}{}
	}

	failures := make(map[string]string)
	for _, file := range untagged {
		if _, kept := integrationFiles[file.Name]; !kept && len(file.Tests) > 0 {
			failures[file.Name] = fmt.Sprintf("%s is excluded when -tags integration is set; %d tests would vanish from the integration lane; the lane model needs the integration set to contain the default set", file.Name, len(file.Tests))
		}
	}
	for _, file := range integration {
		_, inDefault := untaggedFiles[file.Name]
		_, isRecorded := recorded[file.Name]
		if !inDefault && len(file.Tests) > 0 && !isRecorded {
			failures[file.Name] = fmt.Sprintf("%s declares %d tests that compile only with -tags integration and is not in %s; add it in sorted position (if the file already existed, %d tests just left the default lane: confirm that is intended)", file.Name, len(file.Tests), recordPath, len(file.Tests))
		}
	}
	for _, name := range record {
		if _, done := failures[name]; done {
			continue
		}
		defaultFile, inDefault := untaggedFiles[name]
		integrationFile, inIntegration := integrationFiles[name]
		switch {
		case len(defaultFile.Tests) > 0 && inIntegration:
			failures[name] = fmt.Sprintf("%s is in %s but compiles without the integration tag; %d tests move into the default lane; remove it from %s", name, recordPath, len(defaultFile.Tests), recordPath)
		case inDefault || len(integrationFile.Tests) == 0:
			failures[name] = fmt.Sprintf("%s is in %s but has no integration-only tests (file gone, tests gone, or excluded)", name, recordPath)
		}
	}

	names := make([]string, 0, len(failures))
	for name := range failures {
		names = append(names, name)
	}
	slices.Sort(names)
	drift := make([]string, 0, len(names))
	for _, name := range names {
		drift = append(drift, failures[name])
	}
	return drift
}
