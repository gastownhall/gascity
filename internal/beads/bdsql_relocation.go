package beads

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Why this guard exists.
//
// `bd sql` runs against the bd ledger and nothing else. bd has no SQLite
// backend at all — its storage layer is Dolt/embedded-Dolt/dbproxy — and the
// engine a relocated coordination class is served from is never named in
// .beads/metadata.json, so bd cannot know the file exists. A query that names a
// relocated class's beads therefore resolves the workspace metadata, runs
// SUCCESSFULLY against the bd ledger, matches no rows, and returns an empty
// result. Nothing errors, because nothing failed.
//
// That empty answer is indistinguishable from a true negative, and downstream
// it reads as one: a live molecule root reported absent, a full frontier
// reported empty, a held claim reported released. The read has to refuse
// instead, and only gc can make it refuse — bd cannot detect that a class was
// relocated out from under it.
//
// The refusal is deliberately narrow. It fires when a bd-ledger SQL read names
// the id namespace of a class this store does not serve, which is the case
// where the empty answer is provably wrong. A city that relocates nothing
// carries no relocated classes, so nothing here can fire.

// ErrBdSQLClassRelocated is returned when a bd-ledger SQL read targets a
// coordination class that has been relocated to another store. Callers that
// need to distinguish this from a genuine empty result match it with
// errors.Is.
var ErrBdSQLClassRelocated = errors.New("bd sql cannot read a relocated coordination class")

// RelocatedClass names a coordination class whose beads are no longer served
// from a store's bd ledger, and says where they are served from instead.
//
// IDPrefix is the reserved, non-configurable id prefix the relocated class
// mints (graph mints "gcg", messaging "gcm", and so on). It is what makes a
// blind read detectable: an id under a relocated class's reserved prefix can
// never be a row in the bd ledger, so a SQL read that names one is asking the
// wrong store.
type RelocatedClass struct {
	// Class is the coordination class name, e.g. "graph".
	Class string
	// IDPrefix is the reserved bead-id prefix the class mints, without the
	// trailing "-", e.g. "gcg".
	IDPrefix string
	// Location describes where the class is served from, for the operator
	// reading the refusal. Free-form; a binding name and path is typical.
	Location string
}

// matchesID reports whether id falls under this class's reserved namespace.
// The match is exact-or-hyphen, the same shape the API's by-id class routing
// uses, so a prefix never claims an unrelated id that merely starts with the
// same letters.
func (r RelocatedClass) matchesID(id string) bool {
	if r.IDPrefix == "" {
		return false
	}
	return id == r.IDPrefix || strings.HasPrefix(id, r.IDPrefix+"-")
}

// relocatedClassesForIDs returns the relocated classes that own any of ids, in
// declaration order and without duplicates. Empty when nothing is relocated or
// no id belongs to a relocated class.
func relocatedClassesForIDs(relocated []RelocatedClass, ids ...string) []RelocatedClass {
	if len(relocated) == 0 {
		return nil
	}
	var matched []RelocatedClass
	for _, class := range relocated {
		for _, id := range ids {
			if class.matchesID(strings.TrimSpace(id)) {
				matched = append(matched, class)
				break
			}
		}
	}
	return matched
}

// RelocatedClassesInSQL returns the relocated classes whose reserved id
// namespace appears anywhere in a SQL statement — as a literal id, a LIKE
// pattern, an IN list, anything. It is the ad-hoc-query counterpart of the
// id-scoped check: an operator or agent writing SQL by hand names the ids it
// cares about in the text, and that text is the only thing available to
// classify the query before bd answers it confidently and emptily.
//
// The prefix must start at a token boundary and be followed by "-", so "gcg-"
// matches and "mygcg-1" does not.
func RelocatedClassesInSQL(relocated []RelocatedClass, sql string) []RelocatedClass {
	if len(relocated) == 0 || strings.TrimSpace(sql) == "" {
		return nil
	}
	lowered := strings.ToLower(sql)
	var matched []RelocatedClass
	for _, class := range relocated {
		if class.IDPrefix == "" {
			continue
		}
		if containsIDNamespace(lowered, strings.ToLower(class.IDPrefix)+"-") {
			matched = append(matched, class)
		}
	}
	return matched
}

// containsIDNamespace reports whether token appears in text at a boundary that
// makes it the start of a bead id rather than the tail of a longer word.
func containsIDNamespace(text, token string) bool {
	for offset := 0; ; {
		idx := strings.Index(text[offset:], token)
		if idx < 0 {
			return false
		}
		at := offset + idx
		if at == 0 || !isIDBodyByte(text[at-1]) {
			return true
		}
		offset = at + 1
	}
}

// isIDBodyByte reports whether b can appear inside a bead id, which is what
// makes a prefix match a continuation rather than a start.
func isIDBodyByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '_', b == '-':
		return true
	default:
		return false
	}
}

// RelocatedClassRefusal builds the error a blind bd-ledger read returns instead
// of an empty result. op names the read that was refused, so the message says
// which operation stopped rather than only what is wrong.
//
// The message is written for an operator who hit this at 2am with no source in
// front of them: it names the class, the id namespace, where the beads actually
// live, why bd answered emptily rather than failing, and the federated verbs
// that do resolve them.
func RelocatedClassRefusal(op string, matched []RelocatedClass) error {
	if len(matched) == 0 {
		return nil
	}
	sorted := append([]RelocatedClass(nil), matched...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Class < sorted[j].Class })

	parts := make([]string, 0, len(sorted))
	for _, class := range sorted {
		where := strings.TrimSpace(class.Location)
		if where == "" {
			where = "another store"
		}
		parts = append(parts, fmt.Sprintf("%s-class beads (id prefix %q) are served from %s", class.Class, class.IDPrefix+"-", where))
	}

	return fmt.Errorf("%w: %s: %s. bd has no SQLite backend and the relocated store is not named in .beads/metadata.json, "+
		"so this query would run successfully against the bd ledger, match nothing, and return an empty result that is "+
		"indistinguishable from a real one. Use the federated `gc bd show <id>` or `gc bd dep tree <id>` for these beads "+
		"instead of `bd sql`",
		ErrBdSQLClassRelocated, op, strings.Join(parts, "; "))
}
