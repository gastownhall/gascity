package scripts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComplexityReportUpdateAndDiffUseStableJSONKeys(t *testing.T) {
	repoRoot := repoRoot(t)
	binDir := t.TempDir()
	fake := filepath.Join(binDir, "gocyclo")
	argsLog := filepath.Join(t.TempDir(), "args")
	writeExecutable(t, fake, `#!/bin/sh
printf '%s\n' "$*" > "$COMPLEXITY_ARGS_LOG"
if [ "$COMPLEXITY_SCAN_KIND" = base ] || [ "$COMPLEXITY_MODE" != diff ]; then
  printf '%s\n' '23 gc (*Server).Run internal/server.go:10:1' '7 gc helper internal/server.go:30:1' '31 config Load internal/config/load.go:4:1'
else
  printf '%s\n' '26 gc (*Server).Run internal/server.go:99:1' '7 gc helper internal/server.go:30:1' '31 config Load internal/config/load.go:4:1'
fi
printf '%s\n' '99 gc ignored internal/server_test.go:1:1' '99 gc generated internal/genclient/client.go:1:1'
`)
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	if output, err := runComplexity(t, repoRoot, fake, baseline, argsLog, "update"); err != nil {
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
		t.Fatalf("items = %#v, want threshold offenders sorted by complexity", got.Items)
	}
	if got.Items[0].File != "internal/config/load.go" || got.Items[1].Function != "(*Server).Run" {
		t.Fatalf("unstable keys = %#v", got.Items)
	}
	args, err := os.ReadFile(argsLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "./cmd/gc ./internal ./pkg") || !strings.Contains(string(args), "_test\\.go$") {
		t.Fatalf("gocyclo invocation did not use explicit production scope: %s", args)
	}

	// A changed score is reported by diff and rejected by check.
	writeExecutable(t, fake, `#!/bin/sh
if [ "$COMPLEXITY_SCAN_KIND" = base ]; then
  printf '%s\n' '23 gc (*Server).Run internal/server.go:10:1' '7 gc helper internal/server.go:30:1' '31 config Load internal/config/load.go:4:1'
else
  printf '%s\n' '26 gc (*Server).Run internal/server.go:99:1' '7 gc helper internal/server.go:30:1' '31 config Load internal/config/load.go:4:1'
fi
`)
	if output, err := runComplexity(t, repoRoot, fake, baseline, argsLog, "diff"); err != nil || !strings.Contains(string(output), "regressed") {
		t.Fatalf("diff = %v, output %s", err, output)
	}
	if output, err := runComplexity(t, repoRoot, fake, baseline, argsLog, "check"); err == nil || !strings.Contains(string(output), "regressed") {
		t.Fatalf("check = %v, output %s", err, output)
	}
}

func TestComplexityReportRejectsInvalidMode(t *testing.T) {
	root := repoRoot(t)
	if output, err := runComplexity(t, root, "/does/not/exist", filepath.Join(t.TempDir(), "baseline.json"), filepath.Join(t.TempDir(), "args"), "wat"); err == nil || !strings.Contains(string(output), "usage:") {
		t.Fatalf("invalid mode = %v, output %s", err, output)
	}
}

func TestComplexityDiffRejectsMissingBaseRef(t *testing.T) {
	root := repoRoot(t)
	fake := filepath.Join(t.TempDir(), "gocyclo")
	writeExecutable(t, fake, "#!/bin/sh\nprintf '%s\\n' '1 gc helper internal/server.go:1:1'\n")
	output, err := runComplexityEnv(t, root, "diff", "COMPLEXITY_TOOL="+fake, "COMPLEXITY_BASE_REF=refs/heads/does-not-exist")
	if err == nil || !strings.Contains(string(output), "unable to archive base ref") {
		t.Fatalf("missing base ref = %v, output %s", err, output)
	}
}

func TestComplexityDiffIgnoresBaselineContents(t *testing.T) {
	root := repoRoot(t)
	fake := filepath.Join(t.TempDir(), "gocyclo")
	writeExecutable(t, fake, "#!/bin/sh\nprintf '%s\\n' '1 gc helper internal/server.go:1:1'\n")
	baseline := filepath.Join(t.TempDir(), "malformed.json")
	if err := os.WriteFile(baseline, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := runComplexityEnv(t, root, "diff", "COMPLEXITY_TOOL="+fake, "COMPLEXITY_BASELINE="+baseline, "COMPLEXITY_BASE_REF=HEAD")
	if err != nil || !strings.Contains(string(output), "no threshold changes") {
		t.Fatalf("diff with malformed baseline = %v, output %s", err, output)
	}
}

func TestComplexityDiffDuplicateKeysRespectThreshold(t *testing.T) {
	root := repoRoot(t)
	for _, tt := range []struct {
		name    string
		output  string
		wantErr bool
	}{
		{
			name:   "low complexity duplicates are ignored",
			output: "1 events init internal/events/payloads.go:1:1\n1 events init internal/events/payloads.go:2:1\n",
		},
		{
			name:   "low duplicates stay ignored when a later row is high",
			output: "1 events init internal/events/payloads.go:1:1\n1 events init internal/events/payloads.go:2:1\n20 events Other internal/events/other.go:3:1\n",
		},
		{
			name:    "tracked duplicates fail clearly",
			output:  "20 events init internal/events/payloads.go:1:1\n20 events init internal/events/payloads.go:2:1\n",
			wantErr: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fake := filepath.Join(t.TempDir(), "gocyclo")
			writeExecutable(t, fake, "#!/bin/sh\nprintf '%b' '"+strings.ReplaceAll(tt.output, "\n", "\\n")+"'\n")
			output, err := runComplexityEnv(t, root, "diff", "COMPLEXITY_TOOL="+fake, "COMPLEXITY_BASE_REF=HEAD")
			if tt.wantErr {
				if err == nil || !strings.Contains(string(output), "duplicate analyzer key") {
					t.Fatalf("duplicate tracked keys = %v, output %s", err, output)
				}
				return
			}
			if err != nil || !strings.Contains(string(output), "no threshold changes") {
				t.Fatalf("low duplicate keys = %v, output %s", err, output)
			}
		})
	}
}

func TestComplexityDiffReportsHighToLowAsImproved(t *testing.T) {
	root := repoRoot(t)
	fake := filepath.Join(t.TempDir(), "gocyclo")
	writeExecutable(t, fake, `#!/bin/sh
if [ "$COMPLEXITY_SCAN_KIND" = base ]; then
  printf '%s\n' '25 events Init internal/events/payloads.go:1:1'
else
  printf '%s\n' '5 events Init internal/events/payloads.go:88:1'
fi
`)
	output, err := runComplexityEnv(t, root, "diff", "COMPLEXITY_TOOL="+fake, "COMPLEXITY_BASE_REF=HEAD")
	if err != nil || !strings.Contains(string(output), "improved: 5 events Init internal/events/payloads.go (base 25)") {
		t.Fatalf("high-to-low diff = %v, output %s", err, output)
	}
}

func runComplexity(t *testing.T, repoRoot, tool, baseline, argsLog, mode string) ([]byte, error) {
	t.Helper()
	return runComplexityEnv(t, repoRoot, mode,
		"COMPLEXITY_TOOL="+tool,
		"COMPLEXITY_BASELINE="+baseline,
		"COMPLEXITY_ARGS_LOG="+argsLog,
		"COMPLEXITY_THRESHOLD=20",
		"COMPLEXITY_TOP=50",
		"COMPLEXITY_BASE_REF=HEAD")
}

func runComplexityEnv(t *testing.T, repoRoot, mode string, env ...string) ([]byte, error) {
	t.Helper()
	cmd := testCommand(filepath.Join(repoRoot, "scripts", "ci", "complexity.sh"), mode)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), env...)
	return cmd.CombinedOutput()
}
