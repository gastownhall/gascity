package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

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
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"${CAPTURE_PATH}\"\n"), 0o755); err != nil {
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

// TestGcBdShowIsARawPassthroughOnASplitCity is the fact that made the old
// refusal message wrong, pinned so it cannot be forgotten again.
//
// The message used to end "Use the federated `gc bd show <id>` or `gc bd dep
// tree <id>`". Neither verb is federated: doBd resolves a scope and then execs
// the bd binary with the args verbatim and cmd.Dir at that scope root, with no
// coordination-class routing anywhere on the path. Following the advice ran the
// blind read the refusal had just prevented. The class-routed read the message
// names instead is proved by internal/api's TestBeadGetResolvesARelocatedGraphID.
func TestGcBdShowIsARawPassthroughOnASplitCity(t *testing.T) {
	for name, args := range map[string][]string{
		"show":     {"show", "gcg-abc123"},
		"dep tree": {"dep", "tree", "gcg-abc123"},
	} {
		t.Run(name, func(t *testing.T) {
			capture := bdSQLRefusalCity(t, bdSQLRefusalSplitStorage)

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
