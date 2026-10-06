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
	"github.com/gastownhall/gascity/internal/bazeltest"
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
	oapiCodegen := oapiCodegenBinary(t)

	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	// Under bazel the prebuilt gen-client binary ships in runfiles; the
	// `go run` fallback covers plain `go test` (and needs a module cache).
	// Kept to a single call site: the source-resource census counts these.
	genClient := "go"
	args := []string{"run", "./cmd/gen-client"}
	for _, rf := range runfilesRoots() {
		bin := filepath.Join(rf, "_main", "cmd", "gen-client", "gen-client_", "gen-client")
		if _, statErr := os.Stat(bin); statErr == nil {
			genClient = bin
			args = nil
			break
		}
	}
	args = append(args, "-oapi-codegen", oapiCodegen)
	cmd := exec.Command(genClient, args...)
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

// oapiCodegenBinary returns the oapi-codegen the drift check runs. Under
// bazel it is the hermetic //MODULE.bazel-pinned build named by
// GC_OAPI_CODEGEN (a runfiles path); under plain `go test` it is the one on
// PATH (`make install-oapi-codegen`). A missing tool fails the test: a
// skipped drift check would let the committed client rot unnoticed.
func oapiCodegenBinary(t *testing.T) string {
	t.Helper()
	if rel := os.Getenv("GC_OAPI_CODEGEN"); rel != "" {
		for _, rf := range runfilesRoots() {
			bin := filepath.Join(rf, filepath.FromSlash(rel))
			if _, err := os.Stat(bin); err == nil {
				return bin
			}
		}
		t.Fatalf("GC_OAPI_CODEGEN=%q not found under runfiles %v", rel, runfilesRoots())
	}
	bin, err := exec.LookPath("oapi-codegen")
	if err != nil {
		t.Fatalf("oapi-codegen not on PATH (run `make install-oapi-codegen`, or `bazel test //internal/api/genclient:genclient_test` for the pinned build): %v", err)
	}
	return bin
}

// runfilesRoots lists the bazel runfiles directories in lookup order.
func runfilesRoots() []string {
	var roots []string
	for _, rf := range []string{os.Getenv("RUNFILES_DIR"), os.Getenv("TEST_SRCDIR")} {
		if rf != "" {
			roots = append(roots, rf)
		}
	}
	return roots
}

// findRepoRoot walks up from the current working directory until it
// finds a go.mod file.
func findRepoRoot() (string, error) {
	if root := bazeltest.OverrideRoot(); root != "" {
		return root, nil
	}
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
