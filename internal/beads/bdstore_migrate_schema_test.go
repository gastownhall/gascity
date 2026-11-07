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

func TestBdStoreMigrateSchemaClassifiesRemoteBackedRefusals(t *testing.T) {
	for name, detail := range map[string]string{
		"auto-apply refusal": "warning: BD_OTEL_* environment variables are deprecated\n" +
			"Error: refusing to auto-apply 3 pending schema migrations to a remote-backed database\n" +
			"Hint: run bd migrate schema --force from the designated migrator",
		"migrate refusal": "Error: refusing to migrate a remote-backed database whose remote is already migrated\n" +
			"Hint: pull the remote first",
	} {
		t.Run(name, func(t *testing.T) {
			cause := errors.New("exit status 1: " + detail)
			runner := func(_, _ string, _ ...string) ([]byte, error) { return nil, cause }
			_, err := beads.NewBdStore("/city/rigs/api", runner).MigrateSchema()
			if !errors.Is(err, beads.ErrRemoteBackedMigrateRefused) {
				t.Fatalf("err = %v, want ErrRemoteBackedMigrateRefused", err)
			}
			if !errors.Is(err, cause) || !strings.Contains(err.Error(), "Hint:") {
				t.Fatalf("err = %v, want the original error and bd's full diagnostic preserved", err)
			}
		})
	}
}

func TestBdStoreMigrateSchemaRefusalOnStdoutIsClassified(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return []byte("Error: refusing to migrate a remote-backed database\n"), errors.New("exit status 1")
	}
	_, err := beads.NewBdStore("/city/rigs/api", runner).MigrateSchema()
	if !errors.Is(err, beads.ErrRemoteBackedMigrateRefused) {
		t.Fatalf("err = %v, want ErrRemoteBackedMigrateRefused for a refusal bd printed on stdout", err)
	}
}

func TestBdStoreMigrateSchemaOtherFailuresAreNotRemoteBackedRefusals(t *testing.T) {
	for name, detail := range map[string]string{
		"shared server": "Error: refusing to auto-apply 3 pending schema migrations to a database shared with other clients\n" +
			"Hint: run bd migrate schema to consent",
		"dirty table":  "Error: cannot migrate: dirty working set in table issues",
		"remote words": "Error: remote-backed database unreachable while migrating",
	} {
		t.Run(name, func(t *testing.T) {
			cause := errors.New("exit status 1: " + detail)
			runner := func(_, _ string, _ ...string) ([]byte, error) { return nil, cause }
			_, err := beads.NewBdStore("/city/rigs/api", runner).MigrateSchema()
			if err == nil || errors.Is(err, beads.ErrRemoteBackedMigrateRefused) {
				t.Fatalf("err = %v, want a failure that is not ErrRemoteBackedMigrateRefused", err)
			}
			if !errors.Is(err, cause) {
				t.Fatalf("err = %v, want the original error preserved", err)
			}
		})
	}
}
