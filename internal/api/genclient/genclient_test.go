package genclient_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
		t.Skip("oapi-codegen not on PATH; install via `go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.6.0`")
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
