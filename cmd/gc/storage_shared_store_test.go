package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
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
	if code, _, stderr := f.importRigs(); code == 0 || !strings.Contains(stderr, "controller is live") {
		t.Fatalf("an attested import with a live co-tenant = %d %s", code, stderr)
	}
	held.Close() //nolint:errcheck // the co-tenant stops

	if code, _, stderr := f.importRigs(); code != 0 {
		t.Fatalf("an attested import with the co-tenant stopped = %d %s", code, stderr)
	}
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, msg := run(tc.self, tc.theirs)
			if (n > 0) != tc.shared || !strings.Contains(msg, tc.want) {
				t.Fatalf("sharers=%d %q, want shared=%v naming %q", n, msg, tc.shared, tc.want)
			}
		})
	}
}

// TestStoreDoltIdentityReadsHostedBd: a hosted bd scope is its
// storage_database at its storage_endpoint, credentials and host spelling
// folded, so two cities on one hosted database compare equal.
func TestStoreDoltIdentityReadsHostedBd(t *testing.T) {
	write := func(endpoint string) string {
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		meta := fmt.Sprintf(`{"backend":"hosted","storage_endpoint":%q,"storage_database":"prj_x"}`, endpoint)
		if err := os.WriteFile(scopeMetadataJSONPath(root), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
		return root
	}
	a := storeDoltIdentityOf(t.TempDir(), write("https://user:secret@localhost:8443/"))
	b := storeDoltIdentityOf(t.TempDir(), write("https://127.0.0.1:8443"))
	if a.Database != "prj_x" || a.Endpoint == "" || a != b {
		t.Fatalf("hosted identities = %+v / %+v, want one store", a, b)
	}
	if strings.Contains(a.Endpoint, "secret") {
		t.Errorf("the identity carries credentials: %q", a.Endpoint)
	}
	c := storeDoltIdentityOf(t.TempDir(), write("https://127.0.0.1:9443"))
	if c == a {
		t.Error("another hosted server compared equal")
	}
}
