package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func jsonUnmarshalString(s string, v interface{}) error {
	return json.Unmarshal([]byte(s), v)
}

type stubArchiveEnv struct {
	vars map[string]string
}

func (s stubArchiveEnv) get(key string) string { return s.vars[key] }

func initBareArchiveRepo(t *testing.T, dir string, withOrigin bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if withOrigin {
		if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://example.invalid/archive.git").CombinedOutput(); err != nil {
			t.Fatalf("git remote add: %v\n%s", err, out)
		}
	}
}

func writeArchiveState(t *testing.T, stateDir, body string) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(stateDir, "jsonl-export-state.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func runJsonlArchiveCheck(t *testing.T, cityPath string, env map[string]string) *doctor.CheckResult {
	t.Helper()
	check := newJsonlArchiveDoctorCheck(cityPath)
	check.getenv = stubArchiveEnv{vars: env}.get
	return check.Run(&doctor.CheckContext{CityPath: cityPath})
}

func TestJsonlArchiveDoctorCheck_NoStateNoArchive(t *testing.T) {
	cityDir := t.TempDir()
	result := runJsonlArchiveCheck(t, cityDir, map[string]string{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "no archive activity yet") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_MalformedArchiveRepo(t *testing.T) {
	cityDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// No .git subdir → malformed per the check.
	env := map[string]string{"GC_JSONL_ARCHIVE_REPO": archiveDir}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "malformed (no .git)") {
		t.Fatalf("message = %q", result.Message)
	}
	if result.FixHint == "" {
		t.Fatalf("expected a fix hint on malformed archive")
	}
}

func TestJsonlArchiveDoctorCheck_LocalOnlyWarning(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, false)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "local-only mode") {
		t.Fatalf("message = %q", result.Message)
	}
	if !strings.Contains(result.FixHint, "remote add origin") {
		t.Fatalf("fix hint = %q", result.FixHint)
	}
}

func TestJsonlArchiveDoctorCheck_PushModeHealthyWithTimestamp(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, stateDir, `{"consecutive_push_failures":0,"last_push_at":"2026-05-11T12:00:00Z"}`)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "last successful push 2026-05-11T12:00:00Z") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_PushModeHealthyNoTimestamp(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, stateDir, `{}`)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "no pushes attempted yet") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_PushModeFailuresSurfaceStderr(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, stateDir, `{"consecutive_push_failures":3,"last_push_stderr":"fatal: remote rejected"}`)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "3 consecutive push failure") {
		t.Fatalf("message = %q (missing failure count)", result.Message)
	}
	if !strings.Contains(result.Message, "fatal: remote rejected") {
		t.Fatalf("message = %q (missing stderr)", result.Message)
	}
	if !strings.Contains(result.FixHint, "verify credentials") {
		t.Fatalf("fix hint = %q", result.FixHint)
	}
}

func TestJsonlArchiveDoctorCheck_PushModeFailuresWithoutStderr(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, stateDir, `{"consecutive_push_failures":2}`)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "2 consecutive push failure") {
		t.Fatalf("message = %q", result.Message)
	}
	if strings.Contains(result.Message, "Last stderr") {
		t.Fatalf("message %q should not include stderr when stderr is empty", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_MalformedStateTreatedAsEmpty(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, stateDir, `not-json`)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK (malformed state must not escalate); result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "no pushes attempted yet") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_StateInLegacyLocation(t *testing.T) {
	cityDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)

	// Legacy state path: <cityPath>/.gc/jsonl-export-state.json
	legacyDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, "jsonl-export-state.json")
	if err := os.WriteFile(legacyPath, []byte(`{"last_push_at":"2026-05-11T00:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	env := map[string]string{
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "2026-05-11T00:00:00Z") {
		t.Fatalf("message = %q (legacy state path ignored?)", result.Message)
	}
}

func TestDoDoctorRegistersJsonlArchiveCheck(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1
`)
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, false)
	t.Setenv("GC_JSONL_ARCHIVE_REPO", archiveDir)

	prevCityFlag := cityFlag
	prevCityDoltCheck := newDoctorDoltServerCheck
	prevRigDoltCheck := newDoctorRigDoltServerCheck
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		newDoctorDoltServerCheck = prevCityDoltCheck
		newDoctorRigDoltServerCheck = prevRigDoltCheck
	})
	cityFlag = cityDir
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}

	var stdout, stderr strings.Builder
	_ = doDoctor(false, true, false, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "jsonl-archive") {
		t.Fatalf("doctor output missing jsonl-archive check:\n%s", out)
	}
	if !strings.Contains(out, "local-only mode") {
		t.Fatalf("doctor output missing local-only warning:\n%s", out)
	}
}

func TestDoDoctorJSONOutputIncludesArchiveCheck(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1
`)
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, false)
	t.Setenv("GC_JSONL_ARCHIVE_REPO", archiveDir)

	prevCityFlag := cityFlag
	prevCityDoltCheck := newDoctorDoltServerCheck
	prevRigDoltCheck := newDoctorRigDoltServerCheck
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		newDoctorDoltServerCheck = prevCityDoltCheck
		newDoctorRigDoltServerCheck = prevRigDoltCheck
	})
	cityFlag = cityDir
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}

	var stdout, stderr strings.Builder
	_ = doDoctor(false, false, true, &stdout, &stderr)

	out := stdout.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("expected JSON object, got:\n%s", out)
	}
	var decoded struct {
		Passed  int
		Warned  int
		Failed  int
		Results []struct {
			Name    string
			Status  string
			Message string
			FixHint string `json:"fix_hint"`
		}
	}
	if err := jsonUnmarshalString(out, &decoded); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v\n%s", err, out)
	}
	var found *struct {
		Name    string
		Status  string
		Message string
		FixHint string `json:"fix_hint"`
	}
	for i := range decoded.Results {
		if decoded.Results[i].Name == "jsonl-archive" {
			found = &decoded.Results[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("jsonl-archive check missing from JSON output:\n%s", out)
	}
	if found.Status != "warning" {
		t.Fatalf("jsonl-archive status = %q, want \"warning\"", found.Status)
	}
	if !strings.Contains(found.Message, "local-only mode") {
		t.Fatalf("jsonl-archive message = %q", found.Message)
	}
	if found.FixHint == "" {
		t.Fatalf("expected fix_hint in JSON result")
	}
}

func TestFormatArchivePushFailureMessage(t *testing.T) {
	tests := []struct {
		count  int
		stderr string
		want   string
	}{
		{1, "", "push mode, 1 consecutive push failure(s)"},
		{5, "  ", "push mode, 5 consecutive push failure(s)"},
		{2, "fatal: remote rejected", "push mode, 2 consecutive push failure(s). Last stderr: fatal: remote rejected"},
	}
	for _, tt := range tests {
		got := formatArchivePushFailureMessage(tt.count, tt.stderr)
		if got != tt.want {
			t.Errorf("formatArchivePushFailureMessage(%d, %q) = %q, want %q", tt.count, tt.stderr, got, tt.want)
		}
	}
}
