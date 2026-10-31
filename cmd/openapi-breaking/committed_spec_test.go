package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// TestCommittedSpecAgainstBase is the OpenAPI breaking-change gate itself:
// the committed internal/api/openapi.json against the base spec the target
// declares (GC_OPENAPI_BREAKING_BASE_SPEC, @openapi_base_spec: the PR base
// commit's spec in bazel.yml's unit lane, the committed spec when no base is
// configured), with the checked-in waiver policy. Outside Bazel there is no
// declared base and it skips; `make openapi-breaking-check-go` runs the same
// gate with git's merge base instead.
func TestCommittedSpecAgainstBase(t *testing.T) {
	base := bazeltest.DataPath(t, "GC_OPENAPI_BREAKING_BASE_SPEC")
	if base == "" {
		t.Skip("no declared base spec (not under bazel test); run make openapi-breaking-check-go")
	}
	if source := bazeltest.DataPath(t, "GC_OPENAPI_BREAKING_BASE_SOURCE"); source != "" {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("base spec: %s", strings.TrimSpace(string(data)))
	}
	root := bazeltest.RepoRoot(t)
	v, err := check(context.Background(), options{
		Oasdiff:  requireOasdiff(t),
		BaseFile: base,
		Revision: filepath.Join(root, defaultRevision),
		Policy:   filepath.Join(root, defaultPolicy),
	})
	if err != nil {
		t.Fatalf("openapi-breaking gate: %v", err)
	}
	// The report names the policy by its repository path, not the runfiles one.
	var out bytes.Buffer
	ok, err := report(&out, v, defaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("%s breaks clients of the base spec:\n%s", defaultRevision, out.String())
	}
	t.Log(out.String())
}
