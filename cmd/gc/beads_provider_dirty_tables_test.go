package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// bdDirtyTablesErrText is the refusal beads emits (verbatim, modulo the table
// list) when pending schema migrations would alter tables that already carry
// uncommitted working-set changes. Reproduced here so the classifier is tested
// against the real upstream text rather than a paraphrase of it.
const bdDirtyTablesErrText = "bd init: pending schema migrations alter pre-existing dirty tables: " +
	"config, dependencies; run 'bd dolt commit' to commit the working set at the current schema, " +
	"then re-run the migration (gastownhall/beads#4566)"

func TestIsBdInitDirtyTablesError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "verbatim beads refusal", err: errors.New(bdDirtyTablesErrText), want: true},
		{
			name: "ignored-migration variant",
			err: errors.New("pending ignored schema migrations alter pre-existing dirty tables: " +
				"issue_snapshots"),
			want: true,
		},
		{
			name: "wrapped by an exec provider",
			err:  fmt.Errorf("gc-beads-bd init: %w", errors.New(bdDirtyTablesErrText)),
			want: true,
		},
		{
			name: "unrelated migration failure",
			err:  errors.New("bd init: Unknown column 'agent_state' in 'issues'"),
			want: false,
		},
		{name: "already initialized", err: errors.New("bd init: already initialized"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isBdInitDirtyTablesError(tt.err); got != tt.want {
				t.Fatalf("isBdInitDirtyTablesError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// stubCommitDirtyScopeTables swaps the managed-Dolt commit seam for the test's
// own and restores it afterwards.
func stubCommitDirtyScopeTables(t *testing.T, fn func(cityPath, database string) (bool, error)) {
	t.Helper()
	prev := commitDirtyScopeTables
	commitDirtyScopeTables = fn
	t.Cleanup(func() { commitDirtyScopeTables = prev })
}

// The dirty set observed in the field shrank 9 -> 1 -> 0: bd init makes partial
// progress each pass and re-dirties tables, so a single commit does not clear
// it. Recovery must keep committing until init stops refusing.
func TestRecoverBdInitFromDirtyTablesLoopsUntilClean(t *testing.T) {
	var commits int
	stubCommitDirtyScopeTables(t, func(_, database string) (bool, error) {
		commits++
		if database != "gsp" {
			t.Fatalf("commit database = %q, want %q", database, "gsp")
		}
		return true, nil
	})

	var reinits int
	err := recoverBdInitFromDirtyTables("/city", "gsp", errors.New(bdDirtyTablesErrText), func() error {
		reinits++
		if reinits < 2 {
			return errors.New(bdDirtyTablesErrText)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("recoverBdInitFromDirtyTables() = %v, want nil", err)
	}
	if commits != 2 {
		t.Fatalf("commit rounds = %d, want 2", commits)
	}
	if reinits != 2 {
		t.Fatalf("re-init attempts = %d, want 2", reinits)
	}
}

// A commit that reports nothing left to commit cannot make further progress:
// looping again would spin against an unchanged database.
func TestRecoverBdInitFromDirtyTablesStopsWhenNothingLeftToCommit(t *testing.T) {
	initErr := errors.New(bdDirtyTablesErrText)
	var commits, reinits int
	stubCommitDirtyScopeTables(t, func(string, string) (bool, error) {
		commits++
		return false, nil
	})

	err := recoverBdInitFromDirtyTables("/city", "gsp", initErr, func() error {
		reinits++
		return nil
	})
	if !errors.Is(err, initErr) {
		t.Fatalf("error = %v, want the original init error", err)
	}
	if commits != 1 {
		t.Fatalf("commit rounds = %d, want 1", commits)
	}
	if reinits != 0 {
		t.Fatalf("re-init attempts = %d, want 0", reinits)
	}
}

// A non-dirty-table failure on the retry is the real outcome and must surface
// unchanged rather than being retried as if it were still the deadlock.
func TestRecoverBdInitFromDirtyTablesSurfacesUnrelatedRetryFailure(t *testing.T) {
	stubCommitDirtyScopeTables(t, func(string, string) (bool, error) { return true, nil })

	other := errors.New("bd init: connection refused")
	err := recoverBdInitFromDirtyTables("/city", "gsp", errors.New(bdDirtyTablesErrText), func() error {
		return other
	})
	if !errors.Is(err, other) {
		t.Fatalf("error = %v, want %v", err, other)
	}
}

// When committing itself fails, the operator needs both the refusal and the
// reason the documented remedy could not be applied.
func TestRecoverBdInitFromDirtyTablesSurfacesCommitFailure(t *testing.T) {
	initErr := errors.New(bdDirtyTablesErrText)
	commitErr := errors.New("connect to managed Dolt: dial tcp: connection refused")
	stubCommitDirtyScopeTables(t, func(string, string) (bool, error) { return false, commitErr })

	err := recoverBdInitFromDirtyTables("/city", "gsp", initErr, func() error {
		t.Fatal("re-init must not run after a failed commit")
		return nil
	})
	if !errors.Is(err, initErr) {
		t.Fatalf("error = %v, want it to wrap the init refusal", err)
	}
	if !errors.Is(err, commitErr) {
		t.Fatalf("error = %v, want it to wrap the commit failure", err)
	}
}

// A database that never stops refusing must not spin forever.
func TestRecoverBdInitFromDirtyTablesBoundsRounds(t *testing.T) {
	var commits int
	stubCommitDirtyScopeTables(t, func(string, string) (bool, error) {
		commits++
		return true, nil
	})

	err := recoverBdInitFromDirtyTables("/city", "gsp", errors.New(bdDirtyTablesErrText), func() error {
		return errors.New(bdDirtyTablesErrText)
	})
	if err == nil {
		t.Fatal("recoverBdInitFromDirtyTables() = nil, want an error")
	}
	if commits != maxBdInitDirtyTableRounds {
		t.Fatalf("commit rounds = %d, want %d", commits, maxBdInitDirtyTableRounds)
	}
}

// An empty database name leaves nothing to commit against, so recovery must
// decline rather than guess at a target.
func TestRecoverBdInitFromDirtyTablesWithoutDatabase(t *testing.T) {
	initErr := errors.New(bdDirtyTablesErrText)
	stubCommitDirtyScopeTables(t, func(string, string) (bool, error) {
		t.Fatal("commit must not run without a database name")
		return false, nil
	})

	if err := recoverBdInitFromDirtyTables("/city", "  ", initErr, func() error {
		t.Fatal("re-init must not run without a database name")
		return nil
	}); !errors.Is(err, initErr) {
		t.Fatalf("error = %v, want the original init error", err)
	}
}

// End-to-end through the entry point `gc rig add` actually calls: a rig whose
// fresh database comes back dirty must be recovered in-process instead of
// handing the operator beads' circular "run 'bd dolt commit'" advice.
func TestInitBeadsForDirWithExecutorRecoversFromDirtyTables(t *testing.T) {
	cityDir := t.TempDir()
	cityConfig := `[workspace]
name = "demo"

[beads]
provider = "bd"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(cityDir, "rigs", "gascity-packs")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var commits int
	var gotCityPath, gotDatabase string
	stubCommitDirtyScopeTables(t, func(cityPath, database string) (bool, error) {
		commits++
		gotCityPath, gotDatabase = cityPath, database
		return true, nil
	})

	// Fail the first init the way beads does, then stop with a sentinel so the
	// assertion stays on the recovery and never reaches the real store
	// finalization.
	stopAfterRecovery := errors.New("stop after recovery")
	var calls int
	execute := func(_ string, _ []string, _ ...string) error {
		calls++
		if calls == 1 {
			return errors.New(bdDirtyTablesErrText)
		}
		return stopAfterRecovery
	}

	err := initBeadsForDirWithExecutor(cityDir, rigDir, "gsp", "gsp", execute)
	if !errors.Is(err, stopAfterRecovery) {
		t.Fatalf("initBeadsForDirWithExecutor error = %v, want %v", err, stopAfterRecovery)
	}
	if calls != 2 {
		t.Fatalf("bd init attempts = %d, want 2 (initial + post-commit retry)", calls)
	}
	if commits != 1 {
		t.Fatalf("commit rounds = %d, want 1", commits)
	}
	if gotCityPath != cityDir {
		t.Fatalf("commit cityPath = %q, want %q", gotCityPath, cityDir)
	}
	if gotDatabase != "gsp" {
		t.Fatalf("commit database = %q, want %q", gotDatabase, "gsp")
	}
}
