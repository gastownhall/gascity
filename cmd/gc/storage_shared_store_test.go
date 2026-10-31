package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
)

// TestRigImportVerbsHoldTheControllerLock: S4b's writing verbs take the
// controller lock through the storage-verb exclusion, like migrate does.
func TestRigImportVerbsHoldTheControllerLock(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	held, err := acquireControllerLock(f.request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck // test cleanup

	if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "controller lock") {
		t.Fatalf("--from-rigs under a held controller lock = %d, %s", code, stderr)
	}
	if n := len(rigTestList(t, f.binding())); n != 0 {
		t.Errorf("the refused import wrote %d binding row(s)", n)
	}
	var stdout, stderr bytes.Buffer
	if code := doStorageAbandonRigStore(context.Background(), f.request, "packs", &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "controller lock") {
		t.Fatalf("--abandon-rig-store under a held controller lock = %d, %s", code, stderr.String())
	}
}

// otherCityWithRig writes a registered city whose rig lives at rigPath.
func otherCityWithRig(t *testing.T, rigPath string) string {
	t.Helper()
	other := t.TempDir()
	toml := fmt.Sprintf("[workspace]\nname = \"other\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"theirs\"\npath = %q\n", rigPath)
	if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return other
}

// TestRigSharedStoreRefusalIsLiftedByTheSameAttestation: one
// --force=shared-store:<identity> lifts the rig importer's refusal too, and
// a live co-tenant still refuses.
func TestRigSharedStoreRefusalIsLiftedByTheSameAttestation(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	other := otherCityWithRig(t, filepath.Join(t.TempDir(), "elsewhere"))
	f.otherCities = []string{other}
	stubStoreIdentity(t, storeDoltIdentity{Database: "packs", Endpoint: "loopback:3399"})
	token := "packs@loopback:3399"

	code, _, stderr := f.importRigs()
	if code == 0 || !strings.Contains(stderr, "--force=shared-store:"+token) {
		t.Fatalf("a shared rig store was imported, or the refusal names no attestation: %d %s", code, stderr)
	}

	held, err := acquireControllerLock(other)
	if err != nil {
		if mkErr := os.MkdirAll(filepath.Join(other, ".gc"), 0o755); mkErr != nil {
			t.Fatal(mkErr)
		}
		if held, err = acquireControllerLock(other); err != nil {
			t.Fatal(err)
		}
	}
	f.request.Force = storageForce{SharedStores: []string{token}}
	if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "holds its controller lock") {
		t.Fatalf("an attested import with a live co-tenant = %d %s", code, stderr)
	}
	held.Close() //nolint:errcheck // the co-tenant stops

	// Unregistered (`gc stop` does that), the co-tenant is invisible to the
	// check, and the attestation, which now lifts nothing, refuses.
	f.otherCities = nil
	if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "no other registered city") {
		t.Fatalf("an attested import with the co-tenant unregistered = %d %s", code, stderr)
	}
	logPath := filepath.Join(f.request.CityPath, ".gc", storageAttestationLog)
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an attestation that lifted no rig's refusal was recorded (stat: %v)", err)
	}
	f.otherCities = []string{other}

	if code, _, stderr := f.importRigs(); code != 0 {
		t.Fatalf("an attested import with the co-tenant stopped = %d %s", code, stderr)
	}
	if log, err := os.ReadFile(logPath); err != nil || !strings.Contains(string(log), "#gc:attested "+storageHazardSharedStore+":"+token+" ") {
		t.Errorf("the attestation that lifted rig packs's refusal was not recorded: %q, %v", log, err)
	}
}

// TestRigImportSharedStoreAttestationKeepsTheCoTenantStopped: the importer
// keeps an attested co-tenant stopped for the rig's import, as the city
// migration does. A start of the co-tenant after the shared-store check passed
// (create its .gc, take its controller lock; it never started, so it had no
// .gc) is excluded, and the lock is free once the import returns.
func TestRigImportSharedStoreAttestationKeepsTheCoTenantStopped(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	other := otherCityWithRig(t, filepath.Join(t.TempDir(), "elsewhere"))
	f.otherCities = []string{other}
	stubStoreIdentity(t, storeDoltIdentity{Database: "packs", Endpoint: "loopback:3399"})
	f.request.Force = storageForce{SharedStores: []string{"packs@loopback:3399"}}
	probes, admitted := 0, 0
	prevHash := rigImportHashConfigOpener
	rigImportHashConfigOpener = func(cityPath string, cfg *config.City, rig config.Rig) (rigHashLengthConfig, error) {
		probes++
		if err := os.MkdirAll(filepath.Join(other, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		start, err := acquireControllerLock(other)
		if err == nil {
			admitted++
			start.Close() //nolint:errcheck // the co-tenant's start got the lock
		} else if !errors.Is(err, errControllerAlreadyRunning) {
			t.Errorf("starting the co-tenant mid-import: %v", err)
		}
		return prevHash(cityPath, cfg, rig)
	}
	t.Cleanup(func() { rigImportHashConfigOpener = prevHash })

	if code, _, stderr := f.importRigs(); code != 0 {
		t.Fatalf("an attested import with the co-tenant stopped = %d %s", code, stderr)
	}
	if probes == 0 {
		t.Fatal("the import never reached the step after the shared-store check; nothing was proven")
	}
	if admitted > 0 {
		t.Fatalf("co-tenant %s took its controller lock mid-import", other)
	}
	after, err := acquireControllerLock(other)
	if err != nil {
		t.Fatalf("the import left co-tenant %s's controller lock held: %v", other, err)
	}
	after.Close() //nolint:errcheck // test cleanup
}

// TestRigStoreIsComparedWithOtherCitiesCityStores: a rig store that is
// another city's CITY store (not one of its rigs) is a shared store.
func TestRigStoreIsComparedWithOtherCitiesCityStores(t *testing.T) {
	f := newRigImportFixture(t, config.Rig{Name: "packs", Prefix: "gp"})
	f.seed("packs", []beads.Bead{graphRow("gp-a", "gp-a", "closed", nil)}, nil)
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte("[workspace]\nname = \"other\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.otherCities = []string{other}
	self := rigStoreRoot(f.request.CityPath, f.cfg.Rigs[0])
	prev := storeDoltIdentityOf
	t.Cleanup(func() { storeDoltIdentityOf = prev })
	storeDoltIdentityOf = func(_, root string) storeDoltIdentity {
		if samePath(root, self) || samePath(root, other) {
			return storeDoltIdentity{Database: "hq", Endpoint: "loopback:3307"}
		}
		return storeDoltIdentity{}
	}
	if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "city store") {
		t.Fatalf("a rig on another city's city database was imported: %d %s", code, stderr)
	}
}

// TestSharedStoreComparatorRules pins the one comparator both refusals use.
func TestSharedStoreComparatorRules(t *testing.T) {
	city := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte("[workspace]\nname = \"other\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubOtherCities(t, other)
	run := func(self, theirs storeDoltIdentity) (int, string) {
		prev := storeDoltIdentityOf
		storeDoltIdentityOf = func(cityPath, _ string) storeDoltIdentity {
			if cityPath == other {
				return theirs
			}
			return self
		}
		defer func() { storeDoltIdentityOf = prev }()
		_, sharers, err := findStoreSharers(city, "this city's work store", city)
		if err != nil {
			t.Fatal(err)
		}
		msg := ""
		if len(sharers) > 0 {
			msg = sharers[0].Refusal
		}
		return len(sharers), msg
	}
	for _, tc := range []struct {
		name         string
		self, theirs storeDoltIdentity
		shared       bool
		want         string
	}{
		{"same database and endpoint", storeDoltIdentity{Database: "hq", Endpoint: "loopback:3307"}, storeDoltIdentity{Database: "hq", Endpoint: "loopback:3307"}, true, "Dolt database hq"},
		{"same name, other server", storeDoltIdentity{Database: "hq", Endpoint: "loopback:3307"}, storeDoltIdentity{Database: "hq", Endpoint: "loopback:3407"}, false, ""},
		{"their endpoint unresolved, same name", storeDoltIdentity{Database: "hq", Endpoint: "loopback:3307"}, storeDoltIdentity{Database: "hq", Unresolved: true, Err: errors.New("down")}, true, "unregister"},
		{"their endpoint unresolved, other name", storeDoltIdentity{Database: "hq", Endpoint: "loopback:3307"}, storeDoltIdentity{Database: "zz", Unresolved: true, Err: errors.New("down")}, false, ""},
		{"self unresolved with no pin", storeDoltIdentity{Unresolved: true, Err: errors.New("down")}, storeDoltIdentity{Database: "hq", Endpoint: "loopback:3307"}, true, "pins no dolt_database"},
		{"a file store", storeDoltIdentity{}, storeDoltIdentity{Database: "hq", Endpoint: "loopback:3307"}, false, ""},
		{"two managed servers, one database name", storeDoltIdentity{Database: "hq", Endpoint: "managed:/a/.beads/dolt", ServedAt: "loopback:3307"}, storeDoltIdentity{Database: "hq", Endpoint: "managed:/b/.beads/dolt", ServedAt: "loopback:3407"}, false, ""},
		{"two managed servers that recorded one port", storeDoltIdentity{Database: "hq", Endpoint: "managed:/a/.beads/dolt", ServedAt: "loopback:3307"}, storeDoltIdentity{Database: "hq", Endpoint: "managed:/b/.beads/dolt", ServedAt: "loopback:3307"}, false, ""},
		{"their endpoint at this managed server's recorded address", storeDoltIdentity{Database: "hq", Endpoint: "managed:/a/.beads/dolt", ServedAt: "loopback:3307"}, storeDoltIdentity{Database: "hq", Endpoint: "loopback:3307"}, true, "managed:/a/.beads/dolt (served at loopback:3307)"},
		{"this endpoint at their managed server's recorded address", storeDoltIdentity{Database: "hq", Endpoint: "loopback:3307"}, storeDoltIdentity{Database: "hq", Endpoint: "managed:/b/.beads/dolt", ServedAt: "loopback:3307"}, true, "loopback:3307 (where managed:/b/.beads/dolt is served)"},
		{"their endpoint elsewhere than this managed server", storeDoltIdentity{Database: "hq", Endpoint: "managed:/a/.beads/dolt", ServedAt: "loopback:3307"}, storeDoltIdentity{Database: "hq", Endpoint: "loopback:3407"}, false, ""},
		{"their other database on this managed server", storeDoltIdentity{Database: "hq", Endpoint: "managed:/a/.beads/dolt", ServedAt: "loopback:3307"}, storeDoltIdentity{Database: "gp", Endpoint: "loopback:3307"}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, msg := run(tc.self, tc.theirs)
			if (n > 0) != tc.shared || !strings.Contains(msg, tc.want) {
				t.Fatalf("sharers=%d %q, want shared=%v naming %q", n, msg, tc.shared, tc.want)
			}
		})
	}
}

// writeHostedScope makes root a hosted bd scope on database prj_x at
// endpoint.
func writeHostedScope(t *testing.T, root, endpoint string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"backend":"hosted","storage_endpoint":%q,"storage_database":"prj_x"}`, endpoint)
	if err := os.WriteFile(scopeMetadataJSONPath(root), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestStoreDoltIdentityReadsHostedBd: a hosted bd scope is its
// storage_database at its storage_endpoint, credentials, host spelling and
// port spelling folded, so two cities on one hosted database compare equal.
func TestStoreDoltIdentityReadsHostedBd(t *testing.T) {
	identity := func(endpoint string) storeDoltIdentity {
		root := t.TempDir()
		writeHostedScope(t, root, endpoint)
		return storeDoltIdentityOf(t.TempDir(), root)
	}
	a := identity("https://user:secret@localhost:8443/")
	b := identity("https://127.0.0.1:8443")
	if a.Database != "prj_x" || a.Endpoint == "" || a != b {
		t.Fatalf("hosted identities = %+v / %+v, want one store", a, b)
	}
	if strings.Contains(a.Endpoint, "secret") {
		t.Errorf("the identity carries credentials: %q", a.Endpoint)
	}
	c := identity("https://127.0.0.1:9443")
	if c == a {
		t.Error("another hosted server compared equal")
	}

	// A scheme's default port names the same server spelled or implied, and so
	// does a port number spelled with a leading zero.
	for _, pair := range [][2]string{
		{"https://127.0.0.1", "https://127.0.0.1:443"},
		{"https://localhost/", "https://user:secret@127.0.0.1:443/"},
		{"http://127.0.0.1", "http://127.0.0.1:80"},
		{"https://127.0.0.1:8443", "https://127.0.0.1:08443"},
		{"https://127.0.0.1", "https://127.0.0.1:"},
	} {
		if x, y := identity(pair[0]), identity(pair[1]); x != y {
			t.Errorf("%s and %s are one hosted server, but their identities differ: %+v / %+v", pair[0], pair[1], x, y)
		}
	}
	// A port that is not the scheme's default names another server, and so
	// does the other scheme on the same host.
	for _, pair := range [][2]string{
		{"https://127.0.0.1", "https://127.0.0.1:8443"},
		{"http://127.0.0.1", "http://127.0.0.1:8080"},
		{"https://127.0.0.1", "https://127.0.0.1:80"},
		{"http://127.0.0.1", "http://127.0.0.1:443"},
		{"http://127.0.0.1", "https://127.0.0.1"},
	} {
		if x, y := identity(pair[0]), identity(pair[1]); x == y {
			t.Errorf("%s and %s are different hosted servers, but their identities are equal: %+v", pair[0], pair[1], x)
		}
	}
}

// TestStorageMigrateTreatsAHostedDefaultPortAliasAsOneStore: two cities on
// one hosted database, one endpoint spelling the scheme's default port and the
// other implying it, share a store. Read from their real metadata, the
// migration refuses it unattested, naming the other city, and the attestation
// it names reaches the co-tenant lock: refused while the co-tenant holds its
// controller lock, accepted once it is stopped.
func TestStorageMigrateTreatsAHostedDefaultPortAliasAsOneStore(t *testing.T) {
	request, _ := exclusionCity(t)
	writeHostedScope(t, request.CityPath, "https://127.0.0.1")
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte("[workspace]\nname = \"other-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(other, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeHostedScope(t, other, "https://127.0.0.1:443")
	stubOtherCities(t, other)
	const token = "prj_x@hosted:https://loopback"

	code, stderr := migrateFor(t, request)
	if code == 0 || !strings.Contains(stderr, other) || !strings.Contains(stderr, "--force=shared-store:"+token) {
		t.Fatalf("migration over a hosted store another city names with its default port = %d, stderr %q; want the shared-store refusal naming %s and the attestation", code, stderr, other)
	}

	request.Force = storageForce{SharedStores: []string{token}}
	held, err := acquireControllerLock(other)
	if err != nil {
		t.Fatal(err)
	}
	code, stderr = migrateFor(t, request)
	held.Close() //nolint:errcheck // the co-tenant stops
	if code == 0 || !strings.Contains(stderr, "holds its controller lock") {
		t.Fatalf("attested migration while the co-tenant holds its controller lock = %d, stderr %q; want the co-tenant's lock refusal", code, stderr)
	}
	if code, stderr := migrateFor(t, request); code != 0 {
		t.Fatalf("attested migration with the co-tenant stopped = %d: %s", code, stderr)
	}
}

// newOtherCity writes another city's directory: a config the shared-store
// check can read, and the .gc its controller lock lives in.
func newOtherCity(t *testing.T) string {
	t.Helper()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "city.toml"), []byte("[workspace]\nname = \"other-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(other, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	return other
}

// writeManagedDoltCity makes root a city whose store is database hq on the
// Dolt server gc manages for it: gc's default topology, with gc's managed_city
// marker.
func writeManagedDoltCity(t *testing.T, root string) {
	t.Helper()
	writeScopeBeadsMetadata(t, root, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)
	writeCanonicalScopeConfig(t, root, contract.ConfigState{IssuePrefix: "hq", EndpointOrigin: contract.EndpointOriginManagedCity, EndpointStatus: contract.EndpointStatusVerified})
}

// writeExplicitDoltCity makes root a city whose store is database hq on the
// Dolt server its config names at 127.0.0.1:port.
func writeExplicitDoltCity(t *testing.T, root string, port int) {
	t.Helper()
	writeScopeBeadsMetadata(t, root, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`)
	writeCanonicalScopeConfig(t, root, contract.ConfigState{IssuePrefix: "hq", EndpointOrigin: contract.EndpointOriginCityCanonical, EndpointStatus: contract.EndpointStatusVerified, DoltHost: "127.0.0.1", DoltPort: strconv.Itoa(port)})
}

// stopManagedDolt leaves a city's managed Dolt as `gc stop` does: the
// published runtime state removed, and the provider's state not running and
// keeping the port.
func stopManagedDolt(t *testing.T, cityPath string, port int) {
	t.Helper()
	if err := os.Remove(managedDoltStatePath(cityPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := writeDoltRuntimeStateFile(providerManagedDoltStatePath(cityPath), doltRuntimeState{Port: port, DataDir: filepath.Join(cityPath, ".beads", "dolt")}); err != nil {
		t.Fatal(err)
	}
}

// TestManagedStoreIsItsDataDirectoryLiveOrStopped: gc's default topology, two
// cities each on the Dolt server gc manages for it, both databases named hq.
// Each server serves its own city's .beads/dolt, so neither store is the
// other's, live or stopped. This city's identity, the attestation token, names
// that data directory throughout: preflight runs with the city live and the
// writing verbs with it stopped. A rig on the city's managed server is that
// server's too. Identities are read from the cities' real metadata.
func TestManagedStoreIsItsDataDirectoryLiveOrStopped(t *testing.T) {
	city, other := t.TempDir(), newOtherCity(t)
	writeManagedDoltCity(t, city)
	writeManagedDoltCity(t, other)
	rig := t.TempDir()
	writeScopeBeadsMetadata(t, rig, `{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"gp"}`)
	writeCanonicalScopeConfig(t, rig, contract.ConfigState{IssuePrefix: "gp", EndpointOrigin: contract.EndpointOriginInheritedCity, EndpointStatus: contract.EndpointStatusVerified})
	stubOtherCities(t, other)
	cityPort := writeReachableManagedDoltState(t, city)
	otherPort := writeReachableManagedDoltState(t, other)

	endpoint := managedDoltEndpointPrefix + canonicalIdentityPath(filepath.Join(city, ".beads", "dolt"))
	want := map[string]storeDoltIdentity{
		city: {Database: "hq", Endpoint: endpoint, ServedAt: fmt.Sprintf("loopback:%d", cityPort)},
		rig:  {Database: "gp", Endpoint: endpoint, ServedAt: fmt.Sprintf("loopback:%d", cityPort)},
	}
	check := func(state string) {
		t.Helper()
		for root, id := range want {
			if got := storeDoltIdentityOf(city, root); got != id {
				t.Errorf("%s: identity of %s = %+v, want %+v", state, root, got, id)
			}
		}
		token, sharers, err := findStoreSharers(city, "this city's work store", city)
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if len(sharers) != 0 {
			t.Errorf("%s: another city's managed server counts as this city's store: %+v", state, sharers)
		}
		if token != "hq@"+endpoint {
			t.Errorf("%s: token = %q, want %q", state, token, "hq@"+endpoint)
		}
	}
	check("both servers up")
	stopManagedDolt(t, city, cityPort)
	check("this city's server stopped")
	stopManagedDolt(t, other, otherPort)
	check("both servers stopped")
}
