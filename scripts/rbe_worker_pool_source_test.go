package scripts_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// S3b (rbe-worker-repo-design.md §5.3, §9 S3b): the pool and canary
// workflows each gain a "Fetch the pinned rbe-worker" step
// (uses: ./.github/actions/rbe-worker, D8's per-workflow source variable).
// Unset the variable and the shim takes in-tree: this checkout's own
// tools/rbe, byte-for-byte what every job ran before this slice. These
// tests pin that default and prove fetch.sh's two branches dynamically.

// rbeWorkerSourceWorkflows: each pool/canary workflow, the repository
// variable its fetch steps read, and the jobs that carry one.
var rbeWorkerSourceWorkflows = map[string]struct {
	variable string
	jobs     []string
}{
	rbeWorkerWorkflow:   {"RBE_WORKER_SOURCE_OSS", []string{"worker", "await-drift", "report-drift"}},
	rbeForkPoolWorkflow: {"RBE_WORKER_SOURCE_FORK", []string{"worker", "await-drift", "report-drift"}},
	rbeWorkerEnvCanary:  {"RBE_WORKER_SOURCE_CANARY", []string{"measure", "report"}},
}

// TestRBEWorkerPoolSourceDefaultsInTree: every fetch step in every pool and
// canary workflow reads its own repository variable and defaults to
// in-tree, never pinned, so an unset variable (today) changes nothing.
func TestRBEWorkerPoolSourceDefaultsInTree(t *testing.T) {
	root := repoRoot(t)
	for path, want := range rbeWorkerSourceWorkflows {
		t.Run(filepath.Base(path), func(t *testing.T) {
			var wf struct {
				Jobs map[string]struct {
					Steps []struct {
						Uses string         `yaml:"uses"`
						With map[string]any `yaml:"with"`
					} `yaml:"steps"`
				} `yaml:"jobs"`
			}
			if err := yaml.Unmarshal([]byte(readFile(t, root, path)), &wf); err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			wantSource := "${{ vars." + want.variable + " || 'in-tree' }}"
			var found []string
			for id, job := range wf.Jobs {
				for _, step := range job.Steps {
					if step.Uses != "./.github/actions/rbe-worker" {
						continue
					}
					found = append(found, id)
					if got := step.With["source"]; got != wantSource {
						t.Errorf("%s job %s: fetch step source = %v, want %q", path, id, got, wantSource)
					}
				}
			}
			if len(found) != len(want.jobs) {
				t.Errorf("%s: fetch steps in jobs %v, want one each in %v", path, found, want.jobs)
			}
			for _, id := range want.jobs {
				if !strings.Contains(strings.Join(found, ","), id) {
					t.Errorf("%s: job %s has no fetch step", path, id)
				}
			}
		})
	}
}

// TestRBEWorkerPoolSourceUnsetIsInTree: fetch.sh with SOURCE unset (what an
// unset repository variable yields, since the workflow's || 'in-tree'
// fallback never reaches fetch.sh at all -- the expression itself resolves
// to the literal "in-tree") outputs this checkout's own tools/rbe, exactly
// the path every pool/canary job's blacksmith-worker.sh/worker-env-drift
// call resolved to before S3b, with no network fetch.
func TestRBEWorkerPoolSourceUnsetIsInTree(t *testing.T) {
	root := repoRoot(t)
	script := readFile(t, root, rbeWorkerFetchScript)
	dir := t.TempDir()
	copyPath := filepath.Join(dir, "fetch.sh")
	if err := os.WriteFile(copyPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fetch.sh copy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pin"), []byte(strings.Repeat("0", 40)+"\n"), 0o644); err != nil {
		t.Fatalf("write pin: %v", err)
	}
	workspace := t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, "tools", "rbe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "tools", "rbe", "blacksmith-worker.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// fetch.sh's own SOURCE default is pinned (it has no way to know a
	// workflow caller's variable was unset); it is each workflow's
	// ${{ vars.X || 'in-tree' }} expression, pinned above, that turns an
	// unset repository variable into the literal "in-tree" this exercises.
	source := "in-tree"
	stdout, stderr, outputs, err := rbeWorkerRunFetchInWorkspace(t, copyPath, workspace, source, "pin")
	if err != nil {
		t.Fatalf("source %q: fetch.sh failed: %v\nstdout:\n%s\nstderr:\n%s", source, err, stdout, stderr)
	}
	if outputs["dir"] != filepath.Join(workspace, "tools", "rbe") || outputs["sha"] != "in-tree" {
		t.Errorf("source %q: outputs %v, want dir=%s sha=in-tree", source, outputs, filepath.Join(workspace, "tools", "rbe"))
	}
	if !strings.Contains(stdout, "rbe-worker: in-tree tools/rbe") {
		t.Errorf("source %q: stdout %q, want the in-tree log line", source, stdout)
	}
}

// TestRBEWorkerPoolSourcePinnedFetchesAndVerifies: the other side of D8's
// switch (set once S4 flips a repository variable to pinned). source:
// pinned takes the same fetch, verify-by-ancestry, checkout path S3a's
// worker-host job exercises, against a local file:// upstream so no new
// exec.Command call site is needed (reuses runRBEScript via
// rbeWorkerFixture/rbeWorkerFetchCopy, scripts/rbe_worker_shim_test.go).
func TestRBEWorkerPoolSourcePinnedFetchesAndVerifies(t *testing.T) {
	upstream, mainSHA, offMainSHA := rbeWorkerFixture(t)

	t.Run("accepts the pinned main commit", func(t *testing.T) {
		script := rbeWorkerFetchCopy(t, upstream, "pin", mainSHA)
		stdout, stderr, outputs, err := rbeWorkerRunFetch(t, script, "pinned", "pin")
		if err != nil {
			t.Fatalf("source pinned: fetch failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
		}
		if outputs["sha"] != mainSHA || !strings.HasSuffix(outputs["dir"], "/worker") {
			t.Errorf("source pinned: outputs %v, want sha %s and a dir ending in /worker", outputs, mainSHA)
		}
	})

	t.Run("still refuses an off-main commit", func(t *testing.T) {
		script := rbeWorkerFetchCopy(t, upstream, "pin", offMainSHA)
		stdout, _, outputs, err := rbeWorkerRunFetch(t, script, "pinned", "pin")
		if err == nil {
			t.Fatalf("source pinned: fetch of an off-main commit succeeded; it must be refused")
		}
		if !strings.Contains(stdout, "is not in gastownhall/rbe-worker main's history; refused") {
			t.Errorf("stdout = %q, want the refusal message", stdout)
		}
		if outputs["sha"] != "" || outputs["dir"] != "" {
			t.Errorf("outputs = %v, want none for a refused commit", outputs)
		}
	})
}

// rbeWorkerRunFetchInWorkspace runs fetch.sh with GITHUB_WORKSPACE pointed
// at a checkout carrying a runnable tools/rbe/blacksmith-worker.sh, the same
// layout the pool/canary jobs' own checkout step gives it.
func rbeWorkerRunFetchInWorkspace(t *testing.T, script, workspace, source string, args ...string) (stdout, stderr string, outputs map[string]string, err error) {
	t.Helper()
	runnerTemp := t.TempDir()
	outFile := filepath.Join(t.TempDir(), "output")
	summaryFile := filepath.Join(t.TempDir(), "summary")
	for _, f := range []string{outFile, summaryFile} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatalf("create %s: %v", f, err)
		}
	}
	env := append(os.Environ(),
		"RUNNER_TEMP="+runnerTemp,
		"GITHUB_OUTPUT="+outFile,
		"GITHUB_STEP_SUMMARY="+summaryFile,
		"GITHUB_WORKSPACE="+workspace,
	)
	if source != "" {
		env = append(env, "SOURCE="+source)
	}
	stdout, stderr, err = runRBEScript(filepath.Dir(script), env, script, args...)
	outputs = map[string]string{}
	if raw, rerr := os.ReadFile(outFile); rerr == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if k, v, ok := strings.Cut(line, "="); ok {
				outputs[k] = v
			}
		}
	}
	return stdout, stderr, outputs, err
}
