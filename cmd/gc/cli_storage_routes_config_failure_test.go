package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// R1 (split-default read audit): a city.toml that fails to load used to read as
// "a city with no [storage]", so every one-shot class resolver fell back to the
// WORK store, and the answer was memoized for the life of the process. On a
// split city that is a silent misroute: reads go empty and writes strand in the
// work ledger, where the next boot's containment check refuses the city.

// breakSplitCityConfig makes cityPath's city.toml fail to load the way an
// operator's half-finished edit does: the root file parses, but it includes a
// fragment that is not there. It returns the function that repairs it.
func breakSplitCityConfig(t *testing.T, cityPath string) (repair func()) {
	t.Helper()
	path := filepath.Join(cityPath, "city.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	broken := append([]byte("include = [\"missing-fragment.toml\"]\n"), body...)
	if err := os.WriteFile(path, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, path, cliStorageRoutesLoad); err == nil {
		t.Fatal("the broken fixture config still loads")
	}
	return func() {
		t.Helper()
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCLIStorageRoutesRefuseWhenCityConfigFailsToLoad(t *testing.T) {
	cityPath, _ := migratedOneShotCLICity(t)
	stderr := captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	breakSplitCityConfig(t, cityPath)

	work := beads.NewMemStore()
	sessions := cliSessionStore(work, nil, cityPath)
	if sessions == beads.Store(work) {
		t.Fatal("an unloadable city.toml routed the session class to the WORK store (fail-open)")
	}
	_, err := sessions.Create(beads.Bead{Title: "a session", Type: "session"})
	if err == nil {
		t.Fatal("a session write succeeded while city.toml could not be loaded")
	}
	if !strings.Contains(err.Error(), "city.toml") {
		t.Errorf("the refusal does not say the city config failed to load: %v", err)
	}
	if _, err := sessions.List(beads.ListQuery{AllowScan: true}); err == nil {
		t.Error("a session read succeeded while city.toml could not be loaded")
	}
	if !strings.Contains(stderr.String(), "city.toml") {
		t.Errorf("the refusal never reached the operator: %q", stderr.String())
	}
	rows, err := work.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("the refused write still reached the work store: %d row(s)", len(rows))
	}

	// Work beads are not what moved, so they keep their path.
	if got := resolveClassStore(cliStorageRoutes(cityPath), work, nil, cityPath, config.BeadClassWork, nil); got != beads.Store(work) {
		t.Errorf("work resolved to %p on an unloadable config, want the work store %p", got, work)
	}
}

// expireUnsettledCLIRoutesImmediately makes every unsettled answer expire at
// once, so a test can repair and re-resolve without waiting out the TTL.
func expireUnsettledCLIRoutesImmediately(t *testing.T) {
	t.Helper()
	prev := cliStorageRoutesUnsettledTTL
	cliStorageRoutesUnsettledTTL = 0
	t.Cleanup(func() { cliStorageRoutesUnsettledTTL = prev })
}

func TestCLIStorageRoutesDoNotMemoizeAConfigLoadFailure(t *testing.T) {
	expireUnsettledCLIRoutesImmediately(t)
	cityPath, _ := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	repair := breakSplitCityConfig(t, cityPath)

	work := beads.NewMemStore()
	if _, err := cliSessionStore(work, nil, cityPath).Create(beads.Bead{Title: "early", Type: "session"}); err == nil {
		t.Fatal("a session write succeeded on the broken config")
	}

	// The operator fixes the config. The same long-lived process must now see
	// the binding, not the refusal it took a moment ago.
	repair()
	sessions := cliSessionStore(work, nil, cityPath)
	if sessions == beads.Store(work) {
		t.Fatal("after the config was repaired the session class routed to the work store")
	}
	created, err := sessions.Create(beads.Bead{Title: "after repair", Type: "session", Labels: []string{"gc:session"}})
	if err != nil {
		t.Fatalf("a session write still refused after the config was repaired (the failure was memoized): %v", err)
	}
	if _, err := work.Get(created.ID); err == nil {
		t.Errorf("the repaired route wrote %s into the work store", created.ID)
	}
}

// TestCLIMailSendWritesNothingWhenCityConfigFailsToLoad is the end-to-end form
// of R1: a `gc mail send` while city.toml is unloadable must refuse, write no row
// to either store, and leave a city whose next boot still converges.
func TestCLIMailSendWritesNothingWhenCityConfigFailsToLoad(t *testing.T) {
	cityPath, cfg := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	target := mustResolveInfraTarget(t, cityPath, cfg)
	before := splitCityFingerprints(t, cityPath, target)

	repair := breakSplitCityConfig(t, cityPath)
	var stderr bytes.Buffer
	sender, code := openCityMailProvider(&stderr, "gc mail send")
	if sender != nil {
		if sent, err := sender.Send("worker", "mayor", "PR ready", "please review"); err == nil {
			t.Errorf("gc mail send succeeded on an unloadable city.toml and wrote %s", sent.ID)
		}
		t.Error("openCityMailProvider opened a provider on an unloadable city.toml; it must refuse")
	} else {
		if code == 0 {
			t.Error("openCityMailProvider refused with exit code 0")
		}
		if !strings.Contains(stderr.String(), "city.toml") {
			t.Errorf("the mail refusal does not name the config failure: %q", stderr.String())
		}
	}
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}

	after := splitCityFingerprints(t, cityPath, target)
	for name, ids := range before {
		if !slices.Equal(ids, after[name]) {
			t.Errorf("%s store changed during a refused mail send: before %v, after %v", name, ids, after[name])
		}
	}

	repair()
	fixed, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatalf("reload repaired config: %v", err)
	}
	var bootErr bytes.Buffer
	routes, err := storageBootGate(cityPath, fixed, "gc start", nil, &bootErr)
	if routes != nil {
		_ = routes.close()
	}
	if err != nil {
		t.Errorf("the next boot after a refused mail send refused the city: %v (%s)", err, bootErr.String())
	}
}

// splitCityFingerprints reads every row id in the city's work store and its
// binding, closing both handles before it returns.
func splitCityFingerprints(t *testing.T, cityPath string, target infraBindingTarget) map[string][]string {
	t.Helper()
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("opening the work store: %v", err)
	}
	workIDs := infraStoreFingerprint(t, work)
	_ = closeBeadStoreHandle(work)
	binding, err := openInfraDestination(target)
	if err != nil {
		t.Fatalf("opening the binding: %v", err)
	}
	bindingIDs := infraStoreFingerprint(t, binding)
	_ = closeBeadStoreHandle(binding)
	return map[string][]string{"work": workIDs, "binding": bindingIDs}
}

// TestCLIStorageRoutesKeepTheWorkStoreForABrokenSingleStoreCity pins the other
// side of R1: a city that provably relocates nothing keeps today's answer when
// its config fails to load. Its classes have only ever lived in the work store,
// and refusing would take a merely misconfigured city offline.
func TestCLIStorageRoutesKeepTheWorkStoreForABrokenSingleStoreCity(t *testing.T) {
	cityPath := oneShotCLICity(t, "")
	stderr := captureCLIStorageStderr(t)
	// A validation failure in a file that is otherwise readable: every place a
	// [storage] could be declared is visible, and none declares one. (A missing
	// include is an unknown, and is refused; see the evidence table below.)
	path := filepath.Join(cityPath, "city.toml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(body, []byte("\n[ops]\ncontent = \"bogus\"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, path, cliStorageRoutesLoad); err == nil {
		t.Fatal("the broken fixture config still loads")
	}

	work := beads.NewMemStore()
	if got := cliSessionStore(work, nil, cityPath); got != beads.Store(work) {
		t.Errorf("a single-store city with a broken config routed sessions to %p, want the work store %p", got, work)
	}
	if stderr.Len() != 0 {
		t.Errorf("a single-store city with a broken config wrote a routing refusal: %q", stderr.String())
	}
}

// TestCityMayRelocateClassesEvidence is the evidence rule a config-load failure
// is judged by: only a city provably without [storage] anywhere it could be
// declared answers no; every unknown answers yes.
func TestCityMayRelocateClassesEvidence(t *testing.T) {
	const storage = "\n[storage.classes]\nsessions = \"infra\"\n"
	cases := []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{"no storage anywhere", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n"}, false},
		{"validation error but no storage", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n[ops]\ncontent = \"bogus\"\n"}, false},
		{"local fragment without storage", map[string]string{"city.toml": "include = [\"frag.toml\"]\n[workspace]\nname = \"c\"\n", "frag.toml": "[workspace]\nname = \"c\"\n"}, false},
		{"root declares storage", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n" + storage}, true},
		{"root does not parse, no storage text", map[string]string{"city.toml": "[workspace\nname = \n"}, false},
		{"root does not parse, declares storage", map[string]string{"city.toml": "[workspace\nname = \n[storage.classes]\nsessions = \"infra\"\n"}, true},
		{"root does not parse, dotted storage key", map[string]string{"city.toml": "[[[\nstorage.classes.sessions = \"infra\"\n"}, true},
		{"fragment declares storage", map[string]string{"city.toml": "include = [\"frag.toml\"]\n", "frag.toml": storage}, true},
		{"city-root-relative fragment declares storage", map[string]string{"city.toml": "include = [\"//conf/frag.toml\"]\n", "conf/frag.toml": storage}, true},
		{"fragment is missing", map[string]string{"city.toml": "include = [\"missing.toml\"]\n"}, true},
		{"fragment does not parse, declares storage", map[string]string{"city.toml": "include = [\"frag.toml\"]\n", "frag.toml": "[[[\n[storage]\n"}, true},
		{"remote fragment", map[string]string{"city.toml": "include = [\"https://example.invalid/x.toml\"]\n"}, true},
		// [pack] includes name pack directories, and a pack cannot declare
		// [storage] (review probe D: a directory include over-refused).
		{"pack includes a pack directory", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n", "pack.toml": "[pack]\nname = \"c\"\nschema = 1\nincludes = [\"packs/maint\"]\n", "packs/maint/pack.toml": "[pack]\nname = \"maint\"\nschema = 1\n"}, false},
		{"pack.toml does not parse", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n", "pack.toml": "[pack\n"}, false},
		// Review probe C: an unparseable root hides its include list.
		{"root does not parse, names an include", map[string]string{"city.toml": "include = [\"storage.toml\"]\n[workspace]\nname = \"x\"\nthis is not toml\n", "storage.toml": storage}, true},
		{"root does not parse, include of a silent fragment", map[string]string{"city.toml": "include = [\"frag.toml\"]\nthis is not toml\n", "frag.toml": "[workspace]\nname = \"c\"\n"}, true},
		// Review probe E: a UTF-8 BOM before the first header.
		{"BOM before [storage] in a broken root", map[string]string{"city.toml": "\ufeff[storage.classes]\ngraph = \"infra\"\n[workspace\n"}, true},
		{"BOM before [storage] in a parsing root", map[string]string{"city.toml": "\ufeff[storage.classes]\ngraph = \"infra\"\n"}, true},
		{"BOM before [storage] in a fragment", map[string]string{"city.toml": "include = [\"frag.toml\"]\n", "frag.toml": "\ufeff[storage]\n"}, true},
		{"fragment does not parse, no storage text", map[string]string{"city.toml": "include = [\"frag.toml\"]\n", "frag.toml": "[[[\n"}, false},
		{"served-binding note", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n", ".gc/storage-served-binding.json": `{"binding":"infra","provider":"sqlite-beads","location":"/x"}`}, true},
		// Mutant N22: the cleared note is evidence too.
		{"cleared note", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n", ".gc/storage-infra-cleared.json": `{"binding":"infra","complete":true}`}, true},
		// The default binding root inside the city.
		{"binding root at .gc/store", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n", ".gc/store/infra.migrated": "infrastructure classes migrated\n"}, true},
		{"binding database at .gc/store", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n", ".gc/store/graph/beads.sqlite": "SQLite format 3\x00"}, true},
		{"unknown entry beside the legacy sessions database", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n", ".gc/store/sessions.db": "SQLite format 3\x00", ".gc/store/other": ""}, true},
		// Review: .gc/store is also where a single-store city keeps the legacy
		// sessions database, which is no binding, so a syntax error there still
		// gets the parse exemption.
		{"legacy sessions database only at .gc/store", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n", ".gc/store/sessions.db": "SQLite format 3\x00", ".gc/store/sessions.db-wal": "", ".gc/store/sessions.db-shm": "", ".gc/store/sessions.db-journal": ""}, false},
		{"legacy sessions database only, root does not parse", map[string]string{"city.toml": "[workspace\nname = \n", ".gc/store/sessions.db": "SQLite format 3\x00"}, false},
		{"empty .gc/store directory", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n", ".gc/store/": ""}, false},
		{".gc/store is a file", map[string]string{"city.toml": "[workspace]\nname = \"c\"\n", ".gc/store": ""}, true},
		// Mutant N20: an include of a shape this scan cannot read.
		{"include is not a list of strings", map[string]string{"city.toml": "include = 5\n[workspace]\nname = \"c\"\n"}, true},
		{"include list holds a non-string", map[string]string{"city.toml": "include = [\"a.toml\", 5]\n"}, true},
		// Re-review: a quoted key spelled with an escape in an unparseable root
		// or fragment ("incl\u0075de", [ "stor\u0061ge" ]).
		{"root does not parse, escaped include key", map[string]string{"city.toml": "\"incl\\u0075de\" = [\"x.toml\"]\nthis is not toml\n"}, true},
		{"root does not parse, escaped storage header", map[string]string{"city.toml": "[ \"stor\\u0061ge\".classes ]\nthis is not toml\n"}, true},
		{"fragment does not parse, escaped storage header", map[string]string{"city.toml": "include = [\"frag.toml\"]\n", "frag.toml": "[\"stor\\u0061ge\"]\n[[[\n"}, true},
		// The BOM is stripped before parsing: a parseable root behind a BOM whose
		// only fragment is silent is single-store (unstripped, the parse fails
		// and the bare "include" would over-refuse).
		{"BOM before a parseable root with a silent fragment", map[string]string{"city.toml": "\ufeffinclude = [\"frag.toml\"]\n[workspace]\nname = \"c\"\n", "frag.toml": "[workspace]\nname = \"c\"\n"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				path := filepath.Join(dir, name)
				if strings.HasSuffix(name, "/") {
					// A directory where a file belongs: present, but unreadable.
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := cityMayRelocateClasses(dir); got != tc.want {
				t.Errorf("cityMayRelocateClasses = %v, want %v", got, tc.want)
			}
		})
	}
}

// Review nit: within the unsettled window a one-shot command's many class
// resolvers reuse one answer (one config load, one refusal on stderr), and
// after it the answer is resolved again.
func TestCLIStorageRoutesReuseAnUnsettledAnswerBriefly(t *testing.T) {
	cityPath, _ := migratedOneShotCLICity(t)
	stderr := captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	prev := cliStorageRoutesUnsettledTTL
	cliStorageRoutesUnsettledTTL = time.Hour
	t.Cleanup(func() { cliStorageRoutesUnsettledTTL = prev })
	repair := breakSplitCityConfig(t, cityPath)

	first := cliStorageRoutes(cityPath)
	for i := 0; i < 5; i++ {
		if again := cliStorageRoutes(cityPath); again != first {
			t.Fatalf("call %d re-resolved inside the unsettled window", i)
		}
	}
	if n := strings.Count(stderr.String(), "refusing to route"); n != 1 {
		t.Errorf("the refusal was printed %d times, want once:\n%s", n, stderr.String())
	}

	repair()
	cliStorageRoutesUnsettledTTL = 0
	clean := filepath.Clean(cityPath)
	cliStorageRoutesMu.Lock()
	if entry := cliStorageRoutesByCity[clean]; entry != nil {
		entry.expires = time.Now().Add(-time.Second)
	}
	cliStorageRoutesMu.Unlock()
	if after := cliStorageRoutes(cityPath); after == first {
		t.Error("an expired unsettled answer was reused")
	}
	if _, err := cliSessionStore(beads.NewMemStore(), nil, cityPath).List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Errorf("after the window and the repair the session class still refuses: %v", err)
	}
}

// M27: a load that fails only because the builtin readiness pass had not run
// (a cold pack cache) is retried once after that pass, and then serves.
func TestCLIStorageRoutesRetryTheLoadAfterTheBuiltinReadinessPass(t *testing.T) {
	cityPath, _ := migratedOneShotCLICity(t)
	stderr := captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	breakSplitCityConfig(t, cityPath) // includes missing-fragment.toml
	prev := cliStorageRoutesReadyBuiltins
	readied := 0
	cliStorageRoutesReadyBuiltins = func(_ fsys.FS, tomlPath string, _ io.Writer) error {
		readied++
		// What the readiness pass does for a cold cache: put the missing
		// composition input in place.
		return os.WriteFile(filepath.Join(filepath.Dir(tomlPath), "missing-fragment.toml"), []byte("# prepared\n"), 0o644)
	}
	t.Cleanup(func() { cliStorageRoutesReadyBuiltins = prev })

	sessions := cliSessionStore(beads.NewMemStore(), nil, cityPath)
	if _, err := sessions.List(beads.ListQuery{AllowScan: true}); err != nil {
		t.Errorf("the load that the readiness pass would fix was refused: %v", err)
	}
	if readied != 1 {
		t.Errorf("the readiness pass ran %d times, want 1", readied)
	}
	if strings.Contains(stderr.String(), "refusing to route") {
		t.Errorf("a refusal was printed for a load the retry fixed: %q", stderr.String())
	}
}

// M29: a city.toml that does not exist is not a load failure. With no
// served-binding note the absent-section verdict applies (identity routes, no
// refusal); with one, the gate's revert hold refuses, not the config-load
// refusal.
func TestCLIStorageRoutesTreatAMissingCityTOMLAsNoStorage(t *testing.T) {
	expireUnsettledCLIRoutesImmediately(t)
	cityPath := t.TempDir()
	resetCLIStorageRoutes(t)
	stderr := captureCLIStorageStderr(t)
	if routes := cliStorageRoutes(cityPath); routes != nil {
		t.Errorf("a city with no city.toml resolved routes %+v, want none", routes)
	}
	if stderr.Len() != 0 {
		t.Errorf("a city with no city.toml wrote %q", stderr.String())
	}

	served := t.TempDir()
	if err := os.MkdirAll(filepath.Join(served, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeBornSplitServedNote(served, bornSplitServedNote{Binding: "infra", Provider: config.StorageProviderSQLiteBeads, Location: "/x"}); err != nil {
		t.Fatal(err)
	}
	_, err := cliSessionStore(beads.NewMemStore(), nil, served).List(beads.ListQuery{AllowScan: true})
	if err == nil {
		t.Fatal("a city that served a split and lost its city.toml routed sessions to the work store")
	}
	if strings.Contains(err.Error(), "failed to load") {
		t.Errorf("a missing city.toml was reported as a load failure: %v", err)
	}
}

// Mutant N30: each distinct refusal is printed once per process, even when the
// answer is re-resolved every call.
func TestCLIStorageRefusalIsPrintedOncePerProcess(t *testing.T) {
	expireUnsettledCLIRoutesImmediately(t)
	cityPath, _ := migratedOneShotCLICity(t)
	stderr := captureCLIStorageStderr(t)
	if err := closeCLIStorageRoutes(); err != nil {
		t.Fatal(err)
	}
	breakSplitCityConfig(t, cityPath)
	for i := 0; i < 3; i++ {
		_ = cliStorageRoutes(cityPath)
	}
	if n := strings.Count(stderr.String(), "refusing to route"); n != 1 {
		t.Errorf("the refusal was printed %d times across three re-resolutions, want once:\n%s", n, stderr.String())
	}
}
