package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func writeLocalDoctorScript(t *testing.T, cityPath, relPath, body string) {
	t.Helper()
	path := filepath.Join(cityPath, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir script dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
}

func requireSingleDoctorResult(t *testing.T, report *doctor.Report) *doctor.CheckResult {
	t.Helper()
	if len(report.Results) != 1 {
		t.Fatalf("len(report.Results) = %d, want 1", len(report.Results))
	}
	return report.Results[0]
}

func TestRegisterLocalDoctorChecksRunsCityRelativeScript(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_PACK_STATE_DIR", "")
	writeLocalDoctorScript(t, cityPath, filepath.Join("scripts", "check-env.sh"), `#!/bin/sh
if [ "$GC_PACK_DIR" != "$GC_CITY_PATH" ]; then
	echo "GC_PACK_DIR mismatch"
	exit 2
fi
if [ -n "${GC_PACK_STATE_DIR:-}" ]; then
	echo "unexpected GC_PACK_STATE_DIR"
	exit 2
fi
echo "local ok"
`)

	d := &doctor.Doctor{}
	registerLocalDoctorChecks(d, cityPath, []config.LocalDoctorCheck{{
		Name:   "env",
		Script: filepath.Join("scripts", "check-env.sh"),
	}})

	result := requireSingleDoctorResult(t, d.RunCollect(&doctor.CheckContext{CityPath: cityPath}, false))
	if result.Name != "local:env" {
		t.Fatalf("result.Name = %q, want %q", result.Name, "local:env")
	}
	if result.Status != doctor.StatusOK {
		t.Fatalf("result.Status = %v, want OK; message=%q", result.Status, result.Message)
	}
	if result.Message != "local ok" {
		t.Fatalf("result.Message = %q, want %q", result.Message, "local ok")
	}
}

func TestResolveLocalDoctorScriptRejectsUnsafePaths(t *testing.T) {
	cityPath := t.TempDir()
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{
			name:    "absolute",
			path:    filepath.Join(cityPath, "scripts", "check.sh"),
			wantErr: "must be relative",
		},
		{
			name:    "parent",
			path:    "../escape.sh",
			wantErr: "escapes the city directory",
		},
		{
			name:    "nested parent",
			path:    filepath.Join("scripts", "..", "..", "escape.sh"),
			wantErr: "escapes the city directory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := resolveLocalDoctorScript(cityPath, tt.path); err == nil {
				t.Fatal("resolveLocalDoctorScript() error = nil, want error")
			} else if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("resolveLocalDoctorScript() error = %q, want containing %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestRegisterLocalDoctorChecksInvalidScriptPathReportsErrorCheck(t *testing.T) {
	cityPath := t.TempDir()
	d := &doctor.Doctor{}
	registerLocalDoctorChecks(d, cityPath, []config.LocalDoctorCheck{{
		Name:   "bad",
		Script: "../escape.sh",
	}})

	result := requireSingleDoctorResult(t, d.RunCollect(&doctor.CheckContext{CityPath: cityPath}, false))
	if result.Name != "local:bad" {
		t.Fatalf("result.Name = %q, want %q", result.Name, "local:bad")
	}
	if result.Status != doctor.StatusError {
		t.Fatalf("result.Status = %v, want error", result.Status)
	}
	if !strings.Contains(result.Message, "escapes the city directory") {
		t.Fatalf("result.Message = %q, want escape error", result.Message)
	}
}

func TestRegisterLocalDoctorChecksInvalidFixPathReportsErrorCheck(t *testing.T) {
	cityPath := t.TempDir()
	writeLocalDoctorScript(t, cityPath, filepath.Join("scripts", "check.sh"), "#!/bin/sh\necho ok\n")

	d := &doctor.Doctor{}
	registerLocalDoctorChecks(d, cityPath, []config.LocalDoctorCheck{{
		Name:   "bad-fix",
		Script: filepath.Join("scripts", "check.sh"),
		Fix:    "../fix.sh",
	}})

	result := requireSingleDoctorResult(t, d.RunCollect(&doctor.CheckContext{CityPath: cityPath}, false))
	if result.Name != "local:bad-fix" {
		t.Fatalf("result.Name = %q, want %q", result.Name, "local:bad-fix")
	}
	if result.Status != doctor.StatusError {
		t.Fatalf("result.Status = %v, want error", result.Status)
	}
	if !strings.Contains(result.Message, "fix path") || !strings.Contains(result.Message, "escapes the city directory") {
		t.Fatalf("result.Message = %q, want fix escape error", result.Message)
	}
}
