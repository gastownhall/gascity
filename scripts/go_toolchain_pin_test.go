package scripts_test

import (
	"regexp"
	"strings"
	"testing"
)

// The Go toolchain is pinned in several files that no tool keeps in step: the
// go directive in go.mod, the Bazel go_sdk, the gazelle and nogo workspaces,
// and the golang images the k8s tool builds compile with. A bump that moves one
// and misses another still builds, but ships binaries (or lints, or caches)
// against a different Go than the module declares. These tests tie every pin to
// go.mod and hold go.mod at the first patched release.

// patchedGoFloor is the oldest Go release that carries the standard-library fixes
// the module vulnerability scan requires. Go 1.27.3 ships the net/http defect
// GO-2026-6859, which the Go module vulnerabilities check reports on every pull
// request until the toolchain reaches 1.27.4. Raise it with each stdlib advisory;
// never lower it.
const patchedGoFloor = "1.27.4"

var goDirectiveRE = regexp.MustCompile(`(?m)^go (\d+\.\d+\.\d+)$`)

// goDirective returns the "go X.Y.Z" version declared by the go.mod or go.work
// file at rel.
func goDirective(t *testing.T, rel string) string {
	t.Helper()
	m := goDirectiveRE.FindStringSubmatch(readFile(t, repoRoot(t), rel))
	if m == nil {
		t.Fatalf("%s has no `go X.Y.Z` directive", rel)
	}
	return m[1]
}

func TestGoModGoDirectiveIsAtPatchedFloor(t *testing.T) {
	have := goDirective(t, "go.mod")
	if !semverAtLeast(parseModuleSemver(t, have), parseModuleSemver(t, patchedGoFloor)) {
		t.Errorf("go.mod declares go %s, below the patched floor %s (GO-2026-6859); move every Go pin with it: MODULE.bazel go_sdk, gazelle.go.work, tools/nogo/go.mod, the golang images in contrib/k8s, go_repository_tools.bzl _PINS and tools/rbe/worker-env.txt", have, patchedGoFloor)
	}
}

func TestGoToolchainPinsAgreeWithGoMod(t *testing.T) {
	root := repoRoot(t)
	want := goDirective(t, "go.mod")

	for _, rel := range []string{"gazelle.go.work", "tools/nogo/go.mod"} {
		if got := goDirective(t, rel); got != want {
			t.Errorf("%s declares go %s, go.mod declares go %s; move them together", rel, got, want)
		}
	}

	module := readFile(t, root, hermeticCCModuleFile)
	sdk, err := moduleCall(module, "go_sdk.download", `name = "go_sdk"`)
	if err != nil {
		t.Fatal(err)
	}
	sdkVersion := regexp.MustCompile(`version = "([^"]+)"`).FindStringSubmatch(sdk)
	if sdkVersion == nil {
		t.Fatalf("%s: go_sdk.download(name = \"go_sdk\") has no version", hermeticCCModuleFile)
	}
	if sdkVersion[1] != want {
		t.Errorf("%s go_sdk.download version = %q, go.mod declares go %s; move them together", hermeticCCModuleFile, sdkVersion[1], want)
	}

	// Both k8s images compile their tools with the same pinned golang image, so
	// the tag must name go.mod's Go and the two digests must be the same image.
	fromGolang := regexp.MustCompile(`(?m)^FROM golang:(\S+)-bookworm@(sha256:[0-9a-f]{64}) AS `)
	digests := map[string]string{}
	for _, rel := range []string{"contrib/k8s/Dockerfile.base", "contrib/k8s/Dockerfile.agent"} {
		m := fromGolang.FindStringSubmatch(readFile(t, root, rel))
		if m == nil {
			t.Errorf("%s has no `FROM golang:<version>-bookworm@sha256:<digest>` stage", rel)
			continue
		}
		if m[1] != want {
			t.Errorf("%s builds with golang:%s, go.mod declares go %s; move the tag and its digest together", rel, m[1], want)
		}
		digests[rel] = m[2]
	}
	if len(digests) == 2 && digests["contrib/k8s/Dockerfile.base"] != digests["contrib/k8s/Dockerfile.agent"] {
		t.Errorf("the golang digests differ: Dockerfile.base %s, Dockerfile.agent %s; both stages pin one image",
			digests["contrib/k8s/Dockerfile.base"], digests["contrib/k8s/Dockerfile.agent"])
	}

	// nolinttest drives the nogo binary with an explicit -go_version; it must
	// not name an older Go than the module it lints.
	nolint := readFile(t, root, "tools/nogo/nolinttest/storeclass_nolint_test.go")
	if !strings.Contains(nolint, `"-go_version", "`+want+`"`) {
		t.Errorf("tools/nogo/nolinttest/storeclass_nolint_test.go must pass -go_version %s, the Go go.mod declares", want)
	}
}
