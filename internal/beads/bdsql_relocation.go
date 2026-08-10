package beads

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Why this guard exists.
//
// `bd sql` and `bd query` run against the bd ledger this scope's .beads/ names,
// and nothing else. A relocated coordination class is served from a store that
// ledger's metadata never mentions — a SQLite file bd has no backend for
// (storage: Dolt/embedded-Dolt/dbproxy), or a different beads workspace with a
// metadata.json of its own — so bd cannot know it exists. A read that names a
// relocated class's beads therefore resolves this workspace's metadata, runs
// SUCCESSFULLY against this ledger, matches no rows, and returns an empty
// result. Nothing errors, because nothing failed.
//
// That empty answer is indistinguishable from a true negative, and downstream
// it reads as one: a live molecule root reported absent, a full frontier
// reported empty, a held claim reported released. The read has to refuse
// instead, and only gc can make it refuse — bd cannot detect that a class was
// relocated out from under it.
//
// The refusal is deliberately narrow. It fires when a bd-ledger read puts the
// id namespace of a class this store does not serve in an ID-SHAPED POSITION —
// a string literal in SQL, the value side of a comparison in bd's query DSL —
// which is the case where the empty answer is provably wrong. A statement that
// merely mentions such an id somewhere else (a LIKE-contains over a text
// column, a JSON metadata comparison, a comment) is a question about the rows
// THIS ledger holds, and bd answers those correctly and often non-emptily: the
// work ledger really does carry gcg- strings in its metadata, because
// ensureDrainUnitConvoy stamps gc.drain_control_id = <graph control id> onto a
// convoy coordclass deliberately keeps work-class. Refusing those would be a
// false positive, so the anchoring rules below exist to let them through. A
// city that relocates nothing carries no relocated classes, so nothing here can
// fire at all.

// ErrBdSQLClassRelocated is returned when a bd-ledger read targets a
// coordination class that has been relocated to another store. Callers that
// need to distinguish this from a genuine empty result match it with
// errors.Is.
var ErrBdSQLClassRelocated = errors.New("bd cannot read a relocated coordination class")

// RelocatedClass names a coordination class whose beads are no longer served
// from a store's bd ledger, and says where they are served from instead.
//
// IDPrefix is the reserved, non-configurable id prefix the relocated class
// mints (graph mints "gcg", messaging "gcm", and so on). It is what makes a
// blind read detectable: only the relocated class engine mints under it, and a
// migration preserves the ORIGINAL ids of the rows it copies
// (importInfraSnapshot / CreateWithForeignID), which were minted under the
// HQ/rig prefix. So no row of the bd ledger carries a reserved class prefix,
// before or after a cutover, and a read scoped to one is asking the wrong store.
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
// namespace a SQL statement uses as an ID — a literal id, a LIKE pattern over
// ids, a member of an IN list. It is the ad-hoc-query counterpart of the
// id-scoped check: an operator or agent writing SQL by hand names the ids it
// cares about in the text, and that text is the only thing available to
// classify the query before bd answers it confidently and emptily.
//
// The prefix must open a string literal (or the whole statement), so
// `id = 'gcg-1'`, `id like 'gcg%'` and `id in ('bd-1','gcg-2')` match while
// `metadata like '%gcg-1%'`, `-- see gcg-1` and `'mygcg-1'` do not. Those last
// three are questions about the rows this ledger holds, and bd answers them.
func RelocatedClassesInSQL(relocated []RelocatedClass, sql string) []RelocatedClass {
	return relocatedClassesNamedIn(relocated, sql, atSQLLiteralStart)
}

// RelocatedClassesInQueryExpr is RelocatedClassesInSQL for bd's query DSL
// (`bd query "id=gcg-*"`), which names ids without quoting them. bd parses that
// expression into an IssueFilter and pushes `id=<v>` down to an id equality and
// `id=<v>*` down to `id LIKE '<v>%'` against the same ledger, then prints `[]`
// and exits 0 on no match — the same confident empty answer, one word away from
// the SQL form.
//
// An id is anchored to the value side of a comparison or a grouping token
// (bd's lexer skips whitespace, so `id = gcg-1` is the same query as
// `id=gcg-1`). Text that merely contains an id — `title="fix gcg-1 regression"`
// — is a search over this ledger's own rows and passes.
func RelocatedClassesInQueryExpr(relocated []RelocatedClass, expr string) []RelocatedClass {
	return relocatedClassesNamedIn(relocated, expr, atQueryValueStart)
}

// RelocatedClassesInListSelector is the same scan for one `bd list` selector
// argument — `--metadata-field gc.root_bead_id=gcg-abc`, `--id gcg-abc`, and
// the rest of the key=value predicates bd list accepts.
//
// It shares atQueryValueStart with the query DSL because the two dialects pose
// the same question in the same shape: a comparison whose VALUE side names an
// id. The difference is only where the text comes from — one expression for
// `bd query`, one token per selector flag for `bd list` — so a whole-token
// value (`--id gcg-abc`) anchors at offset 0 and a value quoted inside prose
// (`--title-contains "fix gcg-1 regression"`) does not anchor at all, which is
// exactly the split the anchor rule already draws.
//
// It is named separately rather than aliased at the call site because `list` is
// a PROJECTION, not an ad-hoc read: bd runs it successfully against the work
// ledger and answers `[]` with exit 0 for a class that ledger does not serve,
// which is the same confident empty answer the sql/query guard exists for, and
// naming the dialect keeps that reason at the definition instead of in a
// comment on a map entry.
func RelocatedClassesInListSelector(relocated []RelocatedClass, selector string) []RelocatedClass {
	return relocatedClassesNamedIn(relocated, selector, atQueryValueStart)
}

// relocatedClassesNamedIn is the shared scan. anchored decides what counts as
// an id-shaped position for the dialect being scanned; everything else — the
// case folding, the trailing-boundary rule, the per-class loop — is common.
func relocatedClassesNamedIn(relocated []RelocatedClass, text string, anchored func(string, int) bool) []RelocatedClass {
	if len(relocated) == 0 || strings.TrimSpace(text) == "" {
		return nil
	}
	lowered := strings.ToLower(text)
	var matched []RelocatedClass
	for _, class := range relocated {
		if class.IDPrefix == "" {
			continue
		}
		if namesIDNamespace(lowered, strings.ToLower(class.IDPrefix), anchored) {
			matched = append(matched, class)
		}
	}
	return matched
}

// namesIDNamespace reports whether text uses prefix as the start of a bead id
// at a position anchored deems id-shaped.
func namesIDNamespace(text, prefix string, anchored func(string, int) bool) bool {
	for offset := 0; offset <= len(text)-len(prefix); {
		idx := strings.Index(text[offset:], prefix)
		if idx < 0 {
			return false
		}
		at := offset + idx
		if anchored(text, at) && opensAnIDNamespace(text, at+len(prefix)) {
			return true
		}
		offset = at + 1
	}
	return false
}

// opensAnIDNamespace reports whether what follows a matched prefix continues it
// into that namespace rather than into a longer word. "gcg-abc" and "gcg'" do;
// so do the LIKE patterns that stand in for the same rows ("gcg%", "gcg_%",
// "gcg*"). "gcgabc" does not — that is a different prefix entirely.
func opensAnIDNamespace(text string, at int) bool {
	if at >= len(text) {
		return true
	}
	switch b := text[at]; b {
	case '-', '_':
		return true
	default:
		return !isIDBodyByte(b)
	}
}

// atSQLLiteralStart reports whether at opens a string literal (or the whole
// statement). It is what separates `id = 'gcg-1'` — a predicate that can only
// be answered by the store holding gcg- rows — from `metadata like '%gcg-1%'`,
// a predicate over a column of THIS ledger that happens to contain the id.
func atSQLLiteralStart(text string, at int) bool {
	if at == 0 {
		return true
	}
	switch text[at-1] {
	case '\'', '"', '`':
		return true
	default:
		return false
	}
}

// atQueryValueStart reports whether at opens the value side of a comparison in
// bd's query DSL, whose lexer emits '=', '!=', '<', '<=', '>', '>=', '(' and
// ',' as tokens and skips the whitespace around them.
func atQueryValueStart(text string, at int) bool {
	i := at - 1
	for i >= 0 && (text[i] == ' ' || text[i] == '\t') {
		i--
	}
	if i < 0 {
		return true
	}
	switch text[i] {
	case '=', '<', '>', '!', '(', ',', '\'', '"', '`':
		return true
	default:
		return false
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
// live, why bd answered emptily rather than failing, and the one read verb that
// actually routes by class.
//
// Two things it deliberately does NOT say. It does not claim the refused query
// would have returned nothing — that is false for a statement that references a
// relocated id from a column this ledger does own — only that no row under the
// reserved prefix is here, so an id-scoped predicate naming one cannot match.
// And it does not recommend a verb that answers from this same ledger: doing so
// handed the operator the very bug this refusal exists to report.
//
// Which verbs those are is a fact about gc's by-ID routing and changed once.
// `gc bd show <id>` and `gc bd dep list <id>` are now answered in process from
// the binding the class is served from (cmd/gc/cmd_bd_by_id.go), so they are
// the read this message names first — they need no controller, which the API
// lane does. `gc bd dep tree <id>` is not served there, and on a relocated id
// that surface refuses it rather than forwarding it, so it is named as
// unavailable rather than offered as an escape.
//
// The set-returning escape is named too, because a by-ID read is no answer at
// all to a refused PROJECTION: an operator listing a molecule's members by
// gc.root_bead_id has no single id to show. `gc ready` federates the city
// store, the rig stores and the relocated binding as ordered legs and fails
// loud on any leg it cannot read (cmd/gc/ready_federation.go), so it answers a
// class-scoped question without a controller.
//
// `gc beads list` is deliberately NOT offered, under the same rule that keeps a
// blind verb out of this message. Its API lane federates, but its fallback lane
// opens the city and rig stores only (openAllConvoyStoresAt) — so on a city
// whose controller is down it would return exactly the confident empty answer
// being refused here, and the operator hitting this at 2am is often hitting it
// BECAUSE the controller is down.
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

	return fmt.Errorf("%w: %s: %s. This bd ledger does not serve those classes and holds no row under their reserved id "+
		"prefixes, so a read scoped to one cannot match here — and bd does not fail: it runs the read successfully "+
		"against this ledger and returns an empty result indistinguishable from a real one. Read these beads with "+
		"`gc bd show <id>`, which answers a reserved-prefix id in process from the binding its class is served from "+
		"and needs no controller, or with `gc beads show <id>`, which routes by class through the controller API "+
		"(GET /v0/city/{cityName}/bead/{id}) and falls back to a work-store scan when no controller is reachable. "+
		"For a SET of beads rather than one id, use `gc ready --metadata-field \"key=value\" [--status <status>]`, "+
		"which federates the city store, the rig stores and the relocated binding as ordered legs and fails loud on a "+
		"leg it cannot read. "+
		"`gc bd dep tree <id>` is not served in process; on a relocated id it is refused rather than answered from this ledger",
		ErrBdSQLClassRelocated, op, strings.Join(parts, "; "))
}
