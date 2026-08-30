package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestComplexityReportUpdateAndDiffUseStableJSONKeys(t *testing.T) {
	repoRoot := repoRoot(t)
	binDir := t.TempDir()
	fake := filepath.Join(binDir, "gocyclo")
	writeExecutable(t, fake, `#!/bin/sh
printf '%s\n' '23 gc (*Server).Run internal/server.go:10:1' '7 gc helper internal/server.go:30:1' '31 config Load internal/config/load.go:4:1'
`)
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	if output, err := runComplexity(t, repoRoot, fake, baseline, "update"); err != nil {
		t.Fatalf("update failed: %v\n%s", err, output)
	}
	raw, err := os.ReadFile(baseline)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Schema string `json:"schema"`
		Tool   string `json:"tool"`
		Items  []struct {
			Package  string `json:"package"`
			Function string `json:"function"`
			File     string `json:"file"`
			CCN      int    `json:"ccn"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("baseline is not JSON: %v", err)
	}
	if got.Schema != "gascity.complexity/v1" || got.Tool != "gocyclo@v0.6.0" {
		t.Fatalf("metadata = %#v", got)
	}
	if len(got.Items) != 2 || got.Items[0].CCN != 31 || got.Items[1].CCN != 23 {
		t.Fatalf("items = %#v, want sorted threshold offenders", got.Items)
	}
	if got.Items[0].File != "internal/config/load.go" || got.Items[1].Function != "(*Server).Run" {
		t.Fatalf("unstable keys = %#v", got.Items)
	}

	// A changed score is reported by diff and rejected by check.
	writeExecutable(t, fake, `#!/bin/sh
printf '%s\n' '26 gc (*Server).Run internal/server.go:99:1' '7 gc helper internal/server.go:30:1' '31 config Load internal/config/load.go:4:1'
`)
	if output, err := runComplexity(t, repoRoot, fake, baseline, "diff"); err != nil || !strings.Contains(string(output), "regressed") {
		t.Fatalf("diff = %v, output %s", err, output)
	}
	if output, err := runComplexity(t, repoRoot, fake, baseline, "check"); err == nil || !strings.Contains(string(output), "regressed") {
		t.Fatalf("check = %v, output %s", err, output)
	}
}

func TestComplexityReportRejectsInvalidMode(t *testing.T) {
	root := repoRoot(t)
	if output, err := runComplexity(t, root, "/does/not/exist", filepath.Join(t.TempDir(), "baseline.json"), "wat"); err == nil || !strings.Contains(string(output), "usage:") {
		t.Fatalf("invalid mode = %v, output %s", err, output)
	}
}

func runComplexity(t *testing.T, repoRoot, tool, baseline, mode string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(filepath.Join(repoRoot, "scripts", "ci", "complexity.sh"), mode)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"COMPLEXITY_TOOL="+tool,
		"COMPLEXITY_BASELINE="+baseline,
		"COMPLEXITY_THRESHOLD=20",
		"COMPLEXITY_TOP=50",
	)
	return cmd.CombinedOutput()
}
