package beads_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdStoreMigrateSchemaRunsTheVerbAndReturnsItsOutput(t *testing.T) {
	var gotDir, gotName string
	var gotArgs []string
	runner := func(dir, name string, args ...string) ([]byte, error) {
		gotDir, gotName, gotArgs = dir, name, args
		return []byte("Applied 2 schema migrations\n"), nil
	}
	out, err := beads.NewBdStore("/city/rigs/api", runner).MigrateSchema()
	if err != nil {
		t.Fatalf("MigrateSchema: %v", err)
	}
	if string(out) != "Applied 2 schema migrations\n" {
		t.Errorf("output = %q, want bd's output verbatim", out)
	}
	if gotDir != "/city/rigs/api" || gotName != "bd" {
		t.Errorf("ran %q in %q, want bd in the store's directory", gotName, gotDir)
	}
	if got := strings.Join(gotArgs, " "); got != "migrate schema --quiet" {
		t.Errorf("args = %q, want exactly migrate schema --quiet", got)
	}
}

func TestBdStoreMigrateSchemaPreservesTheOriginalError(t *testing.T) {
	cause := errors.New("exit status 1: Error: dirty working set in table issues")
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte("partial output\n"), cause
	}
	out, err := beads.NewBdStore("/city/rigs/api", runner).MigrateSchema()
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want it to wrap the runner's error", err)
	}
	if !strings.Contains(err.Error(), "dirty working set in table issues") {
		t.Errorf("err = %q lost bd's diagnostic", err)
	}
	if string(out) != "partial output\n" {
		t.Errorf("output = %q, want the failed command's output preserved", out)
	}
}
