package scripts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// bazel.yml's lanes keep the LLVM archive in a runner cache entry of its own
// (.github/actions/llvm-archive-cache, keyed on the archive's sha256) and
// out of setup-bazel's runner cache. setup-bazel caches only its repository
// cache's content-addressable store, whose entries Bazel re-verifies on use;
// without this, every save of that entry would carry the ~1.9GB archive.

const (
	llvmArchiveCacheDir  = ".github/actions/llvm-archive-cache"
	llvmArchiveCacheUses = "./" + llvmArchiveCacheDir
	setupBazelUses       = "./" + setupBazelDir
	llvmArchiveSaveStep  = "Save LLVM archive cache"
	llvmArchiveDropStep  = "Keep the LLVM archive out of the runner cache"
	llvmArchiveCheckStep = "llvm-fetched"
	runnerCacheSaveStep  = "Save Bazel runner cache"
	// The lane's runner cache saves are push-to-main runs of the unit lane.
	runnerCacheSaveScope = "always() && matrix.lane == 'unit' && github.event_name == 'push' && github.ref == 'refs/heads/main' && "
)

type compositeAction struct {
	Runs struct {
		Steps []multiLaneStep `yaml:"steps"`
	} `yaml:"runs"`
}

func readCompositeSteps(t *testing.T, dir string) []multiLaneStep {
	t.Helper()
	var action compositeAction
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), dir+"/action.yml")), &action); err != nil {
		t.Fatalf("parse %s/action.yml: %v", dir, err)
	}
	return action.Runs.Steps
}

func stepByID(steps []multiLaneStep, id string) *multiLaneStep {
	for i := range steps {
		if steps[i].ID == id {
			return &steps[i]
		}
	}
	return nil
}

// llvmArchiveSHA256 is MODULE.bazel's llvm_dist archive digest.
func llvmArchiveSHA256(t *testing.T) string {
	t.Helper()
	dist, err := moduleCall(readFile(t, repoRoot(t), hermeticCCModuleFile), "llvm_dist", `name = "llvm_dist_linux_x86_64"`)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`sha256 = "([0-9a-f]{64})"`).FindStringSubmatch(dist)
	if m == nil {
		t.Fatal("llvm_dist pins no sha256")
	}
	return m[1]
}

// runLLVMLocate runs locate.sh against module and returns its outputs.
func runLLVMLocate(t *testing.T, module string) (map[string]string, error) {
	t.Helper()
	dir := t.TempDir()
	modulePath := filepath.Join(dir, "MODULE.bazel")
	if err := os.WriteFile(modulePath, []byte(module), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output")
	script := readFile(t, repoRoot(t), llvmArchiveCacheDir+"/locate.sh")
	out, err := runWorkflowStepScript(t, dir, script, map[string]string{
		"REPO_CACHE":    "/rc/repo",
		"RUNNER_OS":     "Linux",
		"MODULE_BAZEL":  modulePath,
		"GITHUB_OUTPUT": output,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, out)
	}
	got := map[string]string{}
	for _, name := range []string{"path", "key"} {
		if v, ok := readStepOutput(t, output, name); ok {
			got[name] = v
		}
	}
	return got, nil
}

func TestLLVMArchiveCacheLocatesModuleArchive(t *testing.T) {
	sum := llvmArchiveSHA256(t)
	module := readFile(t, repoRoot(t), hermeticCCModuleFile)
	got, err := runLLVMLocate(t, module)
	if err != nil {
		t.Fatalf("locate.sh on %s: %v", hermeticCCModuleFile, err)
	}
	want := map[string]string{
		"path": "/rc/repo/content_addressable/sha256/" + sum,
		"key":  "bazel-llvm-v1-Linux-" + sum,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("locate.sh outputs = %v, want %v", got, want)
	}

	dist, err := moduleCall(module, "llvm_dist", `name = "llvm_dist_linux_x86_64"`)
	if err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"no llvm_dist":  strings.Replace(module, "llvm_dist(\n", "llvm_other(\n", 1),
		"two llvm_dist": module + "\nllvm_dist(\n" + dist + ")\n",
		"unpinned":      strings.Replace(module, `sha256 = "`+sum+`",`, `sha256 = "",`, 1),
	} {
		if got, err := runLLVMLocate(t, bad); err == nil {
			t.Errorf("%s: locate.sh succeeded with %v, want an error", name, got)
		}
	}
}

// The composite's REPO_CACHE must be setup-bazel's --repository_cache, or
// the restored archive lands where Bazel never looks.
func TestLLVMArchiveCacheIsSetupBazelRepositoryCache(t *testing.T) {
	root := repoRoot(t)
	rc := stepByID(readCompositeSteps(t, setupBazelDir), "rc")
	if rc == nil || rc.Env["BAZEL_CI_CACHE_DIR"] == "" {
		t.Fatalf("%s has no Write CI bazelrc step (id rc) with BAZEL_CI_CACHE_DIR", setupBazelDir)
	}
	if !strings.Contains(readFile(t, root, setupBazelDir+"/write-bazelrc.sh"), `echo "common --repository_cache=$cache_dir/repo"`) {
		t.Fatalf("%s/write-bazelrc.sh no longer puts --repository_cache at BAZEL_CI_CACHE_DIR/repo", setupBazelDir)
	}
	steps := readCompositeSteps(t, llvmArchiveCacheDir)
	locate := stepByID(steps, "locate")
	if locate == nil || locate.Env["REPO_CACHE"] != rc.Env["BAZEL_CI_CACHE_DIR"]+"/repo" {
		t.Errorf("%s: REPO_CACHE must be %s/repo (setup-bazel's --repository_cache)", llvmArchiveCacheDir, rc.Env["BAZEL_CI_CACHE_DIR"])
	}
	restore := stepByID(steps, "restore")
	if restore == nil || !strings.HasPrefix(restore.Uses, "actions/cache/restore@") ||
		restore.With["path"] != "${{ steps.locate.outputs.path }}" || restore.With["key"] != "${{ steps.locate.outputs.key }}" ||
		restore.With["restore-keys"] != "" {
		t.Errorf("%s: the restore step must restore the located path under its exact key only", llvmArchiveCacheDir)
	}
}

func TestBazelMultiLaneLLVMArchiveCache(t *testing.T) {
	wf := readMultiLaneWorkflow(t)
	// Every job that sets up Bazel restores the archive right after its last
	// setup-bazel step (the lane job has two: the fork lanes' fallback to
	// the read-only cache), so before its first bazel command.
	jobs := 0
	for id, job := range wf.Jobs {
		last := -1
		for i, step := range job.Steps {
			if step.Uses == setupBazelUses {
				last = i
			}
		}
		if last < 0 {
			continue
		}
		jobs++
		if last+1 >= len(job.Steps) || job.Steps[last+1].Uses != llvmArchiveCacheUses {
			t.Errorf("job %s: the step after its last %s must be %s", id, setupBazelUses, llvmArchiveCacheUses)
		}
		for i, step := range job.Steps[:last] {
			if step.Uses == llvmArchiveCacheUses || regexp.MustCompile(`(?m)^\s*bazel\s`).MatchString(step.Run) {
				t.Errorf("job %s step %d (%q) restores the archive or runs bazel before setup-bazel is done", id, i, step.Name)
			}
		}
	}
	if jobs == 0 {
		t.Fatalf("%s: no job uses %s", bazelMultiLaneWorkflow, setupBazelUses)
	}

	lane := wf.Jobs["lane"].Steps
	index := map[string]int{}
	for i, step := range lane {
		for _, k := range []string{step.Name, step.ID} {
			if k != "" {
				index[k] = i
			}
		}
	}
	order := []string{"llvm", llvmArchiveCheckStep, llvmArchiveSaveStep, llvmArchiveDropStep, runnerCacheSaveStep}
	for i, name := range order {
		if _, ok := index[name]; !ok {
			t.Fatalf("lane job has no step %q", name)
		}
		if i > 0 && index[order[i-1]] >= index[name] {
			t.Errorf("lane job: step %q must come before %q", order[i-1], name)
		}
	}
	if s := lane[index["llvm"]]; s.Uses != llvmArchiveCacheUses {
		t.Errorf("lane job: step llvm uses %q, want %s", s.Uses, llvmArchiveCacheUses)
	}
	save := lane[index[llvmArchiveSaveStep]]
	if !strings.HasPrefix(save.Uses, "actions/cache/save@") ||
		save.With["path"] != "${{ steps.llvm.outputs.path }}" || save.With["key"] != "${{ steps.llvm.outputs.key }}" ||
		save.If != runnerCacheSaveScope+"steps.llvm-fetched.outputs.fetched == 'true' && steps.llvm.outputs.cache-hit != 'true'" {
		t.Errorf("lane job: %q must save steps.llvm's path under its key, on a push to main from the unit lane, when fetched and not restored (if %q)", llvmArchiveSaveStep, save.If)
	}
	runnerSave := lane[index[runnerCacheSaveStep]]
	if !strings.HasPrefix(runnerSave.If, runnerCacheSaveScope) {
		t.Errorf("lane job: %q if %q no longer starts with %q; keep %q in step", runnerCacheSaveStep, runnerSave.If, runnerCacheSaveScope, llvmArchiveSaveStep)
	}
	// The runner cache entry saved is the one setup-bazel restores: same
	// path (part of the cache version) and key.
	restore := stepByID(readCompositeSteps(t, setupBazelDir), "restore")
	if restore == nil {
		t.Fatalf("%s has no restore step", setupBazelDir)
	}
	if runnerSave.With["path"] != restore.With["path"] || runnerSave.With["key"] != restore.With["key"] {
		t.Errorf("lane job: %q saves %v, but setup-bazel restores %v", runnerCacheSaveStep, runnerSave.With, restore.With)
	}
	drop := lane[index[llvmArchiveDropStep]]
	if !strings.HasPrefix(drop.If, "always()") || drop.Env["LLVM_ARCHIVE_DIR"] != "${{ steps.llvm.outputs.path }}" {
		t.Errorf("lane job: %q must run always() on LLVM_ARCHIVE_DIR=steps.llvm.outputs.path", llvmArchiveDropStep)
	}

	// Run the fetched check and the drop step on a repository cache holding
	// the archive and another download: the archive is reported, then only
	// it leaves the tree the runner cache saves.
	dir := t.TempDir()
	cas := filepath.Join(dir, "bazel-ci-cache", "repo", "content_addressable", "sha256")
	llvm := filepath.Join(cas, llvmArchiveSHA256(t))
	other := filepath.Join(cas, strings.Repeat("a", 64), "file")
	for _, f := range []string{filepath.Join(llvm, "file"), other} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	output := filepath.Join(dir, "output")
	env := map[string]string{"LLVM_ARCHIVE_DIR": llvm, "GITHUB_OUTPUT": output}
	fetched := func() string {
		t.Helper()
		if err := os.Remove(output); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if out, err := runWorkflowStepScript(t, dir, lane[index[llvmArchiveCheckStep]].Run, env); err != nil {
			t.Fatalf("%s: %v\n%s", llvmArchiveCheckStep, err, out)
		}
		v, _ := readStepOutput(t, output, "fetched")
		return v
	}
	if v := fetched(); v != "true" {
		t.Errorf("%s: fetched=%q with the archive present, want true", llvmArchiveCheckStep, v)
	}
	if out, err := runWorkflowStepScript(t, dir, drop.Run, env); err != nil {
		t.Fatalf("%s: %v\n%s", llvmArchiveDropStep, err, out)
	}
	if _, err := os.Stat(llvm); !os.IsNotExist(err) {
		t.Errorf("%s left %s in the runner cache tree", llvmArchiveDropStep, llvm)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("%s removed another download: %v", llvmArchiveDropStep, err)
	}
	if v := fetched(); v != "false" {
		t.Errorf("%s: fetched=%q without the archive, want false", llvmArchiveCheckStep, v)
	}
}
