package scripts_test

import (
	"fmt"
	"path"
	"strings"
	"testing"
)

// bazel-test.yml caches the job's --output_user_root so a run does not
// re-hash the repo source tree and the Go SDK. The repository cache's
// download store (cache/repos/v1/content_addressable) lives there too and
// keeps every fetched archive by sha256, including the ~1.9GB LLVM release
// tarball, so it must stay out of the entry. Extracted repositories live in
// the repo contents cache (cache/repos/v1/contents) and must stay in.
//
// actions/cache globs its path list with implicitDescendants off and hands
// the matches to tar, which recurses into every matched directory. A `!`
// exclusion below a matched directory is therefore ignored; the check below
// models that behavior instead of trusting the pattern text.

const (
	bazelCacheRestoreStep = "Restore bazel output base"
	bazelCacheSaveStep    = "Save bazel output base"
	bazelCacheRootExpr    = "${{ runner.temp }}/bazel-outputs"
)

// bazelOutputRootSample is a representative --output_user_root layout:
// the output base (md5 of the workspace path), the extracted install base,
// the repo contents cache and the download store.
var bazelOutputRootSample = []string{
	"0123456789abcdef0123456789abcdef/action_cache/action_cache_v18.blaze",
	"0123456789abcdef0123456789abcdef/external/go_sdk/bin/go",
	"install/abcdef/A-server.jar",
	"cache/repos/v1/contents/0007bc44/llvm/bin/clang",
	"cache/repos/v1/content_addressable/sha256/df0e1ecf/file",
}

// bazelCacheArchived returns the sample files actions/cache would archive
// for patterns: matches are found by walking the tree (last matching pattern
// wins, `!` negates, no implicit descendants) and each match is archived
// recursively.
func bazelCacheArchived(patterns []string, files []string) []string {
	match := func(p string) bool {
		hit := false
		for _, pat := range patterns {
			neg := strings.HasPrefix(pat, "!")
			pat = strings.TrimPrefix(pat, "!")
			if ok, _ := path.Match(pat, p); ok {
				hit = !neg
			}
		}
		return hit
	}
	var archived []string
	for _, f := range files {
		segs := strings.Split(f, "/")
		for i := 1; i <= len(segs); i++ {
			if match(strings.Join(segs[:i], "/")) {
				archived = append(archived, f)
				break
			}
		}
	}
	return archived
}

func checkBazelOutputCachePaths(paths string) []error {
	var patterns []string
	var errs []error
	for _, line := range strings.Split(strings.TrimSpace(paths), "\n") {
		line = strings.TrimSpace(line)
		neg := strings.HasPrefix(line, "!")
		rel, ok := strings.CutPrefix(strings.TrimPrefix(line, "!"), bazelCacheRootExpr)
		if !ok {
			errs = append(errs, fmt.Errorf("cache path %q is outside %s", line, bazelCacheRootExpr))
			continue
		}
		rel = "root" + rel
		if neg {
			rel = "!" + rel
		}
		patterns = append(patterns, rel)
	}
	var sample []string
	for _, f := range bazelOutputRootSample {
		sample = append(sample, "root/"+f)
	}
	archived := map[string]bool{}
	for _, f := range bazelCacheArchived(patterns, sample) {
		archived[strings.TrimPrefix(f, "root/")] = true
	}
	for _, f := range bazelOutputRootSample {
		download := strings.HasPrefix(f, "cache/repos/v1/content_addressable/")
		switch {
		case download && archived[f]:
			errs = append(errs, fmt.Errorf("cache archives the repository download store (%s)", f))
		case !download && !archived[f]:
			errs = append(errs, fmt.Errorf("cache drops %s", f))
		}
	}
	return errs
}

func TestBazelOutputCacheExcludesDownloadStore(t *testing.T) {
	var restore, save *bazelTestWorkflowStep
	steps := bazelTestWorkflowSteps(t, repoRoot(t))
	for i := range steps {
		switch steps[i].Name {
		case bazelCacheRestoreStep:
			restore = &steps[i]
		case bazelCacheSaveStep:
			save = &steps[i]
		}
	}
	if restore == nil || save == nil {
		t.Fatalf("%s needs both %q and %q steps", bazelTestWorkflow, bazelCacheRestoreStep, bazelCacheSaveStep)
	}
	if restore.With["path"] != save.With["path"] {
		t.Errorf("restore and save cache paths differ (paths are part of the cache version):\nrestore:\n%s\nsave:\n%s",
			restore.With["path"], save.With["path"])
	}
	for _, err := range checkBazelOutputCachePaths(save.With["path"]) {
		t.Error(err)
	}
}

func TestCheckBazelOutputCachePathsFixtures(t *testing.T) {
	r := bazelCacheRootExpr
	for name, paths := range map[string]string{
		"whole root":     r + "\n",
		"naive negate":   r + "\n!" + r + "/cache/repos/v1/content_addressable\n",
		"drops contents": r + "/*\n!" + r + "/cache\n",
		"outside root": r + "/*\n!" + r + "/cache\n" + r + "/cache/*\n!" + r + "/cache/repos\n" +
			r + "/cache/repos/*\n!" + r + "/cache/repos/v1\n" + r + "/cache/repos/v1/*\n!" +
			r + "/cache/repos/v1/content_addressable\n~/.cache/bazel\n",
	} {
		if len(checkBazelOutputCachePaths(paths)) == 0 {
			t.Errorf("%s: expected an error for:\n%s", name, paths)
		}
	}
	good := r + "/*\n!" + r + "/cache\n" + r + "/cache/*\n!" + r + "/cache/repos\n" +
		r + "/cache/repos/*\n!" + r + "/cache/repos/v1\n" + r + "/cache/repos/v1/*\n!" +
		r + "/cache/repos/v1/content_addressable\n"
	if errs := checkBazelOutputCachePaths(good); len(errs) != 0 {
		t.Errorf("layered exclusion: unexpected errors: %v", errs)
	}
}
