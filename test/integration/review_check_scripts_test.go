//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reviewCheckCase struct {
	name         string
	script       string
	verdictKey   string
	ralphStepID  string
	approvedText string
}

func reviewCheckCases() []reviewCheckCase {
	return []reviewCheckCase{
		{
			name:         "design review",
			script:       "design-review-approved.sh",
			verdictKey:   "design_review.verdict",
			ralphStepID:  "design-review-loop",
			approvedText: "Design review approved",
		},
		{
			name:         "code review",
			script:       "code-review-approved.sh",
			verdictKey:   "code_review.verdict",
			ralphStepID:  "code-review-loop",
			approvedText: "Code review approved",
		},
		{
			name:         "adopt pr review",
			script:       "adopt-pr-review-approved.sh",
			verdictKey:   "review.verdict",
			ralphStepID:  "review-loop",
			approvedText: "Review approved",
		},
	}
}

func TestReviewCheckScriptsDetectVerdictAcrossRalphStep(t *testing.T) {
	cityDir := setupReviewCheckScriptCity(t)

	for _, tc := range reviewCheckCases() {
		t.Run(tc.name, func(t *testing.T) {
			rootID := createJSONBead(t, cityDir, "workflow-root")
			verdictID := createJSONBead(t, cityDir, "apply")
			checkID := createJSONBead(t, cityDir, "check")

			updateBeadMetadata(t, cityDir, verdictID,
				"gc.root_bead_id="+rootID,
				"gc.ralph_step_id="+tc.ralphStepID,
				tc.verdictKey+"=done",
			)
			updateBeadMetadata(t, cityDir, checkID,
				"gc.root_bead_id="+rootID,
				"gc.ralph_step_id="+tc.ralphStepID,
			)

			scriptPath := filepath.Join(cityDir, ".gc", "scripts", "checks", tc.script)
			out, err := runCommand(cityDir, checkScriptEnv(t, cityDir, checkID), 30*time.Second, "bash", scriptPath)
			if err != nil {
				t.Fatalf("%s failed: %v\noutput: %s", tc.script, err, out)
			}
			if !strings.Contains(out, tc.approvedText) {
				t.Fatalf("%s output = %q, want %q", tc.script, out, tc.approvedText)
			}
		})
	}
}

func TestReviewCheckScriptsPreferNewestVerdictAcrossRalphStep(t *testing.T) {
	cityDir := setupReviewCheckScriptCity(t)

	for _, tc := range reviewCheckCases() {
		t.Run(tc.name, func(t *testing.T) {
			rootID := createJSONBead(t, cityDir, "workflow-root")
			oldVerdictID := createJSONBead(t, cityDir, "apply-old")
			updateBeadMetadata(t, cityDir, oldVerdictID,
				"gc.root_bead_id="+rootID,
				"gc.ralph_step_id="+tc.ralphStepID,
				tc.verdictKey+"=iterate",
			)

			time.Sleep(1100 * time.Millisecond)

			newVerdictID := createJSONBead(t, cityDir, "apply-new")
			updateBeadMetadata(t, cityDir, newVerdictID,
				"gc.root_bead_id="+rootID,
				"gc.ralph_step_id="+tc.ralphStepID,
				tc.verdictKey+"=done",
			)

			checkID := createJSONBead(t, cityDir, "check")
			updateBeadMetadata(t, cityDir, checkID,
				"gc.root_bead_id="+rootID,
				"gc.ralph_step_id="+tc.ralphStepID,
			)

			scriptPath := filepath.Join(cityDir, ".gc", "scripts", "checks", tc.script)
			out, err := runCommand(cityDir, checkScriptEnv(t, cityDir, checkID), 30*time.Second, "bash", scriptPath)
			if err != nil {
				t.Fatalf("%s failed: %v\noutput: %s", tc.script, err, out)
			}
			if !strings.Contains(out, tc.approvedText) {
				t.Fatalf("%s output = %q, want %q", tc.script, out, tc.approvedText)
			}
		})
	}
}

func setupReviewCheckScriptCity(t *testing.T) string {
	t.Helper()

	cityDir := filepath.Join(t.TempDir(), "review-check-script-test")
	configPath := filepath.Join(t.TempDir(), "review-check-script.toml")
	cityToml := "[workspace]\nname = \"review-check-script-test\"\n\n[session]\nprovider = \"subprocess\"\n"
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	out, err := gcDolt("", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init failed: %v\noutput: %s", err, out)
	}

	checksDir := filepath.Join(cityDir, ".gc", "scripts", "checks")
	if err := os.MkdirAll(checksDir, 0o755); err != nil {
		t.Fatalf("mkdir checks: %v", err)
	}
	packChecks := filepath.Join(repoRoot(t), "examples", "gastown", "packs", "gastown", "scripts", "checks")
	checkEntries, err := os.ReadDir(packChecks)
	if err != nil {
		t.Fatalf("reading pack checks: %v", err)
	}
	for _, e := range checkEntries {
		src := filepath.Join(packChecks, e.Name())
		dst := filepath.Join(checksDir, e.Name())
		data, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("reading %s: %v", src, err)
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			t.Fatalf("writing %s: %v", dst, err)
		}
	}

	out, err = gcDolt("", "start", cityDir)
	if err != nil {
		t.Fatalf("gc start failed: %v\noutput: %s", err, out)
	}
	t.Cleanup(func() {
		gcDolt("", "stop", cityDir) //nolint:errcheck
	})

	return cityDir
}

func createJSONBead(t *testing.T, cityDir, title string) string {
	t.Helper()

	out, err := bdDolt(cityDir, "create", "--json", title)
	if err != nil {
		t.Fatalf("bd create failed: %v\noutput: %s", err, out)
	}
	var created graphBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &created); err != nil {
		t.Fatalf("unmarshal create bead: %v\njson: %s", err, out)
	}
	return created.ID
}

func updateBeadMetadata(t *testing.T, cityDir, beadID string, pairs ...string) {
	t.Helper()

	args := []string{"update", beadID}
	for _, pair := range pairs {
		args = append(args, "--set-metadata", pair)
	}
	out, err := bdDolt(cityDir, args...)
	if err != nil {
		t.Fatalf("bd update %s failed: %v\noutput: %s", beadID, err, out)
	}
}

func checkScriptEnv(t *testing.T, cityDir, beadID string) []string {
	t.Helper()

	env := integrationEnvDolt()
	env = filterEnvMany(env,
		"GC_BEAD_ID",
		"GC_CITY",
		"GC_CITY_ROOT",
		"GC_CITY_PATH",
		"GC_CITY_RUNTIME_DIR",
		"GC_DOLT_PORT",
	)
	env = append(env,
		"GC_BEAD_ID="+beadID,
		"GC_CITY="+cityDir,
		"GC_CITY_ROOT="+cityDir,
		"GC_CITY_PATH="+cityDir,
		"GC_CITY_RUNTIME_DIR="+filepath.Join(cityDir, ".gc", "runtime"),
	)
	if data, err := os.ReadFile(filepath.Join(cityDir, ".beads", "dolt-server.port")); err == nil {
		if port := strings.TrimSpace(string(data)); port != "" {
			env = append(env, "GC_DOLT_PORT="+port)
		}
	}
	return env
}
