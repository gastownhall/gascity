package main

import (
	"fmt"
	"os"
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

// bdRelocatedClassGuardedVerbs are the bd read verbs whose positional text
// names ids in a dialect this guard can classify.
//
// `sql` and `query` are the two ad-hoc ones: both take an expression an
// operator or agent wrote by hand, both resolve it against the bd ledger alone,
// and both answer no-match with an empty result and exit 0.
//
// list/ready are left alone — they answer about the ledger they are scoped to
// and claim nothing more. show/dep tree are left alone too, but NOT because
// they are safe: they are raw bd passthroughs against the same blind ledger
// (doBd ends at exec.Command(bdPath, bdArgs...) with no class routing). They
// stay unguarded because a bare id is not a dialect this scan can read without
// refusing every work-store id that starts with the same letters, and because
// the refusal now steers to `gc beads show` instead of to them.
var bdRelocatedClassGuardedVerbs = map[string]bdRelocatedClassScan{
	"sql":   beads.RelocatedClassesInSQL,
	"query": beads.RelocatedClassesInQueryExpr,
}

// bdRelocatedClassScan classifies one positional argument in one bd dialect.
type bdRelocatedClassScan func([]beads.RelocatedClass, string) []beads.RelocatedClass

// bdSQLRelocatedClassRefusal reports whether a `gc bd` invocation is an ad-hoc
// read that names the id namespace of a class this city serves elsewhere, and
// returns the operator-facing refusal when it is.
func bdSQLRelocatedClassRefusal(cfg *config.City, bdArgs []string) (string, bool) {
	relocated := relocatedBeadClasses(cfg)
	if len(relocated) == 0 {
		return "", false
	}
	verb, verbArgs, resolved := bdRelocatedClassVerb(bdArgs)
	scans, op := bdRelocatedClassScans(verb, resolved)
	if len(scans) == 0 {
		return "", false
	}
	var matched []beads.RelocatedClass
	seen := make(map[string]bool, len(relocated))
	for _, arg := range verbArgs {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		for _, namedIn := range scans {
			for _, class := range namedIn(relocated, arg) {
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
