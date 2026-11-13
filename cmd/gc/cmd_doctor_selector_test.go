package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// newDoctorSelectorCity builds the same minimal, all-passing city the other
// doctor CLI tests use, so a failing assertion here is about --check and not
// about workspace health.
func newDoctorSelectorCity(t *testing.T) string {
	t.Helper()
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core")
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte("workspace_name = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	return cityDir
}

type doctorSelectorPayload struct {
	SchemaVersion  string `json:"schema_version"`
	OK             *bool  `json:"ok"`
	Passed         int    `json:"passed"`
	BlockingFailed int    `json:"blocking_failed"`
	Results        []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"results"`
	Error *struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		ExitCode int    `json:"exit_code"`
	} `json:"error"`
	RegisteredChecks []string `json:"registered_checks"`
}

func runDoctorJSON(t *testing.T, cityDir string, args ...string) (int, doctorSelectorPayload, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	argv := append([]string{"--city", cityDir, "doctor", "--json"}, args...)
	code := run(argv, &stdout, &stderr)
	var payload doctorSelectorPayload
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\nstdout=%s\nstderr=%s", err, stdout.String(), stderr.String())
	}
	return code, payload, stderr.String()
}

func resultNames(payload doctorSelectorPayload) []string {
	names := make([]string, 0, len(payload.Results))
	for _, r := range payload.Results {
		names = append(names, r.Name)
	}
	return names
}

func TestDoctorCheckFlagRunsOnlyTheNamedCheck(t *testing.T) {
	cityDir := newDoctorSelectorCity(t)
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	code, payload, stderr := runDoctorJSON(t, cityDir, "--check", "city-structure")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
	}
	if got := resultNames(payload); len(got) != 1 || got[0] != "city-structure" {
		t.Fatalf("results = %v, want [city-structure]", got)
	}
	if payload.Passed != 1 {
		t.Errorf("passed = %d, want 1", payload.Passed)
	}
}

func TestDoctorCheckFlagAcceptsCommaListAndRepetitionAlike(t *testing.T) {
	cityDir := newDoctorSelectorCity(t)
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	_, commaList, _ := runDoctorJSON(t, cityDir, "--check", "city-structure,city-config")
	_, repeated, _ := runDoctorJSON(t, cityDir, "--check", "city-structure", "--check", "city-config")

	want := []string{"city-structure", "city-config"}
	if got := resultNames(commaList); !slices.Equal(got, want) {
		t.Errorf("comma list results = %v, want %v", got, want)
	}
	if got := resultNames(repeated); !slices.Equal(got, want) {
		t.Errorf("repeated flag results = %v, want %v", got, want)
	}
}

func TestDoctorCheckFlagReportsInRunOrderNotRequestOrder(t *testing.T) {
	cityDir := newDoctorSelectorCity(t)
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	_, payload, _ := runDoctorJSON(t, cityDir, "--check", "city-config", "--check", "city-structure")
	want := []string{"city-structure", "city-config"}
	if got := resultNames(payload); !slices.Equal(got, want) {
		t.Errorf("results = %v, want %v (registration order, not request order)", got, want)
	}
}

func TestDoctorUnknownCheckNameFailsAndListsWhatExists(t *testing.T) {
	cityDir := newDoctorSelectorCity(t)
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "doctor", "--check", "city-structur"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `"city-structur"`) {
		t.Errorf("stderr does not name the unmatched check: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), `did you mean "city-structure"`) {
		t.Errorf("stderr does not suggest the near match: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "checks registered in this workspace") {
		t.Errorf("stderr does not list the registered checks: %q", stderr.String())
	}
	// Nothing may have run: a partial report is what this error exists to
	// prevent a caller from reading as a verdict.
	if strings.Contains(stdout.String(), "✓") || strings.Contains(stdout.String(), "passed") {
		t.Errorf("stdout reported check results on an unknown-name failure: %q", stdout.String())
	}
}

func TestDoctorUnknownCheckNameJSONCarriesErrorAndEmptyResults(t *testing.T) {
	cityDir := newDoctorSelectorCity(t)
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	// The failure schema comes from the command itself, so this asserts against
	// the contract gc publishes rather than a copy of it in the test.
	var schemaStdout, schemaStderr bytes.Buffer
	if code := run([]string{"doctor", "--json-schema=failure"}, &schemaStdout, &schemaStderr); code != 0 {
		t.Fatalf("failure schema code=%d stderr=%q", code, schemaStderr.String())
	}
	failureSchema := compileJSONSchema(t, "gc://schemas/failure.schema.json", schemaStdout.Bytes())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "doctor", "--json", "--check", "no-such-check"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero")
	}

	var raw any
	if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if err := failureSchema.Validate(raw); err != nil {
		t.Fatalf("payload does not satisfy the published failure schema: %v\n%s", err, stdout.String())
	}

	var payload doctorSelectorPayload
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	// ok:false is the whole point. A report-shaped payload would get ok:true
	// from withDefaultSuccessOK and report zeroed counts, so a run that
	// measured nothing would read clean to a caller checking only that field.
	if payload.OK == nil || *payload.OK {
		t.Errorf("ok = %v, want false", payload.OK)
	}
	if payload.Error == nil || payload.Error.Code != "unknown_check" {
		t.Errorf("error = %+v, want code unknown_check", payload.Error)
	}
	if payload.Error != nil && payload.Error.ExitCode != code {
		t.Errorf("error.exit_code = %d, want the process exit code %d", payload.Error.ExitCode, code)
	}
	if len(payload.Results) != 0 {
		t.Errorf("results = %v, want empty", resultNames(payload))
	}
	if len(payload.RegisteredChecks) == 0 {
		t.Error("registered_checks is empty; the caller has no way to learn what it could ask for")
	}
}

func TestDoctorSelectedRunStillReportsOKTrue(t *testing.T) {
	// The failure envelope is scoped to the unknown-name path. A selected run
	// that succeeds keeps the ordinary report shape, ok:true included, so
	// existing --json consumers see no change.
	cityDir := newDoctorSelectorCity(t)
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	code, payload, stderr := runDoctorJSON(t, cityDir, "--check", "city-structure")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%s", code, stderr)
	}
	if payload.OK == nil || !*payload.OK {
		t.Errorf("ok = %v, want true", payload.OK)
	}
	if payload.Error != nil {
		t.Errorf("error = %+v, want absent", payload.Error)
	}
	if len(payload.RegisteredChecks) != 0 {
		t.Errorf("registered_checks = %v, want absent on a successful run", payload.RegisteredChecks)
	}
}

func TestDoctorUnknownCheckNameFailsEvenWhenAnotherNameMatched(t *testing.T) {
	cityDir := newDoctorSelectorCity(t)
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	code, payload, _ := runDoctorJSON(t, cityDir, "--check", "city-structure", "--check", "no-such-check")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero: one bad name has to fail the whole run")
	}
	if len(payload.Results) != 0 {
		t.Errorf("results = %v, want empty; the matched half must not run", resultNames(payload))
	}
}

func TestDoctorBlankCheckNameFailsRatherThanRunningEverything(t *testing.T) {
	cityDir := newDoctorSelectorCity(t)
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	code, payload, _ := runDoctorJSON(t, cityDir, "--check", "")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero for an empty check name")
	}
	if len(payload.Results) != 0 {
		t.Errorf("results = %v, want empty", resultNames(payload))
	}
}

func TestDoctorExitCodeReflectsTheSelectedSubsetOnly(t *testing.T) {
	cityDir := newDoctorSelectorCity(t)
	// An empty PATH fails every binary check, which are blocking, so the full
	// sweep exits non-zero. Selecting a healthy check has to exit 0 — that is
	// the whole point of asking for one verdict instead of a report.
	t.Setenv("PATH", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := run([]string{"--city", cityDir, "doctor"}, &stdout, &stderr); code == 0 {
		t.Fatalf("full sweep exit = 0, want non-zero; the fixture is supposed to have failing binary checks\nstdout=%s", stdout.String())
	}

	code, payload, selectedStderr := runDoctorJSON(t, cityDir, "--check", "city-structure")
	if code != 0 {
		t.Fatalf("selected exit = %d, want 0; stderr=%s results=%v", code, selectedStderr, payload.Results)
	}
	if payload.BlockingFailed != 0 {
		t.Errorf("blocking_failed = %d, want 0", payload.BlockingFailed)
	}
}

func TestDoctorCheckSuggestionsCoverMisspellingAndIncompleteNames(t *testing.T) {
	registered := []string{"controller", "custom-types:city", "rig:core:dolt-server", "events-log"}
	cases := []struct {
		typed string
		want  string
	}{
		{"controler", "controller"},                      // dropped letter: no substring relation either way
		{"contorller", "controller"},                     //nolint:misspell // intentional transposition under test
		{"dolt-server", "rig:core:dolt-server"},          // correct but unscoped
		{"custom-types:city:extra", "custom-types:city"}, // over-qualified
	}
	for _, tc := range cases {
		got := doctorCheckSuggestions([]string{tc.typed}, registered)
		if !slices.Contains(got, tc.want) {
			t.Errorf("suggestions for %q = %v, want to include %q", tc.typed, got, tc.want)
		}
	}
}

func TestDoctorCheckSuggestionsStayQuietOnAnUnrelatedName(t *testing.T) {
	registered := []string{"controller", "events-log", "worktree"}
	if got := doctorCheckSuggestions([]string{"zzzzzzzzzz"}, registered); len(got) != 0 {
		t.Errorf("suggestions = %v, want none for an unrelated name", got)
	}
}

// newDoctorFixPrerequisiteCity builds a city whose builtin-pack-imports check
// has a fix to make (no bundled imports declared yet), which is the state in
// which packv2-import-state's fix depends on builtin-pack-imports having run
// first.
func newDoctorFixPrerequisiteCity(t *testing.T) string {
	t.Helper()
	cityDir := writeBuiltinImportTestCity(t, "[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\n")
	writePackToml(t, cityDir, "[pack]\nname = \"demo\"\nschema = 2\n")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")
	return cityDir
}

// readCityDeclarations returns city.toml and pack.toml as one string, so a
// test can tell whether any fix wrote to either.
func readCityDeclarations(t *testing.T, cityDir string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range []string{"city.toml", "pack.toml"} {
		data, err := os.ReadFile(filepath.Join(cityDir, name))
		if err != nil {
			t.Fatal(err)
		}
		b.WriteString(name + ":\n" + string(data))
	}
	return b.String()
}

func TestDoctorCheckFixRefusesWhenFixPrerequisiteNotSelected(t *testing.T) {
	cityDir := newDoctorFixPrerequisiteCity(t)
	before := readCityDeclarations(t, cityDir)

	// Control: the prerequisite really has something to fix in this city.
	if _, payload, stderr := runDoctorJSON(t, cityDir, "--check", "builtin-pack-imports"); len(payload.Results) != 1 || payload.Results[0].Status == "ok" {
		t.Fatalf("fixture invalid: builtin-pack-imports results = %+v, want one non-ok result; stderr=%s", payload.Results, stderr)
	}

	code, payload, stderr := runDoctorJSON(t, cityDir, "--check", "packv2-import-state", "--fix")
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero: --check packv2-import-state --fix skips builtin-pack-imports' fix")
	}
	if payload.Error == nil || payload.Error.Code != "doctor_fix_prerequisite_missing" {
		t.Fatalf("error = %+v, want code doctor_fix_prerequisite_missing; stderr=%s", payload.Error, stderr)
	}
	if !strings.Contains(payload.Error.Message, "builtin-pack-imports") || !strings.Contains(payload.Error.Message, "packv2-import-state") {
		t.Errorf("message = %q, want it to name both packv2-import-state and the prerequisite builtin-pack-imports", payload.Error.Message)
	}
	if len(payload.Results) != 0 {
		t.Errorf("results = %v, want empty: nothing may run on a refusal", resultNames(payload))
	}
	if after := readCityDeclarations(t, cityDir); after != before {
		t.Errorf("a refused --fix changed the city:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestDoctorCheckFixRefusalNamesPrerequisiteInTextMode(t *testing.T) {
	cityDir := newDoctorFixPrerequisiteCity(t)

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "doctor", "--check", "packv2-import-state", "--fix"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "--check builtin-pack-imports") {
		t.Errorf("stderr = %q, want it to say which --check to add", stderr.String())
	}
	if strings.Contains(stdout.String(), "✓") || strings.Contains(stdout.String(), "passed") {
		t.Errorf("stdout reported check results on a refusal: %q", stdout.String())
	}
}

func TestDoctorCheckFixRunsPrerequisiteFirstWhenBothNamed(t *testing.T) {
	cityDir := newDoctorFixPrerequisiteCity(t)
	before := readCityDeclarations(t, cityDir)

	// Request order is the reverse of the order the fixes have to run in.
	_, payload, stderr := runDoctorJSON(t, cityDir, "--check", "packv2-import-state", "--check", "builtin-pack-imports", "--fix")
	if payload.Error != nil {
		t.Fatalf("error = %+v, want none when the prerequisite is named; stderr=%s", payload.Error, stderr)
	}
	want := []string{"builtin-pack-imports", "packv2-import-state"}
	if got := resultNames(payload); !slices.Equal(got, want) {
		t.Errorf("results = %v, want %v (registration order)", got, want)
	}
	if after := readCityDeclarations(t, cityDir); after == before {
		t.Errorf("builtin-pack-imports' fix did not run: city unchanged:\n%s", after)
	}
}

func TestDoctorCheckWithoutFixDoesNotRequireFixPrerequisites(t *testing.T) {
	cityDir := newDoctorFixPrerequisiteCity(t)

	_, payload, stderr := runDoctorJSON(t, cityDir, "--check", "packv2-import-state")
	if payload.Error != nil {
		t.Fatalf("error = %+v, want none: the ordering only matters when a fix runs; stderr=%s", payload.Error, stderr)
	}
	if got := resultNames(payload); !slices.Equal(got, []string{"packv2-import-state"}) {
		t.Errorf("results = %v, want [packv2-import-state]", got)
	}
}
