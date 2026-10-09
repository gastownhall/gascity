package scripts_test

import (
	"archive/zip"
	"bytes"
	"encoding/json"
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
	goModVerifyScriptPath   = ".github/scripts/go-mod-verify-cache.sh"
	defaultGoProxy          = "https://proxy.golang.org,direct"
	resilientGoProxy        = "https://proxy.golang.org|https://proxy.golang.org|direct"
)

// goModDownloadExemptWorkflows lists workflows whose actions/setup-go jobs do
// not build this module through the go command's module fetcher, with the
// reason. Anything not listed must run the go-mod-download action.
var goModDownloadExemptWorkflows = map[string]string{
	// Bazel fetches modules through gazelle's go_deps, not the go command
	// in the job; setup-bazel already gives fetch_repo a "|" GOPROXY list.
	"bazel.yml": "Bazel fetches modules via go_deps",
	// Same: its one job is a Bazel run (setup-go only supplies the
	// /usr/local/go that locally executed tests exec in mode cache).
	"review-formulas.yml": "Bazel fetches modules via go_deps",
	// Installs gocyclo with `go install pkg@version`; never builds this module.
	"complexity.yml": "go install of a pinned tool only",
	// pull_request_target: GitHub reads it from the default branch's tip but
	// it checks out the PR's recorded base SHA, which can predate the action
	// (TestWatchdogWorkflow_RunsNoLocalActions in scripts/prwatchdog). The
	// watchdog it runs imports only the standard library.
	"pr-evidence-watchdog.yml": "checks out the PR's base SHA, which can predate the action; stdlib-only watchdog",
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
	ID              string            `yaml:"id"`
	If              string            `yaml:"if"`
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	ContinueOnError string            `yaml:"continue-on-error"`
	With            map[string]string `yaml:"with"`
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
// retried download, a go.sum verification of every module, and a save that
// runs only after that verification passed, on a cache miss from trusted
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
	verify, verifyIndex := findGoFetchStepByID(steps, "verify")
	if restoreIndex < 0 || saveIndex < 0 || downloadIndex < 0 || verifyIndex < 0 {
		t.Fatalf("go-mod-download must restore the cache, run %s, verify with %s (id: verify), and save the cache (restore=%d download=%d verify=%d save=%d)",
			goModDownloadScriptPath, goModVerifyScriptPath, restoreIndex, downloadIndex, verifyIndex, saveIndex)
	}
	if restoreIndex >= downloadIndex || downloadIndex >= verifyIndex || verifyIndex >= saveIndex {
		t.Fatalf("go-mod-download must restore, then download, then verify, then save; a save before the download completes would cache a partial module set, and one before verification could cache tampered modules")
	}
	if verifyIndex != downloadIndex+1 || verifyIndex != len(steps)-2 {
		t.Errorf("the verify step must run right after the download and be the last step before the save, so no later step can change the verified modules")
	}
	if !strings.Contains(verify.Run, "go-mod-verify-cache.sh") {
		t.Errorf("verify step must run %s, got %q", goModVerifyScriptPath, verify.Run)
	}
	if verify.If != "" || verify.ContinueOnError != "" {
		t.Errorf("verify step must always run and fail the job on a mismatch; got if=%q continue-on-error=%q", verify.If, verify.ContinueOnError)
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
		"steps.verify.outcome == 'success'",
		"steps.restore.outputs.cache-hit != 'true'",
		"github.event_name == 'push'",
		"github.event_name == 'schedule'",
	} {
		if !strings.Contains(save.If, want) {
			t.Errorf("save step condition must include %q so only a cache miss on a trusted default-branch run writes; got %q", want, save.If)
		}
	}
	for _, status := range []string{"always()", "failure()", "canceled()"} {
		if strings.Contains(save.If, status) {
			t.Errorf("save step must not run after a failed verification; drop %s from its condition %q", status, save.If)
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

// TestGoModVerifyCacheDetectsTamperedModules proves the verify step catches
// a poisoned module cache that the go command itself accepts. It serves a
// real module from a file:// proxy, downloads it with the retry script, then
// tampers with the cached zip and runs the verify script. Set
// GO_MOD_DOWNLOAD_TEST_BASH to run the scripts under another bash (for
// example macOS's bash 3.2).
func TestGoModVerifyCacheDetectsTamperedModules(t *testing.T) {
	root := repoRoot(t)
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("find go: %v", err)
	}
	shell := os.Getenv("GO_MOD_DOWNLOAD_TEST_BASH")
	if shell == "" {
		shell = "bash"
	}

	const (
		modPath    = "example.com/tampered"
		modVersion = "v1.0.0"
		modGoMod   = "module example.com/tampered\n\ngo 1.21\n"
		modSource  = "package tampered\n\n// Answer is what a build of this module returns.\nconst Answer = 42\n"
	)
	// Same length as modSource with one byte flipped: 42 becomes 43.
	tamperedSource := strings.Replace(modSource, "42", "43", 1)

	// moduleSum asks the go command for the module's go.sum hashes as
	// served by proxyDir, through a throwaway module cache.
	moduleSum := func(t *testing.T, fx goModFixture, proxyDir string) (sum, goModSum string) {
		t.Helper()
		scratch := t.TempDir()
		scratchCache := filepath.Join(scratch, "modcache")
		t.Cleanup(func() { makeTreeWritable(scratchCache) })
		env := fx.env(proxyDir)
		for i, kv := range env {
			if strings.HasPrefix(kv, "GOMODCACHE=") {
				env[i] = "GOMODCACHE=" + scratchCache
			}
		}
		cmd := exec.Command(goBin, "mod", "download", "-json", fx.modPath+"@"+fx.version)
		cmd.Dir = scratch
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go mod download -json: %v\n%s", err, out)
		}
		var info struct{ Sum, GoModSum string }
		if err := json.Unmarshal(out, &info); err != nil {
			t.Fatalf("parse go mod download -json: %v\n%s", err, out)
		}
		if !strings.HasPrefix(info.Sum, "h1:") || !strings.HasPrefix(info.GoModSum, "h1:") {
			t.Fatalf("go mod download -json returned no hashes: %s", out)
		}
		return info.Sum, info.GoModSum
	}

	tests := []struct {
		name string
		// tamper changes the module cache (and maybe the proxy) after a
		// clean download, as a poisoned Actions cache restore would.
		tamper func(t *testing.T, fx goModFixture)
		// redownload runs the download step again after tampering, as CI
		// runs it after a restore.
		redownload      bool
		wantDownloadErr string
		wantExit        int
		wantOut         []string
		wantNoOut       []string
	}{
		{
			name:      "clean cache verifies",
			tamper:    func(*testing.T, goModFixture) {},
			wantOut:   []string{"all modules verified"},
			wantNoOut: []string{"::error"},
		},
		{
			name: "zip with one byte flipped and its original ziphash is purged and re-downloaded",
			tamper: func(t *testing.T, fx goModFixture) {
				fx.removeExtracted(t)
				writeModuleZip(t, fx.cachedZip(), modPath, modVersion, modGoMod, tamperedSource)
			},
			redownload: true,
			wantOut: []string{
				"zip has been modified",
				"::error title=Go module cache failed verification::",
				"module cache re-downloaded and verified against go.sum",
			},
		},
		{
			name: "corrupt zip bytes are purged and re-downloaded",
			tamper: func(t *testing.T, fx goModFixture) {
				zipPath := fx.cachedZip()
				data, err := os.ReadFile(zipPath)
				if err != nil {
					t.Fatal(err)
				}
				i := bytes.Index(data, []byte("Answer = 42"))
				if i < 0 {
					t.Fatal("stored module source not found in the cached zip")
				}
				data[i+len("Answer = 4")] ^= 0x07
				chmodWrite(t, zipPath, data)
			},
			wantOut: []string{
				"::error title=Go module cache failed verification::",
				"module cache re-downloaded and verified against go.sum",
			},
		},
		{
			name: "tampered zip that cannot be healed fails the step",
			tamper: func(t *testing.T, fx goModFixture) {
				fx.removeExtracted(t)
				writeModuleZip(t, fx.cachedZip(), modPath, modVersion, modGoMod, tamperedSource)
				writeModuleZip(t, fx.proxyZip(), modPath, modVersion, modGoMod, tamperedSource)
			},
			redownload: true,
			wantExit:   1,
			wantOut: []string{
				"zip has been modified",
				"::error title=Go module cache failed verification::",
				"go mod download failed after 1 attempts",
			},
			wantNoOut: []string{"re-downloaded and verified"},
		},
		{
			name: "zip and ziphash rewritten together are rejected by the download step",
			tamper: func(t *testing.T, fx goModFixture) {
				fx.removeExtracted(t)
				writeModuleZip(t, fx.cachedZip(), modPath, modVersion, modGoMod, tamperedSource)
				// A consistent forgery: the ziphash matches the forged zip.
				sum, _ := moduleSum(t, fx, fx.proxyWith(t, tamperedSource))
				chmodWrite(t, fx.cachedZip()+"hash", []byte(sum))
			},
			redownload:      true,
			wantDownloadErr: "checksum mismatch",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newGoModFixture(t, goBin, modPath, modVersion, modGoMod, modSource)
			sum, goModSum := moduleSum(t, fx, fx.proxyDir)
			writeTestFile(t, filepath.Join(fx.mainDir, "go.sum"),
				modPath+" "+modVersion+" "+sum+"\n"+modPath+" "+modVersion+"/go.mod "+goModSum+"\n")
			run := func(script string) (string, int) {
				t.Helper()
				cmd := exec.Command(shell, filepath.Join(root, script))
				cmd.Dir = fx.mainDir
				cmd.Env = fx.env(fx.proxyDir)
				out, err := cmd.CombinedOutput()
				if err == nil {
					return string(out), 0
				}
				exitErr := &exec.ExitError{}
				if !errors.As(err, &exitErr) {
					t.Fatalf("run %s: %v\n%s", script, err, out)
				}
				return string(out), exitErr.ExitCode()
			}

			if out, code := run(goModDownloadScriptPath); code != 0 {
				t.Fatalf("clean download exit = %d\n%s", code, out)
			}
			tt.tamper(t, fx)
			if tt.redownload {
				out, code := run(goModDownloadScriptPath)
				if tt.wantDownloadErr != "" {
					if code == 0 || !strings.Contains(out, tt.wantDownloadErr) {
						t.Fatalf("download step exit = %d, want a failure mentioning %q\n%s", code, tt.wantDownloadErr, out)
					}
					return
				}
				// The go command itself accepts the tampered zip: only the
				// verify step stands between it and the build.
				if code != 0 {
					t.Fatalf("download of the tampered cache exit = %d; the go command was expected to trust the cached ziphash\n%s", code, out)
				}
			}

			out, code := run(goModVerifyScriptPath)
			if code != tt.wantExit {
				t.Fatalf("verify exit = %d, want %d\n%s", code, tt.wantExit, out)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out, want) {
					t.Errorf("verify output missing %q:\n%s", want, out)
				}
			}
			for _, unwanted := range tt.wantNoOut {
				if strings.Contains(out, unwanted) {
					t.Errorf("verify output unexpectedly contains %q:\n%s", unwanted, out)
				}
			}
			if tt.wantExit == 0 {
				// Whatever was healed, the cache now holds the real module.
				zipData, err := os.ReadFile(fx.cachedZip())
				if err != nil {
					t.Fatalf("read verified zip: %v", err)
				}
				if bytes.Contains(zipData, []byte("Answer = 43")) {
					t.Errorf("verified cache still holds the tampered module source")
				}
			}
		})
	}
}

// goModFixture is a main module that requires one module served from a
// file:// proxy; the test writes its go.sum from the module's real hashes.
type goModFixture struct {
	goBin    string
	dir      string
	mainDir  string
	proxyDir string
	modPath  string
	version  string
	goMod    string
}

func newGoModFixture(t *testing.T, goBin, modPath, version, goMod, source string) goModFixture {
	t.Helper()
	dir := t.TempDir()
	fx := goModFixture{
		goBin:    goBin,
		dir:      dir,
		mainDir:  filepath.Join(dir, "main"),
		proxyDir: filepath.Join(dir, "proxy"),
		modPath:  modPath,
		version:  version,
		goMod:    goMod,
	}
	t.Cleanup(func() { makeTreeWritable(filepath.Join(dir, "modcache")) })
	fx.writeProxy(t, fx.proxyDir, source)

	writeTestFile(t, filepath.Join(fx.mainDir, "go.mod"),
		"module example.com/main\n\ngo 1.21\n\nrequire "+modPath+" "+version+"\n")
	return fx
}

func (fx goModFixture) writeProxy(t *testing.T, proxyDir, source string) {
	t.Helper()
	at := filepath.Join(proxyDir, filepath.FromSlash(fx.modPath), "@v")
	writeTestFile(t, filepath.Join(at, "list"), fx.version+"\n")
	writeTestFile(t, filepath.Join(at, fx.version+".info"), `{"Version":"`+fx.version+`","Time":"2026-01-01T00:00:00Z"}`)
	writeTestFile(t, filepath.Join(at, fx.version+".mod"), fx.goMod)
	writeModuleZip(t, filepath.Join(at, fx.version+".zip"), fx.modPath, fx.version, fx.goMod, source)
}

// proxyWith serves the module with different source from a second proxy.
func (fx goModFixture) proxyWith(t *testing.T, source string) string {
	t.Helper()
	proxyDir := filepath.Join(fx.dir, "proxy-alt")
	fx.writeProxy(t, proxyDir, source)
	return proxyDir
}

// env is the scripts' whole environment: the real go, an isolated module and
// build cache, the file:// proxy with no checksum database (go.sum pins the
// module), and one quick download attempt.
func (fx goModFixture) env(proxyDir string) []string {
	return []string{
		"PATH=" + filepath.Dir(fx.goBin) + ":/usr/bin:/bin",
		"HOME=" + fx.dir,
		"GOMODCACHE=" + filepath.Join(fx.dir, "modcache"),
		"GOCACHE=" + filepath.Join(fx.dir, "gocache"),
		"GOPROXY=file://" + filepath.ToSlash(proxyDir),
		"GOSUMDB=off",
		"GOFLAGS=",
		"GOENV=off",
		"GOWORK=off",
		"GOTOOLCHAIN=local",
		"GO_MOD_DOWNLOAD_ATTEMPTS=1",
		"GO_MOD_DOWNLOAD_BACKOFF_SECONDS=0",
	}
}

func (fx goModFixture) cachedZip() string {
	return filepath.Join(fx.dir, "modcache", "cache", "download", filepath.FromSlash(fx.modPath), "@v", fx.version+".zip")
}

func (fx goModFixture) proxyZip() string {
	return filepath.Join(fx.proxyDir, filepath.FromSlash(fx.modPath), "@v", fx.version+".zip")
}

// removeExtracted deletes the extracted module tree, leaving only
// cache/download, which is all the Actions cache restores.
func (fx goModFixture) removeExtracted(t *testing.T) {
	t.Helper()
	extracted := filepath.Join(fx.dir, "modcache", filepath.FromSlash(fx.modPath)+"@"+fx.version)
	makeTreeWritable(extracted)
	if err := os.RemoveAll(extracted); err != nil {
		t.Fatalf("remove extracted module: %v", err)
	}
}

// writeModuleZip writes a module zip in the layout the go command expects,
// storing files uncompressed so a byte of source is a byte of the zip.
func writeModuleZip(t *testing.T, path, modPath, version, goMod, source string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	prefix := modPath + "@" + version + "/"
	for _, f := range []struct{ name, body string }{
		{"go.mod", goMod},
		{"tampered.go", source},
	} {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: prefix + f.name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	chmodWrite(t, path, buf.Bytes())
}

// chmodWrite overwrites path even when the go command left it read-only.
func chmodWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeTreeWritable lets t.TempDir cleanup remove the go command's read-only
// extracted module directories.
func makeTreeWritable(root string) {
	_ = filepath.WalkDir(root, func(path string, _ os.DirEntry, err error) error {
		if err == nil {
			_ = os.Chmod(path, 0o755)
		}
		return nil
	})
}
