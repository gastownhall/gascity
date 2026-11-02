package scripts_test

import (
	"path/filepath"
	"regexp"
	"testing"
)

// The OpenAPI breaking-change gate runs oasdiff twice over: Bazel's
// //cmd/openapi-breaking:openapi-breaking_test runs the MODULE.bazel
// oasdiff_bin release archive, and `make openapi-breaking-check-go`
// installs the Makefile's OASDIFF_VERSION. Both must be the same oasdiff, or
// a check that passes locally can fail in CI (oasdiff's checks and their
// default severities change between releases).
func TestOasdiffPinsAgree(t *testing.T) {
	root := repoRoot(t)
	makefile := readFile(t, root, "Makefile")
	module := readFile(t, root, "MODULE.bazel")

	m := regexp.MustCompile(`(?m)^OASDIFF_VERSION := (\S+)$`).FindStringSubmatch(makefile)
	if m == nil {
		t.Fatalf("Makefile has no OASDIFF_VERSION")
	}
	makeVersion := m[1]

	block := regexp.MustCompile(`(?ms)^http_archive\(\n    name = "oasdiff_bin",\n.*?^\)`).FindString(module)
	if block == "" {
		t.Fatalf("%s has no oasdiff_bin http_archive", filepath.Join(root, "MODULE.bazel"))
	}
	want := "https://github.com/oasdiff/oasdiff/releases/download/v" + makeVersion + "/oasdiff_" + makeVersion + "_linux_amd64.tar.gz"
	if !regexp.MustCompile(`urls = \["` + regexp.QuoteMeta(want) + `"\]`).MatchString(block) {
		t.Errorf("MODULE.bazel oasdiff_bin does not download the Makefile's OASDIFF_VERSION %s (want url %s):\n%s", makeVersion, want, block)
	}
}
