package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// minted city-scope database names look like city_<8 hex>.
var mintedCityDoltDatabaseRE = regexp.MustCompile(`^city_[0-9a-f]{8}$`)

func writeCityDoltDatabaseIDForTest(t *testing.T, cityPath, content string) {
	t.Helper()
	path := cityDoltDatabaseIDPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A fresh city (no .beads identity) is minted a per-city id at init, and the
// city-scope default derived from it is not the shared literal "hq" and does
// not change between calls.
func TestMintCityDoltDatabaseIDGivesFreshCityStableNonHQDefault(t *testing.T) {
	cityPath := t.TempDir()

	if err := mintCityDoltDatabaseID(cityPath); err != nil {
		t.Fatalf("mintCityDoltDatabaseID: %v", err)
	}

	first := defaultScopeDoltDatabase(cityPath, cityPath, "gc")
	if !mintedCityDoltDatabaseRE.MatchString(first) {
		t.Fatalf("defaultScopeDoltDatabase(city) = %q, want city_<8 hex>", first)
	}
	if second := defaultScopeDoltDatabase(cityPath, cityPath, "gc"); second != first {
		t.Fatalf("default re-rolled between calls: %q then %q", first, second)
	}

	// A second mint (re-run init, retry after a blocked finalize) must keep
	// the id it already wrote.
	if err := mintCityDoltDatabaseID(cityPath); err != nil {
		t.Fatalf("second mintCityDoltDatabaseID: %v", err)
	}
	if again := defaultScopeDoltDatabase(cityPath, cityPath, "gc"); again != first {
		t.Fatalf("second mint changed the default: %q -> %q", first, again)
	}
}

// The point of the change: two cities pointed at one Dolt server no longer
// collide on the city-scope database name by construction.
func TestMintCityDoltDatabaseIDDiffersAcrossCities(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, city := range []string{a, b} {
		if err := mintCityDoltDatabaseID(city); err != nil {
			t.Fatalf("mintCityDoltDatabaseID(%s): %v", city, err)
		}
	}
	got, other := defaultScopeDoltDatabase(a, a, "gc"), defaultScopeDoltDatabase(b, b, "gc")
	if got == other {
		t.Fatalf("two fresh cities share city-scope database %q", got)
	}
}

// Every pre-existing city has no minted id, so it keeps defaulting to "hq":
// no migration, no backfill.
func TestDefaultScopeDoltDatabaseWithoutMintedIDStaysHQ(t *testing.T) {
	cityPath := t.TempDir()
	if got := defaultScopeDoltDatabase(cityPath, cityPath, "gc"); got != "hq" {
		t.Fatalf("defaultScopeDoltDatabase(city without id) = %q, want hq", got)
	}
}

// A city that already carries a beads identity is an existing city. Minting
// must not give it an id, or a metadata-deleted city (which self-heals to
// "hq" today) would silently rebind to an empty database.
func TestMintCityDoltDatabaseIDSkipsCityWithPersistedBeadsIdentity(t *testing.T) {
	for _, name := range []string{"metadata.json", "config.yaml"} {
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(cityPath, ".beads", name), []byte("{}"), 0o644); err != nil {
				t.Fatal(err)
			}

			if err := mintCityDoltDatabaseID(cityPath); err != nil {
				t.Fatalf("mintCityDoltDatabaseID: %v", err)
			}
			if _, err := os.Stat(cityDoltDatabaseIDPath(cityPath)); !os.IsNotExist(err) {
				t.Fatalf("existing city was minted an id (stat err = %v)", err)
			}
			if got := defaultScopeDoltDatabase(cityPath, cityPath, "gc"); got != "hq" {
				t.Fatalf("defaultScopeDoltDatabase(existing city) = %q, want hq", got)
			}
		})
	}
}

// A stamped metadata.json dolt_database keeps precedence over the minted
// default, including a stamp that is literally "hq".
func TestStampedDoltDatabaseOutranksMintedCityDefault(t *testing.T) {
	cityPath := t.TempDir()
	if err := mintCityDoltDatabaseID(cityPath); err != nil {
		t.Fatalf("mintCityDoltDatabaseID: %v", err)
	}
	minted := defaultScopeDoltDatabase(cityPath, cityPath, "gc")
	if minted == "hq" {
		t.Fatal("precondition: expected a minted non-hq default")
	}

	if got := canonicalScopeDoltDatabase(cityPath, cityPath, "gc"); got != minted {
		t.Fatalf("unstamped canonicalScopeDoltDatabase = %q, want minted default %q", got, minted)
	}

	for _, stamp := range []string{"hq", "legacy_db"} {
		if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		meta := `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"` + stamp + `"}`
		if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := canonicalScopeDoltDatabase(cityPath, cityPath, "gc"); got != stamp {
			t.Fatalf("canonicalScopeDoltDatabase with stamp %q = %q, want the stamp", stamp, got)
		}
	}
}

// Rig defaults are untouched by a minted city id, and still go through
// sanitizeDoltDatabaseName.
func TestMintedCityIDLeavesRigDefaultsUnchanged(t *testing.T) {
	cityPath := t.TempDir()
	if err := mintCityDoltDatabaseID(cityPath); err != nil {
		t.Fatalf("mintCityDoltDatabaseID: %v", err)
	}
	rigPath := filepath.Join(cityPath, "rigs", "001")
	for prefix, want := range map[string]string{"ga": "ga", "001": "r001", "g1": "g1"} {
		if got := defaultScopeDoltDatabase(cityPath, rigPath, prefix); got != want {
			t.Errorf("rig default for prefix %q = %q, want %q", prefix, got, want)
		}
	}
}

// The id file is read back at a trust boundary: anything that is not what
// mint writes is ignored rather than becoming a database name.
func TestDefaultScopeDoltDatabaseIgnoresMalformedMintedID(t *testing.T) {
	for name, content := range map[string]string{
		"empty":       "",
		"whitespace":  " \n",
		"non-hex":     "zzzzzzzz\n",
		"too short":   "abc123\n",
		"too long":    "0123456789abcdef\n",
		"injection":   "1;drop db\n",
		"upper-hex":   "ABCDEF01\n",
		"path-escape": "../../x\n",
	} {
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			writeCityDoltDatabaseIDForTest(t, cityPath, content)
			if got := defaultScopeDoltDatabase(cityPath, cityPath, "gc"); got != "hq" {
				t.Fatalf("defaultScopeDoltDatabase with id %q = %q, want hq", content, got)
			}
		})
	}
}

// A well-formed id written by mint is read back whatever its trailing
// newline, and yields the documented city_<id> name.
func TestDefaultScopeDoltDatabaseReadsWellFormedMintedID(t *testing.T) {
	cityPath := t.TempDir()
	writeCityDoltDatabaseIDForTest(t, cityPath, "0123abcd\n")
	if got := defaultScopeDoltDatabase(cityPath, cityPath, "gc"); got != "city_0123abcd" {
		t.Fatalf("defaultScopeDoltDatabase = %q, want city_0123abcd", got)
	}
}

// First init of a fresh city stamps the minted default into metadata.json, so
// the name survives losing the id file.
func TestNormalizeCanonicalBdScopeFilesForInitStampsMintedCityDatabase(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "bd")
	cityPath := t.TempDir()
	writeCityTOMLForBdProvider(t, cityPath)
	if err := mintCityDoltDatabaseID(cityPath); err != nil {
		t.Fatalf("mintCityDoltDatabaseID: %v", err)
	}
	want := defaultScopeDoltDatabase(cityPath, cityPath, "gc")
	if !mintedCityDoltDatabaseRE.MatchString(want) {
		t.Fatalf("precondition: city default = %q, want city_<8 hex>", want)
	}

	if err := normalizeCanonicalBdScopeFilesForInit(cityPath, cityPath, "gc", ""); err != nil {
		t.Fatalf("normalizeCanonicalBdScopeFilesForInit: %v", err)
	}
	if got := readScopeMetadataMap(t, cityPath)["dolt_database"]; got != want {
		t.Fatalf("metadata dolt_database = %v, want minted default %q", got, want)
	}
}

// All four gc init paths funnel through finalizeInit, so that is where a fresh
// city gets its id: before any readiness check that can stop init, so a
// retried init reuses the same id.
func TestFinalizeInitMintsCityDoltDatabaseIDForFreshCity(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	disableBootstrapForTests(t)
	stubInitDependencyChecks(t)
	stubInitDoltAuthorIdentity(t, map[string]string{})

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	if code := doInit(fsys.OSFS{}, cityPath, wizardConfig{configName: "minimal", provider: "claude"}, "", &initStdout, &initStderr, false); code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}
	var stdout, stderr bytes.Buffer
	// Blocked on the missing Dolt author identity: init stops, the id is
	// already minted.
	if code := finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{commandName: "gc init"}); code != 1 {
		t.Fatalf("finalizeInit = %d, want 1: %s", code, stderr.String())
	}
	first := defaultScopeDoltDatabase(cityPath, cityPath, "bl")
	if !mintedCityDoltDatabaseRE.MatchString(first) {
		t.Fatalf("after finalizeInit, city default = %q, want city_<8 hex>", first)
	}

	stdout.Reset()
	stderr.Reset()
	if code := finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{commandName: "gc init"}); code != 1 {
		t.Fatalf("retried finalizeInit = %d, want 1: %s", code, stderr.String())
	}
	if again := defaultScopeDoltDatabase(cityPath, cityPath, "bl"); again != first {
		t.Fatalf("retried finalizeInit re-rolled the id: %q -> %q", first, again)
	}
}

// Re-running init over an existing city (a persisted beads identity) mints
// nothing, so the city keeps its "hq" default.
func TestFinalizeInitDoesNotMintForCityWithBeadsIdentity(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "")
	disableBootstrapForTests(t)
	stubInitDependencyChecks(t)
	stubInitDoltAuthorIdentity(t, map[string]string{})

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var initStdout, initStderr bytes.Buffer
	if code := doInit(fsys.OSFS{}, cityPath, wizardConfig{configName: "minimal", provider: "claude"}, "", &initStdout, &initStderr, false); code != 0 {
		t.Fatalf("doInit = %d, want 0: %s", code, initStderr.String())
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	_ = finalizeInit(cityPath, &stdout, &stderr, initFinalizeOptions{commandName: "gc init"})
	if _, err := os.Stat(cityDoltDatabaseIDPath(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("finalizeInit minted an id for an existing city (stat err = %v)", err)
	}
	if got := canonicalScopeDoltDatabase(cityPath, cityPath, "bl"); got != "hq" {
		t.Fatalf("existing city's database = %q, want hq", got)
	}
}

// Concurrent mints for one fresh city converge on a single id: the write-once
// contract holds under a race, not just across sequential calls.
func TestMintCityDoltDatabaseIDConcurrentMintsConverge(t *testing.T) {
	cityPath := t.TempDir()

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	ids := make(chan string, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if err := mintCityDoltDatabaseID(cityPath); err != nil {
				errs <- err
				return
			}
			ids <- readCityDoltDatabaseID(cityPath)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(ids)
	for err := range errs {
		t.Fatalf("mintCityDoltDatabaseID: %v", err)
	}
	final := readCityDoltDatabaseID(cityPath)
	if final == "" {
		t.Fatal("no id was published")
	}
	for id := range ids {
		if id != final {
			t.Fatalf("a mint observed id %q, but the city ended with %q", id, final)
		}
	}
	entries, err := os.ReadDir(filepath.Join(cityPath, ".gc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf(".gc holds %d entries after the mints, want only dolt-database-id (temp files leaked)", len(entries))
	}
}

// A malformed id file still falls back to "hq" (the city keeps working), but
// the fallback is logged instead of silent.
func TestReadCityDoltDatabaseIDWarnsOnMalformedFile(t *testing.T) {
	cityPath := t.TempDir()
	path := cityDoltDatabaseIDPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not-an-id\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })

	if got := readCityDoltDatabaseID(cityPath); got != "" {
		t.Fatalf("readCityDoltDatabaseID = %q, want \"\" for a malformed file", got)
	}
	if got := defaultScopeDoltDatabase(cityPath, cityPath, "gc"); got != "hq" {
		t.Fatalf("defaultScopeDoltDatabase = %q, want the legacy hq fallback", got)
	}
	if !strings.Contains(buf.String(), "ignoring city dolt database id") {
		t.Fatalf("no warning logged for a malformed id file; log = %q", buf.String())
	}
}
