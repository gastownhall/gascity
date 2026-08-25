package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// NamespaceCensus is an optional Store capability: answering, in one query,
// whether the store holds ANY bead whose id falls outside a set of namespaces.
//
// This is a VERDICT, not a filter. The caller does not want the rows — it wants
// to know whether a by-id probe over this store can be retired — so the question
// is deliberately not a ListQuery field. A ListQuery field would oblige every
// backend to honor the superset-plus-ApplyListQuery contract for a question
// that has one shape and one caller, and would let a backend that ignored it
// answer with a superset. There is no safe superset of a verdict.
//
// A prefix claims a namespace under the same rule the caller reads ids by
// (storeref.IDInNamespace): id == prefix, or id begins with prefix + "-". The
// prefix is trimmed of surrounding whitespace first, and an empty prefix claims
// nothing — it is not a wildcard. An implementation whose rule differs from that
// one silently disagrees with its caller about what a namespace is, so a
// prefix-match written as LIKE prefix || '%' is wrong twice over: it swallows
// the neighboring gcgx namespace, and it reads % and _ inside a prefix as
// wildcards.
//
// Implementations MUST count CLOSED beads and BOTH tiers. Closing a bead does
// not make it findable by prefix: it is still shown, reopened, claimed and
// written by id, and the class migration that carried it across never deletes
// the work store's pre-migration copy. A predicate that skipped closed rows
// would report clean the moment the last relic closed, retiring the probe and
// sending that id back to a frozen copy that reads OPEN forever (ga-qdt5y.19).
// Because nothing deletes such a bead, the closed-inclusive answer is also
// MONOTONE, which is what makes it safe to take once per process.
//
// Answering FALSE strands every bead the implementation failed to see, so a
// store that cannot answer the question exactly must not implement this at all.
// Callers type-assert for it and fall back to listing the store and applying the
// rule themselves, which is always available and merely slower.
type NamespaceCensus interface {
	HasResidentOutside(prefixes []string) (bool, error)
}

var _ NamespaceCensus = (*SQLiteStore)(nil)

// namespaceExclusionSQL builds the WHERE fragment matching rows whose id
// expression falls outside every prefix, together with its bind arguments. It
// returns an empty fragment when no prefix is usable, which matches every row:
// a store that declares no namespace recognizes nothing, so all of it is
// outside.
//
// The prefixes are compile-time reserved-class constants at every call site
// today, and they are still bound as parameters. A literal here would be a
// standing invitation for the next caller to pass something it read from disk.
//
// substr/length rather than LIKE: LIKE would treat % and _ inside a prefix as
// wildcards, and length() counts in the same units substr() indexes in, so the
// comparison stays exact whatever the prefix encodes.
func namespaceExclusionSQL(idExpr string, prefixes []string) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	for _, prefix := range prefixes {
		prefix = strings.TrimSpace(prefix)
		if prefix == "" {
			continue
		}
		clauses = append(clauses, "("+idExpr+" = ? OR substr("+idExpr+", 1, length(?)) = ?)")
		args = append(args, prefix, prefix+"-", prefix+"-")
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "NOT (" + strings.Join(clauses, " OR ") + ")", args
}

// HasResidentOutside reports whether the store holds any bead whose id none of
// prefixes claims. It satisfies NamespaceCensus.
//
// One statement over the beads table, reading the id column and stopping at the
// first match. The table holds both tiers and every status with no discriminator
// applied, so counting all of them is the absence of a filter rather than the
// presence of one — there is no closed-status clause here to accidentally delete.
//
// A NOT-prefix predicate cannot seek, so this is still a scan; what it stops
// being is a hydration. The listing path it replaces decodes every row's
// bead_json into a Bead and builds a slice of the whole history before the
// caller looks at the first id, and on a binding that does hold a relic the
// LIMIT ends the read at the first one.
func (s *SQLiteStore) HasResidentOutside(prefixes []string) (bool, error) {
	if err := s.ensureOpen(); err != nil {
		return false, err
	}
	query := "SELECT 1 FROM beads"
	where, args := namespaceExclusionSQL("COALESCE(id, '')", prefixes)
	if where != "" {
		query += " WHERE " + where
	}
	query += " LIMIT 1"

	var found int
	switch err := s.readDB.QueryRowContext(context.Background(), query, args...).Scan(&found); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("censusing the id namespaces of the sqlite bead store at %s: %w", s.path, err)
	default:
		return true, nil
	}
}
