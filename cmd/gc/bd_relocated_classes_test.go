package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

		// `search` takes the same --metadata-field predicate as `list` (bd
		// registers it on exactly three verbs: list, ready, search) and answers
		// no-match the same way. Guarding `list` alone left the identical silent
		// empty one verb over, on the same molecule, through the same flag.
		//
		// `ready` is the third and is NOT in this table: it is refused by the
		// city's topology rather than by its argument text, so every argv is
		// refused and none of them exercises this dialect. Its rows live in
		// TestBdRelocatedClassFrontierRefusalIsDecidedByTopology.
		"search on a graph root id": {[]string{"search", "--metadata-field", "gc.root_bead_id=gcg-abc123"}, true},
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

// TestBdRelocatedClassFrontierRefusalIsDecidedByTopology is the unit-level pin
// on the trigger ga-jbn6f turns on.
//
// Every row is the SAME verb with a different argument, and the answer is the
// same for all of them, which is the whole claim: `bd ready` computes a
// frontier over one ledger and takes no selector that could reach another, so
// what makes its answer short is the class assignment in [storage.classes], not
// anything the operator typed. The rows that carry no reserved prefix at all —
// bare `ready`, the pool-demand selector, a limit — are the ones the argv scan
// provably could not have caught, and they are the ones the live city measured.
//
// The `search` row is the control: it takes the same --metadata-field flag but
// is a projection over this ledger's own rows, so it stays on the argv trigger
// and a work-class selector still passes.
func TestBdRelocatedClassFrontierRefusalIsDecidedByTopology(t *testing.T) {
	split := splitCityConfig()
	for name, tc := range map[string]struct {
		args   []string
		refuse bool
	}{
		"bare ready":                         {[]string{"ready"}, true},
		"ready --json":                       {[]string{"ready", "--json"}, true},
		"ready unassigned":                   {[]string{"ready", "--unassigned", "--json"}, true},
		"ready on a pool route":              {[]string{"ready", "--metadata-field", "gc.routed_to=demo/worker"}, true},
		"ready on a graph root id":           {[]string{"ready", "--metadata-field", "gc.root_bead_id=gcg-abc123"}, true},
		"ready on a graph root id inline":    {[]string{"ready", "--metadata-field=gc.root_bead_id=gcg-abc123"}, true},
		"ready on a label named for an id":   {[]string{"ready", "--label", "gcg-abc123"}, true},
		"ready with a limit":                 {[]string{"ready", "--limit", "1"}, true},
		"ready behind a bd root flag":        {[]string{"--json", "ready"}, true},
		"ready behind a valued root flag":    {[]string{"-C", "/tmp/x", "ready"}, true},
		"search on a work-class selector":    {[]string{"search", "--metadata-field", "gc.routed_to=demo/worker"}, false},
		"list on a work-class selector":      {[]string{"list", "--metadata-field", "gc.routed_to=demo/worker"}, false},
		"a root flag value spelling 'ready'": {[]string{"--actor", "ready", "list", "--status", "open"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			msg, blind := bdSQLRelocatedClassRefusal(split, tc.args)
			if blind != tc.refuse {
				t.Fatalf("bdSQLRelocatedClassRefusal(%v) refused = %v, want %v (%s)", tc.args, blind, tc.refuse, msg)
			}
			if !blind {
				return
			}
			if !strings.Contains(msg, "graph-class beads") {
				t.Errorf("the refusal does not name the class that cannot be seen: %s", msg)
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
		"ready bare":             {"ready"},
		"ready json":             {"ready", "--unassigned", "--json"},
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
	capture, _ = bdSQLRefusalCityDir(t, storageTOML)
	return capture
}

// bdSQLRefusalCityDir is bdSQLRefusalCity plus the city root, for the rows that
// have to seed the relocated binding before they can address a bead it HOLDS.
func bdSQLRefusalCityDir(t *testing.T, storageTOML string) (capture, cityDir string) {
	t.Helper()

	origCityFlag, origRigFlag, origProbe := cityFlag, rigFlag, bdBeadExists
	t.Cleanup(func() {
		cityFlag, rigFlag, bdBeadExists = origCityFlag, origRigFlag, origProbe
	})
	bdBeadExists = func(string, *config.City, execStoreTarget, string) bool { return false }
	cityFlag, rigFlag = "", ""

	cityDir = t.TempDir()
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
	return capture, cityDir
}

// seedRelocatedBindingBead plants a bead in the split city's relocated class
// binding and returns it, minted by the binding's own sequence so it carries the
// reserved prefix production would give it.
//
// Every other fixture in this file leaves that binding EMPTY, which is enough to
// prove a refusal fires but not enough to prove what it fires ON: against an
// empty binding "the binding holds this bead" and "the token carries a reserved
// prefix" are the same observation. A row that means to pin ownership has to
// address a bead that is actually there.
func seedRelocatedBindingBead(t *testing.T, cityDir, title string) beads.Bead {
	t.Helper()
	binding, relocated, err := cliSoleClassBinding(cityDir)
	if err != nil {
		t.Fatalf("resolving the relocated class binding at %s: %v", cityDir, err)
	}
	if !relocated {
		t.Fatalf("the split city at %s resolved no relocated class binding", cityDir)
	}
	created, err := binding.Store.Create(beads.Bead{Title: title, Type: "task"})
	if err != nil {
		t.Fatalf("seeding the relocated class binding: %v", err)
	}
	if !bdIDIsClassReserved(created.ID) {
		t.Fatalf("the binding minted %q, which is outside every reserved class namespace", created.ID)
	}
	return created
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
// `gc bd list` and `gc bd search`, both carrying `--metadata-field
// <k>=<gcg id>`, are two projections over the same data, asked through the same
// command, on the same city. The failure this pins is disagreement about what
// happens when the class cannot be seen — one refusing with exit 1 while the
// other answers `[]` with exit 0. Two failure semantics for one fact is worse
// than either one alone, because an operator who learned the loud one trusts the
// quiet one.
//
// The assertion is the correspondence, not the wording: BOTH exit non-zero,
// BOTH name the id namespace that cannot be seen AND the binding it is served
// from, and NEITHER reaches the ledger that cannot answer. Those three are what
// an operator needs and what a script can rely on.
//
// This pin used to pair `list` against `gc bd dep tree <gcg id>`, whose refusal
// was the loud arm the quiet one had to be squared with. ga-pxppl squared it the
// other way: dep tree is now ANSWERED in process from the binding the class is
// served from, so it is no longer a projection that cannot see the class and has
// no place in this correspondence. `search` takes its row because it is blind for
// the same reason `list` is and shares its scan. The coherence claim that spans
// both outcomes — answer from the owning store, or refuse; never a quiet `[]` —
// is conformanceProjectionCoherence (I14).
func TestGcBdProjectionsAgreeOnAClassTheyCannotSee(t *testing.T) {
	for name, args := range map[string][]string{
		"list":   bdListGraphProjection,
		"search": {"search", "--metadata-field", "gc.root_bead_id=gcg-abc123", "--json"},
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
// `--id`, `--parent` and `-p` are in bdIDValuedFlags, so the by-id door — not
// the dialect guard — is what answers them, and it names the BEAD and the
// binding that owns it. Anchoring the selector scan at offset 0 to catch them a
// second time would shadow that with the vaguer namespace message while adding
// no coverage.
//
// Ownership here is RESIDENCE, so the refused id is one the binding was seeded
// with. The second half drives the same three flags at a reserved id the
// binding does not hold: the binding is the namespace's authority, not its only
// lawful holder, so that id belongs to whatever ledger the passthrough is
// pointed at and the argv must reach bd unchanged. Both halves run together —
// either alone is satisfied by a door that stopped discriminating.
func TestGcBdListOnAnIDValuedFlagRefusesByOwnership(t *testing.T) {
	for name, flag := range map[string]string{
		"--id":     "--id",
		"--parent": "--parent",
		"-p":       "-p",
	} {
		t.Run(name, func(t *testing.T) {
			capture, cityDir := bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage)
			t.Setenv("BD_STUB_STDOUT", "[]")
			resetCLIStorageRoutes(t)
			captureCLIStorageStderr(t)
			resident := seedRelocatedBindingBead(t, cityDir, "held by the binding")

			args := []string{"list", flag, resident.ID, "--json"}
			var stdout, stderr bytes.Buffer
			if code := doBd(args, &stdout, &stderr); code == 0 {
				t.Fatalf("doBd(%v) exited 0; stdout=%q", args, stdout.String())
			}
			if !strings.Contains(stderr.String(), resident.ID+" is owned by") {
				t.Errorf("the refusal is not the by-id OWNERSHIP one, so the dialect guard is shadowing a more specific message; stderr=%q", stderr.String())
			}
			// The by-id door resolves the class binding before it refuses, and
			// that resolution censuses the work store through bd. What must not
			// reach bd is the REFUSED argv.
			if data, err := os.ReadFile(capture); err == nil && strings.Contains(string(data), strings.Join(args, " ")) {
				t.Fatalf("the refused invocation was forwarded to bd: %q", data)
			}

			missing := []string{"list", flag, "gcg-abc123", "--json"}
			stdout.Reset()
			stderr.Reset()
			if code := doBd(missing, &stdout, &stderr); code != 0 {
				t.Fatalf("doBd(%v) exited %d for a reserved id the binding does not hold; stderr=%q", missing, code, stderr.String())
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatalf("bd was not invoked for %v: %v", missing, err)
			}
			if !strings.Contains(string(data), strings.Join(missing, " ")) {
				t.Fatalf("bd received %q, want %v forwarded verbatim", data, missing)
			}
		})
	}
}

// TestGcBdHeartbeatOnARelocatedClassIDRefusesByOwnership pins the second half of
// PR #5213's honest-claim contract. `gc bd heartbeat` now forwards to bd's native
// owner-only lease-refresh verb (commit 80aad8c), a literal spelling the by-id door
// does not serve. On a split city where a reserved class is relocated, that
// unserved verb addressed at a class-owned id must NOT fall through to the work
// store — where the bead does not live and bd answers a misleading substring
// not-found. Instead the ownership gate (bdArgsNameClassOwnedBead) catches the
// reserved-prefix positional and refuseClassOwnedTarget names the routing cause.
// This is the regression the scorecard's required change #2 asks for: an explicit
// unsupported-routing diagnostic in place of bd not-found.
//
// "Class-owned" is RESIDENCE, so the refused bead is seeded into the binding.
// The second half is the boundary: a reserved id the binding does not hold is a
// bead the binding never leased and cannot refresh, and the ledger the
// passthrough is pointed at is the only one that could — a rig configured with
// a prefix inside the reserved namespace mints work beads there and heartbeats
// them on every tick.
func TestGcBdHeartbeatOnARelocatedClassIDRefusesByOwnership(t *testing.T) {
	capture, cityDir := bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage)
	t.Setenv("BD_STUB_STDOUT", "[]")
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)
	resident := seedRelocatedBindingBead(t, cityDir, "leased by the binding")

	args := []string{"heartbeat", resident.ID}
	var stdout, stderr bytes.Buffer
	if code := doBd(args, &stdout, &stderr); code == 0 {
		t.Fatalf("doBd(%v) exited 0; a heartbeat against a bead the binding holds must not run against the work store; stdout=%q", args, stdout.String())
	}
	// The refusal must be the by-id OWNERSHIP one, naming the bead, its binding
	// and heartbeat as the unserved verb — the routing cause — not bd's substring
	// not-found from the one ledger that holds no gcg- row.
	for _, want := range []string{resident.ID + " is owned by", "heartbeat", "is not served in process"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal does not name the routing cause (missing %q); stderr=%q", want, stderr.String())
		}
	}
	// Whatever the by-id door censused while resolving the class binding, the
	// REFUSED heartbeat argv must never have been forwarded to bd.
	if data, err := os.ReadFile(capture); err == nil && strings.Contains(string(data), strings.Join(args, " ")) {
		t.Fatalf("the refused heartbeat invocation was forwarded to bd: %q", data)
	}

	missing := []string{"heartbeat", "gcg-abc123"}
	stdout.Reset()
	stderr.Reset()
	if code := doBd(missing, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd(%v) exited %d for a reserved id the binding does not hold; stderr=%q", missing, code, stderr.String())
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("bd was not invoked for %v: %v", missing, err)
	}
	if !strings.Contains(string(data), strings.Join(missing, " ")) {
		t.Fatalf("bd received %q, want %v forwarded verbatim", data, missing)
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
//
// The `ready` rows are now decided one step earlier, by the topology, so this
// row set no longer distinguishes the dialect for that verb — it pins that the
// selector shape stays refused, which is what an operator following #5162's
// message will still type. TestGcBdReadyRefusesTheWholeFrontierOnASplitCity is
// where the wider claim lives.
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

// bdReadyLiveShortAnswer is the win3 live-proof measurement as a bd stub body:
// the five work-class rows `gc bd ready` returned on a converged split city at
// the same instant `gc ready` and the controller API both returned nine, three
// of them graph-resident (`/var/tmp/win3-splitstore-live-proof.md`, item 1).
// Nothing in the array is malformed, nothing is missing, and the exit code is
// 0 — which is exactly why it was believed.
const bdReadyLiveShortAnswer = `[{"id":"jc-026"},{"id":"jc-3nj"},{"id":"jc-aj3"},{"id":"jc-cpd"},{"id":"jc-wve"}]`

// TestGcBdReadyRefusesTheWholeFrontierOnASplitCity is the ga-jbn6f measurement
// as a test, and it is the one the selector guard could not have caught.
//
// The live proof ran three readers against one converged split city with the
// controller up:
//
//	gc ready                 n=9 gcg=3   ≡ the API's 9/3, id for id
//	GET …/beads/ready        n=9 gcg=3
//	gc bd ready --json       n=5 gcg=0   exit 0
//
// Every argv below is short in exactly that way, and none of them names a
// relocated id — the bare invocation names nothing at all. So a guard that
// classifies argument TEXT cannot fire on a single one of them, and #5162's
// `ready --metadata-field <k>=<gcg id>` row, which does fire, is the invocation
// nobody types. The trigger has to be the city's topology.
//
// The pool-demand row is here deliberately. It is the same selector the
// generated work query uses, it carries no reserved prefix, and it was the
// argv-level proof that "the work loop does not notice this guard". It is now
// refused too, and the work loop still does not notice, for a reason that does
// not depend on argv: generated queries invoke raw `bd ready` (single-store
// tiers) or bare `gc ready` (split tiers) — never `gc bd ready`. See
// internal/config/workquery.go's readyReaderCommand.
//
// The stub answers with the live short array and exits 0, so removing the guard
// fails this test in the exact shape the city produced.
func TestGcBdReadyRefusesTheWholeFrontierOnASplitCity(t *testing.T) {
	for name, args := range map[string][]string{
		"bare":                {"ready"},
		"json":                {"ready", "--json"},
		"unassigned":          {"ready", "--unassigned", "--json"},
		"pool demand":         {"ready", "--metadata-field", "gc.routed_to=demo/worker", "--unassigned", "--json"},
		"behind a root flag":  {"--json", "ready"},
		"label naming an id":  {"ready", "--label", "gcg-abc123"},
		"limit only":          {"ready", "--limit", "1", "--json"},
		"assignee restricted": {"ready", "--assignee", "demo/worker", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
			t.Setenv("BD_STUB_STDOUT", bdReadyLiveShortAnswer)

			var stdout, stderr bytes.Buffer
			if code := doBd(args, &stdout, &stderr); code == 0 {
				t.Fatalf("`gc bd %s` exited 0 with stdout=%q; that is the confident short frontier — no gcg- row, no warning, no non-zero exit — that `gc ready` and the API both contradict on the same city",
					strings.Join(args, " "), stdout.String())
			}
			// A refused frontier has no id to show, so the steer is the
			// federated reader and nothing else. The escape hint must be the
			// FRONTIER wording: there is no argument here to have misclassified,
			// so the selector arm's "this read is about work rows that merely
			// REFERENCE such an id" would describe a read nobody performed.
			for _, want := range []string{
				"graph-class beads", `"gcg-"`, splitEnvBinding, "gc ready", "TOPOLOGY",
				bdRelocatedClassOverrideEnvVar, "the work-class subset is the answer you want",
			} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("the refusal does not name %q; stderr=%q", want, stderr.String())
				}
			}
			if strings.Contains(stderr.String(), "merely REFERENCE such an id") {
				t.Errorf("the frontier refusal carries the SELECTOR arm's escape hint, which describes a predicate this read never had; stderr=%q", stderr.String())
			}
			if _, err := os.Stat(capture); err == nil {
				data, _ := os.ReadFile(capture) //nolint:errcheck // diagnostic only
				t.Fatalf("bd was invoked despite the refusal: %q", data)
			}
		})
	}
}

// TestGcBdReadyIsByteIdenticalOnASingleStoreCity is the mutation proof for the
// topology trigger, and it carries the claim the old
// TestGcBdReadyKeepsAnsweringItsOrdinaryWorkQueries used to carry on the wrong
// topology.
//
// That test asserted the pool-demand argv reached bd verbatim on a SPLIT city,
// which was true and is no longer: a split city's frontier is short whatever
// the selector says. The claim that survives — and the one the fix actually
// owes — is that a city which relocates nothing sees no change at all. The same
// argvs the split city now refuses are forwarded here byte for byte.
func TestGcBdReadyIsByteIdenticalOnASingleStoreCity(t *testing.T) {
	for name, args := range map[string][]string{
		"bare":                  {"ready"},
		"json":                  {"ready", "--json"},
		"pool demand":           {"ready", "--metadata-field", "gc.routed_to=demo/worker", "--unassigned", "--json"},
		"graph-shaped selector": {"ready", "--metadata-field", "gc.root_bead_id=gcg-abc123", "--json"},
		"list --ready":          {"list", "--ready", "--json"},
		"list --ready=true":     {"list", "--ready=true", "--json"},
		"list --ready=false":    {"list", "--ready=false", "--json"},
		"list --reverse":        {"list", "-r", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			capture := bdSQLRefusalCity(t, "")
			t.Setenv("BD_STUB_STDOUT", bdReadyLiveShortAnswer)

			var stdout, stderr bytes.Buffer
			if code := doBd(args, &stdout, &stderr); code != 0 {
				t.Fatalf("doBd(%v) = %d on a single-store city; stderr=%q", args, code, stderr.String())
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatalf("bd was not invoked for %v: %v", args, err)
			}
			if !strings.Contains(string(data), strings.Join(args, " ")) {
				t.Fatalf("bd received %q, want %v forwarded verbatim", data, args)
			}
		})
	}
}

// TestGcBdListReadyIsTheSameFrontierAndIsRefusedToo closes the spelling the
// verb-keyed topology arm missed.
//
// bd registers --ready on `list` as "Show only ready issues (no active
// blockers, same semantics as bd ready)" and dispatches it to the same
// GetReadyWork store methods `bd ready` calls, so it computes the identical
// short frontier over the identical one ledger and exits 0 — including while
// the relocated binding is unreadable, which is the state ga-jbn6f exists for.
// Refusing the verb and answering the flag would have minted the inversion the
// guard's own rationale calls worse than either failure alone: `gc bd list
// --ready --metadata-field gc.root_bead_id=<gcg>` was refused by the argv scan
// while the bare frontier `gc bd list --ready` was answered short.
//
// Red before the fix, on bdSQLRefusalSplitStorage with the live short answer
// stubbed:
//
//	doBd([list --ready --json])      = 0; bd got "list --ready --json"; stdout = the 5-row work-class-only array
//	doBd([list --ready=true --json]) = 0; doBd([list --json --ready]) = 0
//	bdSQLRelocatedClassRefusal(splitCityConfig(), [list --ready --json]) = ("", false)
func TestGcBdListReadyIsTheSameFrontierAndIsRefusedToo(t *testing.T) {
	for name, args := range map[string][]string{
		"flag":                {"list", "--ready", "--json"},
		"inline true":         {"list", "--ready=true", "--json"},
		"after a root flag":   {"list", "--json", "--ready"},
		"with a selector too": {"list", "--ready", "--metadata-field", "gc.root_bead_id=gcg-abc123", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
			t.Setenv("BD_STUB_STDOUT", bdReadyLiveShortAnswer)

			var stdout, stderr bytes.Buffer
			if code := doBd(args, &stdout, &stderr); code == 0 {
				t.Fatalf("`gc bd %s` exited 0 with stdout=%q; that is the same confident short frontier `gc bd ready` is refused for, one flag over",
					strings.Join(args, " "), stdout.String())
			}
			// The refusal must name the read the operator typed, or they go
			// looking for a selector they never wrote.
			for _, want := range []string{"bd list --ready", "graph-class beads", "gc ready", "TOPOLOGY"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("the refusal does not name %q; stderr=%q", want, stderr.String())
				}
			}
			if _, err := os.Stat(capture); err == nil {
				data, _ := os.ReadFile(capture) //nolint:errcheck // diagnostic only
				t.Fatalf("bd was invoked despite the refusal: %q", data)
			}
		})
	}

	// --ready=false runs an ordinary ledger query, and `-r` is bd's --reverse
	// shorthand, not a frontier switch (cmd/bd/list.go BoolP("reverse", "r")).
	// Refusing either would be a false positive on a read this ledger answers.
	for name, args := range map[string][]string{
		"explicitly off":    {"list", "--ready=false", "--json"},
		"reverse shorthand": {"list", "-r", "--json"},
		"ordinary status":   {"list", "--status", "open", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, refused := bdSQLRelocatedClassRefusal(splitCityConfig(), args); refused {
				t.Fatalf("`gc bd %s` is refused as a frontier read; it is an ordinary question about this ledger's own rows",
					strings.Join(args, " "))
			}
		})
	}
}

// TestGcBdReadyOverrideStillRunsTheFrontierLoudly keeps the escape hatch open.
//
// The topology refusal is never a false positive — the class really is served
// elsewhere — but a one-ledger frontier is still a thing an operator asks for
// on purpose, and the refusal has to hand them the way to it. The override runs
// the read AND says what was overridden, so the short array they are about to
// trust arrives with its own caveat.
func TestGcBdReadyOverrideStillRunsTheFrontierLoudly(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
	t.Setenv("BD_STUB_STDOUT", bdReadyLiveShortAnswer)
	t.Setenv(bdRelocatedClassOverrideEnvVar, "1")

	args := []string{"ready", "--json"}
	var stdout, stderr bytes.Buffer
	if code := doBd(args, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd = %d with %s=1; the override must run the read, not soften the refusal", code, bdRelocatedClassOverrideEnvVar)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("bd was not invoked under the override: %v", err)
	}
	if !strings.Contains(string(data), strings.Join(args, " ")) {
		t.Fatalf("bd received %q, want the frontier read forwarded verbatim", data)
	}
	for _, want := range []string{bdRelocatedClassOverrideEnvVar, "running anyway", "graph-class beads"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the override is silent about %q; stderr=%q", want, stderr.String())
		}
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
//
// A verb satisfies the requirement through EITHER map. The argv scan
// (bdRelocatedClassGuardedVerbs) catches a selector that names a relocated
// namespace; the topology trigger (bdRelocatedClassBlindVerbs) refuses the verb
// outright and so covers that selector and every other argv with it. `ready`
// moved from the first to the second in ga-jbn6f, which is a strictly wider
// refusal, and this pin has to accept that without being loosened into "some
// map somewhere mentions it" — hence the explicit union rather than a rename.
func TestBdRelocatedClassGuardCoversEverySelectorVerb(t *testing.T) {
	refused := func(sub string) bool {
		if _, guarded := bdRelocatedClassGuardedVerbs[sub]; guarded {
			return true
		}
		return bdRelocatedClassBlindVerbs[sub]
	}
	for _, sub := range bdflags.Subcommands() {
		if !bdflags.ValueFlags(sub)["--metadata-field"] {
			continue
		}
		if !refused(sub) {
			t.Errorf("bd %q takes --metadata-field but is in neither bdRelocatedClassGuardedVerbs nor bdRelocatedClassBlindVerbs, so `gc bd %s --metadata-field <k>=<relocated id>` answers the empty set against a ledger that cannot hold the rows", sub, sub)
		}
	}
	// bd search is not in bdflags (the manifest is generated from the
	// subcommands gc's own lint check walks), so it is named explicitly.
	if !refused("search") {
		t.Error("`search` takes --metadata-field (cmd/bd/search.go) and is not guarded")
	}
	// A verb in both maps has a dead branch: the topology arm runs first, so the
	// scan registered for it can never fire. That reads as coverage and is not.
	for sub := range bdRelocatedClassBlindVerbs {
		if _, alsoScanned := bdRelocatedClassGuardedVerbs[sub]; alsoScanned {
			t.Errorf("bd %q is refused by topology AND registered for an argv scan; the scan is unreachable", sub)
		}
	}
}

// TestBdRelocatedClassGuardCoversEveryFrontierSurface is the other half of the
// completeness claim, and it is the one that was missing: the selector pin above
// derives its surface from --metadata-field, a VALUE flag, so it is structurally
// blind to a frontier expressed as a BOOL flag. `bd list --ready` sat in that
// blind spot — refused as a verb, answered as a flag.
//
// The requirement is derived from bdflags.BoolFlags rather than restated, so a
// verb that grows --ready is covered without an edit to the guard, and a verb
// that loses it stops being asserted about.
func TestBdRelocatedClassGuardCoversEveryFrontierSurface(t *testing.T) {
	surfaces := 0
	for _, sub := range bdflags.Subcommands() {
		if !bdflags.BoolFlags(sub)[bdRelocatedClassFrontierFlag] {
			continue
		}
		surfaces++
		for _, args := range [][]string{
			{sub, bdRelocatedClassFrontierFlag},
			{sub, bdRelocatedClassFrontierFlag + "=true"},
			{sub, "--json", bdRelocatedClassFrontierFlag},
		} {
			if _, refused := bdSQLRelocatedClassRefusal(splitCityConfig(), args); !refused {
				t.Errorf("`gc bd %s` computes bd's ready frontier over the one work ledger and is answered exit 0 on a split city",
					strings.Join(args, " "))
			}
		}
	}
	if surfaces == 0 {
		t.Fatalf("bdflags registers %q on no subcommand; either the manifest regressed or the frontier flag was renamed, and this pin is asserting nothing",
			bdRelocatedClassFrontierFlag)
	}
	// Every blind VERB is a frontier surface too, whatever argv it carries.
	for sub := range bdRelocatedClassBlindVerbs {
		if _, refused := bdSQLRelocatedClassRefusal(splitCityConfig(), []string{sub}); !refused {
			t.Errorf("`gc bd %s` is a blind verb that is not refused", sub)
		}
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
// Servability is not what decides where `dep tree` goes, and ga-pxppl is why
// that distinction had to survive the verb becoming servable. On a class-owned
// id the walk is now answered in process from the class binding, because bd
// would answer from the one ledger that cannot hold the bead; on a work id it is
// still forwarded verbatim, because that ledger is exactly the right answerer.
// Both halves are asserted together: either alone is satisfied by a surface that
// stopped discriminating.
//
// Ownership is residence, so the class-owned leg seeds the binding and walks a
// bead that is actually in it. The third leg is the boundary the first two
// cannot show: a reserved-prefix id the binding does NOT hold is not owned by
// it, and goes back out to bd with the rest. That the walk itself is correct
// over a deeper graph is pinned separately by
// TestBdByIDServesDepTreeFromTheClassBinding.
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

	t.Run("class-owned id is answered in process", func(t *testing.T) {
		capture, cityDir := bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage)
		resetCLIStorageRoutes(t)
		captureCLIStorageStderr(t)
		resident := seedRelocatedBindingBead(t, cityDir, "the root of the walk")

		args := []string{"dep", "tree", resident.ID}
		var stdout, stderr bytes.Buffer
		if code := doBd(args, &stdout, &stderr); code != 0 {
			t.Fatalf("doBd(%v) = %d for a bead the binding holds; stderr=%q", args, code, stderr.String())
		}
		if data, err := os.ReadFile(capture); err == nil && strings.Contains(string(data), strings.Join(args, " ")) {
			t.Fatalf("the class-owned dep tree was forwarded to bd: %q", data)
		}
		if !strings.Contains(stdout.String(), resident.ID) {
			t.Errorf("the walk does not name its root; stdout=%q", stdout.String())
		}
	})

	t.Run("reserved id the binding does not hold is forwarded verbatim", func(t *testing.T) {
		args := []string{"dep", "tree", "gcg-abc123"}
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
}

// TestGcBdShowNeverReachesBdForAClassOwnedIDOnASplitCity is the other half, and
// the behavior change this pin used to forbid.
//
// The old passthrough answered a relocated-class read from the work ledger and
// exited 0 — a confident wrong answer about a bead that ledger cannot hold. The
// routed surface serves it from the binding that holds it and never reaches the
// subprocess.
//
// It is asserted against a SEEDED bead, because that is what "class-owned"
// means. An empty binding would make the row pass on the prefix alone, and the
// prefix does not decide ownership: it names the binding as the namespace's
// AUTHORITY, and an id it does not hold goes back out to bd — which
// TestGcBdDepTreeSplitsOnOwnershipNotOnServability's third leg pins for the
// recursive read on the same fixture.
func TestGcBdShowNeverReachesBdForAClassOwnedIDOnASplitCity(t *testing.T) {
	capture, cityDir := bdSQLRefusalCityDir(t, bdSQLRefusalSplitStorage)
	resetCLIStorageRoutes(t)
	captureCLIStorageStderr(t)
	resident := seedRelocatedBindingBead(t, cityDir, "held by the binding")

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"show", resident.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("doBd exited %d for a bead the binding holds; stderr=%q", code, stderr.String())
	}
	// The capture records every bd invocation this command made, including the
	// convergence check's own census of the work store — which is a read of the
	// ledger it is entitled to read. What must not appear is the operator's
	// read, forwarded verbatim.
	if data, err := os.ReadFile(capture); err == nil && strings.Contains(string(data), "show "+resident.ID) {
		t.Fatalf("the by-ID read was forwarded to bd: %q", data)
	}
	if !strings.Contains(stdout.String(), resident.ID) {
		t.Errorf("the answer does not name the bead; stdout=%q", stdout.String())
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

// TestBdCreateRefusesInfraShapedCreateOnSplitCity is the stranded mint, caught
// from the only evidence the passthrough has: the shape argv states.
//
// `gc bd create` hands its arguments to bd, and bd writes one ledger — the work
// one. On a city that serves a coordination class from a storage binding, a
// create whose shape belongs to that class lands the bead in the ledger the
// class is no longer read from, and nothing reports it, because bd did exactly
// what it was asked and exited 0. Placement is impossible on this seam (only
// argv crosses it), so the create is refused at the boundary instead.
//
// Every row asserts the same three facts an operator needs to act: the class,
// where its beads actually live, and the gc-native command that mints one
// correctly. Each row also runs against a city that relocates nothing, where
// the identical argv must pass through untouched — the single-store
// compatibility claim, checked per row rather than once.
func TestBdCreateRefusesInfraShapedCreateOnSplitCity(t *testing.T) {
	messaging := []string{"messaging-class", `"gcm-"`, `"infra" storage binding`, "gc mail send"}
	for name, tc := range map[string]struct {
		args []string
		want []string
	}{
		"mail by type":            {[]string{"create", "--type", "message", "hello"}, messaging},
		"mail by type shorthand":  {[]string{"create", "-t", "message", "hello"}, messaging},
		"mail by inline type":     {[]string{"create", "--type=message", "hello"}, messaging},
		"mail by attached type":   {[]string{"create", "-tmessage", "hello"}, messaging},
		"mail behind a root flag": {[]string{"--json", "create", "--type", "message", "hello"}, messaging},
		"mail through the alias":  {[]string{"new", "--type", "message", "hello"}, messaging},
		// The title is not the shape. A bead titled "message" is a work bead.
		"mail type beside a title flag": {[]string{"create", "--title", "ship it", "--type", "message"}, messaging},
		// The reason the classifier has to be the runtime one: an extmsg record
		// is a type=task bead, indistinguishable from work by type alone.
		"extmsg by label":               {[]string{"create", "--type", "task", "--labels", "gc:extmsg-binding", "x"}, messaging},
		"extmsg in a label list":        {[]string{"create", "-l", "triage,gc:extmsg-delivery", "x"}, messaging},
		"session by type":               {[]string{"create", "--type", "session", "x"}, []string{"sessions-class", `"gcs-"`, "gc session new"}},
		"session by label":              {[]string{"create", "--label", "gc:session", "x"}, []string{"sessions-class", "gc session new"}},
		"session through quick capture": {[]string{"q", "-t", "session", "x"}, []string{"sessions-class", "gc session new"}},
		"nudge by label":                {[]string{"create", "--type", "chore", "-l", "gc:nudge", "x"}, []string{"nudges-class", `"gcn-"`, "gc nudge"}},
		"order tracking by label":       {[]string{"create", "--type", "task", "-l", "order-tracking", "x"}, []string{"orders-class", `"gco-"`, "gc order run"}},
		"wisp by metadata":              {[]string{"create", "--metadata", `{"gc.kind":"wisp"}`, "x"}, []string{"graph-class", `"gcg-"`, "gc sling"}},
		"graph node by metadata":        {[]string{"create", "--metadata", `{"gc.root_bead_id":"gcg-abc123"}`, "x"}, []string{"graph-class", "gc sling"}},
	} {
		t.Run(name, func(t *testing.T) {
			msg, refused := bdRelocatedClassCreateRefusal(splitCityConfig(), "", tc.args)
			if !refused {
				t.Fatalf("`gc bd %s` was forwarded to bd on a split city; bd writes the work ledger only, so that mint is stranded", strings.Join(tc.args, " "))
			}
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal is missing %q; msg=%q", want, msg)
				}
			}
			for cityName, cfg := range map[string]*config.City{
				"no storage section":  nil,
				"every class on work": allWorkCityConfig(),
			} {
				if msg, refused := bdRelocatedClassCreateRefusal(cfg, "", tc.args); refused {
					t.Errorf("a city with %s relocates nothing, so this create is not stranded, but it was refused: %q", cityName, msg)
				}
			}
		})
	}
}

// TestBdCreateForwardsAWorkShapedCreateOnASplitCity is the false-positive proof.
//
// A split city still mints work beads through this passthrough — that is what
// the work ledger is for — so the guard must fire on the SHAPE and on nothing
// else. A title that reads like a class, a label that is merely near one, and
// an id-valued flag naming a relocated bead are all left alone here: the last
// belongs to the by-id ownership door in cmd_bd_by_id.go, which names the bead
// rather than the class, and a second refusal on this seam would shadow it with
// a vaguer message while adding no coverage.
func TestBdCreateForwardsAWorkShapedCreateOnASplitCity(t *testing.T) {
	for name, args := range map[string][]string{
		"bare create":                {"create", "plain work"},
		"explicit task":              {"create", "-t", "task", "plain work"},
		"task with labels":           {"create", "--type", "task", "--labels", "triage,p1", "x"},
		"title that reads as a type": {"create", "--title", "message", "-t", "task"},
		"label near but not a class": {"create", "-l", "gc:nudged", "x"},
		"epic":                       {"create", "--type", "epic", "roll up"},
		"convoy":                     {"create", "--type", "convoy", "batch"},
		"id-valued flag":             {"create", "--type", "task", "--parent", "gcg-abc123", "x"},
		"not a create verb":          {"list", "--type", "message", "--json"},
		"show is not a create verb":  {"show", "gcm-abc123"},
	} {
		t.Run(name, func(t *testing.T) {
			if msg, refused := bdRelocatedClassCreateRefusal(splitCityConfig(), "", args); refused {
				t.Fatalf("`gc bd %s` mints a work bead into the work ledger, which is where it belongs, but it was refused: %q", strings.Join(args, " "), msg)
			}
		})
	}
}

// TestGcBdCreateRefusesAnInfraShapedCreateOnASplitCity drives the refusal
// through the real command: the exit code is non-zero and bd is never spawned,
// so nothing is written anywhere.
func TestGcBdCreateRefusesAnInfraShapedCreateOnASplitCity(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"create", "--type", "message", "hello"}, &stdout, &stderr); code == 0 {
		t.Fatalf("doBd exited 0 on a stranded mint; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	for _, want := range []string{"messaging-class", `"infra" storage binding`, "gc mail send"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal is missing %q; stderr=%q", want, stderr.String())
		}
	}
	if _, err := os.Stat(capture); err == nil {
		data, _ := os.ReadFile(capture) //nolint:errcheck // diagnostic only
		t.Fatalf("bd was invoked despite the refusal: %q", data)
	}
}

// TestGcBdCreateRefusalIsNotLiftedByTheReadOverride pins the one thing the
// existing escape hatch must not reach.
//
// GC_BD_ALLOW_RELOCATED_CLASS_READ exists because the READ scan classifies
// text, and text is not always decidable — an operator holding a false positive
// needs a way to run the query anyway. A refused create is not that: it is a
// WRITE that would leave a row in the wrong ledger, where no later read can
// find it and no migration will move it. Honoring the read knob here would turn
// an escape hatch into a data-loss switch.
func TestGcBdCreateRefusalIsNotLiftedByTheReadOverride(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
	t.Setenv(bdRelocatedClassOverrideEnvVar, "1")

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"create", "--type", "message", "hello"}, &stdout, &stderr); code == 0 {
		t.Fatalf("the read override let a stranded mint through; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if _, err := os.Stat(capture); err == nil {
		data, _ := os.ReadFile(capture) //nolint:errcheck // diagnostic only
		t.Fatalf("bd was invoked with the read override set: %q", data)
	}
}

// TestGcBdCreateIsUnchangedOnASingleStoreCity is the compatibility proof end to
// end: the exact invocation a split city refuses reaches bd verbatim, and
// succeeds, on a city that relocates nothing.
func TestGcBdCreateIsUnchangedOnASingleStoreCity(t *testing.T) {
	for name, tc := range map[string]struct {
		storage string
		args    []string
	}{
		"infra shape, no split": {"", []string{"create", "--type", "message", "hello"}},
		"work shape, split":     {bdSQLRefusalSplitStorage, []string{"create", "--type", "task", "hello"}},
	} {
		t.Run(name, func(t *testing.T) {
			capture := bdSQLRefusalCity(t, tc.storage)
			t.Setenv("BD_STUB_STDOUT", `{"id":"demo-1"}`)

			var stdout, stderr bytes.Buffer
			if code := doBd(tc.args, &stdout, &stderr); code != 0 {
				t.Fatalf("doBd = %d; stderr=%q", code, stderr.String())
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatalf("bd was not invoked: %v", err)
			}
			if !strings.Contains(string(data), strings.Join(tc.args, " ")) {
				t.Fatalf("bd received %q, want the create forwarded verbatim", data)
			}
		})
	}
}

// TestBdRelocatedClassCreateNamesAMintPathForEveryRelocatableClass is the
// anti-drift pin on the one part of the refusal an operator acts on.
//
// A refusal that names no alternative leaves the operator with a command that
// does not work and no command that does, and the failure mode of a hand-kept
// table is that a class grows without one. So the table is checked against the
// same class list the migration uses, and each command it names is resolved in
// the real cobra tree rather than trusted as a string.
func TestBdRelocatedClassCreateNamesAMintPathForEveryRelocatableClass(t *testing.T) {
	root := newRootCmdWithOptions(io.Discard, io.Discard, rootCommandOptions{})
	for _, class := range infraMigrationClasses {
		path, named := bdRelocatedClassMintPaths[string(class)]
		if !named {
			t.Errorf("class %q can be relocated but the refusal names no gc-native command that mints one", class)
			continue
		}
		words := strings.Fields(path)
		if len(words) < 2 || words[0] != "gc" {
			t.Errorf("class %q names mint path %q, which is not a `gc ...` command", class, path)
			continue
		}
		cmd, _, err := root.Find(words[1:])
		if err != nil {
			t.Errorf("class %q names mint path %q, which does not resolve: %v", class, path, err)
			continue
		}
		if cmd.Name() != words[len(words)-1] {
			t.Errorf("class %q names mint path %q, which resolved to `gc %s` instead", class, path, cmd.CommandPath())
		}
	}
}

// writeGraphPlanFile marshals a plan to the exact JSON wire shape bd's
// `create --graph <file>` consumes — internal/beads.ApplyGraphPlanWithStorage
// json.Marshals this same struct before handing the path to bd — and returns the
// file path. Building it from the struct rather than a string literal is what
// makes the test's plan and bd's plan provably the same format.
func writeGraphPlanFile(t *testing.T, plan beads.GraphApplyPlan) string {
	t.Helper()
	data, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshaling graph plan: %v", err)
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing graph plan: %v", err)
	}
	return path
}

// graphMarkedPlan is a minimal plan whose root node carries a graph marker
// (gc.root_bead_id), so coordclass.ClassifyGraphPlan routes the whole plan to
// ClassGraph — the shape a formula pour applies through `create --graph`.
func graphMarkedPlan() beads.GraphApplyPlan {
	return beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{{
		Key:      "root",
		Title:    "workflow root",
		Type:     "task",
		Metadata: map[string]string{"gc.root_bead_id": "gcg-abc123"},
	}}}
}

// workOnlyPlan is a plan of plain work nodes with no graph markers, so
// ClassifyGraphPlan falls to its root node and classifies ClassWork: a graph
// create that belongs in the work ledger even on a split city.
func workOnlyPlan() beads.GraphApplyPlan {
	return beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{{
		Key:   "root",
		Title: "plain work",
		Type:  "task",
	}}}
}

// TestBdCreateClassifiesAGraphPlanCreateOnASplitCity is the graph-door fix. A
// `create --graph <plan>` carries its beads inside a file the argv-shape walk
// never sees, so before this the highest-value infra shape — a graph-apply plan
// — classified as work and forwarded, stranding a graph-class bead in the work
// ledger. The plan is JSON in the beads.GraphApplyPlan shape bd consumes, so it
// is classified with the production coordclass.ClassifyGraphPlan and refused
// when it names a relocated class, across every spelling of the flag.
func TestBdCreateClassifiesAGraphPlanCreateOnASplitCity(t *testing.T) {
	for name, spell := range map[string]func(path string) []string{
		"separated":   func(p string) []string { return []string{"create", "--graph", p, "--json"} },
		"inline":      func(p string) []string { return []string{"create", "--graph=" + p} },
		"alias":       func(p string) []string { return []string{"new", "--graph", p} },
		"after title": func(p string) []string { return []string{"create", "--title", "roll up", "--graph", p} },
	} {
		t.Run(name, func(t *testing.T) {
			args := spell(writeGraphPlanFile(t, graphMarkedPlan()))
			msg, refused := bdRelocatedClassCreateRefusal(splitCityConfig(), "", args)
			if !refused {
				t.Fatalf("`gc bd %s` forwards a graph-apply plan to bd on a split city, stranding a graph-class bead", strings.Join(args, " "))
			}
			for _, want := range []string{"graph-class", `"gcg-"`, "gc sling"} {
				if !strings.Contains(msg, want) {
					t.Errorf("refusal is missing %q; msg=%q", want, msg)
				}
			}
			for cityName, cfg := range map[string]*config.City{
				"no storage section":  nil,
				"every class on work": allWorkCityConfig(),
			} {
				if msg, refused := bdRelocatedClassCreateRefusal(cfg, "", args); refused {
					t.Errorf("a city with %s relocates nothing, but the graph create was refused: %q", cityName, msg)
				}
			}
		})
	}
}

// TestBdCreateForwardsAWorkOnlyGraphPlanOnASplitCity is the graph-door
// false-positive proof: the guard fires on the plan's CLASS, not on the mere
// presence of `--graph`, so a plan of plain work nodes still mints into the work
// ledger where it belongs.
func TestBdCreateForwardsAWorkOnlyGraphPlanOnASplitCity(t *testing.T) {
	args := []string{"create", "--graph", writeGraphPlanFile(t, workOnlyPlan()), "--json"}
	if msg, refused := bdRelocatedClassCreateRefusal(splitCityConfig(), "", args); refused {
		t.Fatalf("a work-only graph plan belongs in the work ledger, but it was refused: %q", msg)
	}
}

// TestBdCreateFailsClosedOnAFileBackedBulkCreateOnASplitCity pins the -f/--file
// decision. bd's multi-issue markdown states per-issue type and labels that CAN
// name a relocated class, and reparsing that format here would drift from bd, so
// on a split city these create forms fail closed rather than forward a payload
// the guard cannot classify. A city that relocates nothing forwards them.
func TestBdCreateFailsClosedOnAFileBackedBulkCreateOnASplitCity(t *testing.T) {
	for name, args := range map[string][]string{
		"-f separated":      {"create", "-f", "issues.md", "x"},
		"--file separated":  {"create", "--file", "issues.md"},
		"--file inline":     {"create", "--file=issues.md"},
		"-f attached":       {"create", "-fissues.md"},
		"through the alias": {"new", "-f", "issues.md"},
	} {
		t.Run(name, func(t *testing.T) {
			msg, refused := bdRelocatedClassCreateRefusal(splitCityConfig(), "", args)
			if !refused {
				t.Fatalf("`gc bd %s` reads beads from a file this guard cannot classify, but it was forwarded on a split city", strings.Join(args, " "))
			}
			// The message must explain the file-backed limit and leave the
			// operator a classified door, not just say no.
			for _, want := range []string{"file", "gc sling", "gc mail send"} {
				if !strings.Contains(msg, want) {
					t.Errorf("fail-closed refusal is missing %q; msg=%q", want, msg)
				}
			}
			for cityName, cfg := range map[string]*config.City{
				"no storage section":  nil,
				"every class on work": allWorkCityConfig(),
			} {
				if msg, refused := bdRelocatedClassCreateRefusal(cfg, "", args); refused {
					t.Errorf("a city with %s relocates nothing, so a file-backed create strands nothing, but it was refused: %q", cityName, msg)
				}
			}
		})
	}
}

// TestGcBdCreateRefusesAGraphPlanCreateOnASplitCity drives the graph-door fix
// through the real command: a graph-apply plan on a split city exits non-zero
// and bd is never spawned, so nothing is written anywhere.
func TestGcBdCreateRefusesAGraphPlanCreateOnASplitCity(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
	planPath := writeGraphPlanFile(t, graphMarkedPlan())

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"create", "--graph", planPath, "--json"}, &stdout, &stderr); code == 0 {
		t.Fatalf("doBd exited 0 on a stranded graph mint; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	for _, want := range []string{"graph-class", "gc sling"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal is missing %q; stderr=%q", want, stderr.String())
		}
	}
	if _, err := os.Stat(capture); err == nil {
		data, _ := os.ReadFile(capture) //nolint:errcheck // diagnostic only
		t.Fatalf("bd was invoked despite the refusal: %q", data)
	}
}

// TestGcBdCreateFailsClosedOnAFileBackedBulkCreateOnASplitCity drives the
// -f/--file decision through the real command: even a work-only markdown file is
// refused on a split city — the guard classifies from argv and will not reparse
// bd's file format, so it cannot prove the file mints no relocated bead — and bd
// is never spawned.
func TestGcBdCreateFailsClosedOnAFileBackedBulkCreateOnASplitCity(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
	mdPath := filepath.Join(t.TempDir(), "issues.md")
	if err := os.WriteFile(mdPath, []byte("## plain work\nType: task\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"create", "-f", mdPath}, &stdout, &stderr); code == 0 {
		t.Fatalf("doBd exited 0 on a file-backed create; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "file") {
		t.Errorf("fail-closed refusal does not explain the file-backed limit; stderr=%q", stderr.String())
	}
	if _, err := os.Stat(capture); err == nil {
		data, _ := os.ReadFile(capture) //nolint:errcheck // diagnostic only
		t.Fatalf("bd was invoked despite the fail-closed refusal: %q", data)
	}
}

// TestBdCreateRefusesAnInfraShapedCreateBehindABdRootFlag pins the guard
// against the flags that used to switch it off.
//
// bd accepts its root flags BEFORE the subcommand, and four of them consume the
// next argument as a value (`--profile`, `--database`, `--server-url`,
// `--mem-profile`). A global manifest that filed `--profile` as a bool and did
// not know the other three left bdRelocatedClassVerb reading a flag's VALUE as
// the verb (`--profile default create …` resolves to "default") or reporting the
// argv undecidable — and both answers forward the create. bd accepts every one
// of these flags and goes on to mint, so the disarmed guard was the only thing
// between an ordinary invocation and a stranded bead.
func TestBdCreateRefusesAnInfraShapedCreateBehindABdRootFlag(t *testing.T) {
	for name, prefix := range map[string][]string{
		"--profile":              {"--profile", "default"},
		"--profile inline":       {"--profile=default"},
		"--database":             {"--database", "beads_other"},
		"--server-url":           {"--server-url", "http://127.0.0.1:8080"},
		"--mem-profile":          {"--mem-profile", "/tmp/heap.out"},
		"--no-color":             {"--no-color"},
		"--cpu-profile":          {"--cpu-profile"},
		"stacked with the known": {"--json", "--mem-profile", "/tmp/heap.out", "-C", "/d"},
	} {
		t.Run(name, func(t *testing.T) {
			args := append(append([]string{}, prefix...), "create", "--type", "message", "hello")
			msg, refused := bdRelocatedClassCreateRefusal(splitCityConfig(), "", args)
			if !refused {
				t.Fatalf("`gc bd %s` reached bd unguarded on a split city; the root flag disarmed the create guard", strings.Join(args, " "))
			}
			if !strings.Contains(msg, "messaging-class") || !strings.Contains(msg, "gc mail send") {
				t.Errorf("refusal does not name the class and its mint path; msg=%q", msg)
			}
			// The same prefix in front of a work-shaped create must still pass
			// through: completing the manifest resolves the verb, it does not
			// turn a root flag into a refusal of its own.
			work := append(append([]string{}, prefix...), "create", "--type", "task", "hello")
			if msg, refused := bdRelocatedClassCreateRefusal(splitCityConfig(), "", work); refused {
				t.Errorf("`gc bd %s` mints a work bead into the work ledger, but it was refused: %q", strings.Join(work, " "), msg)
			}
		})
	}
}

// TestBdCreateClassifiesARelativeGraphPlanAgainstTheScopeRoot pins the root a
// `--graph` path is resolved against.
//
// doBd runs the bd subprocess with cmd.Dir = target.ScopeRoot, so bd reads a
// relative plan path from THERE while this guard reads it in the wrapper's own
// cwd. When those differ, the guard's read fails, the plan classifies as
// nothing, and the create is forwarded — after which bd finds the plan and mints
// the relocated-class bead the guard exists to stop.
func TestBdCreateClassifiesARelativeGraphPlanAgainstTheScopeRoot(t *testing.T) {
	scopeRoot := t.TempDir()
	data, err := json.Marshal(graphMarkedPlan())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, "plan.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	// Stand somewhere the plan is NOT, which is the whole point: an operator or
	// agent naming a scope-root-relative path from a subdirectory.
	t.Chdir(t.TempDir())

	msg, refused := bdRelocatedClassCreateRefusal(splitCityConfig(), scopeRoot, []string{"create", "--graph", "plan.json"})
	if !refused {
		t.Fatalf("a scope-root-relative graph plan was forwarded to bd, which reads it from the scope root and mints it; msg=%q", msg)
	}
	if !strings.Contains(msg, "graph-class") || !strings.Contains(msg, "gc sling") {
		t.Errorf("refusal does not name the class and its mint path; msg=%q", msg)
	}

	// An absolute path is already unambiguous and must not be re-rooted.
	absolute := []string{"create", "--graph", writeGraphPlanFile(t, graphMarkedPlan())}
	if _, refused := bdRelocatedClassCreateRefusal(splitCityConfig(), scopeRoot, absolute); !refused {
		t.Error("an absolute graph plan path was re-rooted against the scope root and lost")
	}

	// The class still decides: a work-only plan at the same relative path is
	// forwarded, so resolving the path did not turn `--graph` into a refusal.
	workData, err := json.Marshal(workOnlyPlan())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, "work.json"), workData, 0o644); err != nil {
		t.Fatal(err)
	}
	if msg, refused := bdRelocatedClassCreateRefusal(splitCityConfig(), scopeRoot, []string{"create", "--graph", "work.json"}); refused {
		t.Errorf("a work-only graph plan belongs in the work ledger, but it was refused: %q", msg)
	}
}

// TestGcBdCreateRefusesARelativeGraphPlanFromAnotherDirectory drives the
// scope-root resolution through the real command: `gc bd` invoked from outside
// the store root, naming the plan the way bd itself will read it, still exits
// non-zero without spawning bd.
//
// The second invocation pins the same refusal behind bd's own `-C/--directory`.
// bd's help text reads like a cwd change ("like git -C"), but `-C` selects the
// beads PROJECT bd opens, not the base for a relative path argument, so the
// scope root remains the only root either side resolves against. That is the
// claim the guard's doc comment makes, and without this row it rests on observed
// bd behavior with nothing in the build behind it.
func TestGcBdCreateRefusesARelativeGraphPlanFromAnotherDirectory(t *testing.T) {
	capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)
	scopeRoot := os.Getenv("GC_CITY_PATH")
	data, err := json.Marshal(graphMarkedPlan())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, "plan.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"create", "--graph", "plan.json"}, &stdout, &stderr); code == 0 {
		t.Fatalf("doBd exited 0 on a stranded graph mint named relative to the scope root; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "graph-class") {
		t.Errorf("refusal is missing the class; stderr=%q", stderr.String())
	}
	if _, err := os.Stat(capture); err == nil {
		body, _ := os.ReadFile(capture) //nolint:errcheck // diagnostic only
		t.Fatalf("bd was invoked despite the refusal: %q", body)
	}

	// A `-C` directory that is neither the cwd nor the scope root, and that no
	// rig claims: resolveRigForDir finds no match and leaves the scope root
	// unchanged, so the guard reads the same plan and refuses the same way. The
	// refusal fires before bd is invoked, so no subprocess runs and no mint is
	// possible regardless of how bd would have read the path.
	otherDir := t.TempDir()
	stdout.Reset()
	stderr.Reset()
	if code := doBd([]string{"create", "-C", otherDir, "--graph", "plan.json"}, &stdout, &stderr); code == 0 {
		t.Fatalf("doBd exited 0 on the same stranded graph mint behind `-C`; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "graph-class") {
		t.Errorf("refusal behind `-C` is missing the class; stderr=%q", stderr.String())
	}
	if _, err := os.Stat(capture); err == nil {
		body, _ := os.ReadFile(capture) //nolint:errcheck // diagnostic only
		t.Fatalf("bd was invoked despite the refusal behind `-C`: %q", body)
	}
}
