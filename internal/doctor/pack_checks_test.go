package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCheckScript(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "check.sh")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPackScriptCheckOK(t *testing.T) {
	dir := t.TempDir()
	script := writeCheckScript(t, dir, "#!/bin/sh\necho 'all good'\nexit 0\n")

	c := &PackScriptCheck{
		CheckName: "test-topo:check",
		Script:    script,
		PackDir:   dir,
		PackName:  "test-topo",
	}

	if c.Name() != "test-topo:check" {
		t.Errorf("Name() = %q, want %q", c.Name(), "test-topo:check")
	}
	if c.CanFix() {
		t.Error("CanFix() should return false")
	}

	ctx := &CheckContext{CityPath: dir}
	result := c.Run(ctx)

	if result.Status != StatusOK {
		t.Errorf("Status = %d, want StatusOK", result.Status)
	}
	if result.Message != "all good" {
		t.Errorf("Message = %q, want %q", result.Message, "all good")
	}
}

func TestPackScriptCheckWarning(t *testing.T) {
	dir := t.TempDir()
	script := writeCheckScript(t, dir, "#!/bin/sh\necho 'minor issue'\necho 'detail one'\nexit 1\n")

	c := &PackScriptCheck{
		CheckName: "topo:warn",
		Script:    script,
		PackDir:   dir,
		PackName:  "topo",
	}

	result := c.Run(&CheckContext{CityPath: dir})

	if result.Status != StatusWarning {
		t.Errorf("Status = %d, want StatusWarning", result.Status)
	}
	if result.Message != "minor issue" {
		t.Errorf("Message = %q, want %q", result.Message, "minor issue")
	}
	if len(result.Details) != 1 || result.Details[0] != "detail one" {
		t.Errorf("Details = %v, want [detail one]", result.Details)
	}
}

func TestPackScriptCheckError(t *testing.T) {
	dir := t.TempDir()
	script := writeCheckScript(t, dir, "#!/bin/sh\necho 'missing binary'\necho 'foo not found'\necho 'bar not found'\nexit 2\n")

	c := &PackScriptCheck{
		CheckName: "topo:err",
		Script:    script,
		PackDir:   dir,
		PackName:  "topo",
	}

	result := c.Run(&CheckContext{CityPath: dir})

	if result.Status != StatusError {
		t.Errorf("Status = %d, want StatusError", result.Status)
	}
	if result.Message != "missing binary" {
		t.Errorf("Message = %q, want %q", result.Message, "missing binary")
	}
	if len(result.Details) != 2 {
		t.Errorf("Details count = %d, want 2", len(result.Details))
	}
}

func TestPackScriptCheckNotFound(t *testing.T) {
	c := &PackScriptCheck{
		CheckName: "topo:missing",
		Script:    "/nonexistent/script.sh",
		PackDir:   t.TempDir(),
		PackName:  "topo",
	}

	result := c.Run(&CheckContext{CityPath: t.TempDir()})

	if result.Status != StatusError {
		t.Errorf("Status = %d, want StatusError", result.Status)
	}
	if result.Message == "" {
		t.Error("Message should not be empty for missing script")
	}
}

func TestPackScriptCheckNotExecutable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "check.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &PackScriptCheck{
		CheckName: "topo:noexec",
		Script:    path,
		PackDir:   dir,
		PackName:  "topo",
	}

	result := c.Run(&CheckContext{CityPath: dir})

	if result.Status != StatusError {
		t.Errorf("Status = %d, want StatusError", result.Status)
	}
}

func TestPackScriptCheckEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	script := writeCheckScript(t, dir, "#!/bin/sh\nexit 0\n")

	c := &PackScriptCheck{
		CheckName: "topo:empty",
		Script:    script,
		PackDir:   dir,
		PackName:  "topo",
	}

	result := c.Run(&CheckContext{CityPath: dir})

	if result.Status != StatusOK {
		t.Errorf("Status = %d, want StatusOK", result.Status)
	}
	if result.Message != "check completed" {
		t.Errorf("Message = %q, want %q", result.Message, "check completed")
	}
}

func TestPackScriptCheckEnvVars(t *testing.T) {
	dir := t.TempDir()
	cityPath := t.TempDir()
	// Script echoes env vars to verify they're passed.
	script := writeCheckScript(t, dir,
		"#!/bin/sh\necho \"cityctx=$GC_CITY city=$GC_CITY_PATH runtime=$GC_CITY_RUNTIME_DIR topo=$GC_PACK_DIR state=$GC_PACK_STATE_DIR\"\nexit 0\n")

	c := &PackScriptCheck{
		CheckName: "topo:env",
		Script:    script,
		PackDir:   dir,
		PackName:  "topo",
	}

	result := c.Run(&CheckContext{CityPath: cityPath})

	if result.Status != StatusOK {
		t.Errorf("Status = %d, want StatusOK", result.Status)
	}
	expected := "cityctx=" + cityPath +
		" city=" + cityPath +
		" runtime=" + filepath.Join(cityPath, ".gc", "runtime") +
		" topo=" + dir +
		" state=" + filepath.Join(cityPath, ".gc", "runtime", "packs", "topo")
	if result.Message != expected {
		t.Errorf("Message = %q, want %q", result.Message, expected)
	}
}

func writeFixScript(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "fix.sh")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPackScriptCheckCanFixWithoutScript(t *testing.T) {
	c := &PackScriptCheck{
		CheckName: "topo:diag-only",
		Script:    "/irrelevant",
		PackDir:   t.TempDir(),
		PackName:  "topo",
	}
	if c.CanFix() {
		t.Error("CanFix() = true without FixScript, want false")
	}
	if err := c.Fix(&CheckContext{CityPath: t.TempDir()}); err != nil {
		t.Errorf("Fix() on diagnostic-only check returned error: %v", err)
	}
}

func TestPackScriptCheckCanFixWithScript(t *testing.T) {
	dir := t.TempDir()
	fix := writeFixScript(t, dir, "#!/bin/sh\nexit 0\n")
	c := &PackScriptCheck{
		CheckName: "topo:fixable",
		Script:    "/irrelevant",
		FixScript: fix,
		PackDir:   dir,
		PackName:  "topo",
	}
	if !c.CanFix() {
		t.Error("CanFix() = false with FixScript set, want true")
	}
}

func TestPackScriptCheckFixSuccess(t *testing.T) {
	dir := t.TempDir()
	city := t.TempDir()
	// Fix script: create a marker at $GC_CITY_PATH/.marker. Exit 0.
	fix := writeFixScript(t, dir,
		"#!/bin/sh\nset -e\necho hello\necho 'pack='$GC_PACK_DIR\n: > \"$GC_CITY_PATH/.marker\"\nexit 0\n")
	c := &PackScriptCheck{
		CheckName: "topo:fix-ok",
		Script:    "/irrelevant",
		FixScript: fix,
		PackDir:   dir,
		PackName:  "topo",
	}

	if err := c.Fix(&CheckContext{CityPath: city}); err != nil {
		t.Fatalf("Fix() returned unexpected error: %v", err)
	}
	// Marker file confirms (a) the fix executed, (b) GC_CITY_PATH env
	// var was delivered, and (c) the script had write access to the
	// city directory.
	marker := filepath.Join(city, ".marker")
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("marker not created by fix script: %v", err)
	}
}

func TestPackScriptCheckFixFailure(t *testing.T) {
	dir := t.TempDir()
	// Fix script prints details and exits non-zero.
	fix := writeFixScript(t, dir,
		"#!/bin/sh\necho 'remediation failed'\necho 'reason: disk full'\nexit 5\n")
	c := &PackScriptCheck{
		CheckName: "topo:fix-fail",
		Script:    "/irrelevant",
		FixScript: fix,
		PackDir:   dir,
		PackName:  "topo",
	}

	err := c.Fix(&CheckContext{CityPath: t.TempDir()})
	if err == nil {
		t.Fatal("Fix() returned nil, want error for non-zero exit")
	}
	msg := err.Error()
	if !strings.Contains(msg, "status 5") {
		t.Errorf("error missing exit code: %q", msg)
	}
	if !strings.Contains(msg, "remediation failed") {
		t.Errorf("error missing captured output: %q", msg)
	}
}

func TestDoctorRunPackScriptCheckReportsFixFailure(t *testing.T) {
	dir := t.TempDir()
	check := writeCheckScript(t, dir, "#!/bin/sh\necho 'marker missing'\nexit 2\n")
	fix := writeFixScript(t, dir, "#!/bin/sh\necho 'cannot create marker'\nexit 5\n")
	d := &Doctor{}
	d.Register(&PackScriptCheck{
		CheckName: "topo:fix-fail",
		Script:    check,
		FixScript: fix,
		PackDir:   dir,
		PackName:  "topo",
	})

	var buf bytes.Buffer
	report := d.Run(&CheckContext{CityPath: t.TempDir()}, &buf, true)

	if report.Fixed != 0 {
		t.Errorf("Fixed = %d, want 0", report.Fixed)
	}
	if report.Failed != 1 {
		t.Errorf("Failed = %d, want 1", report.Failed)
	}
	out := buf.String()
	if !strings.Contains(out, "fix failed: fix script exited with status 5: cannot create marker") {
		t.Errorf("output missing fix script failure: %q", out)
	}
}

func TestPackScriptCheckFixMissingScript(t *testing.T) {
	c := &PackScriptCheck{
		CheckName: "topo:fix-missing",
		Script:    "/irrelevant",
		FixScript: "/nonexistent/fix.sh",
		PackDir:   t.TempDir(),
		PackName:  "topo",
	}

	err := c.Fix(&CheckContext{CityPath: t.TempDir()})
	if err == nil {
		t.Fatal("Fix() returned nil for missing script, want error")
	}
}

func TestParseScriptOutput(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantMsg    string
		wantDetail int
	}{
		{"empty", "", "", 0},
		{"single line", "hello\n", "hello", 0},
		{"message and details", "msg\ndetail1\ndetail2\n", "msg", 2},
		{"blank lines skipped", "msg\n\n  \ndetail\n\n", "msg", 1},
		{"whitespace trimmed", "  msg  \n  detail  \n", "msg", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, details := parseScriptOutput(tt.input)
			if msg != tt.wantMsg {
				t.Errorf("message = %q, want %q", msg, tt.wantMsg)
			}
			if len(details) != tt.wantDetail {
				t.Errorf("details count = %d, want %d", len(details), tt.wantDetail)
			}
		})
	}
}
