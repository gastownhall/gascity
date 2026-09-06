package main

import (
	"fmt"
	"strconv"
	"strings"
)

// Recovery from beads' dirty-table schema-migration refusal.
//
// Every beads store open runs pending schema migrations, and the migration
// runner refuses to alter a table that already carries uncommitted working-set
// changes — it will not entangle a migration's DDL with unrelated dirty rows
// (github.com/steveyegge/beads internal/storage/schema.DirtyTablesError). The
// refusal prescribes `bd dolt commit` as the remedy, but that command also
// opens the store and hits the same refusal before its commit can clear the
// dirty state: the prescribed remedy is circular.
//
// Upstream broke that deadlock in beads#4567, but only for the embedded
// backend — cmd/bd's newDoltStore returns on cfg.ServerMode before it ever
// reaches the cfg.LenientOpen branch that opens past the guard, so a
// server-mode store (what `bd init --server` and therefore every `gc rig add`
// uses) still has no in-tool escape. The dirty working set is not exotic
// either: a healthy managed database normally sits with `config` modified, so
// the next schema bump meets this guard by default.
//
// `gc rig add` creates the database, so it owns leaving it in a committable
// state rather than handing the operator a dirty database and an impossible
// instruction. The recovery below applies the remedy that does work — commit
// the working set directly through Dolt, bypassing bd — and re-runs init.

// maxBdInitDirtyTableRounds bounds the commit -> re-init recovery loop. One
// pass is not always enough: bd init makes partial progress and re-dirties
// tables as it goes, and the dirty set observed in the field shrank 9 -> 1 -> 0
// across two rounds. The bound keeps a database that never converges from
// spinning forever.
const maxBdInitDirtyTableRounds = 5

// bdInitDirtyTablesMarker is the stable part of beads' refusal text. It matches
// both the plain ("pending schema migrations alter pre-existing dirty tables")
// and ignored-migration ("pending ignored schema migrations alter ...") forms,
// and survives the wrapping every exec provider adds around bd's output.
const bdInitDirtyTablesMarker = "alter pre-existing dirty tables"

// dirtyScopeTablesCommitMessage is the Dolt commit message recorded for a
// working set committed by this recovery. It is a fixed literal with no quote
// characters so it needs no SQL escaping.
const dirtyScopeTablesCommitMessage = "gc: commit working set before beads schema migration"

// isBdInitDirtyTablesError reports whether err is beads' refusal to migrate
// tables that already have uncommitted working-set changes.
func isBdInitDirtyTablesError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), bdInitDirtyTablesMarker)
}

// commitDirtyScopeTables commits a scope database's uncommitted working set on
// the managed Dolt server, reporting whether there was anything to commit. It
// is a variable so tests can exercise the recovery loop without a live server.
var commitDirtyScopeTables = commitDirtyScopeTablesViaManagedDolt

// commitDirtyScopeTablesViaManagedDolt applies the remedy that actually clears
// the guard: commit the dirty tables straight through Dolt rather than through
// bd, whose own open would hit the refusal first. It reports false when the
// database has no dirty tables, which tells the caller that committing cannot
// make further progress.
//
// It only acts on a managed local Dolt server, mirroring the preconditions of
// verifyManagedDoltDatabaseExistsAfterInit: an external or gateway endpoint is
// not ours to commit against, and its working set is not what bd is refusing
// over here.
func commitDirtyScopeTablesViaManagedDolt(cityPath, database string) (bool, error) {
	database = strings.TrimSpace(database)
	if database == "" {
		return false, fmt.Errorf("no Dolt database resolved for scope")
	}
	if !cityUsesBdStoreContract(cityPath) {
		return false, fmt.Errorf("city %q does not use the bd store contract", cityPath)
	}
	if isExternalDolt(cityPath) {
		return false, fmt.Errorf("dolt endpoint for city %q is external; commit its working set at the endpoint", cityPath)
	}
	port := currentResolvableManagedDoltPort(cityPath)
	if strings.TrimSpace(port) == "" {
		return false, fmt.Errorf("no managed Dolt port resolvable for city %q", cityPath)
	}

	dirty, err := managedDoltDirtyTableCount("", port, "root", database)
	if err != nil {
		return false, err
	}
	if dirty == 0 {
		return false, nil
	}

	commitSQL := "USE " + managedDoltQuoteIdent(database) + "; " +
		"CALL DOLT_COMMIT('-A', '-m', '" + dirtyScopeTablesCommitMessage + "');"
	if _, err := runManagedDoltSQL("", port, "root", "-q", commitSQL); err != nil {
		return false, fmt.Errorf("committing dirty tables in Dolt database %q: %w", database, err)
	}
	return true, nil
}

// managedDoltDirtyTableCount returns how many tables in database have
// uncommitted working-set changes.
func managedDoltDirtyTableCount(host, port, user, database string) (int, error) {
	out, err := runManagedDoltSQL(host, port, user, "-r", "csv", "-q",
		"SELECT COUNT(*) AS cnt FROM "+managedDoltQuoteIdent(database)+".dolt_status")
	if err != nil {
		return 0, fmt.Errorf("reading dolt_status for Dolt database %q: %w", database, err)
	}
	n, err := parseSmokeCount(out)
	if err != nil {
		return 0, fmt.Errorf("reading dolt_status for Dolt database %q: %w", database, err)
	}
	return n, nil
}

// parseSmokeCount extracts the integer from `SELECT COUNT(*)` csv output by
// taking the last numeric line (the value row, after the "cnt" header).
// Inlined here so this PR is self-contained on upstream (the fork-only
// maintenance_dolt_ops.go helper is not available upstream).
func parseSmokeCount(out string) (int, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		n, err := strconv.Atoi(line)
		if err != nil {
			return 0, fmt.Errorf("parse smoke count %q: %w", line, err)
		}
		return n, nil
	}
	return 0, fmt.Errorf("empty smoke count output")
}

// recoverBdInitFromDirtyTables clears the dirty-table deadlock that initErr
// reports and re-runs init, repeating until init stops refusing. reinit re-runs
// the same bd init the caller just attempted.
//
// It returns nil once init succeeds. Otherwise it returns an error that wraps
// the refusal, so a caller that cannot be helped still sees why.
func recoverBdInitFromDirtyTables(cityPath, database string, initErr error, reinit func() error) error {
	if strings.TrimSpace(database) == "" {
		return initErr
	}

	err := initErr
	for round := 0; round < maxBdInitDirtyTableRounds; round++ {
		committed, commitErr := commitDirtyScopeTables(cityPath, database)
		if commitErr != nil {
			return fmt.Errorf("%w; committing the working set to clear it failed: %w", err, commitErr)
		}
		if !committed {
			// Nothing left to commit yet init still refuses — another commit
			// would change nothing, so stop rather than spin.
			return err
		}
		retryErr := reinit()
		if retryErr == nil {
			return nil
		}
		if !isBdInitDirtyTablesError(retryErr) {
			return retryErr
		}
		err = retryErr
	}
	return fmt.Errorf("bd init still reports dirty tables in Dolt database %q after %d commit rounds: %w",
		database, maxBdInitDirtyTableRounds, err)
}
