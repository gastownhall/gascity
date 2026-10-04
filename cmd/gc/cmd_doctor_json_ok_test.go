package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/gastownhall/gascity/internal/doctor"
)

// doctorPublishedSchema returns the doctor schema gc publishes for role, so the
// assertions below hold against the contract rather than a copy of it.
func doctorPublishedSchema(t *testing.T, role string) *jsonschema.Schema {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"doctor", "--json-schema=" + role}, &stdout, &stderr); code != 0 {
		t.Fatalf("doctor --json-schema=%s code=%d stderr=%q", role, code, stderr.String())
	}
	return compileJSONSchema(t, "gc://schemas/doctor/"+role+".schema.json", stdout.Bytes())
}

type doctorJSONOKPayload struct {
	SchemaVersion  string `json:"schema_version"`
	OK             *bool  `json:"ok"`
	Failed         int    `json:"failed"`
	BlockingFailed int    `json:"blocking_failed"`
	Results        []struct {
		Name     string `json:"name"`
		Status   string `json:"status"`
		Severity string `json:"severity"`
	} `json:"results"`
	Error *struct {
		Code     string `json:"code"`
		Message  string `json:"message"`
		ExitCode int    `json:"exit_code"`
	} `json:"error"`
}

func decodeDoctorJSONOK(t *testing.T, data []byte) (any, doctorJSONOKPayload) {
	t.Helper()
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, data)
	}
	var payload doctorJSONOKPayload
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("decode doctor JSON: %v\n%s", err, data)
	}
	return raw, payload
}

// A blocking failure makes gc doctor exit non-zero, so its --json record is a
// failure record: ok:false with an error whose exit_code is the process exit
// code, per the shared failure schema. Reporting ok:true there told a caller
// that reads the field instead of the exit code that a failing city was clean.
func TestWriteDoctorJSONBlockingFailureReportsOKFalse(t *testing.T) {
	report := &doctor.Report{
		Passed:         1,
		Failed:         1,
		BlockingFailed: 1,
		Results: []*doctor.CheckResult{
			{Name: "healthy", Status: doctor.StatusOK, Severity: doctor.SeverityBlocking, Message: "fine"},
			{Name: "broken", Status: doctor.StatusError, Severity: doctor.SeverityBlocking, Message: "broken"},
		},
	}
	var buf bytes.Buffer
	if err := writeDoctorJSON(&buf, report); err != nil {
		t.Fatalf("writeDoctorJSON: %v", err)
	}
	raw, payload := decodeDoctorJSONOK(t, buf.Bytes())

	if err := doctorPublishedSchema(t, jsonSchemaFailureRole).Validate(raw); err != nil {
		t.Fatalf("blocking-failure record does not satisfy the published failure schema: %v\n%s", err, buf.String())
	}
	if payload.OK == nil || *payload.OK {
		t.Fatalf("ok = %v, want false for a run with blocking failures", payload.OK)
	}
	if payload.Error == nil {
		t.Fatalf("error absent, want a structured error; out=%s", buf.String())
	}
	if payload.Error.Code != doctorBlockingChecksFailedErrorCode {
		t.Errorf("error.code = %q, want %q", payload.Error.Code, doctorBlockingChecksFailedErrorCode)
	}
	if payload.Error.ExitCode != doctorBlockingFailureExitCode {
		t.Errorf("error.exit_code = %d, want %d", payload.Error.ExitCode, doctorBlockingFailureExitCode)
	}
	// The report itself still rides along: a failing run is exactly when the
	// caller needs the per-check results.
	if payload.BlockingFailed != 1 || len(payload.Results) != 2 {
		t.Errorf("blocking_failed=%d results=%d, want 1 and 2", payload.BlockingFailed, len(payload.Results))
	}
}

// Advisory failures do not gate the exit code, so the record stays a success
// record: ok:true, no error, valid against the published result schema.
func TestWriteDoctorJSONAdvisoryFailuresKeepOKTrue(t *testing.T) {
	report := &doctor.Report{
		Failed: 1,
		Results: []*doctor.CheckResult{
			{Name: "advisory", Status: doctor.StatusError, Severity: doctor.SeverityAdvisory, Message: "advisory issue"},
		},
	}
	var buf bytes.Buffer
	if err := writeDoctorJSON(&buf, report); err != nil {
		t.Fatalf("writeDoctorJSON: %v", err)
	}
	raw, payload := decodeDoctorJSONOK(t, buf.Bytes())

	if err := doctorPublishedSchema(t, jsonSchemaResultRole).Validate(raw); err != nil {
		t.Fatalf("advisory-only record does not satisfy the published result schema: %v\n%s", err, buf.String())
	}
	if payload.OK == nil || !*payload.OK {
		t.Fatalf("ok = %v, want true when no blocking check failed", payload.OK)
	}
	if payload.Error != nil {
		t.Errorf("error = %+v, want absent", payload.Error)
	}
}

// End to end: the ok field and error.exit_code agree with the exit code gc
// doctor --json actually returns for a city with a blocking failure.
func TestDoctorJSONBlockingFailureOKMatchesExitCode(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte("workspace_name = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No builtin imports: builtin-pack-imports fails as a blocking check.
	t.Setenv("GC_BEADS", "file")
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "doctor", "--json", "--check", "builtin-pack-imports"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("exit = 0, want non-zero for a missing builtin import; stdout=%s", stdout.String())
	}
	raw, payload := decodeDoctorJSONOK(t, stdout.Bytes())
	if err := doctorPublishedSchema(t, jsonSchemaFailureRole).Validate(raw); err != nil {
		t.Fatalf("non-zero exit record does not satisfy the published failure schema: %v\n%s", err, stdout.String())
	}
	if payload.OK == nil || *payload.OK {
		t.Fatalf("ok = %v with exit %d, want false", payload.OK, code)
	}
	if payload.Error == nil || payload.Error.ExitCode != code {
		t.Fatalf("error = %+v, want exit_code %d", payload.Error, code)
	}
	if payload.BlockingFailed == 0 {
		t.Errorf("blocking_failed = 0, want the failing check counted")
	}
}
