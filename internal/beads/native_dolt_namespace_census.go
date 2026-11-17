package beads

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

	beadslib "github.com/steveyegge/beads"
)

// The native engine's namespace census.
//
// A binding served by the native Dolt engine used to be censused by listing
// it: every row of both tiers, closed ones included, hydrated into a Bead so
// the caller could look at its id. The verdict is taken once per process, so
// every one-shot gc paid for the binding's whole history before it answered
// anything — on a city holding a week of closed order runs, seconds of each
// prompt hook.
//
// The engine reaches its rows through the beads library and holds no SQL of
// its own, but a server-mode library handle exports the pool it reads through
// (UnderlyingDB, the accessor ProxiedOpenedObservation already uses). The
// census is asked over that pool: one statement per tier table, reading the id
// column and stopping at the first resident, inside one transaction so both
// tiers are read from the same root.
//
// The statement takes one of two forms, by what the table's id column says of
// itself. A column that compares bytes (nativeCensusExactCollations — the
// schema the library creates) is asked for an id in the gaps between the
// namespaces, which its primary key answers by seeking: the cost does not grow
// with the binding's history. Any other column is asked byte for byte with
// HEX, which reads every id of a clean binding but still hydrates none.

var (
	_ NamespaceCensus               = (*NativeDoltStore)(nil)
	_ NamespaceCensusHandleProvider = (*NativeDoltStore)(nil)
)

// nativeCensusTables are the tables the library keeps beads in, wisps first.
// Both tiers hold every status: a closed bead stays in its table.
var nativeCensusTables = []string{"wisps", "issues"}

// nativeCensusPool is the library handle surface the census reads through.
type nativeCensusPool interface {
	UnderlyingDB() *sql.DB
}

// NamespaceCensusHandle offers the census only over a library handle that
// exports its pool. One that does not cannot be asked exactly, so discovery
// says no and the caller lists the binding.
func (s *NativeDoltStore) NamespaceCensusHandle() (NamespaceCensus, bool) {
	if s == nil {
		return nil, false
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return nil, false
	}
	defer release()
	if _, ok := storage.(nativeCensusPool); !ok {
		return nil, false
	}
	return s, true
}

// HasResidentOutside reports whether either tier holds a bead whose id none of
// prefixes claims, closed beads included. It satisfies NamespaceCensus.
//
// A failed read is an error and never a clean binding, a missing tier table
// included; the caller keeps its probe on any error. A handle with no pool
// answers ErrNamespaceCensusUnsupported.
func (s *NativeDoltStore) HasResidentOutside(prefixes []string) (bool, error) {
	var has bool
	err := s.withReadRetry(func(ctx context.Context, storage beadslib.Storage) error {
		pool, ok := storage.(nativeCensusPool)
		if !ok {
			return fmt.Errorf("censusing the id namespaces of the native store over %T: %w", storage, ErrNamespaceCensusUnsupported)
		}
		var err error
		has, err = nativeHasResidentOutside(ctx, pool.UnderlyingDB(), prefixes)
		return err
	})
	if err != nil {
		return false, err
	}
	return has, nil
}

// nativeHasResidentOutside asks each tier table for one id outside prefixes,
// in one transaction.
func nativeHasResidentOutside(ctx context.Context, db *sql.DB, prefixes []string) (bool, error) {
	if db == nil {
		return false, fmt.Errorf("censusing the id namespaces of the native store: the library handle's pool is nil: %w", ErrNamespaceCensusUnsupported)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("censusing the id namespaces of the native store: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only: nothing to keep

	for _, table := range nativeCensusTables {
		collation, empty, err := nativeCensusIDCollation(ctx, tx, table)
		if err == nil && empty {
			continue
		}
		// A collation that could not be read is an unknown one, asked byte
		// for byte; a table that cannot be read at all fails the statement
		// below.
		where, args := namespaceExclusionHexSQL("id", prefixes)
		if err == nil && nativeCensusExactCollations[collation] {
			where, args = namespaceGapSQL("id", prefixes)
		}
		query := "SELECT 1 FROM " + table
		if where != "" {
			query += " WHERE " + where
		}
		query += " LIMIT 1"
		var found int
		switch err := tx.QueryRowContext(ctx, query, args...).Scan(&found); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return false, fmt.Errorf("censusing the id namespaces of native table %s: %w", table, err)
		default:
			return true, nil
		}
	}
	return false, nil
}

// namespaceExclusionHexSQL builds the WHERE fragment matching rows whose id
// falls outside every prefix, comparing bytes: HEX(id) against the hex of the
// prefix and of the prefix plus its dash. It returns an empty fragment when no
// prefix is usable, which matches every row.
//
// Hex rather than the column's own comparison because the rule the caller
// reads ids by is a byte rule (storeref.IDInNamespace), and a column's
// collation may fold case or pad spaces. Hex also keeps % and _ in a prefix
// literal, and spells the same on MySQL and SQLite. It cannot seek: every id
// of the table is read when the binding is clean.
func namespaceExclusionHexSQL(idColumn string, prefixes []string) (string, []any) {
	hexID := "HEX(COALESCE(" + idColumn + ", ''))"
	var (
		clauses []string
		args    []any
	)
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		dashed := strings.ToUpper(hex.EncodeToString([]byte(prefix + "-")))
		clauses = append(clauses, "("+hexID+" = ? OR SUBSTR("+hexID+", 1, ?) = ?)")
		args = append(args, strings.ToUpper(hex.EncodeToString([]byte(prefix))), len(dashed), dashed)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "NOT (" + strings.Join(clauses, " OR ") + ")", args
}

// nativeCensusExactCollations are the id-column collations whose comparison is
// the byte order of the id: binary, with no padding. A range over such a
// column selects exactly the ids a byte comparison in Go would. The older
// utf8mb4_bin is absent on purpose: it pads, so 'gcg ' equals 'gcg' there.
var nativeCensusExactCollations = map[string]bool{
	"utf8mb4_0900_bin": true,
	"binary":           true,
}

// nativeCensusIDCollation reports the collation of table's id column, or that
// the table holds no row (an empty table has no resident, and no row to ask).
// A variable so tests can stand in for a server: SQLite has no COLLATION().
var nativeCensusIDCollation = func(ctx context.Context, tx *sql.Tx, table string) (collation string, empty bool, err error) {
	switch err := tx.QueryRowContext(ctx, "SELECT COLLATION(id) FROM "+table+" LIMIT 1").Scan(&collation); {
	case errors.Is(err, sql.ErrNoRows):
		return "", true, nil
	case err != nil:
		return "", false, err
	}
	return collation, false, nil
}

// namespaceSpan is a run of ids one prefix claims, from lo (inclusive) to hi.
type namespaceSpan struct {
	lo, hi string
	hiIncl bool
}

// namespaceGapSQL builds the WHERE fragment matching rows whose id falls
// outside every prefix as ranges over the id column: the ids below the first
// namespace, between each two, and above the last. It returns an empty
// fragment when no prefix is usable, which matches every row.
//
// A prefix claims the id equal to it and the ids beginning with it and a dash.
// In byte order the second set is one run: from prefix + "-" up to, and not
// including, prefix + "." — the dash's successor. The fragment is exact only
// where the column compares in that order (nativeCensusExactCollations).
func namespaceGapSQL(idColumn string, prefixes []string) (string, []any) {
	var spans []namespaceSpan
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		spans = append(spans,
			namespaceSpan{lo: prefix, hi: prefix, hiIncl: true},
			namespaceSpan{lo: prefix + "-", hi: prefix + "."})
	}
	if len(spans) == 0 {
		return "", nil
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].lo < spans[j].lo })

	clauses := []string{idColumn + " IS NULL", idColumn + " < ?"}
	args := []any{spans[0].lo}
	end, endIncl := spans[0].hi, spans[0].hiIncl
	for _, span := range spans[1:] {
		if span.lo > end {
			// Uncovered ids lie between the run so far and this span.
			if endIncl {
				clauses = append(clauses, "("+idColumn+" > ? AND "+idColumn+" < ?)")
			} else {
				clauses = append(clauses, "("+idColumn+" >= ? AND "+idColumn+" < ?)")
			}
			args = append(args, end, span.lo)
			end, endIncl = span.hi, span.hiIncl
			continue
		}
		if span.hi > end || (span.hi == end && span.hiIncl && !endIncl) {
			end, endIncl = span.hi, span.hiIncl
		}
	}
	if endIncl {
		clauses = append(clauses, idColumn+" > ?")
	} else {
		clauses = append(clauses, idColumn+" >= ?")
	}
	args = append(args, end)
	return strings.Join(clauses, " OR "), args
}
