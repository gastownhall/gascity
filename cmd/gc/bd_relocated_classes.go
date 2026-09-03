package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// relocatedBeadClasses reports the coordination classes a city serves from
// somewhere other than its work ledger, in the form the bd store's SQL guard
// consumes.
//
// It is the companion of resolveClassStore: that function decides which store a
// class is READ from, and this one states the same decision as a fact bd-ledger
// SQL can be checked against. Both derive from one input — the class-to-binding
// assignment in [storage.classes] — so they cannot disagree about which classes
// moved. TestRelocatedBeadClassesAgreeWithClassStoreRouting pins that.
//
// The answer is pure configuration: storageSplitShapeOf reads no filesystem and
// neither does this, so it is the same answer before and after a migration has
// physically moved the beads. That is the property the guard needs — a city
// configured to serve graph from a binding must refuse graph SQL against bd
// whether or not the copy has happened yet.
//
// A city with no [storage] section, or one that leaves every class on the
// reserved work binding, relocates nothing and gets nil. That is the whole of
// the single-store compatibility claim: no relocated classes, no guard.
func relocatedBeadClasses(cfg *config.City) []beads.RelocatedClass {
	if cfg == nil || cfg.Storage == nil {
		return nil
	}
	storage := cfg.EffectiveStorage()
	var relocated []beads.RelocatedClass
	for _, class := range infraMigrationClasses {
		binding := strings.TrimSpace(storage.Classes.BindingFor(class))
		if binding == "" || binding == config.StorageWorkBinding {
			continue
		}
		prefix, ok := config.ReservedClassPrefix(string(class))
		if !ok {
			// A class with no reserved id prefix mints ids indistinguishable
			// from work ids, so a blind read of it is not detectable by id and
			// claiming otherwise would be worse than saying nothing.
			continue
		}
		relocated = append(relocated, beads.RelocatedClass{
			Class:    string(class),
			IDPrefix: prefix,
			Location: relocatedClassLocation(storage, binding),
		})
	}
	return relocated
}

// bdRelocatedClassOverrideEnvVar lets an operator run a refused `gc bd`
// read anyway.
//
// It exists because the scan classifies TEXT, and text is not always decidable:
// a work-ledger query whose value side legitimately holds a relocated id — a
// JSON metadata comparison on gc.drain_control_id, say — is indistinguishable
// from an id-scoped predicate, and bd answers the former correctly and
// non-emptily. Without a knob, the guard boxes an operator out of a ledger they
// can still read, during exactly the incident it was built for.
//
// It is scoped to this one CLI pre-flight on purpose. The store-level guards
// (ReleaseIfCurrent, the ready projection) protect the controller's own
// automated reads, where no human is present to judge and an override would be
// a silent correctness hole. And honoring it is never quiet: doBd prints what
// it is letting through.
//
// Deliberately NOT an internal/rollout gate, which is where the GC_* vocabulary
// ratchet in internal/testenv steers a new env read. A Spec must name two
// mechanical code paths it selects between and must bind to a config.City field
// (Spec.ConfigPath is reflection-verified), and rollout precedence is
// builtin < config < env — so registering this knob means minting a city.toml
// field whose presence disarms the guard for every operator and every later
// invocation. What makes the override safe is that it is per-invocation and
// persists nowhere. GC_WORK_RECORD_ENFORCE is the in-tree precedent for the
// shape: same CLI seam, same truthy switch, same operator-facing scope.
const bdRelocatedClassOverrideEnvVar = "GC_BD_ALLOW_RELOCATED_CLASS_READ"

// bdRelocatedClassOverrideEnabled reports whether the operator has explicitly
// taken responsibility for a read this ledger cannot answer by class.
func bdRelocatedClassOverrideEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(bdRelocatedClassOverrideEnvVar))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// bdRelocatedClassGuardedVerbs are the bd read verbs whose argument text names
// ids in a dialect this guard can classify.
//
// `sql` and `query` are the two ad-hoc ones: both take an expression an
// operator or agent wrote by hand, both resolve it against the bd ledger alone,
// and both answer no-match with an empty result and exit 0.
//
// `list`, `ready` and `search` are here for the same reason and they were the
// ones that took longest to see, because the flag the id arrives through does
// not look like an id position. `gc bd list --metadata-field
// gc.root_bead_id=<gcg root>` answered `[]` with exit 0 on a converged split
// city: --metadata-field is not id-VALUED, so the by-id door in cmd_bd_by_id.go
// correctly declined it (a quoted id decides nothing about ownership), and bd
// then ran the projection successfully against the one ledger that holds no
// gcg- row. The value named an id but the VERB is a PROJECTION over a class
// this ledger cannot see, and a projection that cannot see a class must fail
// loudly rather than answer with the empty set. That is the whole of ga-iaj7k's
// Invariant 0, and it is what makes `list` COHERENT with `dep tree` — which
// answers a relocated id from the store that holds it, rather than emptily from
// the one that does not. Two projections over the same data with opposite
// failure semantics is worse than either one alone, because an operator who
// learned the loud one trusts the quiet one.
//
// `search` is the same projection over the same flag: bd registers
// --metadata-field for it too, and it answers no-match with `[]` and exit 0.
// Guarding `list` alone would have minted the asymmetry it retired — one verb
// over, on the same molecule, through the same selector. It shares `list`'s
// scan because it shares bd's selector dialect, so the negatives that keep a
// free-text search answerable hold for both unchanged.
//
// `ready` takes --metadata-field as well but is NOT here: it is refused by
// topology instead, in bdRelocatedClassBlindVerbs, which strictly subsumes what
// this scan would have caught for it. See there for why. `list` stays here
// because its selectors span this ledger's own rows — except when it carries
// --ready, which is not a selector at all but a switch onto the same frontier
// query, and is refused by the same topology arm.
//
// The other verbs are unguarded because they are no longer blind, not because
// they were ever safe:
//
//   - `show`, `update` (including `--claim`), `release-if-current`, `dep list`
//     and `dep tree` are answered IN PROCESS from the binding their class is
//     served from — cmd_bd_by_id.go, wired into doBd immediately after this
//     scan — so they never reach the subprocess for a class-owned bead.
//   - Spellings of those verbs the in-process arm does not implement — `dep tree
//     --show-all-paths`, `--status`, `--format`, `--direction=both` — are
//     REFUSED there (exit 1, naming the bead and the binding) rather than
//     forwarded, because serving them by dropping the flag would answer a
//     different question than the one asked.
//   - Every other bd subcommand that ADDRESSES a reserved-prefix id — in a
//     positional or an id-valued flag — is refused there too, by ownership
//     rather than by servability.
//
// The selector surface is COMPLETE, and that is checkable rather than hopeful:
// --metadata-field — the only bd flag whose value side is a key=value predicate
// on a read — is registered on exactly three subcommands (list.go, ready.go,
// search.go in the pinned beads module), and every one of them is refused by
// this map or by bdRelocatedClassBlindVerbs.
// TestBdRelocatedClassGuardCoversEverySelectorVerb pins that against bdflags so
// a fourth cannot appear unguarded.
var bdRelocatedClassGuardedVerbs = map[string]bdRelocatedClassScan{
	"sql":    beads.RelocatedClassesInSQL,
	"query":  beads.RelocatedClassesInQueryExpr,
	"list":   beads.RelocatedClassesInSelector,
	"search": beads.RelocatedClassesInSelector,
}

// bdRelocatedClassBlindVerbs are the bd read verbs whose whole RESULT SET omits
// a relocated class, so the trigger is the city's topology and not the argv.
//
// # Why `ready` is here and bare `list` is not
//
// The selector guard above classifies TEXT: it fires when an argument names a
// relocated id namespace in an id-shaped position, because that is the case
// where the empty answer is provably wrong for the predicate that was asked.
// That is the right trigger for `list` and `search`, whose selectors span the
// whole ledger — `bd list --status open` is a question about the rows THIS
// ledger holds and bd answers it correctly.
//
// `ready` is not that shape. It computes a FRONTIER — "the claimable work in
// this store" — and takes no selector that could reach another store, so on a
// city that serves a coordination class from a binding its result is the
// work-class subset of the city's ready set, short by exactly the beads the
// split moved, for every invocation. Measured on a converged split city, `gc
// ready` returned 9 beads including 3 graph-resident ones and matched the API
// id-for-id, while `gc bd ready` returned 5 with none of them, exit 0 — and it
// still returned exit 0 with the relocated binding chmod-000, which is the
// state a loud failure exists for (ga-jbn6f). Scanning argv would have guarded
// the rare `gc bd ready --metadata-field gc.root_bead_id=<gcg root>` and left
// the bare `gc bd ready` — the invocation an operator types, and the one the
// original report measured — answering short and quiet. So the trigger is the
// class assignment in [storage.classes]: a fact about the city, checked before
// any argument is read.
//
// The topology trigger strictly subsumes the selector one for this verb, which
// is why `ready` was removed from bdRelocatedClassGuardedVerbs rather than
// added here alongside it — two triggers for one verb, where one always fires
// first, is a dead branch that reads as coverage.
//
// A city that relocates nothing has no entry to match against, so a
// single-store `gc bd ready` is byte-identical: same argv, same bd binary, same
// exit code. bdSQLRelocatedClassRefusal returns early on an empty relocated
// set, before this map is consulted at all.
//
// Not guarded here: a `ready` hidden behind an unrecognized root flag, which
// bdRelocatedClassVerb cannot locate. That case already fails closed for TEXT
// (every dialect scan runs over the remaining arguments), and bd itself rejects
// the unknown flag before running anything, so the invocation cannot produce a
// short answer either way.
var bdRelocatedClassBlindVerbs = map[string]bool{
	"ready": true,
}

// bdRelocatedClassFrontierFlag is the bd flag that switches a verb which is not
// a frontier verb onto the frontier query anyway.
//
// Refusing the VERB `ready` and answering `bd list --ready` would have been the
// same asymmetry the selector guard exists to retire, one flag over instead of
// one verb over: bd registers --ready on `list` as "Show only ready issues (no
// active blockers, same semantics as bd ready)" (cmd/bd/list.go), and it
// dispatches to the same GetReadyWork store methods `bd ready` calls. So it
// computes the identical short frontier over the identical one ledger, exits 0,
// and does so with the relocated binding unreadable — the state ga-jbn6f exists
// for. An operator refused on `gc bd ready` who retried with `gc bd list
// --ready` would have got the confident short answer back.
//
// Which verbs accept it is derived from bdflags rather than restated, so a verb
// that grows the flag is covered without an edit here, and
// TestBdRelocatedClassGuardCoversEveryFrontierSurface pins that derivation.
const bdRelocatedClassFrontierFlag = "--ready"

// bdRelocatedClassScan classifies one argument's text in one bd dialect.
type bdRelocatedClassScan func([]beads.RelocatedClass, string) []beads.RelocatedClass

// bdRelocatedClassScanText returns the part of an argument a dialect scan
// should read, and whether there is one.
//
// A separated flag value arrives as its own token (`--metadata-field
// gc.root_bead_id=gcg-1`) and is scanned as a positional. The INLINE spelling
// of the same selector (`--metadata-field=gc.root_bead_id=gcg-1`) is one token
// that begins with a dash, and skipping it wholesale — which is what this scan
// used to do — let a single `=` switch the guard off on the exact query it was
// added for. So the flag NAME is dropped and everything after the first `=` is
// scanned, which is the value bd itself parses out of that token.
//
// A flag carrying no value (`--json`, `-q`) has no value text and is skipped,
// which is what keeps `bd sql --json 'select 1'` from classifying its own flags.
func bdRelocatedClassScanText(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "-") {
		return arg, true
	}
	_, value, inline := strings.Cut(arg, "=")
	if !inline {
		return "", false
	}
	return value, true
}

// bdSQLRelocatedClassRefusal reports whether a `gc bd` invocation is an ad-hoc
// read that names the id namespace of a class this city serves elsewhere, and
// returns the operator-facing refusal when it is.
//
// The override is named HERE rather than inside beads.RelocatedClassRefusal
// because this is the only seam where it works: the same error text is returned
// by BdStore's id-scoped guard, which honors no env var, and a message that
// offers an escape the reader cannot take is worse than one that offers none.
// An escape hatch nobody can find is not an escape hatch — the scan classifies
// TEXT, so a false positive is always possible, and the operator holding one
// needs the way out in the message that stopped them.
func bdSQLRelocatedClassRefusal(cfg *config.City, bdArgs []string) (string, bool) {
	relocated := relocatedBeadClasses(cfg)
	if len(relocated) == 0 {
		return "", false
	}
	verb, verbArgs, resolved := bdRelocatedClassVerb(bdArgs)
	// The topology arm runs first and reads no predicate: a frontier read is
	// short by the relocated class whatever selector it was given.
	if op, frontier := bdRelocatedClassFrontierRead(verb, verbArgs, resolved); frontier {
		return beads.RelocatedClassFrontierRefusal(op, relocated).Error(), true
	}
	scans, op := bdRelocatedClassScans(verb, resolved)
	if len(scans) == 0 {
		return "", false
	}
	var matched []beads.RelocatedClass
	seen := make(map[string]bool, len(relocated))
	for _, arg := range verbArgs {
		text, scannable := bdRelocatedClassScanText(arg)
		if !scannable {
			continue
		}
		for _, namedIn := range scans {
			for _, class := range namedIn(relocated, text) {
				if seen[class.Class] {
					continue
				}
				seen[class.Class] = true
				matched = append(matched, class)
			}
		}
	}
	if len(matched) == 0 {
		return "", false
	}
	return beads.RelocatedClassRefusal(op, matched).Error(), true
}

// bdRelocatedClassEscapeHint is the sentence appended to a refusal that is
// actually being ENFORCED, naming the knob that lifts it.
//
// It is not part of beads.RelocatedClassRefusal because that same text is
// returned by BdStore's id-scoped guard, which honors no env var: a message
// that offers an escape its reader cannot take is worse than one that offers
// none. And it is not appended when the override is already set, because there
// the operator is being told what they overrode, not how.
//
// The scan classifies TEXT, so a false positive is always possible — a work-row
// query whose value legitimately holds a relocated id (gc.drain_control_id) is
// indistinguishable from a class-scoped one. An escape hatch nobody can find is
// not an escape hatch, so it travels with the refusal that stopped them.
//
// frontier selects the wording for the topology arm, where there is no argument
// to have misclassified. That refusal is never a false positive — the class
// really is served elsewhere and the frontier really is short — so the sentence
// cannot be about a misread predicate. What it names instead is the narrower
// question the short answer does answer, because a rig-local or one-ledger
// frontier is a thing an operator sometimes wants on purpose.
func bdRelocatedClassEscapeHint(frontier bool) string {
	if frontier {
		return fmt.Sprintf(" If the work-class subset is the answer you want — a deliberate look at this one ledger "+
			"rather than the city's frontier — %s=1 runs it anyway.", bdRelocatedClassOverrideEnvVar)
	}
	return fmt.Sprintf(" If this read is about work rows that merely REFERENCE such an id — a metadata comparison on "+
		"gc.drain_control_id, say — it is a question this ledger can answer, and %s=1 runs it anyway.",
		bdRelocatedClassOverrideEnvVar)
}

// bdRelocatedClassInvocationComputesFrontier reports whether an argv is refused
// by the city's topology rather than by its argument text, so the CLI can print
// the escape hint that matches the arm that fired.
func bdRelocatedClassInvocationComputesFrontier(bdArgs []string) bool {
	verb, verbArgs, resolved := bdRelocatedClassVerb(bdArgs)
	_, frontier := bdRelocatedClassFrontierRead(verb, verbArgs, resolved)
	return frontier
}

// bdRelocatedClassFrontierRead reports whether an invocation computes bd's
// ready frontier — by verb or by flag — and the name the refusal reports the
// read under.
//
// The name carries the flag when the flag is what made it a frontier, because
// an operator who typed `gc bd list --ready` and read a refusal about "bd list"
// would look for a selector they never wrote.
func bdRelocatedClassFrontierRead(verb string, verbArgs []string, resolved bool) (string, bool) {
	if !resolved {
		return "", false
	}
	if bdRelocatedClassBlindVerbs[verb] {
		return "bd " + verb, true
	}
	if !bdflags.BoolFlags(verb)[bdRelocatedClassFrontierFlag] {
		return "", false
	}
	for _, arg := range verbArgs {
		if bdRelocatedClassFrontierFlagIsOn(arg) {
			return "bd " + verb + " " + bdRelocatedClassFrontierFlag, true
		}
	}
	return "", false
}

// bdRelocatedClassFrontierFlagIsOn reports whether one argv token turns the
// frontier flag on.
//
// A bool flag arrives bare (`--ready`) or inline (`--ready=true`), and only the
// inline spelling can turn it OFF — `--ready=false` really does run an ordinary
// ledger query, so refusing it would be a false positive on a selector this
// ledger can answer. A value bd's own flag parser would reject fails CLOSED:
// bd exits before running anything, so the invocation produces no answer to be
// short, and a guard a typo can switch off is not a guard.
func bdRelocatedClassFrontierFlagIsOn(arg string) bool {
	name, value, inline := strings.Cut(arg, "=")
	if name != bdRelocatedClassFrontierFlag {
		return false
	}
	if !inline {
		return true
	}
	on, err := strconv.ParseBool(strings.TrimSpace(value))
	return err != nil || on
}

// bdRelocatedClassScans returns the dialect scans to run over an invocation's
// positional arguments, and the name the refusal reports the read under. An
// unresolved verb runs every scan — see bdRelocatedClassVerb for why the
// ambiguous case fails closed rather than disengaging.
func bdRelocatedClassScans(verb string, resolved bool) ([]bdRelocatedClassScan, string) {
	if !resolved {
		return []bdRelocatedClassScan{beads.RelocatedClassesInSQL, beads.RelocatedClassesInQueryExpr},
			"bd read (subcommand hidden behind an unrecognized flag)"
	}
	if namedIn, guarded := bdRelocatedClassGuardedVerbs[verb]; guarded {
		return []bdRelocatedClassScan{namedIn}, "bd " + verb
	}
	return nil, ""
}

// bdRelocatedClassVerb resolves the bd subcommand in an argv and the arguments
// that follow it.
//
// bd accepts its root flags BEFORE the subcommand (`bd --json sql ...`,
// `bd -C /d query ...`), and `gc bd` forwards argv verbatim — extractBdScopeFlags
// strips only --city/--rig — so indexing bdArgs[0] read a flag token as the verb
// and disarmed this guard on an ordinary invocation of the command it protects.
// bdflags.SplitGlobalFlags is the tree's answer to that hazard and is already
// used by the sibling pre-flight three lines above this one in doBd.
//
// The ambiguous case fails CLOSED. An unrecognized flag may or may not consume
// the next token as its value, so the verb cannot be located; rather than
// disengage, the scan judges every remaining argument. A guard a typo can
// switch off is not a guard, and the cost of the choice is bounded: only text
// that actually names a relocated namespace in an id-shaped position refuses.
func bdRelocatedClassVerb(bdArgs []string) (verb string, verbArgs []string, ok bool) {
	globals := bdflags.GlobalValueFlags()
	bools := bdflags.GlobalBoolFlags()
	for i := 0; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		if !strings.HasPrefix(arg, "-") {
			return arg, bdArgs[i+1:], true
		}
		if strings.IndexByte(arg, '=') >= 0 || bools[arg] {
			continue
		}
		if globals[arg] {
			i++
			continue
		}
		// Unrecognized flag: the verb is undecidable from here on, so scan
		// everything that is left under every dialect this guard knows.
		return "", bdArgs[i+1:], false
	}
	return "", nil, false
}

// relocatedClassLocation describes where a binding serves from, for the
// operator reading a refusal. It reports the configured location rather than
// the opened one so it is available to every process that loads the config,
// including the ones that never open the binding.
func relocatedClassLocation(storage config.StorageConfig, binding string) string {
	where := strings.TrimSpace(configuredBindingLocation(storage.Bindings[binding]))
	provider := strings.TrimSpace(storage.Bindings[binding].Provider)
	switch {
	case where != "" && provider != "":
		return fmt.Sprintf("the %q storage binding (provider %s, %s)", binding, provider, where)
	case where != "":
		return fmt.Sprintf("the %q storage binding (%s)", binding, where)
	default:
		return fmt.Sprintf("the %q storage binding", binding)
	}
}
