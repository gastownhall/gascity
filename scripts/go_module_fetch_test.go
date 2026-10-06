package scripts_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Go module fetch resilience.
//
// The go command never retries a module fetch, and proxy.golang.org
// occasionally resets an HTTP/2 stream ("stream error: stream ID N;
// INTERNAL_ERROR; received from peer"). When that happens inside `go test`
// the package fails as "[setup failed]" and the job has to be re-run. The
// .github/actions/go-mod-download composite restores a go.sum-keyed module
// download cache and runs a retried `go mod download` so build and test steps
// find every module locally. These tests pin that every Go-building job runs
// it and that the action and its retry script keep their contract.

const (
	goModDownloadActionRef  = "./.github/actions/go-mod-download"
	goModDownloadActionPath = ".github/actions/go-mod-download/action.yml"
	goModDownloadScriptPath = ".github/scripts/go-mod-download-retry.sh"
	defaultGoProxy          = "https://proxy.golang.org,direct"
	resilientGoProxy        = "https://proxy.golang.org|https://proxy.golang.org|direct"
)

// goModDownloadExemptWorkflows lists workflows whose actions/setup-go jobs do
// not build this module through the go command's module fetcher, with the
// reason. Anything not listed must run the go-mod-download action.
var goModDownloadExemptWorkflows = map[string]string{
	// Bazel fetches modules through gazelle's go_deps, not the go command
	// in the job; setup-bazel already gives fetch_repo a "|" GOPROXY list.
	"bazel.yml":      "Bazel fetches modules via go_deps",
	"bazel-test.yml": "Bazel fetches modules via go_deps",
	// Installs gocyclo with `go install pkg@version`; never builds this module.
	"complexity.yml": "go install of a pinned tool only",
	// Publishing jobs are not migrated: they keep actions/setup-go's own
	// cache (its default), which restores GOMODCACHE from setup-go-* entries,
	// so they never read the go-mod-download cache this action writes.
	"release.yml":         "publishing job keeps setup-go's own cache; not migrated",
	"rc-release.yml":      "publishing job keeps setup-go's own cache; not migrated",
	"gc-edge-publish.yml": "publishing job keeps setup-go's own cache; not migrated",
}

type goFetchWorkflow struct {
	Jobs map[string]struct {
		Steps []goFetchStep `yaml:"steps"`
	} `yaml:"jobs"`
}

type goFetchAction struct {
	Runs struct {
		Using string        `yaml:"using"`
		Steps []goFetchStep `yaml:"steps"`
	} `yaml:"runs"`
}

type goFetchStep struct {
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

func isSetupGo(step goFetchStep) bool {
	return strings.HasPrefix(step.Uses, "actions/setup-go@")
}

// TestGoBuildingJobsRunGoModDownloadAfterSetupGo requires the step right
// after every actions/setup-go in a workflow job or a shared setup action to
// be the go-mod-download action, so no build or test step reaches the module
// proxy first.
func TestGoBuildingJobsRunGoModDownloadAfterSetupGo(t *testing.T) {
	root := repoRoot(t)

	workflows, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(workflows) == 0 {
		t.Fatal("no workflows found under .github/workflows")
	}
	sort.Strings(workflows)

	checked := 0
	for _, path := range workflows {
		name := filepath.Base(path)
		var wf goFetchWorkflow
		if err := yaml.Unmarshal([]byte(readFile(t, root, filepath.Join(".github", "workflows", name))), &wf); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		_, exempt := goModDownloadExemptWorkflows[name]
		usesSetupGo := false
		jobNames := make([]string, 0, len(wf.Jobs))
		for jobName := range wf.Jobs {
			jobNames = append(jobNames, jobName)
		}
		sort.Strings(jobNames)
		for _, jobName := range jobNames {
			steps := wf.Jobs[jobName].Steps
			for i, step := range steps {
				if !isSetupGo(step) {
					continue
				}
				usesSetupGo = true
				if exempt {
					continue
				}
				checked++
				where := name + " job " + strconv.Quote(jobName)
				assertGoModDownloadFollowsSetupGo(t, where, steps, i)
			}
		}
		if exempt && !usesSetupGo {
			t.Errorf("%s is exempt from go-mod-download but no longer calls actions/setup-go; drop the stale exemption", name)
		}
	}
	if checked == 0 {
		t.Fatal("found no actions/setup-go steps to check; the workflow scan is broken")
	}

	for _, action := range []string{
		".github/actions/setup-gascity-ubuntu/action.yml",
		".github/actions/setup-gascity-macos/action.yml",
	} {
		steps := loadGoFetchAction(t, root, action).Runs.Steps
		found := false
		for i, step := range steps {
			if !isSetupGo(step) {
				continue
			}
			found = true
			assertGoModDownloadFollowsSetupGo(t, action, steps, i)
		}
		if !found {
			t.Errorf("%s no longer calls actions/setup-go; update this test", action)
		}
	}
}

// TestGoModDownloadActionCachesOnlyCompleteTrustedModuleSets pins the cache
// contract: a go.sum-keyed restore of the module download directory, the
// retried download, and a save that runs only on a cache miss from trusted
// default-branch events, with restore and save naming the same path and key.
func TestGoModDownloadActionCachesOnlyCompleteTrustedModuleSets(t *testing.T) {
	root := repoRoot(t)
	action := loadGoFetchAction(t, root, goModDownloadActionPath)
	if action.Runs.Using != "composite" {
		t.Fatalf("go-mod-download must be a composite action, got %q", action.Runs.Using)
	}
	steps := action.Runs.Steps

	restore, restoreIndex := findGoFetchStep(steps, "actions/cache/restore@")
	save, saveIndex := findGoFetchStep(steps, "actions/cache/save@")
	downloadIndex := -1
	for i, step := range steps {
		if strings.Contains(step.Run, "go-mod-download-retry.sh") {
			downloadIndex = i
		}
	}
	if restoreIndex < 0 || saveIndex < 0 || downloadIndex < 0 {
		t.Fatalf("go-mod-download must restore the cache, run %s, and save the cache (restore=%d download=%d save=%d)",
			goModDownloadScriptPath, restoreIndex, downloadIndex, saveIndex)
	}
	if restoreIndex >= downloadIndex || downloadIndex >= saveIndex {
		t.Fatalf("go-mod-download must restore, then download, then save; a save before the download completes would cache a partial module set")
	}

	wantKey := "go-mod-download-v1-${{ runner.os }}-${{ hashFiles('go.sum') }}"
	if restore.With["key"] != wantKey || save.With["key"] != wantKey {
		t.Errorf("restore and save keys must both be %q, got restore=%q save=%q", wantKey, restore.With["key"], save.With["key"])
	}
	if restore.With["path"] == "" || restore.With["path"] != save.With["path"] {
		t.Errorf("restore and save must name the same path, got restore=%q save=%q", restore.With["path"], save.With["path"])
	}
	if !strings.Contains(restore.With["path"], "steps.modcache.outputs.dir") {
		t.Errorf("cache path must come from the modcache step (GOMODCACHE/cache/download), got %q", restore.With["path"])
	}
	modcache, _ := findGoFetchStepByID(steps, "modcache")
	if !strings.Contains(modcache.Run, "$(go env GOMODCACHE)/cache/download") {
		t.Errorf("modcache step must resolve $(go env GOMODCACHE)/cache/download; extracted module directories are read-only and break a second restore, got %q", modcache.Run)
	}

	for _, want := range []string{
		"steps.restore.outputs.cache-hit != 'true'",
		"github.event_name == 'push'",
		"github.event_name == 'schedule'",
	} {
		if !strings.Contains(save.If, want) {
			t.Errorf("save step condition must include %q so only a cache miss on a trusted default-branch run writes; got %q", want, save.If)
		}
	}
	if strings.Contains(save.If, "pull_request") || strings.Contains(save.If, "workflow_dispatch") {
		t.Errorf("save step must never run for pull_request or dispatched runs, which may check out untrusted code; got %q", save.If)
	}
}

// TestGoModDownloadRetryScriptRetriesTransientFailures drives the retry
// script against a fake go command.
func TestGoModDownloadRetryScriptRetriesTransientFailures(t *testing.T) {
	root := repoRoot(t)
	script := filepath.Join(root, goModDownloadScriptPath)

	tests := []struct {
		name          string
		failures      int
		attempts      string
		goproxy       string
		wantExit      int
		wantCalls     int
		wantGoProxy   string
		wantInMessage string
	}{
		{
			name:        "first attempt succeeds",
			failures:    0,
			attempts:    "4",
			goproxy:     defaultGoProxy,
			wantCalls:   1,
			wantGoProxy: resilientGoProxy,
		},
		{
			name:          "transient stream errors then success",
			failures:      2,
			attempts:      "4",
			goproxy:       defaultGoProxy,
			wantCalls:     3,
			wantGoProxy:   resilientGoProxy,
			wantInMessage: "attempt 2 of 4 failed",
		},
		{
			name:          "persistent failure exhausts attempts",
			failures:      99,
			attempts:      "3",
			goproxy:       defaultGoProxy,
			wantExit:      1,
			wantCalls:     3,
			wantGoProxy:   resilientGoProxy,
			wantInMessage: "go mod download failed after 3 attempts",
		},
		{
			name:        "explicit GOPROXY is kept",
			failures:    1,
			attempts:    "4",
			goproxy:     "https://mirror.example.com",
			wantCalls:   2,
			wantGoProxy: "https://mirror.example.com",
		},
		{
			name:          "invalid attempt count",
			attempts:      "zero",
			goproxy:       defaultGoProxy,
			wantExit:      2,
			wantCalls:     0,
			wantInMessage: "GO_MOD_DOWNLOAD_ATTEMPTS must be a positive integer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err := os.MkdirAll(bin, 0o755); err != nil {
				t.Fatal(err)
			}
			calls := filepath.Join(dir, "calls")
			writeExecutable(t, filepath.Join(bin, "go"), `#!/usr/bin/env bash
set -euo pipefail
case "$1 ${2:-}" in
  "env GOPROXY") printf '%s\n' "$FAKE_GOPROXY" ;;
  "env GOSUMDB") echo sum.golang.org ;;
  "env GOFLAGS") echo ;;
  "mod download")
    printf '%s\n' "$GOPROXY" >> "$FAKE_CALLS"
    n=$(wc -l < "$FAKE_CALLS")
    if [ "$n" -le "$FAKE_FAILURES" ]; then
      echo 'go: golang.org/x/net@v0.58.0: read "https://proxy.golang.org/golang.org/x/net/@v/v0.58.0.zip": stream error: stream ID 129; INTERNAL_ERROR; received from peer' >&2
      exit 1
    fi
    ;;
  *) echo "fake go: unexpected args: $*" >&2; exit 64 ;;
esac
`)
			cmd := exec.Command("bash", script)
			cmd.Env = []string{
				"PATH=" + bin + ":/usr/bin:/bin",
				"HOME=" + dir,
				"FAKE_GOPROXY=" + tt.goproxy,
				"FAKE_CALLS=" + calls,
				"FAKE_FAILURES=" + strconv.Itoa(tt.failures),
				"GO_MOD_DOWNLOAD_ATTEMPTS=" + tt.attempts,
				"GO_MOD_DOWNLOAD_BACKOFF_SECONDS=0",
			}
			out, err := cmd.CombinedOutput()
			exitCode := 0
			if err != nil {
				exitErr := &exec.ExitError{}
				ok := errors.As(err, &exitErr)
				if !ok {
					t.Fatalf("run script: %v\n%s", err, out)
				}
				exitCode = exitErr.ExitCode()
			}
			if exitCode != tt.wantExit {
				t.Fatalf("exit = %d, want %d\n%s", exitCode, tt.wantExit, out)
			}
			var seen []string
			if data, err := os.ReadFile(calls); err == nil {
				seen = strings.Fields(string(data))
			}
			if len(seen) != tt.wantCalls {
				t.Fatalf("go mod download ran %d times, want %d\n%s", len(seen), tt.wantCalls, out)
			}
			for i, proxy := range seen {
				if proxy != tt.wantGoProxy {
					t.Errorf("attempt %d GOPROXY = %q, want %q", i+1, proxy, tt.wantGoProxy)
				}
			}
			if tt.wantInMessage != "" && !strings.Contains(string(out), tt.wantInMessage) {
				t.Errorf("output missing %q:\n%s", tt.wantInMessage, out)
			}
		})
	}
}

// assertGoModDownloadFollowsSetupGo checks the setup-go step at index i: its
// own module cache is off and the next step is the go-mod-download action.
//
// setup-go's cache is first-writer-wins on an immutable go.sum key and saves
// whatever GOMODCACHE holds when the first job finishes (3 KB on Blacksmith,
// whose GOCACHEPROG leaves GOCACHE empty), so it cannot be trusted to be
// complete. go-mod-download is the single owner of the module cache.
func assertGoModDownloadFollowsSetupGo(t *testing.T, where string, steps []goFetchStep, i int) {
	t.Helper()
	if got := steps[i].With["cache"]; got != "false" {
		t.Errorf("%s: actions/setup-go must set `cache: false` (got %q); %s owns the module cache", where, got, goModDownloadActionRef)
	}
	if i+1 >= len(steps) || steps[i+1].Uses != goModDownloadActionRef {
		t.Errorf("%s: the step after actions/setup-go must be `uses: %s` so no build or test step fetches modules from the proxy unretried",
			where, goModDownloadActionRef)
	}
}

func loadGoFetchAction(t *testing.T, root, rel string) goFetchAction {
	t.Helper()
	var action goFetchAction
	if err := yaml.Unmarshal([]byte(readFile(t, root, rel)), &action); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return action
}

func findGoFetchStep(steps []goFetchStep, usesPrefix string) (goFetchStep, int) {
	for i, step := range steps {
		if strings.HasPrefix(step.Uses, usesPrefix) {
			return step, i
		}
	}
	return goFetchStep{}, -1
}

func findGoFetchStepByID(steps []goFetchStep, id string) (goFetchStep, int) {
	for i, step := range steps {
		if step.ID == id {
			return step, i
		}
	}
	return goFetchStep{}, -1
}
