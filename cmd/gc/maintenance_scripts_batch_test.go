package main

// Smoke tests for the maintenance pack shell scripts after they were
// converted to use `bd batch` (see beads#6). Shell scripts are awkward
// to unit-test, so this suite only:
//
//   1. discovers all .sh files in the embedded maintenance PackFS,
//   2. runs `bash -n` on each to confirm it still parses,
//   3. asserts presence (or documented absence) of `bd batch` usage,
//   4. asserts that converted scripts no longer call their old
//      per-iteration `bd` commands outside a batch stream.
//
// We do NOT try to execute the scripts end-to-end — that requires a
// real `bd` binary with batch support, which is a beads#6 integration
// concern, not a gascity unit-test concern.

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/examples/gastown/packs/maintenance"
)

// knownBatchScript describes a script that has been converted to use
// `bd batch`. Its forbiddenPatterns guard against regressions back to
// per-iteration `bd` calls.
type knownBatchScript struct {
	forbiddenPatterns []*regexp.Regexp
}

// batchScripts lists scripts that actually invoke `bd batch` as a
// command. Any script NOT in this map must mention `bd batch` in a
// comment documenting why it was not converted.
var batchScripts = map[string]knownBatchScript{
	"cross-rig-deps.sh": {
		forbiddenPatterns: []*regexp.Regexp{
			// The original per-iteration shell calls must be gone.
			regexp.MustCompile(`(?m)^\s*bd dep remove\b`),
			regexp.MustCompile(`(?m)^\s*bd dep add\b`),
		},
	},
}

// discoverScripts returns all .sh embed paths in the maintenance pack.
func discoverScripts(t *testing.T) []string {
	t.Helper()
	var scripts []string
	err := fs.WalkDir(maintenance.PackFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".sh") {
			scripts = append(scripts, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk maintenance.PackFS: %v", err)
	}
	if len(scripts) == 0 {
		t.Fatal("no .sh files found in maintenance.PackFS")
	}
	return scripts
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

// TestMaintenanceScripts_BashSyntax runs `bash -n` against every
// script in the maintenance pack. Refactoring shell pipelines is easy
// to get wrong; this catches dangling heredocs, bad quoting, stray
// backticks, etc.
func TestMaintenanceScripts_BashSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}

	for _, embedPath := range discoverScripts(t) {
		name := filepath.Base(embedPath)
		t.Run(name, func(t *testing.T) {
			body := loadEmbeddedScript(t, embedPath)
			path := writeTempScript(t, name, body)

			cmd := exec.Command(bash, "-n", path)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("bash -n %s failed: %v\n%s", name, err, out)
			}
		})
	}
}

// TestMaintenanceScripts_BatchUsage dynamically discovers all scripts
// and asserts that each one either invokes `bd batch` as a command or
// documents why it was not converted. This prevents new scripts from
// silently skipping the batch consideration.
func TestMaintenanceScripts_BatchUsage(t *testing.T) {
	// Matches `bd batch` as an actual command invocation at the start
	// of a (possibly-indented) line or after a pipe / and-chain.
	cmdBatchRe := regexp.MustCompile(`(?m)(^|[|&;]\s*)\s*(if !?\s*)?bd batch\b`)
	// Matches the string `bd batch` anywhere — used for the weaker
	// "this script must at least document bd batch" check.
	mentionBatchRe := regexp.MustCompile(`bd batch`)

	for _, embedPath := range discoverScripts(t) {
		name := filepath.Base(embedPath)
		t.Run(name, func(t *testing.T) {
			body := loadEmbeddedScript(t, embedPath)
			text := string(body)

			bs, isBatchScript := batchScripts[name]
			if isBatchScript {
				// This script should actually invoke `bd batch`.
				if !cmdBatchRe.MatchString(text) {
					t.Errorf("%s: expected an actual `bd batch` command invocation, found none", name)
				}
				for _, re := range bs.forbiddenPatterns {
					if re.MatchString(text) {
						t.Errorf("%s: forbidden pattern %q still present — did the refactor regress?", name, re.String())
					}
				}
			} else if !mentionBatchRe.MatchString(text) {
				// Not converted — must document why in a comment.
				t.Errorf("%s: not converted to bd batch and lacks a comment explaining why — "+
					"add a '# NOTE on `bd batch`' block to the script header", name)
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
