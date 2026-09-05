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
// Callers discover it through NamespaceCensusFor — never a bare type assertion,
// see there — and fall back to listing the store and applying the rule
// themselves, which is always available and merely slower.
type NamespaceCensus interface {
	HasResidentOutside(prefixes []string) (bool, error)
}

var _ NamespaceCensus = (*SQLiteStore)(nil)

// ErrNamespaceCensusUnsupported reports a census asked of a store that cannot
// answer it. It is what a wrapper carrying HasResidentOutside structurally
// returns when its backing has no census: the one answer that is neither a
// verdict nor a lie, and the caller reads it the same way it reads any other
// failed census — keep the probe.
var ErrNamespaceCensusUnsupported = errors.New("namespace census unsupported")

// NamespaceCensusHandleProvider lets a wrapper expose the census capability of
// its backing without claiming it when that backing cannot answer.
//
// A wrapper needs this precisely when something else forces it to carry
// HasResidentOutside structurally — cmd/gc's emitting class store is held to
// every engine method by TestEmittingClassStoreKeepsEveryEngineCapability — so
// the method's presence stops being evidence that an answer exists behind it.
type NamespaceCensusHandleProvider interface {
	NamespaceCensusHandle() (NamespaceCensus, bool)
}

// NamespaceCensusFor returns store's census capability when one is really
// there. Callers MUST discover the capability through this rather than
// asserting NamespaceCensus directly.
//
// The provider is consulted BEFORE the plain assertion, which is the opposite
// of GraphApplyFor's order and the same as AtomicConditionalCloserFor's, for
// the same reason as there: a wrapper that carries the method structurally
// satisfies the plain assertion too, so an assertion-first lookup would return
// the wrapper's method and never reach the handle that knows whether the
// backing can honor it. Discovery here is a hard capability gate — answering
// FALSE strands every bead the answer failed to see — so the honest "no" has
// to win.
//
// Unlike AtomicConditionalCloserFor this does NOT follow
// ConditionalWritesResolveTarget. That seam is declared for one capability and
// says so (internal/beads/class_store.go: "the one optional capability where
// forgetting the unwrap would not fail loudly but silently resolve
// unset→legacy (fatal under require). All other optional capabilities keep the
// assert-on-.Store convention"), and the asymmetry that earns it does not exist
// here: a conditional writer that goes undiscovered collapses to a legacy
// write, while a census that goes undiscovered falls back to the scan, which
// returns the same verdict a little slower. Borrowing the follow would let a
// wrapper's read-shaping decisions be stepped around by a question about the
// wrapper's contents, to buy nothing but speed.
func NamespaceCensusFor(store Store) (NamespaceCensus, bool) {
	if store == nil {
		return nil, false
	}
	if provider, ok := store.(NamespaceCensusHandleProvider); ok {
		return provider.NamespaceCensusHandle()
	}
	census, ok := store.(NamespaceCensus)
	return census, ok
}

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
// wildcards. Both callers execute this on SQLite — the DoltLite read store
// included, which opens the pure-Go sqlite driver — and there length() counts
// in the same units substr() indexes in, so the comparison stays exact whatever
// the prefix encodes. That pairing is engine-specific rather than universal: on
// a MySQL-executed engine LENGTH() is bytes while SUBSTR() indexes characters,
// so a third caller running this against the native Dolt engine has to spell it
// CHAR_LENGTH() (mc-wuwe). Every prefix bound today is an ASCII reserved-class
// constant, where the two agree either way.
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
