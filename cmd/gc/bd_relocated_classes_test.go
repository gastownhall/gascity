package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func splitCityConfig() *config.City {
	return &config.City{Storage: &config.StorageConfig{
		Classes: config.StorageClasses{
			Work:      config.StorageWorkBinding,
			Graph:     "infra",
			Sessions:  "infra",
			Messaging: "infra",
			Orders:    "infra",
			Nudges:    "infra",
		},
		Bindings: map[string]config.StorageBindingConfig{
			"infra": {Provider: config.StorageProviderSQLiteBeads, Path: ".gc/store"},
		},
	}}
}

func allWorkCityConfig() *config.City {
	return &config.City{Storage: &config.StorageConfig{Classes: config.StorageClasses{
		Work:      config.StorageWorkBinding,
		Graph:     config.StorageWorkBinding,
		Sessions:  config.StorageWorkBinding,
		Messaging: config.StorageWorkBinding,
		Orders:    config.StorageWorkBinding,
		Nudges:    config.StorageWorkBinding,
	}}}
}

func relocatedClassNames(relocated []beads.RelocatedClass) []string {
	names := make([]string, 0, len(relocated))
	for _, class := range relocated {
		names = append(names, class.Class)
	}
	sort.Strings(names)
	return names
}

// TestRelocatedBeadClassesIsEmptyForASingleStoreCity is the compatibility
// proof, stated as the negative it actually is: no relocated classes means the
// bd store gets no guard option and every SQL read behaves exactly as before.
func TestRelocatedBeadClassesIsEmptyForASingleStoreCity(t *testing.T) {
	for name, cfg := range map[string]*config.City{
		"nil config":            nil,
		"no storage section":    {},
		"everything on work":    allWorkCityConfig(),
		"empty class bindings":  {Storage: &config.StorageConfig{}},
		"beads config only":     {Beads: config.BeadsConfig{Provider: "bd"}},
		"bd 1.0.5 semantics on": {Beads: config.BeadsConfig{BDCompatibility: config.BeadsBDCompatibility105}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := relocatedBeadClasses(cfg); len(got) != 0 {
				t.Fatalf("relocatedBeadClasses = %v, want none", got)
			}
		})
	}
}

func TestRelocatedBeadClassesNamesEverySplitClass(t *testing.T) {
	got := relocatedBeadClasses(splitCityConfig())
	want := []string{"graph", "messaging", "nudges", "orders", "sessions"}
	if names := relocatedClassNames(got); !strings.EqualFold(strings.Join(names, ","), strings.Join(want, ",")) {
		t.Fatalf("relocated classes = %v, want %v", names, want)
	}
	for _, class := range got {
		prefix, ok := config.ReservedClassPrefix(class.Class)
		if !ok || class.IDPrefix != prefix {
			t.Errorf("class %s carries id prefix %q, want the reserved %q", class.Class, class.IDPrefix, prefix)
		}
		for _, want := range []string{`"infra"`, config.StorageProviderSQLiteBeads, ".gc/store"} {
			if !strings.Contains(class.Location, want) {
				t.Errorf("class %s location %q does not name %q", class.Class, class.Location, want)
			}
		}
	}
}

// TestRelocatedBeadClassesAgreeWithClassStoreRouting is the anti-drift pin for
// this whole bug family: a read that moved with its guard left behind is
// exactly the failure being fixed. For every class, the store resolver routing
// it off the work store and relocatedBeadClasses naming it must be the same
// answer, derived from the same [storage.classes] assignment.
func TestRelocatedBeadClassesAgreeWithClassStoreRouting(t *testing.T) {
	for name, cfg := range map[string]*config.City{
		"a split city":       splitCityConfig(),
		"everything on work": allWorkCityConfig(),
		"no storage section": nil,
	} {
		t.Run(name, func(t *testing.T) {
			work := beads.NewMemStore()
			routes := routesForConfig(cfg)

			named := make(map[string]bool)
			for _, class := range relocatedBeadClasses(cfg) {
				named[class.Class] = true
			}
			for _, class := range coordclass.Classes() {
				if class == coordclass.ClassWork {
					continue
				}
				routed := resolveClassStore(routes, work, cfg, t.TempDir(), class.String(), nil) != beads.Store(work)
				if routed != named[class.String()] {
					t.Errorf("class %s: resolveClassStore routes away = %v, relocatedBeadClasses names it = %v", class, routed, named[class.String()])
				}
			}
		})
	}
}

// routesForConfig builds the routes a boot would open for cfg: every class the
// config assigns to a non-work binding maps to that binding's store, which is
// what openStorageRoutes does with the planned assignment.
func routesForConfig(cfg *config.City) *storageRoutes {
	if cfg == nil || cfg.Storage == nil {
		return nil
	}
	storage := cfg.EffectiveStorage()
	stores := make(map[coordclass.Class]beads.Store)
	relocatedStore := beads.NewMemStore()
	for _, class := range infraMigrationClasses {
		binding := storage.Classes.BindingFor(class)
		if binding == "" || binding == config.StorageWorkBinding {
			continue
		}
		stores[coordclassFor(string(class))] = relocatedStore
	}
	if len(stores) == 0 {
		return nil
	}
	return &storageRoutes{stores: stores, binding: "infra"}
}

func TestBdSQLRelocatedClassRefusalOnASplitCity(t *testing.T) {
	split := splitCityConfig()
	for name, tc := range map[string]struct {
		args   []string
		refuse bool
	}{
		"sql naming a graph id":        {[]string{"sql", "select * from issues where id = 'gcg-abc'"}, true},
		"sql with a graph like":        {[]string{"sql", "select id from issues where id like 'gcg-%'", "--json"}, true},
		"sql naming a nudge id":        {[]string{"sql", "select * from issues where id = 'gcn-1'"}, true},
		"sql over the work ledger":     {[]string{"sql", "select id from issues where status <> 'closed'"}, false},
		"sql naming a work id":         {[]string{"sql", "select * from issues where id = 'bd-42'"}, false},
		"show of a graph bead":         {[]string{"show", "gcg-abc"}, false},
		"dep tree of a graph bead":     {[]string{"dep", "tree", "gcg-abc"}, false},
		"list":                         {[]string{"list", "--status", "open"}, false},
		"a flag that looks like an id": {[]string{"sql", "--json", "select 1"}, false},
		"no args":                      {nil, false},

		// The selector dialect: `list`, `ready` and `search` carry key=value
		// predicates whose value side names ids, and a no-match answer is `[]`
		// with exit 0. Both spellings of every selector are covered because a
		// guard a `=` can switch off is not a guard.
		"list on a graph root id":              {[]string{"list", "--metadata-field", "gc.root_bead_id=gcg-abc123"}, true},
		"list on a graph root id inline":       {[]string{"list", "--metadata-field=gc.root_bead_id=gcg-abc123"}, true},
		"list on a graph id behind --json":     {[]string{"list", "--json", "--metadata-field", "gc.root_bead_id=gcg-abc123"}, true},
		"list on a nudge id":                   {[]string{"list", "--metadata-field", "gc.nudge_id=gcn-1"}, true},
		"list on a work root id":               {[]string{"list", "--metadata-field", "gc.root_bead_id=demo-abc"}, false},
		"list with a metadata key only":        {[]string{"list", "--has-metadata-key", "gc.root_bead_id"}, false},
		"list on a prefix continuation":        {[]string{"list", "--metadata-field", "gc.root_bead_id=gcgx-1"}, false},
		"list on a graph id in a rig-scoped q": {[]string{"--json", "list", "--metadata-field", "gc.root_bead_id=gcg-abc123"}, true},

		// A selector flag whose value is SEARCH TEXT is a LIKE-contains over a
		// column this ledger owns, and bd answers it correctly and often
		// non-emptily. `bd list` takes no positionals (cmd/bd/list.go: "bd list
		// does not accept positional arguments"), so EVERY token this scan sees
		// is some flag's value — which is why the dialect anchors on the `=` of
		// a predicate and not on the start of a token. Each row below refused
		// while the scan reused the query DSL's anchor.
		"list on a title search for an id":         {[]string{"list", "--title-contains", "gcg-abc123"}, false},
		"list on a title search for an id inline":  {[]string{"list", "--title-contains=gcg-abc123"}, false},
		"list on a description search for an id":   {[]string{"list", "--desc-contains", "gcg-abc123"}, false},
		"list on a notes search for an id":         {[]string{"list", "--notes-contains", "gcg-abc123"}, false},
		"list on a title match for an id":          {[]string{"list", "--title", "gcg-abc123"}, false},
		"list on a label named for an id":          {[]string{"list", "--label", "gcg-abc123"}, false},
		"list EXCLUDING a label named for an id":   {[]string{"list", "--exclude-label", "gcg-abc123"}, false},
		"list on a label glob":                     {[]string{"list", "--label-pattern", "gcg-*"}, false},
		"list on an assignee named for a class":    {[]string{"list", "--assignee", "gcg-worker"}, false},
		"list on a short assignee flag":            {[]string{"list", "-a", "gcg-worker"}, false},
		"list whose text mentions a graph id":      {[]string{"list", "--title-contains", "fix gcg-1 regression"}, false},
		"list whose text parenthesizes a graph id": {[]string{"list", "--title-contains", "fix (gcg-1) regression"}, false},
		"list whose text lists graph ids":          {[]string{"list", "--title-contains", "regressions: gcg-1, gcg-2"}, false},
		"list whose text quotes a graph id":        {[]string{"list", "--title-contains", "root is 'gcg-1' here"}, false},

		// `ready` and `search` take the same --metadata-field predicate (bd
		// registers it on exactly these three verbs) and answer no-match the
		// same way. Guarding `list` alone left the identical silent empty one
		// verb over, on the same molecule, through the same flag.
		"ready on a graph root id":         {[]string{"ready", "--metadata-field", "gc.root_bead_id=gcg-abc123"}, true},
		"ready on a graph root id inline":  {[]string{"ready", "--metadata-field=gc.root_bead_id=gcg-abc123"}, true},
		"ready on a pool route":            {[]string{"ready", "--metadata-field", "gc.routed_to=demo/worker"}, false},
		"ready on a label named for an id": {[]string{"ready", "--label", "gcg-abc123"}, false},
		"ready with no selector":           {[]string{"ready", "--unassigned", "--json"}, false},
		"search on a graph root id":        {[]string{"search", "--metadata-field", "gc.root_bead_id=gcg-abc123"}, true},
		// `bd search <query>` takes a positional, and free text is a search over
		// this ledger's own columns, so the DIALECT lets it through. It is still
		// refused end-to-end — `search` is not in bdflags, so the by-id door's
		// fail-closed widening treats the positional as an addressed id, exactly
		// as it did before this change.
		"search on free text": {[]string{"search", "gcg-abc123"}, false},

		// A query about the work ledger that merely mentions a relocated id is
		// answered correctly and non-emptily by bd, so it must pass.
		"sql matching work rows that reference a graph id": {[]string{"sql", "select id from issues where metadata like '%gcg-abc%'"}, false},

		// bd root flags are accepted BEFORE the subcommand (beads
		// cmd/bd/main.go persistent flags) and `gc bd` forwards argv verbatim,
		// so keying the guard on bdArgs[0] disarmed it on an ordinary
		// invocation of the one command the guard advertises as protected.
		"leading --json":                             {[]string{"--json", "sql", "select * from issues where id = 'gcg-abc'"}, true},
		"leading -C dir":                             {[]string{"-C", "/tmp/x", "sql", "select * from issues where id = 'gcg-abc'"}, true},
		"leading --actor":                            {[]string{"--actor", "me", "sql", "select * from issues where id = 'gcg-abc'"}, true},
		"leading -q":                                 {[]string{"-q", "sql", "select * from issues where id = 'gcg-abc'"}, true},
		"leading --db inline":                        {[]string{"--db=/tmp/x.db", "sql", "select * from issues where id = 'gcg-abc'"}, true},
		"leading --directory":                        {[]string{"--directory", "/d", "sql", "select * from issues where id = 'gcg-abc'"}, true},
		"stacked leading globals":                    {[]string{"--actor", "bob", "--json", "-C", "/d", "sql", "select * from issues where id = 'gcg-abc'"}, true},
		"a global flag value that looks like a verb": {[]string{"--actor", "sql", "list", "--status", "open"}, false},

		// bd query is the other ad-hoc verb whose text names ids: its DSL
		// pushes id=<v> down to filter.IDs and id=<v>* to an id LIKE '<v>%',
		// against the same ledger, and on no match it prints [] and exits 0.
		"query naming a graph id":            {[]string{"query", "id=gcg-abc123"}, true},
		"query with a graph wildcard":        {[]string{"query", "--json", "id=gcg-*"}, true},
		"query on a graph parent":            {[]string{"query", "parent=gcg-root"}, true},
		"query with spaces around =":         {[]string{"query", "id = gcg-1"}, true},
		"query compound with a graph id":     {[]string{"query", "status=open AND id=gcg-1"}, true},
		"query over the work ledger":         {[]string{"query", "status=open AND priority>1"}, false},
		"query naming a work id":             {[]string{"query", "id=bd-42"}, false},
		"query text merely mentioning an id": {[]string{"query", `title="fix gcg-1 regression"`}, false},
	} {
		t.Run(name, func(t *testing.T) {
			msg, blind := bdSQLRelocatedClassRefusal(split, tc.args)
			if blind != tc.refuse {
				t.Fatalf("bdSQLRelocatedClassRefusal(%v) refused = %v, want %v (%s)", tc.args, blind, tc.refuse, msg)
			}
			if blind && !strings.Contains(msg, "gc beads show <id>") {
				t.Errorf("refusal does not point at the class-routed verb: %s", msg)
			}
		})
	}
}

// TestBdSQLRelocatedClassRefusalFailsClosedOnAnUnrecognizedFlag pins the
// direction the ambiguity resolves in. An unrecognized leading flag may or may
// not consume the next token as its value, so the verb cannot be located; the
// scan then judges every remaining argument rather than disengaging, because a
// guard that a typo can switch off is not a guard.
func TestBdSQLRelocatedClassRefusalFailsClosedOnAnUnrecognizedFlag(t *testing.T) {
	split := splitCityConfig()
	for name, tc := range map[string]struct {
		args   []string
		refuse bool
	}{
		"unknown value flag hides the verb": {[]string{"--frobnicate", "x", "sql", "select * from issues where id = 'gcg-abc'"}, true},
		"unknown flag with no relocated id": {[]string{"--frobnicate", "x", "list", "--status", "open"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			msg, blind := bdSQLRelocatedClassRefusal(split, tc.args)
			if blind != tc.refuse {
				t.Fatalf("bdSQLRelocatedClassRefusal(%v) refused = %v, want %v (%s)", tc.args, blind, tc.refuse, msg)
			}
		})
	}
}

// TestBdSQLRelocatedClassRefusalIsInertOnASingleStoreCity is the mutation proof
// for the agent surface: the exact query a split city refuses passes through
// untouched when nothing has been relocated.
func TestBdSQLRelocatedClassRefusalIsInertOnASingleStoreCity(t *testing.T) {
	guarded := map[string][]string{
		"sql":                    {"sql", "select * from issues where id = 'gcg-abc'"},
		"sql behind a root flag": {"--json", "sql", "select * from issues where id = 'gcg-abc'"},
		"query":                  {"query", "id=gcg-abc"},
		"list":                   {"list", "--metadata-field", "gc.root_bead_id=gcg-abc"},
		"list inline":            {"list", "--metadata-field=gc.root_bead_id=gcg-abc"},
		"list free text":         {"list", "--title-contains", "gcg-abc"},
		"ready":                  {"ready", "--metadata-field", "gc.root_bead_id=gcg-abc"},
		"search":                 {"search", "--metadata-field", "gc.root_bead_id=gcg-abc"},
		"unrecognized flag":      {"--frobnicate", "x", "sql", "select * from issues where id = 'gcg-abc'"},
	}
	for name, cfg := range map[string]*config.City{
		"nil config":         nil,
		"no storage section": {},
		"everything on work": allWorkCityConfig(),
	} {
		t.Run(name, func(t *testing.T) {
			for shape, args := range guarded {
				if msg, blind := bdSQLRelocatedClassRefusal(cfg, args); blind {
					t.Fatalf("single-store city refused %s %v: %s", shape, args, msg)
				}
			}
		})
	}
}

// TestBdStoreOptionsCarryRelocatedClassesOnlyWhenSplit pins the wiring: the one
// choke point every cmd/gc bd store is built through must hand the guard down
// on a split city and add nothing on a single-store one.
func TestBdStoreOptionsCarryRelocatedClassesOnlyWhenSplit(t *testing.T) {
	if got := len(bdStoreOptionsForConfig(allWorkCityConfig())); got != 0 {
		t.Fatalf("single-store city produced %d bd store options, want 0", got)
	}
	base := len(bdStoreOptionsForConfig(allWorkCityConfig()))
	if got := len(bdStoreOptionsForConfig(splitCityConfig())); got != base+1 {
		t.Fatalf("split city produced %d bd store options, want %d", got, base+1)
	}

	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, errBdRunnerShouldNotRun
	}
	store := beads.NewBdStore(t.TempDir(), runner, bdStoreOptionsForConfig(splitCityConfig())...)
	if _, err := store.ReleaseIfCurrent("gcg-abc", "worker-1"); err == nil {
		t.Fatal("a store built from a split city's options did not refuse a graph-class CAS")
	}
}

// errBdRunnerShouldNotRun fails a test that lets a guarded call reach bd.
var errBdRunnerShouldNotRun = errors.New("bd must not run for a refused read")

// bdSQLRefusalCity builds a city whose bd passthrough is fully wired and whose
// storage section is supplied by the caller, so the only difference between the
// split and single-store runs is the relocation itself.
func bdSQLRefusalCity(t *testing.T, storageTOML string) (capture string) {
	t.Helper()

	origCityFlag, origRigFlag, origProbe := cityFlag, rigFlag, bdBeadExists
	t.Cleanup(func() {
		cityFlag, rigFlag, bdBeadExists = origCityFlag, origRigFlag, origProbe
	})
	bdBeadExists = func(string, *config.City, execStoreTarget, string) bool { return false }
	cityFlag, rigFlag = "", ""

	cityDir := t.TempDir()
	writeReachableManagedDoltState(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\nprefix = \"demo\"\n"+storageTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "config.yaml"), []byte(`issue_prefix: demo
gc.endpoint_origin: managed_city
gc.endpoint_status: verified
dolt.auto-start: false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	capture = filepath.Join(t.TempDir(), "bd-invocation.txt")
	// The stub records every invocation and, when BD_STUB_STDOUT is set, answers
	// with that body and exit 0 — which is how a projection's confident empty
	// answer (`[]`, exit 0) is reproduced without a real ledger. It exits 0
	// explicitly so an unset BD_STUB_STDOUT does not leak the test's exit code.
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"${CAPTURE_PATH}\"\nif [ -n \"${BD_STUB_STDOUT}\" ]; then printf '%s\\n' \"${BD_STUB_STDOUT}\"; fi\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CAPTURE_PATH", capture)
	t.Setenv("GC_CITY_PATH", cityDir)
	return capture
}

const bdSQLRefusalSplitStorage = `
[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"
`

// TestGcBdSQLRefusesAGraphClassQueryOnASplitCity is the production incident in
// a test: the query that reported every live molecule root as missing must now
// stop before bd ever answers it.
func TestGcBdSQLRefusesAGraphClassQueryOnASplitCity(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)

	var stdout, stderr bytes.Buffer
	code := doBd([]string{"sql", "select id, status from issues where id = 'gcg-abc123'"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("doBd exited 0 on a graph-blind query; stderr=%q", stderr.String())
	}
	for _, want := range []string{"graph-class beads", `"gcg-"`, "gc beads show <id>", "holds no row under their reserved id prefixes"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal is missing %q; stderr=%q", want, stderr.String())
		}
	}
	if _, err := os.Stat(capture); err == nil {
		t.Fatal("bd was invoked despite the refusal")
	}
}

// TestGcBdSQLRefusesBehindALeadingRootFlagOnASplitCity drives the fail-open
// through the real command. A single leading bd root flag used to make the
// guard return early and hand bd the graph-blind query verbatim.
func TestGcBdSQLRefusesBehindALeadingRootFlagOnASplitCity(t *testing.T) {
	for name, args := range map[string][]string{
		"--json": {"--json", "sql", "select id, status from issues where id = 'gcg-abc123'"},
		"-C":     {"-C", ".", "sql", "select id, status from issues where id = 'gcg-abc123'"},
		"-q":     {"-q", "sql", "select id, status from issues where id = 'gcg-abc123'"},
	} {
		t.Run(name, func(t *testing.T) {
			capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)

			var stdout, stderr bytes.Buffer
			if code := doBd(args, &stdout, &stderr); code == 0 {
				t.Fatalf("doBd(%v) exited 0 on a graph-blind query; stderr=%q", args, stderr.String())
			}
			if _, err := os.Stat(capture); err == nil {
				data, _ := os.ReadFile(capture) //nolint:errcheck // diagnostic only
				t.Fatalf("bd was invoked despite the refusal: %q", data)
			}
		})
	}
}

// TestGcBdQueryRefusesAGraphClassQueryOnASplitCity covers the sibling verb: an
// operator steered off `bd sql` by the refusal lands on `bd query`, whose
// id=<id> filter names the same relocated namespace and whose no-match answer
// is `[]` with exit 0 — the original incident, one word away.
func TestGcBdQueryRefusesAGraphClassQueryOnASplitCity(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"query", "--json", "id=gcg-abc123"}, &stdout, &stderr); code == 0 {
		t.Fatalf("doBd exited 0 on a graph-blind query; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc beads show <id>") {
		t.Errorf("refusal does not point at the class-routed verb; stderr=%q", stderr.String())
	}
	if _, err := os.Stat(capture); err == nil {
		t.Fatal("bd was invoked despite the refusal")
	}
}

// bdListGraphProjection is win-mc-forge's measurement row #2, verbatim: a
// set-returning `gc bd list` whose selector names a graph-class molecule root.
var bdListGraphProjection = []string{"list", "--metadata-field", "gc.root_bead_id=gcg-abc123", "--json"}

// TestGcBdListRefusesAGraphClassProjectionOnASplitCity is the measurement this
// whole program started from, as a test.
//
// On a converged split city the command below answered `[]` with exit 0. Every
// piece of that was working as designed: the guard was scoped to `sql`/`query`,
// --metadata-field is not an id-valued flag so cmd_bd_by_id.go's by-id door
// never fired, and bd ran the projection successfully against the one ledger
// that holds no gcg- row. The value named an id, but the VERB is a projection —
// and a projection that cannot see a class must fail loudly rather than answer
// with the empty set (ga-iaj7k Invariant 0).
//
// The stub answers `[]` and exits 0, so this test fails in exactly the shape the
// live city produced if the guard is removed: a well-formed empty array,
// indistinguishable from "this molecule has no members".
func TestGcBdListRefusesAGraphClassProjectionOnASplitCity(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
	t.Setenv("BD_STUB_STDOUT", "[]")

	var stdout, stderr bytes.Buffer
	code := doBd(bdListGraphProjection, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("`gc bd %s` exited 0 with stdout=%q; that empty array is the silent-empty this refusal exists to remove", strings.Join(bdListGraphProjection, " "), stdout.String())
	}
	for _, want := range []string{"graph-class beads", `"gcg-"`, "gc beads show <id>", "gc ready --metadata-field"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal is missing %q; stderr=%q", want, stderr.String())
		}
	}
	if _, err := os.Stat(capture); err == nil {
		data, _ := os.ReadFile(capture) //nolint:errcheck // diagnostic only
		t.Fatalf("bd was invoked despite the refusal: %q", data)
	}
}

// TestGcBdProjectionsAgreeOnAClassTheyCannotSee is the coherence assertion, and
// it is the reason this fix is not just "one more guarded verb".
//
// `gc bd dep tree <gcg id>` and `gc bd list --metadata-field <k>=<gcg id>` are
// two projections over the same data, asked through the same command, on the
// same city. Before this change they disagreed about what happens when the class
// cannot be seen: dep tree refused with exit 1 while list answered `[]` with exit
// 0. Two failure semantics for one fact is worse than either one alone, because
// an operator who learned the loud one trusts the quiet one.
//
// The assertion is the correspondence, not the wording: BOTH exit non-zero,
// BOTH name the id namespace that cannot be seen AND the binding it is served
// from, and NEITHER reaches the ledger that cannot answer. Those three are what
// an operator needs and what a script can rely on.
//
// The two messages are deliberately not compared verbatim, because they are
// produced by different arms answering different questions and the difference
// is real: `dep tree` is refused by the by-id door, which knows the exact bead
// and reports OWNERSHIP of it, while `list` is refused by the dialect guard,
// which knows only that the query names the namespace and reports that. Pinning
// identical wording would force one arm to say something it does not know.
func TestGcBdProjectionsAgreeOnAClassTheyCannotSee(t *testing.T) {
	for name, args := range map[string][]string{
		"dep tree": {"dep", "tree", "gcg-abc123"},
		"list":     bdListGraphProjection,
	} {
		t.Run(name, func(t *testing.T) {
			capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
			t.Setenv("BD_STUB_STDOUT", "[]")
			resetCLIStorageRoutes(t)
			captureCLIStorageStderr(t)

			var stdout, stderr bytes.Buffer
			if code := doBd(args, &stdout, &stderr); code == 0 {
				t.Fatalf("doBd(%v) exited 0; stdout=%q", args, stdout.String())
			}
			for _, want := range []string{"gcg", "infra"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("the refusal does not name %q — the namespace that cannot be seen and the binding it is served from; stderr=%q", want, stderr.String())
				}
			}
			if data, err := os.ReadFile(capture); err == nil && strings.Contains(string(data), strings.Join(args, " ")) {
				t.Fatalf("the blind projection was forwarded to bd: %q", data)
			}
		})
	}
}

// TestGcBdListIsUnchangedOnASingleStoreCity is the mutation proof for the new
// arm: the exact invocation a split city refuses reaches bd verbatim, and
// answers, when nothing has been relocated.
func TestGcBdListIsUnchangedOnASingleStoreCity(t *testing.T) {
	capture := bdSQLRefusalCity(t, "")
	t.Setenv("BD_STUB_STDOUT", "[]")

	var stdout, stderr bytes.Buffer
	if code := doBd(bdListGraphProjection, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd = %d on a single-store city; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("bd was not invoked on a single-store city: %v", err)
	}
	if !strings.Contains(string(data), strings.Join(bdListGraphProjection, " ")) {
		t.Fatalf("bd received %q, want the projection forwarded verbatim", data)
	}
	if strings.TrimSpace(stdout.String()) != "[]" {
		t.Fatalf("stdout = %q, want bd's own answer passed through untouched", stdout.String())
	}
}

// TestGcBdListOverrideRunsTheProjectionLoudly pins the escape hatch on the arm
// that needs it most.
//
// The work ledger legitimately carries gcg- strings in its metadata —
// ensureDrainUnitConvoy stamps gc.drain_control_id = <graph control id> on a
// convoy coordclass deliberately keeps work-class — so a
// `--metadata-field gc.drain_control_id=gcg-…` projection is a real question
// about real work rows, and indistinguishable from a class-scoped one by text.
// The operator says "I know, run it", and gc says so on stderr rather than
// letting the override be silent.
func TestGcBdListOverrideRunsTheProjectionLoudly(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
	t.Setenv(bdRelocatedClassOverrideEnvVar, "1")
	t.Setenv("BD_STUB_STDOUT", "[]")

	args := []string{"list", "--metadata-field", "gc.drain_control_id=gcg-abc123", "--json"}
	var stdout, stderr bytes.Buffer
	if code := doBd(args, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd = %d with the override set; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("bd was not invoked with the override set: %v", err)
	}
	if !strings.Contains(string(data), strings.Join(args, " ")) {
		t.Fatalf("bd received %q, want the unmodified projection", data)
	}
	if !strings.Contains(stderr.String(), bdRelocatedClassOverrideEnvVar) {
		t.Errorf("override was honored silently; stderr=%q", stderr.String())
	}
}

// TestGcBdListForwardsAFreeTextSearchThatNamesAGraphID is the false-positive
// proof, end to end, through the real command.
//
// `--title-contains gcg-abc123` is `title LIKE '%gcg-abc123%'` over a column
// THIS ledger owns, and the work ledger really does carry gcg- strings — the
// drain-unit convoys minted at internal/dispatch/drain.go title themselves
// after the member they drain. bd answers this correctly and often
// non-emptily, so refusing it is the exact false positive
// internal/beads/bdsql_relocation.go's header promises the anchoring rules
// exist to let through.
//
// The stub answers a NON-EMPTY row on purpose: a refusal here does not merely
// inconvenience an operator, it withholds rows that exist.
func TestGcBdListForwardsAFreeTextSearchThatNamesAGraphID(t *testing.T) {
	const row = `[{"id":"demo-1","title":"drain unit 3 for gcg-abc123"}]`
	for name, args := range map[string][]string{
		"title search":           {"list", "--title-contains", "gcg-abc123", "--json"},
		"title search inline":    {"list", "--title-contains=gcg-abc123", "--json"},
		"notes search":           {"list", "--notes-contains", "gcg-abc123", "--json"},
		"description search":     {"list", "--desc-contains", "gcg-abc123", "--json"},
		"label filter":           {"list", "--label", "gcg-abc123", "--json"},
		"label exclusion":        {"list", "--exclude-label", "gcg-abc123", "--json"},
		"assignee filter":        {"list", "--assignee", "gcg-worker", "--json"},
		"prose with punctuation": {"list", "--title-contains", "fix (gcg-1) regression", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
			t.Setenv("BD_STUB_STDOUT", row)

			var stdout, stderr bytes.Buffer
			if code := doBd(args, &stdout, &stderr); code != 0 {
				t.Fatalf("`gc bd %s` exited %d on a split city; a LIKE-contains over this ledger's own columns is a question bd answers, and refusing it withholds rows that exist. stderr=%q",
					strings.Join(args, " "), code, stderr.String())
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatalf("bd was not invoked for %v: %v", args, err)
			}
			if !strings.Contains(string(data), strings.Join(args, " ")) {
				t.Fatalf("bd received %q, want the search forwarded verbatim", data)
			}
			if strings.TrimSpace(stdout.String()) != row {
				t.Fatalf("stdout = %q, want bd's own rows passed through untouched", stdout.String())
			}
		})
	}
}

// TestGcBdListOnAnIDValuedFlagRefusesByOwnership pins which door answers an
// ADDRESSED id, and it is the reason the selector dialect does not need an
// offset-0 anchor.
//
// `--id`, `--parent` and `-p` are in bdIDValuedFlags, so the by-id door has
// always refused them and names the BEAD and the binding that owns it.
// Anchoring the selector scan at offset 0 to catch them a second time would
// shadow that with the vaguer namespace message — the dialect guard runs first
// — while adding no coverage. So the behavior here must be the ownership
// refusal, byte for byte the same one a legacy build produced.
func TestGcBdListOnAnIDValuedFlagRefusesByOwnership(t *testing.T) {
	for name, args := range map[string][]string{
		"--id":     {"list", "--id", "gcg-abc123", "--json"},
		"--parent": {"list", "--parent", "gcg-abc123", "--json"},
		"-p":       {"list", "-p", "gcg-abc123", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
			t.Setenv("BD_STUB_STDOUT", "[]")
			resetCLIStorageRoutes(t)
			captureCLIStorageStderr(t)

			var stdout, stderr bytes.Buffer
			if code := doBd(args, &stdout, &stderr); code == 0 {
				t.Fatalf("doBd(%v) exited 0; stdout=%q", args, stdout.String())
			}
			if !strings.Contains(stderr.String(), "gcg-abc123 is owned by") {
				t.Errorf("the refusal is not the by-id OWNERSHIP one, so the dialect guard is shadowing a more specific message; stderr=%q", stderr.String())
			}
			// The by-id door resolves the class binding before it refuses, and
			// that resolution censuses the work store through bd. What must not
			// reach bd is the REFUSED argv.
			if data, err := os.ReadFile(capture); err == nil && strings.Contains(string(data), strings.Join(args, " ")) {
				t.Fatalf("the refused invocation was forwarded to bd: %q", data)
			}
		})
	}
}

// TestGcBdReadyRefusesAGraphClassProjectionOnASplitCity closes the asymmetry
// the `list` fix would otherwise have moved one verb over.
//
// `bd ready` takes the same --metadata-field predicate as `bd list` (bd
// registers it on exactly three verbs: list, ready, search) and answers no
// match the same way — `[]`, exit 0. Before this, `gc bd ready --metadata-field
// gc.root_bead_id=<gcg root>` ran that projection against the one ledger that
// holds no gcg- row, on the same molecule where `gc bd list` had just refused,
// and where `gc bd ready --parent <gcg root>` refuses loudly through the by-id
// door. One verb, two opposite failure semantics.
func TestGcBdReadyRefusesAGraphClassProjectionOnASplitCity(t *testing.T) {
	for name, args := range map[string][]string{
		"ready":        {"ready", "--metadata-field", "gc.root_bead_id=gcg-abc123", "--json"},
		"ready inline": {"ready", "--metadata-field=gc.root_bead_id=gcg-abc123", "--json"},
		"search":       {"search", "--metadata-field", "gc.root_bead_id=gcg-abc123", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
			t.Setenv("BD_STUB_STDOUT", "[]")

			var stdout, stderr bytes.Buffer
			if code := doBd(args, &stdout, &stderr); code == 0 {
				t.Fatalf("`gc bd %s` exited 0 with stdout=%q; that empty array is the same silent-empty `gc bd list` refuses on the same molecule",
					strings.Join(args, " "), stdout.String())
			}
			if !strings.Contains(stderr.String(), "graph-class beads") {
				t.Errorf("the refusal does not name the class that cannot be seen; stderr=%q", stderr.String())
			}
			if _, err := os.Stat(capture); err == nil {
				data, _ := os.ReadFile(capture) //nolint:errcheck // diagnostic only
				t.Fatalf("bd was invoked despite the refusal: %q", data)
			}
		})
	}
}

// TestGcBdReadyKeepsAnsweringItsOrdinaryWorkQueries is the other half of the
// ready guard: the work loop must not notice it.
//
// The pool-demand probe and the control dispatcher select on
// `--metadata-field gc.routed_to=<pool>` / `gc.run_target=<route>`, whose
// values are pool template names and never bead ids, and they invoke raw `bd`
// rather than `gc bd` anyway. This pins the argv-level claim: a selector whose
// value carries no reserved prefix is forwarded verbatim.
func TestGcBdReadyKeepsAnsweringItsOrdinaryWorkQueries(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
	t.Setenv("BD_STUB_STDOUT", "[]")

	args := []string{"ready", "--metadata-field", "gc.routed_to=demo/worker", "--unassigned", "--json"}
	var stdout, stderr bytes.Buffer
	if code := doBd(args, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd = %d on an ordinary pool-demand query; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("bd was not invoked for the pool-demand query: %v", err)
	}
	if !strings.Contains(string(data), strings.Join(args, " ")) {
		t.Fatalf("bd received %q, want the work query forwarded verbatim", data)
	}
}

// TestGcBdRefusalNamesTheOverride pins the minor that makes the rest usable: an
// escape hatch nobody can find is not an escape hatch.
//
// The scan classifies TEXT, so a false positive is always possible — the work
// ledger legitimately carries gcg- strings under gc.drain_control_id. An
// operator holding one gets exit 1 and needs the way out in the message that
// stopped them, not in the source. It is appended at the CLI seam rather than
// inside beads.RelocatedClassRefusal because the store-level guard that shares
// that string honors no override.
func TestGcBdRefusalNamesTheOverride(t *testing.T) {
	bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
	t.Setenv("BD_STUB_STDOUT", "[]")

	var stdout, stderr bytes.Buffer
	if code := doBd(bdListGraphProjection, &stdout, &stderr); code == 0 {
		t.Fatalf("doBd exited 0; stdout=%q", stdout.String())
	}
	if !strings.Contains(stderr.String(), bdRelocatedClassOverrideEnvVar) {
		t.Errorf("the refusal never names %s, so the operator holding a false positive has no in-band way out; stderr=%q",
			bdRelocatedClassOverrideEnvVar, stderr.String())
	}
}

// TestBdRelocatedClassGuardCoversEverySelectorVerb is the anti-drift pin for
// the completeness claim on bdRelocatedClassGuardedVerbs.
//
// --metadata-field is the only bd read flag whose value side is a key=value
// predicate, and it is registered on exactly three subcommands. Two of them are
// in bdflags, so this derives the requirement from the manifest rather than
// restating a list: if bd grows a fourth and bdflags picks it up, this fails
// instead of the guard silently covering two thirds of its own surface.
func TestBdRelocatedClassGuardCoversEverySelectorVerb(t *testing.T) {
	for _, sub := range bdflags.Subcommands() {
		if !bdflags.ValueFlags(sub)["--metadata-field"] {
			continue
		}
		if _, guarded := bdRelocatedClassGuardedVerbs[sub]; !guarded {
			t.Errorf("bd %q takes --metadata-field but is not in bdRelocatedClassGuardedVerbs, so `gc bd %s --metadata-field <k>=<relocated id>` answers the empty set against a ledger that cannot hold the rows", sub, sub)
		}
	}
	// bd search is not in bdflags (the manifest is generated from the
	// subcommands gc's own lint check walks), so it is named explicitly.
	if _, guarded := bdRelocatedClassGuardedVerbs["search"]; !guarded {
		t.Error("`search` takes --metadata-field (cmd/bd/search.go) and is not guarded")
	}
}

// TestGcBdDepTreeSplitsOnOwnershipNotOnServability is the fact that made the old
// refusal message wrong, pinned in the shape it now takes.
//
// The message used to end "Use the federated `gc bd show <id>` or `gc bd dep
// tree <id>`". Neither verb was federated: doBd resolved a scope and then
// exec'd the bd binary with the args verbatim, with no coordination-class
// routing anywhere on the path, so following the advice ran the blind read the
// refusal had just prevented.
//
// `dep tree` is still not served in process — this surface implements no tree
// walk — but servability is no longer what decides where it goes. On a
// class-owned id it is refused before the subprocess, because bd would answer
// from the one ledger that cannot hold the bead; on a work id it is forwarded
// verbatim, because that ledger is exactly the right answerer. Both halves are
// asserted together: either alone is satisfied by a surface that stopped
// discriminating.
func TestGcBdDepTreeSplitsOnOwnershipNotOnServability(t *testing.T) {
	t.Run("work id is forwarded verbatim", func(t *testing.T) {
		args := []string{"dep", "tree", "demo-abc123"}
		capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
		resetCLIStorageRoutes(t)
		captureCLIStorageStderr(t)

		var stdout, stderr bytes.Buffer
		if code := doBd(args, &stdout, &stderr); code != 0 {
			t.Fatalf("doBd(%v) = %d; stderr=%q", args, code, stderr.String())
		}
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatalf("bd was not invoked for %v: %v", args, err)
		}
		if !strings.Contains(string(data), strings.Join(args, " ")) {
			t.Fatalf("bd received %q, want the args forwarded verbatim", data)
		}
	})

	t.Run("class-owned id is refused", func(t *testing.T) {
		args := []string{"dep", "tree", "gcg-abc123"}
		capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
		resetCLIStorageRoutes(t)
		captureCLIStorageStderr(t)

		var stdout, stderr bytes.Buffer
		if code := doBd(args, &stdout, &stderr); code == 0 {
			t.Fatalf("doBd(%v) exited 0 for a class-owned id; stdout=%q", args, stdout.String())
		}
		if data, err := os.ReadFile(capture); err == nil && strings.Contains(string(data), strings.Join(args, " ")) {
			t.Fatalf("the class-owned dep tree was forwarded to bd: %q", data)
		}
		if !strings.Contains(stderr.String(), "gc bd dep tree") {
			t.Errorf("the refusal does not name the command the operator ran; stderr=%q", stderr.String())
		}
	})
}

// TestGcBdShowNeverReachesBdForAClassOwnedIDOnASplitCity is the other half, and
// the behavior change this pin used to forbid.
//
// The split city here serves its binding, and it is empty, so the read is a
// genuine absence. The old passthrough answered it from the work ledger and
// exited 0 — a confident wrong answer about a bead that ledger cannot hold. The
// routed surface reports the absence in bd's own shape and never reaches the
// subprocess.
func TestGcBdShowNeverReachesBdForAClassOwnedIDOnASplitCity(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"show", "gcg-abc123"}, &stdout, &stderr); code == 0 {
		t.Fatalf("doBd exited 0 for a class-owned id the binding does not hold; stdout=%q", stdout.String())
	}
	// The capture records every bd invocation this command made, including the
	// convergence check's own census of the work store — which is a read of the
	// ledger it is entitled to read. What must not appear is the operator's
	// read, forwarded verbatim.
	if data, err := os.ReadFile(capture); err == nil && strings.Contains(string(data), "show gcg-abc123") {
		t.Fatalf("the by-ID read was forwarded to bd: %q", data)
	}
	if !strings.Contains(stderr.String(), "gcg-abc123") {
		t.Errorf("the answer does not name the bead; stderr=%q", stderr.String())
	}
}

// bdUnservableStorage is a class arrangement this build refuses outright: a
// partial split, with graph moved and the other four classes left on work. It
// reaches the funnel's refusing store without needing a binding that fails to
// open, so a test can stand in the state where every relocated-class read
// carries a standing refusal.
const bdUnservableStorage = `
[storage.classes]
work = "work"
graph = "infra"
sessions = "work"
messaging = "work"
orders = "work"
nudges = "work"

[storage.bindings.infra]
provider = "sqlite-beads"
path = ".gc/store"
`

// TestGcBdOnARefusedCitySeparatesWorkFromClassOwnedIDs is the boundary of the
// refusal, and it is the regression the first draft of the routed surface
// introduced.
//
// A city this build must not serve resolves every relocated class at a store
// whose operations all return the boot refusal. Probing a WORK id against that
// store returns the refusal too — and reading THAT as "the class binding owns
// this bead" refused every `gc bd` write on the city, including writes to the
// work ledger the refusal explicitly leaves alone. A storage misconfiguration
// must not take a city's work offline.
//
// Both halves are asserted together because either one alone is satisfied by a
// surface that has simply stopped discriminating.
func TestGcBdOnARefusedCitySeparatesWorkFromClassOwnedIDs(t *testing.T) {
	t.Run("work id still reaches bd", func(t *testing.T) {
		capture := bdSQLRefusalCity(t, bdUnservableStorage)
		resetCLIStorageRoutes(t)
		captureCLIStorageStderr(t)

		args := []string{"update", "demo-abc123", "--status", "closed"}
		var stdout, stderr bytes.Buffer
		if code := doBd(args, &stdout, &stderr); code != 0 {
			t.Fatalf("doBd(%v) = %d on a work id; stderr=%q", args, code, stderr.String())
		}
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatalf("bd was not invoked for a work mutation: %v", err)
		}
		if !strings.Contains(string(data), strings.Join(args, " ")) {
			t.Fatalf("bd received %q, want the work mutation forwarded verbatim", data)
		}
	})

	t.Run("class-owned id is refused", func(t *testing.T) {
		capture := bdSQLRefusalCity(t, bdUnservableStorage)
		resetCLIStorageRoutes(t)
		captureCLIStorageStderr(t)

		var stdout, stderr bytes.Buffer
		if code := doBd([]string{"show", "gcg-abc123"}, &stdout, &stderr); code == 0 {
			t.Fatalf("doBd exited 0 for a class-owned id on a city this build must not serve; stdout=%q", stdout.String())
		}
		if data, err := os.ReadFile(capture); err == nil && strings.Contains(string(data), "show gcg-abc123") {
			t.Fatalf("the by-ID read was forwarded to bd: %q", data)
		}
		if !strings.Contains(stderr.String(), storageSupportedTopologyStatement) {
			t.Errorf("the refusal does not carry the reason this city cannot be served; stderr=%q", stderr.String())
		}
	})
}

// TestGcBdSQLOverrideRunsTheQueryLoudly pins the escape hatch. The matcher
// cannot tell an id-scoped predicate from a work-ledger query that legitimately
// references a relocated id in a JSON or text column, so an operator must be
// able to say "I know, run it" — and gc must say so on stderr rather than
// letting the override be silent.
func TestGcBdSQLOverrideRunsTheQueryLoudly(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
	t.Setenv(bdRelocatedClassOverrideEnvVar, "1")

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"sql", "select id from issues where id = 'gcg-abc123'"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd = %d with the override set; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("bd was not invoked with the override set: %v", err)
	}
	if !strings.Contains(string(data), "gcg-abc123") {
		t.Fatalf("bd received %q, want the unmodified query", data)
	}
	if !strings.Contains(stderr.String(), bdRelocatedClassOverrideEnvVar) {
		t.Errorf("override was honored silently; stderr=%q", stderr.String())
	}
}

// TestGcBdSQLIsUnchangedOnASingleStoreCity is the mutation counterpart: the
// same query, the same city, with the [storage] split removed, still reaches bd.
func TestGcBdSQLIsUnchangedOnASingleStoreCity(t *testing.T) {
	capture := bdSQLRefusalCity(t, "")

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"sql", "select id, status from issues where id = 'gcg-abc123'"}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd = %d on a single-store city; stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("bd was not invoked on a single-store city: %v", err)
	}
	if !strings.Contains(string(data), "gcg-abc123") {
		t.Fatalf("bd received %q, want the unmodified query", data)
	}
}
