package main

// Smoke tests for the maintenance pack shell scripts after they were
// converted to use `bd batch` (see beads#6). Shell scripts are awkward
// to unit-test, so this suite only:
//
//   1. reads each script from the embedded maintenance PackFS,
//   2. runs `bash -n` on it to confirm it still parses,
//   3. asserts presence (or documented absence) of `bd batch` usage,
//   4. asserts that the one script we actually converted
//      (cross-rig-deps.sh) no longer calls `bd dep remove` / `bd dep
//      add` in a per-iteration fashion outside a batch stream.
//
// We do NOT try to execute the scripts end-to-end — that requires a
// real `bd` binary with batch support, which is a beads#6 integration
// concern, not a gascity unit-test concern.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/examples/gastown/packs/maintenance"
)

// maintenanceScript is one of the three in-scope scripts for the
// batch-order-bead-ops refactor.
type maintenanceScript struct {
	// name is the basename used in error messages.
	name string
	// embedPath is the slash-separated path inside maintenance.PackFS.
	embedPath string
	// wantsBatch is true if this script should actually invoke
	// `bd batch` as a command (not just mention it in a comment).
	wantsBatch bool
	// forbiddenPatterns are regexes that must NOT match anywhere in the
	// script — used to catch un-refactored per-iteration bd calls.
	// Only populated for scripts that were genuinely converted.
	forbiddenPatterns []*regexp.Regexp
}

var inScopeScripts = []maintenanceScript{
	{
		name:       "gate-sweep.sh",
		embedPath:  "scripts/gate-sweep.sh",
		wantsBatch: false, // documented: uses `bd gate close`, not batchable
	},
	{
		name:       "spawn-storm-detect.sh",
		embedPath:  "scripts/spawn-storm-detect.sh",
		wantsBatch: false, // documented: loops use `bd show` + `gc mail`
	},
	{
		name:       "cross-rig-deps.sh",
		embedPath:  "scripts/cross-rig-deps.sh",
		wantsBatch: true,
		forbiddenPatterns: []*regexp.Regexp{
			// The original per-iteration shell calls must be gone.
			// These would be the regressions we're guarding against.
			regexp.MustCompile(`(?m)^\s*bd dep remove\b`),
			regexp.MustCompile(`(?m)^\s*bd dep add\b`),
		},
	},
}

// loadEmbeddedScript fetches a script body from the maintenance PackFS.
func loadEmbeddedScript(t *testing.T, embedPath string) []byte {
	t.Helper()
	data, err := maintenance.PackFS.ReadFile(embedPath)
	if err != nil {
		t.Fatalf("read embedded script %q: %v", embedPath, err)
	}
	if len(data) == 0 {
		t.Fatalf("embedded script %q is empty", embedPath)
	}
	return data
}

// writeTempScript drops body to a temp file and returns its path.
// Needed because `bash -n` wants a filesystem path, and the scripts
// are otherwise served from the embed FS.
func writeTempScript(t *testing.T, name string, body []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatalf("write temp script: %v", err)
	}
	return path
}

// TestMaintenanceScripts_BashSyntax runs `bash -n` against each
// in-scope script. Refactoring shell pipelines is easy to get wrong;
// this catches dangling heredocs, bad quoting, stray backticks, etc.
func TestMaintenanceScripts_BashSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}

	for _, s := range inScopeScripts {
		s := s
		t.Run(s.name, func(t *testing.T) {
			body := loadEmbeddedScript(t, s.embedPath)
			path := writeTempScript(t, s.name, body)

			cmd := exec.Command(bash, "-n", path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("bash -n %s failed: %v\n%s", s.name, err, out)
			}
		})
	}
}

// TestMaintenanceScripts_BatchUsage asserts that cross-rig-deps.sh
// actually invokes `bd batch` and that none of the forbidden
// per-iteration `bd` calls remain in it. For the two scripts that
// were intentionally NOT converted, it asserts a documenting comment
// mentioning `bd batch` is present so future readers understand why.
func TestMaintenanceScripts_BatchUsage(t *testing.T) {
	// Matches `bd batch` as an actual command invocation at the start
	// of a (possibly-indented) line or after a pipe / and-chain. This
	// is purposefully strict so that a mention of "bd batch" in a
	// comment does NOT satisfy it.
	cmdBatchRe := regexp.MustCompile(`(?m)(^|[|&;]\s*)\s*(if !?\s*)?bd batch\b`)
	// Matches the string `bd batch` anywhere — used for the weaker
	// "this script must at least document bd batch" check.
	mentionBatchRe := regexp.MustCompile(`bd batch`)

	for _, s := range inScopeScripts {
		s := s
		t.Run(s.name, func(t *testing.T) {
			body := loadEmbeddedScript(t, s.embedPath)
			text := string(body)

			if s.wantsBatch {
				if !cmdBatchRe.MatchString(text) {
					t.Errorf("%s: expected an actual `bd batch` command invocation, found none", s.name)
				}
			} else {
				// Not converted, but should document why.
				if !mentionBatchRe.MatchString(text) {
					t.Errorf("%s: not converted to bd batch and lacks a comment explaining why", s.name)
				}
				if strings.Contains(text, "#") == false {
					t.Errorf("%s: expected at least one comment line", s.name)
				}
			}

			for _, re := range s.forbiddenPatterns {
				if re.MatchString(text) {
					t.Errorf("%s: forbidden pattern %q still present — did the refactor regress?", s.name, re.String())
				}
			}
		})
	}
}

// TestMaintenanceScripts_CrossRigDepsBatchShape spot-checks the
// batch-stream content we emit from cross-rig-deps.sh. We can't run
// the script end-to-end, but we can assert it (a) passes a commit
// message via -m, (b) pipes or feeds via -f a file to bd batch, and
// (c) produces the `dep remove` / `dep add ... related` lines the
// beads#6 batch grammar expects.
func TestMaintenanceScripts_CrossRigDepsBatchShape(t *testing.T) {
	body := string(loadEmbeddedScript(t, "scripts/cross-rig-deps.sh"))

	wantSubstrings := []string{
		// commit message hint so dolt history records the order
		`-m "cross-rig-deps sweep"`,
		// batch input sourced from a file (we build a tempfile stream)
		`bd batch -f`,
		// jq emits the batch grammar lines
		`dep remove`,
		`dep add`,
		`related`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(body, want) {
			t.Errorf("cross-rig-deps.sh: expected substring %q not found", want)
		}
	}

	// Make sure we still propagate failures: `set -euo pipefail` at the
	// top and an explicit `exit 1` on batch failure.
	if !strings.Contains(body, "set -euo pipefail") {
		t.Error("cross-rig-deps.sh: lost `set -euo pipefail` during refactor")
	}
	if !strings.Contains(body, "exit 1") {
		t.Error("cross-rig-deps.sh: missing explicit exit 1 on batch failure")
	}
}
