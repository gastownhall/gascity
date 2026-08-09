package main

import (
	"fmt"
	"strings"

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
// moved. TestRelocatedBeadClassesAgreeWithStorageRoutes pins that.
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

// bdSQLRelocatedClassRefusal reports whether a `gc bd` invocation is an ad-hoc
// SQL read that names the id namespace of a class this city serves elsewhere,
// and returns the operator-facing refusal when it is.
//
// Only the sql subcommand is examined. list/ready answer about the ledger they
// are scoped to and do not claim otherwise, and show/dep tree are the federated
// verbs the refusal recommends — guarding those would break the escape hatch.
func bdSQLRelocatedClassRefusal(cfg *config.City, bdArgs []string) (string, bool) {
	if len(bdArgs) == 0 || bdArgs[0] != "sql" {
		return "", false
	}
	relocated := relocatedBeadClasses(cfg)
	if len(relocated) == 0 {
		return "", false
	}
	var matched []beads.RelocatedClass
	seen := make(map[string]bool, len(relocated))
	for _, arg := range bdArgs[1:] {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		for _, class := range beads.RelocatedClassesInSQL(relocated, arg) {
			if seen[class.Class] {
				continue
			}
			seen[class.Class] = true
			matched = append(matched, class)
		}
	}
	if len(matched) == 0 {
		return "", false
	}
	return beads.RelocatedClassRefusal("bd sql", matched).Error(), true
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
