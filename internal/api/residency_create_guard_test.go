package api

// The create seam has to stay the only door.
//
// createStoreForBead is a routing rule, and a routing rule that one call site
// obeys is not a rule — the next handler that mints a raw bead through
// findStore reopens exactly the hole this lane closed, silently, on converged
// cities only. The shared residency-boundary ratchet cannot express this: its
// rows are line regexes over a vocabulary of store access, and "this create
// took placement" is a property of the bead's CLASS, which no line contains.
// So the enforcement is local, named, and allowlisted with reasons.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rawBeadLiteral matches a composite literal carrying at least one field. The
// negated class is what excludes `beads.Bead{}` zero values, which this package
// returns from every error path and which create nothing; without it the guard
// would flag half the package and be turned off.
var rawBeadLiteral = regexp.MustCompile(`beads\.Bead\{([^}]|$)`)

var storeCreateCall = regexp.MustCompile(`\.Create\(`)

// rawBeadCreateRequired is the file the guard exists for. Pinning it keeps a
// detector regression honest: a change that stopped matching composite literals
// would otherwise leave this test passing over an empty set.
const rawBeadCreateRequired = "huma_handlers_beads.go"

// rawBeadCreateExemptions are the non-test files that mint a raw bead outside
// the placement seam, each with the reason it is safe to.
var rawBeadCreateExemptions = map[string]string{
	"huma_handlers_convoys.go": "a convoy bead is ClassWork by construction and must be co-resident with the members it tracks; " +
		"placing it by class would separate a convoy from its own rows",
	"rigidem.go": "a gc-idem record is ClassWork and is written through, and read back only through, the rig store that " +
		"owns the request being deduplicated (see TestIdemRecordsAreWorkClass)",
}

func TestRawBeadCreatesTakeThePlacementSeam(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	found := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		text := string(src)
		if !rawBeadLiteral.MatchString(text) || !storeCreateCall.MatchString(text) {
			continue
		}
		found[name] = true
		if reason, exempt := rawBeadCreateExemptions[name]; exempt {
			t.Logf("%s is exempt: %s", name, reason)
			continue
		}
		if !strings.Contains(text, "createStoreForBead(") {
			t.Errorf("%s builds a raw beads.Bead and calls Create without going through createStoreForBead.\n"+
				"A create picks its store from the bead's class, not from the request's rig: on a converged city "+
				"findStore sends an infrastructure-class bead to the work ledger the city moved that class off, "+
				"minting it under the wrong id prefix. Route it through createStoreForBead, or add %q to "+
				"rawBeadCreateExemptions with the reason it is work-class by construction.", name, name)
		}
	}

	if !found[rawBeadCreateRequired] {
		t.Errorf("the detector no longer matches %s, the create door this guard exists for; "+
			"it is now scanning an empty set and would pass over any new unrouted create", rawBeadCreateRequired)
	}

	// An exemption that no longer names a real create site is worse than no
	// exemption: it silently blesses whatever file later takes the name.
	for name := range rawBeadCreateExemptions {
		if !found[name] {
			t.Errorf("exemption %q no longer builds a raw beads.Bead and calls Create; drop the entry", name)
		}
	}
}
