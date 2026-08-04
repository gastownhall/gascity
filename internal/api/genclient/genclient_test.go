package genclient_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// TestGeneratedClientInSync regenerates client_gen.go from the live spec
// and diffs against the committed copy. If they differ, the regenerated
// content is dumped so the developer can either commit the change or fix
// the underlying spec drift.
//
// This is the parallel of TestOpenAPISpecInSync (in internal/api): both
// guard the spec → committed-artifact pipeline so the typed contract
// can't drift unnoticed.
func TestGeneratedClientInSync(t *testing.T) {
	if _, err := exec.LookPath("oapi-codegen"); err != nil {
		// CI installs oapi-codegen via `make spec-ci`, which also runs
		// regeneration and fails on drift. Only skip when running locally
		// without the tool — CI has the GC_REQUIRE_OAPI_CODEGEN=1 env set.
		if os.Getenv("GC_REQUIRE_OAPI_CODEGEN") == "1" {
			t.Fatalf("oapi-codegen not on PATH; install via `go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.6.0`")
		}
		t.Skip("oapi-codegen not on PATH; install via `go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.6.0` (or set GC_REQUIRE_OAPI_CODEGEN=1 in CI to fatal)")
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	cmd := exec.Command("go", "run", "./cmd/gen-client")
	cmd.Dir = repoRoot
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("regenerate client: %v\nstderr: %s", err, errBuf.String())
	}

	committedPath := filepath.Join(repoRoot, "internal", "api", "genclient", "client_gen.go")
	committed, err := os.ReadFile(committedPath)
	if err != nil {
		t.Fatalf("read committed client: %v", err)
	}

	if !bytes.Equal(committed, out.Bytes()) {
		t.Errorf("generated client differs from committed file at %s", committedPath)
		t.Errorf("regenerate via `go generate ./internal/api/genclient` and commit the result")
	}
}

func TestEventStreamEnvelopePreservesTopologyPresence(t *testing.T) {
	for _, tc := range []struct {
		name        string
		deps        *[]string
		wantPresent bool
	}{
		{name: "unknown"},
		{name: "root", deps: ptrToStrings([]string{}), wantPresent: true},
		{name: "dependent", deps: ptrToStrings([]string{"build"}), wantPresent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(genclient.EventStreamEnvelope{DependsOnStepIds: tc.deps})
			if err != nil {
				t.Fatalf("marshal envelope: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatalf("unmarshal fields: %v", err)
			}
			_, present := fields["depends_on_step_ids"]
			if present != tc.wantPresent {
				t.Fatalf("topology field present = %v, want %v; JSON = %s", present, tc.wantPresent, encoded)
			}

			var decoded genclient.EventStreamEnvelope
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			if !sameStepDependencies(decoded.DependsOnStepIds, tc.deps) {
				t.Fatalf("round-trip dependencies = %#v, want %#v", decoded.DependsOnStepIds, tc.deps)
			}
		})
	}
}

func ptrToStrings(values []string) *[]string { return &values }

func sameStepDependencies(got, want *[]string) bool {
	if got == nil || want == nil {
		return got == nil && want == nil
	}
	return slices.Equal(*got, *want)
}

// findRepoRoot walks up from the current working directory until it
// finds a go.mod file.
func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd, nil
		}
		parent := filepath.Dir(wd)
		if parent == wd {
			return "", os.ErrNotExist
		}
		wd = parent
	}
}
