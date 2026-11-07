package beads_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdStoreMigrateSchemaRunsTheQuietSchemaVerb(t *testing.T) {
	type call struct {
		dir  string
		name string
		args []string
	}
	var got call
	runner := func(dir, name string, args ...string) ([]byte, error) {
		got = call{dir: dir, name: name, args: args}
		return []byte("applied 2 schema migrations\n"), nil
	}
	out, err := beads.NewBdStore("/rig", runner).MigrateSchema()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "applied 2 schema migrations\n" {
		t.Errorf("out = %q, want bd's output preserved", out)
	}
	if got.dir != "/rig" || got.name != "bd" {
		t.Errorf("ran %q in %q, want bd in /rig", got.name, got.dir)
	}
	if strings.Join(got.args, " ") != "migrate schema --quiet" {
		t.Errorf("args = %q, want exactly 'migrate schema --quiet'", strings.Join(got.args, " "))
	}
}

// Only the remote-migrate gate's two refusal shapes are the sentinel, and the
// underlying error survives the wrap: callers report bd's own words.
func TestBdStoreMigrateSchemaClassifiesRemoteBackedRefusals(t *testing.T) {
	for name, stderr := range map[string]string{
		"auto-apply refusal":                 "Error: refusing to auto-apply 3 migrations to a remote-backed database\nHint: run bd migrate schema with BD_ALLOW_REMOTE_MIGRATE=1 to consent",
		"auto-apply refusal after a warning": "beads: ignoring GT_ROOT from .beads/.env (not in allowlist)\nError: refusing to auto-apply 1 migration of 'issues' to a remote-backed database",
		"already-migrated or forked refusal": "Error: refusing to migrate a remote-backed database whose schema has forked from the remote",
	} {
		t.Run(name, func(t *testing.T) {
			underlying := errors.New(stderr)
			runner := func(_, _ string, _ ...string) ([]byte, error) {
				return nil, fmt.Errorf("exit status 1: %w", underlying)
			}
			_, err := beads.NewBdStore("/rig", runner).MigrateSchema()
			if err == nil {
				t.Fatal("expected the refusal to be an error")
			}
			if !errors.Is(err, beads.ErrRemoteBackedMigrateRefused) {
				t.Fatalf("err = %v, want the remote-backed refusal sentinel", err)
			}
			if !errors.Is(err, underlying) {
				t.Fatalf("err = %v, want the underlying bd error preserved", err)
			}
			if !strings.Contains(err.Error(), stderr) {
				t.Fatalf("err = %v, want bd's diagnostic in the message", err)
			}
		})
	}
}

// A shared-server consent refusal and every other failure are the runner's
// original error, not the remote-backed sentinel.
func TestBdStoreMigrateSchemaLeavesOtherFailuresUntouched(t *testing.T) {
	for name, msg := range map[string]string{
		"shared-server consent refusal":  "Error: refusing to auto-apply 4 migrations to a database on a shared server; run bd migrate schema from the designated migrator",
		"dirty working set":              "Error: cannot complete schema migrations: table 'issues' has uncommitted changes in the working set",
		"remote mentioned, not the gate": "Error: migration failed while diffing against remote origin",
		"bare exec failure":              "exit status 1",
	} {
		t.Run(name, func(t *testing.T) {
			underlying := errors.New(msg)
			runner := func(_, _ string, _ ...string) ([]byte, error) {
				return nil, fmt.Errorf("exit status 1: %w", underlying)
			}
			_, err := beads.NewBdStore("/rig", runner).MigrateSchema()
			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, beads.ErrRemoteBackedMigrateRefused) {
				t.Fatalf("err = %v, want it without the remote-backed sentinel", err)
			}
			if !errors.Is(err, underlying) {
				t.Fatalf("err = %v, want the original error returned unchanged", err)
			}
		})
	}
}
