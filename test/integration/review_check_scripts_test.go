//go:build integration

package integration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reviewCheckCase struct {
	name         string
	script       string
	formula      string
	applyStepID  string
	verdictKey   string
	ralphStepID  string
	approvedText string
}

func reviewCheckCases() []reviewCheckCase {
	return []reviewCheckCase{
		{
			name:         "design review",
			script:       "design-review-approved.sh",
			formula:      "mol-personal-work-v2",
			applyStepID:  "apply-design-changes",
			verdictKey:   "design_review.verdict",
			ralphStepID:  "design-review-loop",
			approvedText: "Design review approved",
		},
		{
			name:         "code review",
			script:       "code-review-approved.sh",
			formula:      "mol-personal-work-v2",
			applyStepID:  "apply-code-fixes",
			verdictKey:   "code_review.verdict",
			ralphStepID:  "code-review-loop",
			approvedText: "Code review approved",
		},
		{
			name:         "adopt pr review",
			script:       "adopt-pr-review-approved.sh",
			formula:      "mol-adopt-pr-v2",
			applyStepID:  "apply-fixes",
			verdictKey:   "review.verdict",
			ralphStepID:  "review-loop",
			approvedText: "Review approved",
		},
	}
}

func (c reviewCheckCase) attemptStepRef(attempt int) string {
	return fmt.Sprintf("%s.%s.run.%d.%s", c.formula, c.ralphStepID, attempt, c.applyStepID)
}

func (c reviewCheckCase) checkStepRef(attempt int) string {
	return fmt.Sprintf("%s.%s.check.%d", c.formula, c.ralphStepID, attempt)
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
				"gc.attempt=1",
				"gc.ralph_step_id="+tc.ralphStepID,
				"gc.step_ref="+tc.attemptStepRef(1),
				tc.verdictKey+"=done",
			)
			updateBeadMetadata(t, cityDir, checkID,
				"gc.root_bead_id="+rootID,
				"gc.attempt=1",
				"gc.ralph_step_id="+tc.ralphStepID,
				"gc.step_ref="+tc.checkStepRef(1),
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
				"gc.attempt=1",
				"gc.ralph_step_id="+tc.ralphStepID,
				"gc.step_ref="+tc.attemptStepRef(1),
				tc.verdictKey+"=iterate",
			)

			time.Sleep(1100 * time.Millisecond)

			newVerdictID := createJSONBead(t, cityDir, "apply-new")
			updateBeadMetadata(t, cityDir, newVerdictID,
				"gc.root_bead_id="+rootID,
				"gc.attempt=1",
				"gc.ralph_step_id="+tc.ralphStepID,
				"gc.step_ref="+tc.attemptStepRef(1),
				tc.verdictKey+"=done",
			)

			checkID := createJSONBead(t, cityDir, "check")
			updateBeadMetadata(t, cityDir, checkID,
				"gc.root_bead_id="+rootID,
				"gc.attempt=1",
				"gc.ralph_step_id="+tc.ralphStepID,
				"gc.step_ref="+tc.checkStepRef(1),
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

func TestReviewCheckScriptsPreferNewestVerdictWhenListOrderIsStale(t *testing.T) {
	for _, tc := range reviewCheckCases() {
		t.Run(tc.name, func(t *testing.T) {
			fakeDir := t.TempDir()
			writeFakeBDCommand(t, filepath.Join(fakeDir, "bd"), tc)

			env := newIsolatedToolEnv(t, false)
			envMap := parseEnvList(env)
			env = replaceEnv(env, "PATH", prependPath(fakeDir, envMap["PATH"]))
			env = filterEnvMany(env,
				"GC_BEAD_ID",
				"GC_CITY",
				"GC_CITY_PATH",
				"GC_CITY_ROOT",
				"GC_CITY_RUNTIME_DIR",
			)
			env = append(env, "GC_BEAD_ID=check-1")

			scriptPath := filepath.Join(repoRoot(t), "examples", "gastown", "packs", "gastown", "assets", "scripts", "checks", tc.script)
			out, err := runCommand(repoRoot(t), env, 30*time.Second, "bash", scriptPath)
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
	env := newIsolatedCommandEnv(t, true)

	cityDir := filepath.Join(t.TempDir(), "review-check-script-test")
	configPath := filepath.Join(t.TempDir(), "review-check-script.toml")
	cityToml := "[workspace]\nname = \"review-check-script-test\"\n\n[session]\nprovider = \"subprocess\"\n"
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing config: %v", err)
	}

	checksDir := filepath.Join(cityDir, ".gc", "scripts", "checks")
	if err := os.MkdirAll(checksDir, 0o755); err != nil {
		t.Fatalf("mkdir checks: %v", err)
	}
	packChecks := filepath.Join(repoRoot(t), "examples", "gastown", "packs", "gastown", "assets", "scripts", "checks")
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
	initCityWithManagedDoltRecovery(t, env, configPath, cityDir)
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)                //nolint:errcheck
		runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck
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

	env := commandEnvForDir(cityDir, true)
	env = filterEnvMany(env,
		"GC_BEAD_ID",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_CITY_ROOT",
		"GC_CITY_RUNTIME_DIR",
		"GC_DOLT_PORT",
	)
	env = append(env,
		"GC_BEAD_ID="+beadID,
		"GC_CITY="+cityDir,
		"GC_CITY_PATH="+cityDir,
		"GC_CITY_RUNTIME_DIR="+filepath.Join(cityDir, ".gc", "runtime"),
	)
	if port, ok := ensureManagedDoltPortForTest(cityDir); ok {
		env = append(env, "GC_DOLT_PORT="+port)
	}
	return env
}

func writeFakeBDCommand(t *testing.T, path string, tc reviewCheckCase) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
set -eu

cmd="$1"
shift || true

case "$cmd" in
  show)
    printf '%%s\n' '{"metadata":{"gc.attempt":"1","gc.root_bead_id":"root-1"}}'
    ;;
  list)
    cat <<'EOF'
[
  {
    "id": "old-verdict",
    "created_at": "2026-01-01T00:00:00Z",
    "metadata": {
      "gc.step_ref": %q,
      "gc.root_bead_id": "root-1",
      %q: "iterate"
    }
  },
  {
    "id": "new-verdict",
    "created_at": "2026-01-01T00:00:01Z",
    "metadata": {
      "gc.step_ref": %q,
      "gc.root_bead_id": "root-1",
      %q: "done"
    }
  }
]
EOF
    ;;
  *)
    echo "unexpected bd command: $cmd" >&2
    exit 1
    ;;
esac
`, tc.attemptStepRef(1), tc.verdictKey, tc.attemptStepRef(1), tc.verdictKey)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd command: %v", err)
	}
}

// TestReviewCheckScriptsStripBeadsRoleWarningFromStdout guards against the
// `bd list`/`bd show` output being polluted by a stdout warning (e.g.,
// "warning: beads.role not configured (GH#2950)") that breaks jq parsing
// in the check scripts. The original flake surfaced as
// TestReviewCheckScriptsPreferNewestVerdictAcrossRalphStep failing with
// exit status 1 and empty output: `bd` printed its role-not-configured
// diagnostic ahead of the JSON, and the scripts piped the combined
// stdout straight into jq. The fix adds a json_payload awk filter that
// strips lines until the first `{` or `[`; this test drives a fake bd
// that emits the warning to confirm the filter is in place.
func TestReviewCheckScriptsStripBeadsRoleWarningFromStdout(t *testing.T) {
	for _, tc := range reviewCheckCases() {
		t.Run(tc.name, func(t *testing.T) {
			fakeDir := t.TempDir()
			writeFakeBDCommandWithWarning(t, filepath.Join(fakeDir, "bd"), tc)

			env := newIsolatedToolEnv(t, false)
			envMap := parseEnvList(env)
			env = replaceEnv(env, "PATH", prependPath(fakeDir, envMap["PATH"]))
			env = filterEnvMany(env,
				"GC_BEAD_ID",
				"GC_CITY",
				"GC_CITY_PATH",
				"GC_CITY_ROOT",
				"GC_CITY_RUNTIME_DIR",
			)
			env = append(env, "GC_BEAD_ID=check-1")

			scriptPath := filepath.Join(repoRoot(t), "examples", "gastown", "packs", "gastown", "assets", "scripts", "checks", tc.script)
			out, err := runCommand(repoRoot(t), env, 30*time.Second, "bash", scriptPath)
			if err != nil {
				t.Fatalf("%s failed with bd warning-prefixed output: %v\noutput: %s", tc.script, err, out)
			}
			if !strings.Contains(out, tc.approvedText) {
				t.Fatalf("%s output = %q, want %q", tc.script, out, tc.approvedText)
			}
		})
	}
}

// writeFakeBDCommandWithWarning is writeFakeBDCommand with a stdout
// "warning:" prefix prepended to both `show` and `list` output, mirroring
// real `bd`'s GH#2950 diagnostic so the check scripts' json_payload
// filter is actually exercised.
func writeFakeBDCommandWithWarning(t *testing.T, path string, tc reviewCheckCase) {
	t.Helper()

	script := fmt.Sprintf(`#!/bin/sh
set -eu

emit_warning() {
  printf 'warning: beads.role not configured (GH#2950).\n'
  printf '  Fix: git config beads.role maintainer\n'
  printf '  Or:  git config beads.role contributor\n'
}

cmd="$1"
shift || true

case "$cmd" in
  show)
    emit_warning
    printf '%%s\n' '{"metadata":{"gc.attempt":"1","gc.root_bead_id":"root-1"}}'
    ;;
  list)
    emit_warning
    cat <<'EOF'
[
  {
    "id": "old-verdict",
    "created_at": "2026-01-01T00:00:00Z",
    "metadata": {
      "gc.step_ref": %q,
      "gc.root_bead_id": "root-1",
      %q: "iterate"
    }
  },
  {
    "id": "new-verdict",
    "created_at": "2026-01-01T00:00:01Z",
    "metadata": {
      "gc.step_ref": %q,
      "gc.root_bead_id": "root-1",
      %q: "done"
    }
  }
]
EOF
    ;;
  *)
    echo "unexpected bd command: $cmd" >&2
    exit 1
    ;;
esac
`, tc.attemptStepRef(1), tc.verdictKey, tc.attemptStepRef(1), tc.verdictKey)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd command: %v", err)
	}
}
