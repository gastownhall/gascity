// The default-city qualification for storage class-to-binding assignment. It
// answers the first question the feature has to answer: does a city with NO
// [storage] configuration still behave exactly as it does today, with every
// semantic class served by the reserved `work` binding?
//
// It needs no binding, no migration, and no provider registration — and the
// suite is written so that if any of those were required, it would fail rather
// than quietly acquire the dependency. It runs in process against real SQLite
// engines and real composed configuration; it starts no city process.
//
// Three rules shape everything here.
//
// The baseline is DEPLOYED behavior, not this suite's own output. Where a
// "what a no-storage city looks like" fact is needed, it is read from the
// example cities this repo ships (examples/*/city.toml) — artifacts that exist
// independently of the storage work — never from a config literal written to
// match the resolver.
//
// Every claim is observed through a read path the writing code never writes
// through. A class front door's effect is read back from the canonical engine
// (or from another class's front door), so a projection that quietly kept its
// writes to itself cannot pass.
//
// Every positive claim is paired with a control that must FAIL. A resolution
// that succeeds against an EMPTY provider registry proves the default path
// needs no provider only if the same call with a provider-backed binding is
// rejected; a cross-class read that finds a bead proves one shared binding only
// if the same read against two engines does not.

package qualification_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storebinding"
)

// q1RepoRoot returns the module root. Tests run with the package directory as
// their working directory, and this package lives at test/qualification.
func q1RepoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving the module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s is not the module root: %v", root, err)
	}
	return root
}

// q1ShippedCity is one city.toml this repo ships, loaded through the production
// composer. These are the deployed artifacts Q1 derives its baseline from.
type q1ShippedCity struct {
	Name string
	Path string
	City *config.City
}

// q1ShippedCities loads every example city in the repo through
// config.LoadWithIncludes — the same composer the binary uses, including its
// storage validation. A city that does not load is a failure, not a skip:
// silently dropping the corpus would leave the baseline empty and every
// assertion over it vacuous.
func q1ShippedCities(t *testing.T) []q1ShippedCity {
	t.Helper()
	// The composer resolves pack imports below GC_HOME. Point it at a scratch
	// directory so a qualification run never reads or writes the host's.
	t.Setenv("GC_HOME", t.TempDir())

	examples := filepath.Join(q1RepoRoot(t), "examples")
	entries, err := os.ReadDir(examples)
	if err != nil {
		t.Fatalf("reading the shipped example cities in %s: %v", examples, err)
	}
	var cities []q1ShippedCity
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(examples, entry.Name(), "city.toml")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		city, _, err := config.LoadWithIncludes(fsys.OSFS{}, path)
		if err != nil {
			t.Fatalf("loading the shipped city %s: %v", path, err)
		}
		cities = append(cities, q1ShippedCity{Name: entry.Name(), Path: path, City: city})
	}
	if len(cities) == 0 {
		t.Fatalf("no shipped example cities found below %s; the Q1 baseline corpus is empty", examples)
	}
	sort.Slice(cities, func(i, j int) bool { return cities[i].Name < cities[j].Name })
	return cities
}

// q1LoadCityFrom writes files below a fresh temp root and loads city.toml
// through the production composer. Keys are paths relative to that root, so a
// case can exercise include fragments as well as a single file.
func q1LoadCityFrom(t *testing.T, files map[string]string) *config.City {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	city, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(root, "city.toml"))
	if err != nil {
		t.Fatalf("loading the composed city below %s: %v", root, err)
	}
	return city
}

// q1MinimalNoStorageCity is the smallest city that loads and authors no
// [storage] section at all — the shape every pre-storage-classes city has.
const q1MinimalNoStorageCity = `
[workspace]
name = "q1-default"
`

// beadStoreCloser is the close half of an opened engine. beads.Store is a pure
// data contract with no Close, so the concrete engines carry it separately.
type beadStoreCloser interface{ CloseStore() error }

// q1Engine opens one real SQLite bead engine below a per-test temp root and
// closes it when the test ends. This is the canonical Beads engine a default
// city's classes are all served by.
func q1Engine(t *testing.T) beads.Store {
	t.Helper()
	dir := t.TempDir()
	store, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatalf("opening the SQLite bead engine below %s: %v", dir, err)
	}
	closer, ok := store.(beadStoreCloser)
	if !ok {
		t.Fatalf("the SQLite bead engine %T cannot be closed; every assertion would leak a handle", store)
	}
	t.Cleanup(func() {
		if err := closer.CloseStore(); err != nil {
			t.Errorf("closing the SQLite bead engine below %s: %v", dir, err)
		}
	})
	return store
}

// q1DefaultProjection projects one canonical engine into all six class front
// doors, the way a default no-storage city's composition root does: ONE
// already-open canonical store, one physical identity, the Beads-backed nudge
// queue bound as the queue front.
func q1DefaultProjection(t *testing.T, store beads.Store, physical string) storebinding.BeadsAdapters {
	t.Helper()
	queue, err := storebinding.NewBeadsNudgeQueue(beads.NudgesStore{Store: store})
	if err != nil {
		t.Fatalf("binding the beads nudge queue: %v", err)
	}
	adapters, err := storebinding.NewBeadsAdapters(store, q1Identity(physical), queue)
	if err != nil {
		t.Fatalf("projecting the canonical engine into the class front doors: %v", err)
	}
	return adapters
}

// q1Identity is the physical identity of one canonical Beads binding. The
// component is constant because a scope holds exactly one canonical ledger.
func q1Identity(physical string) storebinding.BeadsAdapterIdentity {
	return storebinding.BeadsAdapterIdentity{OpenerID: "beads", ComponentID: "beads", PhysicalID: physical}
}

// q1EmptyFrozenRegistry is a frozen provider registry with NOTHING registered.
// Resolving a plan against it succeeds only if the plan performs no provider
// lookup at all, which is precisely the claim Q1 makes about the default path.
func q1EmptyFrozenRegistry(t *testing.T) *storebinding.ProviderRegistry {
	t.Helper()
	registry := storebinding.NewProviderRegistry()
	if err := registry.Freeze(); err != nil {
		t.Fatalf("freezing the empty provider registry: %v", err)
	}
	return registry
}

// q1ConfigDigest builds a canonical secret-free digest for a pinned Work
// topology's configuration context.
func q1ConfigDigest(text string) storebinding.ConfigRefDigest {
	sum := sha256.Sum256([]byte(text))
	return storebinding.ConfigRefDigest("sha256:" + hex.EncodeToString(sum[:]))
}

// q1Pin builds one pinned Work scope. physical is the identity WorkTopology
// deduplicates on, so two scopes sharing a value are one physical workspace.
func q1Pin(scope storebinding.WorkScope, prefix, physical string) storebinding.WorkScopePin {
	return storebinding.WorkScopePin{
		Scope:       scope,
		Prefix:      prefix,
		OpenerID:    "beads",
		ComponentID: "beads",
		PhysicalID:  physical,
	}
}

// q1Suspended returns pin with the suspended flag set, so a case can state
// suspension without restating every other pinned fact.
func q1Suspended(pin storebinding.WorkScopePin) storebinding.WorkScopePin {
	pin.Suspended = true
	return pin
}

// q1DefaultPins is the bootstrap Work topology of a default city: an HQ
// workspace and its configured rigs, each on its own physical ledger.
func q1DefaultPins(hq storebinding.WorkScopePin, rigs ...storebinding.WorkScopePin) storebinding.WorkPinInputs {
	return storebinding.WorkPinInputs{
		ConfigContext: q1ConfigDigest("q1-default-city"),
		HQ:            hq,
		Rigs:          rigs,
	}
}

// q1ScopeNames renders scopes for failure messages in a stable order-preserving
// form, so a mismatch reports what the enumeration actually returned.
func q1ScopeNames(scopes []storebinding.WorkScope) string {
	names := make([]string, len(scopes))
	for index, scope := range scopes {
		names[index] = scope.String()
	}
	return strings.Join(names, ",")
}

// q1PinnedScopeNames is q1ScopeNames over pinned scopes.
func q1PinnedScopeNames(pinned []storebinding.PinnedWorkScope) string {
	scopes := make([]storebinding.WorkScope, len(pinned))
	for index, scope := range pinned {
		scopes[index] = scope.Scope
	}
	return q1ScopeNames(scopes)
}
