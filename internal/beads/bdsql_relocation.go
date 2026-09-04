package beads

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Why this guard exists.
//
// `bd sql`, `bd query` and the selector-flag verbs (`bd list`, `bd ready`,
// `bd search`) run against the bd ledger this scope's .beads/ names,
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
// a string literal in SQL, the value side of a comparison in bd's query DSL,
// the value side of a `key=value` selector predicate — which is the case where
// the empty answer is provably wrong. A statement that merely mentions such an
// id somewhere else (a LIKE-contains over a text column, a JSON metadata
// comparison, a comment) is a question about the rows THIS ledger holds, and
// bd answers those correctly and often non-emptily: the
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
	// HeldPrefixes are the reserved prefixes this class's store HOLDS beads
	// under without minting them — a subsystem inside the binding that mints
	// its own ids, like the nudge queue's "gcnq" records inside the nudges
	// store.
	//
	// They are as blind to bd as the mint prefix is, for the same reason: no
	// row of the work ledger carries one, so a read scoped to one runs
	// successfully here and matches nothing. Splitting them from IDPrefix
	// rather than folding them in keeps the operator-facing message able to
	// say which namespace the class MINTS, which is the one an id in the wild
	// usually carries.
	HeldPrefixes []string
	// Location describes where the class is served from, for the operator
	// reading the refusal. Free-form; a binding name and path is typical.
	Location string
}

// namespaces returns every reserved prefix this class's store answers for,
// mint prefix first. It is what every match below scans, so a namespace the
// class holds without minting is guarded exactly as strongly as the one it
// mints — an unguarded held namespace is the same silent empty answer, one
// subsystem in.
func (r RelocatedClass) namespaces() []string {
	out := make([]string, 0, 1+len(r.HeldPrefixes))
	if r.IDPrefix != "" {
		out = append(out, r.IDPrefix)
	}
	for _, held := range r.HeldPrefixes {
		if held != "" {
			out = append(out, held)
		}
	}
	return out
}

// matchesID reports whether id falls under any of this class's reserved
// namespaces. The match is exact-or-hyphen, the same shape the API's by-id
// class routing uses, so a prefix never claims an unrelated id that merely
// starts with the same letters.
func (r RelocatedClass) matchesID(id string) bool {
	for _, prefix := range r.namespaces() {
		if id == prefix || strings.HasPrefix(id, prefix+"-") {
			return true
		}
	}
	return false
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

// RelocatedClassesInSelector is the same scan for one selector argument of the
// flag dialect `bd list`, `bd ready` and `bd search` share —
// `--metadata-field gc.root_bead_id=gcg-abc` and the rest of the key=value
// predicates those verbs accept.
//
// It does NOT share atQueryValueStart with the query DSL, and that is the whole
// of the dialect. In `bd query` one token is a whole expression, so an id at
// offset 0 is a value position (`bd query gcg-1` is a bare term). In this
// dialect the flag has already consumed its own token, so what arrives here is
// one flag's VALUE — and `bd list` accepts no positionals at all, so there is
// no argument that is not some flag's value. Anchoring at offset 0 would
// therefore make EVERY selector value id-shaped: `--title-contains gcg-abc123`
// would be indistinguishable from `--metadata-field gc.root_bead_id=gcg-abc123`
// even though the first is a LIKE-contains over this ledger's own title column,
// which is the first false positive the header above promises to let through.
//
// So the anchor is the `=` of a key=value predicate, and nothing else. That is
// the shape that makes an empty answer provably wrong: the selector asks bd for
// rows whose field EQUALS an id no row here can carry. A whole-token value is a
// search term over a column this ledger owns and passes, wherever the id sits
// in it.
//
// The two id-VALUED selectors that look like a hole — `--id`, `--parent`/`-p` —
// are not one. Ownership of an ADDRESSED id is decided before servability, by
// the by-id door in cmd/gc/cmd_bd_by_id.go, which already refuses them and
// names the bead and its binding rather than only the namespace. Anchoring at
// offset 0 to catch them here would shadow that more specific refusal with a
// vaguer one and buy no coverage.
//
// The dialect is named separately rather than aliased at the call site because
// these verbs are PROJECTIONS, not ad-hoc reads: bd runs one successfully
// against the work ledger and answers `[]` with exit 0 for a class that ledger
// does not serve, which is the same confident empty answer the sql/query guard
// exists for, and naming the dialect keeps that reason at the definition
// instead of in a comment on a map entry.
func RelocatedClassesInSelector(relocated []RelocatedClass, selector string) []RelocatedClass {
	return relocatedClassesNamedIn(relocated, selector, atSelectorValueStart)
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
		for _, prefix := range class.namespaces() {
			if namesIDNamespace(lowered, strings.ToLower(prefix), anchored) {
				matched = append(matched, class)
				break
			}
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

// atSelectorValueStart reports whether at opens the value side of a `key=value`
// predicate inside one selector-flag token.
//
// Quotes are skipped on the way back because a shell can leave them inside the
// token bd parses (`--metadata-field 'gc.root_bead_id="gcg-1"'`), and the value
// is the same value either way. Whitespace is skipped for the same reason it is
// in the query DSL. Everything else — prose, punctuation, the start of the
// token — is not a predicate, so it does not anchor: `fix (gcg-1) regression`,
// `regressions: gcg-1, gcg-2` and a bare `gcg-abc123` are all search text.
func atSelectorValueStart(text string, at int) bool {
	i := at - 1
	for i >= 0 && isSelectorValuePadding(text[i]) {
		i--
	}
	return i >= 0 && text[i] == '='
}

// isSelectorValuePadding reports whether b can sit between a predicate's `=`
// and the value it compares.
func isSelectorValuePadding(b byte) bool {
	switch b {
	case ' ', '\t', '\'', '"', '`':
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
// # And it states what that escape does NOT cover
//
// Steering is only worth printing if the command it names answers the question
// that was refused, and `gc ready` answers a NARROWER one. With no --status it
// issues a ReadyQuery — claimable, unblocked work only — so a molecule whose
// steps are in flight, blocked or done comes back `[]` from the very command
// this message sent the operator to, which is the refused bug one command over.
// --status takes exactly ONE of open, in_progress, blocked, closed
// (readyKnownStatuses); there is no `--all` and no comma list, and `deferred`
// is not selectable at all. So the membership question `bd list --all` answers
// in one invocation has no single federated spelling, and the message says so
// rather than letting the operator discover it as an empty array. Overstating
// the escape is the same failure as the silent empty: a confident answer to a
// question that was not asked.
//
// `gc beads list` is deliberately NOT offered, under the same rule that keeps a
// blind verb out of this message. Its API lane federates, but its fallback lane
// opens the city and rig stores only (openAllConvoyStoresAt) — so on a city
// whose controller is down it would return exactly the confident empty answer
// being refused here, and the operator hitting this at 2am is often hitting it
// BECAUSE the controller is down.
//
// # The override is named by the CLI, not here
//
// GC_BD_ALLOW_RELOCATED_CLASS_READ is a `gc bd` pre-flight knob and applies to
// nothing else. This error is also returned by BdStore's own id-scoped guard,
// where no override exists, so naming it in this shared string would advertise
// an escape that does not work on half the paths that print it. cmd/gc appends
// it where it is true (bdSQLRelocatedClassRefusal).
func RelocatedClassRefusal(op string, matched []RelocatedClass) error {
	if len(matched) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s: %s. This bd ledger does not serve those classes and holds no row under their reserved id "+
		"prefixes, so a read scoped to one cannot match here — and bd does not fail: it runs the read successfully "+
		"against this ledger and returns an empty result indistinguishable from a real one. Read these beads with "+
		"`gc bd show <id>`, which answers a reserved-prefix id in process from the binding its class is served from "+
		"and needs no controller, or with `gc beads show <id>`, which routes by class through the controller API "+
		"(GET /v0/city/{cityName}/bead/{id}) and falls back to a work-store scan when no controller is reachable. "+
		"For a SET of beads rather than one id, `gc ready --metadata-field \"key=value\"` federates the city store, the "+
		"rig stores and the relocated binding as ordered legs and fails loud on a leg it cannot read — but it answers a "+
		"NARROWER question than this read did: with no --status it returns only claimable work, and --status takes "+
		"exactly one of open, in_progress, blocked, closed (no --all, no comma list, and deferred is not selectable), "+
		"so enumerating a molecule's full membership takes one invocation per status and cannot reach a deferred member "+
		"at all. There is no federated equivalent of `bd list --all` yet. "+
		"`gc bd dep tree <id>` is not served in process; on a relocated id it is refused rather than answered from this ledger",
		ErrBdSQLClassRelocated, op, describeRelocatedClasses(matched))
}

// RelocatedClassFrontierRefusal is RelocatedClassRefusal for a verb whose whole
// RESULT SET omits a relocated class, no matter what arguments it is given.
//
// # Why this is a different refusal from the selector one
//
// RelocatedClassRefusal answers a read whose TEXT named a relocated namespace:
// the operator asked about beads that are somewhere else, and the empty answer
// is provably wrong for that predicate. The trigger there is the argv, and it
// has to be — `bd list --title-contains gcg-1` is a question this ledger really
// can answer.
//
// A frontier read has no such predicate to inspect. `bd ready` — and `bd list
// --ready`, which bd documents as the same semantics and dispatches to the same
// store methods — computes "the claimable work in this store" and takes no
// selector that could reach another one, so on a city that serves a
// coordination class elsewhere its answer is the WORK-CLASS SUBSET of the
// city's ready set: short by exactly the beads the split moved, for every
// invocation, including the bare one with no flags at all. Anchoring on argv
// would guard the rare call that happens to name a relocated id and leave the
// common one — the one an operator actually types, and the one the original
// measurement used — answering a confident short list with exit 0. So the
// trigger is the TOPOLOGY: the class assignment in [storage.classes], which is
// a fact about the city and not about the command.
//
// # Why it does not offer a by-id escape
//
// RelocatedClassRefusal steers to `gc bd show <id>` because the read it refused
// named an id. A refused frontier has no id to show — the question was "what is
// ready", and only a federated reader answers it. `gc ready` is that reader:
// ordered over the city store, the rig stores and the relocated binding, and
// loud on a leg it cannot open rather than short by that leg's rows.
//
// The steer states its LIMITS for the same reason RelocatedClassRefusal states
// its own: overstating the escape is the same failure as the silent empty — a
// confident answer to a question that was not asked. `gc ready` is flag
// compatible with the `bd ready` invocation the generated work query builds,
// which is a subset of bd's ready surface, and an operator sent to a command
// that rejects their invocation has been sent to a dead end. The authority for
// what it takes is the flag set `gc ready` actually registers, so cmd/gc's
// TestGcReadySteerDescribesTheFlagsItActuallyAccepts derives both sides from
// cobra and bdflags and fails if this sentence drifts from either.
//
// Like its sibling, this message leaves GC_BD_ALLOW_RELOCATED_CLASS_READ to the
// CLI seam that can honor it.
func RelocatedClassFrontierRefusal(op string, matched []RelocatedClass) error {
	if len(matched) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s: %s. This bd ledger does not serve those classes and holds no row under their reserved id "+
		"prefixes, so the frontier this read computes is the work-class SUBSET of the city's ready set — and bd does not "+
		"fail: it runs the read successfully against this ledger and returns that short list with exit 0, "+
		"indistinguishable from the city's whole ready set. The refusal is decided by the TOPOLOGY and not by the "+
		"arguments, because this read takes no selector that could reach another store: no argv makes its answer "+
		"complete. Use `gc ready`, which federates the city store, the rig stores and the relocated binding as ordered "+
		"legs and fails loud on a leg it cannot read instead of returning a short array. It is flag-compatible with the "+
		"`bd ready` invocation the generated work query builds — NOT with all of `bd ready`: it takes --assignee, "+
		"--unassigned, --metadata-field, --exclude-type, --exclude-label, --sort, --limit, --include-ephemeral, --status "+
		"and --json, and rejects the rest of bd's ready surface (--label, --label-any, --parent, --type, --priority, "+
		"--offset, --has-metadata-key, --mol, --include-deferred, --gated, --claim, and every single-letter shorthand), "+
		"with --sort taking oldest|newest rather than bd's priority|hybrid|oldest. A query only bd's surface can express "+
		"has no federated spelling yet: narrow with --metadata-field, or read the relocated class directly from the "+
		"binding",
		ErrBdSQLClassRelocated, op, describeRelocatedClasses(matched))
}

// describeRelocatedClasses renders the matched classes for an operator: what
// moved, the id namespaces it answers for, and where it is served from now.
// Shared by both refusals so the two cannot describe the same topology
// differently.
//
// A held namespace is named alongside the mint prefix rather than folded into
// it, because the operator reading this is holding an id and needs to see the
// one they are holding. An id under a namespace the class merely holds — a
// "gcnq-" queue record — would otherwise be refused by a message that names
// only "gcn-", which reads as the guard having misfired.
func describeRelocatedClasses(matched []RelocatedClass) string {
	sorted := append([]RelocatedClass(nil), matched...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Class < sorted[j].Class })

	parts := make([]string, 0, len(sorted))
	for _, class := range sorted {
		where := strings.TrimSpace(class.Location)
		if where == "" {
			where = "another store"
		}
		parts = append(parts, fmt.Sprintf("%s-class beads (%s) are served from %s", class.Class, describeIDNamespaces(class), where))
	}
	return strings.Join(parts, "; ")
}

// describeIDNamespaces renders one class's reserved namespaces for a refusal,
// mint prefix first.
func describeIDNamespaces(class RelocatedClass) string {
	spaces := class.namespaces()
	if len(spaces) == 0 {
		return "no reserved id prefix"
	}
	quoted := make([]string, 0, len(spaces))
	for _, prefix := range spaces {
		quoted = append(quoted, fmt.Sprintf("%q", prefix+"-"))
	}
	if len(quoted) == 1 {
		return "id prefix " + quoted[0]
	}
	return "id prefix " + quoted[0] + ", and also held under " + strings.Join(quoted[1:], ", ")
}
