package storeclass

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	gascity = "github.com/gastownhall/gascity/"
	beads   = gascity + "internal/beads"
)

// MainRepoDataEnv names the files rules_go hands a main-repository nogo
// action (third_party/patches/rules_go_nogo_main_repo_data.patch), comma
// separated, relative to the execution root. The baseline is one of them
// rather than part of the nogo binary, so editing it re-lints only gascity
// packages.
const MainRepoDataEnv = "GC_NOGO_MAIN_REPO_DATA"

// LoadBaselineFromEnv reads the baseline from the MainRepoDataEnv file
// ending in storeclass/baseline.txt.
func LoadBaselineFromEnv() (map[Key]int, error) {
	files := os.Getenv(MainRepoDataEnv)
	if files == "" {
		return nil, errors.New("storeclass: no baseline: " + MainRepoDataEnv + " is unset (nogo must run through the patched rules_go)")
	}
	for _, f := range strings.Split(files, ",") {
		if strings.HasSuffix(f, "storeclass/baseline.txt") {
			text, err := os.ReadFile(f)
			if err != nil {
				return nil, fmt.Errorf("storeclass: reading the baseline: %w", err)
			}
			rows, err := ParseBaseline(string(text))
			if err != nil {
				return nil, fmt.Errorf("storeclass: %s: %w", f, err)
			}
			return rows, nil
		}
	}
	return nil, fmt.Errorf("storeclass: no storeclass/baseline.txt among %s=%s", MainRepoDataEnv, files)
}

// Default is the gascity configuration nogo runs.
var Default = Config{
	Module:      gascity,
	Store:       beads + ".Store",
	HandleField: "Store",
	Handles: []string{
		beads + ".WorkStore",
		beads + ".GraphStore",
		beads + ".SessionStore",
		beads + ".MailStore",
		beads + ".OrdersStore",
		beads + ".NudgesStore",
	},
	UnwrapMethods: []string{"Raw", "ConditionalWritesResolveTarget"},
	AsFuncs:       []string{beads + ".As"},
	// The mint lands with the router (T2): storeref.NewStores is its only
	// production caller. Until then the handles are minted by literal, and
	// every literal outside the allowlist is baselined.
	MintFuncs: []string{beads + ".MintClassStores"},
	TestMintPackages: []string{
		beads + "/beadstest",
		beads + "/splittest",
	},
	// D9: the split-strict gate's markers (#8335) are frozen. A call site
	// lands only as a reviewed baseline row, and as an entry in the gate's
	// own reviewedMarkers table (the package doc's two ledgers); T7 replaces
	// them with plan roles.
	Markers: []string{
		beads + ".FederatedRead",
		beads + ".FederatedGet",
		beads + ".RelicRead",
		beads + ".RelicGet",
	},
	// Each package is named: a new one under internal/beads or
	// internal/storebinding is linted until an entry here, a change the
	// growth gate refuses without the label, exempts it.
	Allow: []Allow{
		{Package: "internal/beads", Rules: AllRulesBut(TestMintImport), Reason: beadsEngines},
		{Package: "internal/beads/closeorder", Rules: AllRulesBut(TestMintImport), Reason: beadsEngines},
		{Package: "internal/beads/contract", Rules: AllRulesBut(TestMintImport), Reason: beadsEngines},
		{Package: "internal/beads/exec", Rules: AllRulesBut(TestMintImport), Reason: beadsEngines},
		{Package: "internal/beads/ledgerfeed", Rules: AllRulesBut(TestMintImport), Reason: beadsEngines},
		{Package: "internal/beads/proxyendpoint", Rules: AllRulesBut(TestMintImport), Reason: beadsEngines},
		{Package: "internal/beads/proxyendpoint/proxyendpointtest", Rules: AllRulesBut(TestMintImport), Reason: beadsEngines},
		{Package: "internal/beads/beadstest", Reason: "the test minting helpers: test-only, so they may mint, and use the other test mint"},
		{Package: "internal/beads/splittest", Reason: "the split-city test helpers: test-only, so they may mint, and use the other test mint"},
		{Package: "internal/storebinding", Reason: storeBindings},
		{Package: "internal/storebinding/beadsworkspace", Reason: storeBindings},
		{Package: "internal/storebinding/sqlite", Reason: storeBindings},
		{Package: "internal/storebinding/storebindingtest", Reason: storeBindings},
		{Package: "internal/storeref", Rules: AllRulesBut(CapAssert), Reason: "the residency resolver: the mint (T2) and the plan executors that run legs over physical stores. A capability assertion here still goes through beads.As"},
		{Package: "cmd/gc", File: "storage_boot.go", Reason: "storage-class routing at boot: opens and holds the physical engines the router later wraps"},
		{Package: "cmd/gc", File: "infra_class_migrate.go", Rules: rawRules, Reason: "the storage-class migration: copies a class's rows between physical stores, raw by nature (S5)"},
		{Package: "cmd/gc", File: "infra_class_clear.go", Rules: rawRules, Reason: "the migration's clear: deletes migrated rows from the source physical store"},
		{Package: "cmd/gc", File: "infra_class_recover.go", Rules: rawRules, Reason: "the stranded-write recovery: moves rows between physical stores"},
		{Package: "cmd/gc", File: "infra_class_recover_held.go", Rules: rawRules, Reason: "the stranded-write recovery for held rows: moves rows between physical stores"},
		{Package: "cmd/gc", File: "infra_class_relower.go", Rules: rawRules, Reason: "the reverse migration: copies rows back to the work store"},
	},
	// Sealed packages have no baseline rows: a phase ends by adding its
	// package here, and the list only grows (TestSealedOnlyGrows).
	Sealed: []string{
		"internal/workladder",
	},
	LoadBaseline: LoadBaselineFromEnv,
}

// rawRules are the raw-signature rules: the migration engine takes physical
// stores, but it still mints, unwraps and asserts only through the baseline.
var rawRules = []Rule{RawParam, RawResult, RawField, RawVar, RawType}

// The reasons the allowlist's package entries share.
const (
	beadsEngines  = "the engines, the typed handles and the capability helpers; a production file here may not import a test mint either"
	storeBindings = "the binding engines and the migration copy and clear: they move rows between physical stores, raw by nature"
)

// Analyzer runs the rules with Default.
var Analyzer = New(Default)
