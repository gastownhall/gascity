package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// R10 (split-default read audit): an ordinary city or rig name derives a
// reserved class prefix — gas-city-gateway -> gcg, gas-city-sandbox -> gcs,
// gas-city-main -> gcm, gas-city-ops -> gco, gas-city-nightly -> gcn. On a split
// city the binding mints gcg-N, gcs-N, ... in the same namespace as that work
// store, so ids collide and the binding wins every by-id read. Boot only warned.

func splitCityNamed(name string, rigs ...Rig) *City {
	return &City{
		Workspace: Workspace{Name: name},
		Rigs:      rigs,
		Storage: &StorageConfig{
			Classes: StorageClasses{
				Work: StorageWorkBinding, Graph: "infra", Sessions: "infra",
				Messaging: "infra", Orders: "infra", Nudges: "infra",
			},
			Bindings: map[string]StorageBindingConfig{"infra": {Provider: StorageProviderSQLiteBeads}},
		},
	}
}

func TestReservedPrefixSplitRefusalForEveryDerivedCollision(t *testing.T) {
	for name, prefix := range map[string]string{
		"gas-city-gateway": "gcg", "gas-city-sandbox": "gcs", "gas-city-main": "gcm",
		"gas-city-ops": "gco", "gas-city-nightly": "gcn",
	} {
		t.Run(name, func(t *testing.T) {
			err := ReservedPrefixSplitRefusal(splitCityNamed(name))
			if err == nil {
				t.Fatalf("a split city named %q (HQ prefix %q) was not refused", name, prefix)
			}
			for _, want := range []string{prefix, "workspace_prefix", "reserved"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal %q does not mention %q", err, want)
				}
			}
		})
	}
}

func TestReservedPrefixSplitRefusalNamesTheRigAndItsFix(t *testing.T) {
	err := ReservedPrefixSplitRefusal(splitCityNamed("my-city", Rig{Name: "gas-city-gateway"}))
	if err == nil {
		t.Fatal("a split city with a rig deriving gcg was not refused")
	}
	for _, want := range []string{`"gas-city-gateway"`, "gcg", "prefix ="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}

func TestReservedPrefixSplitRefusalAcceptsAnExplicitPrefix(t *testing.T) {
	cfg := splitCityNamed("gas-city-gateway", Rig{Name: "gas-city-ops", Prefix: "ops"})
	cfg.Workspace.Prefix = "gw"
	if err := ReservedPrefixSplitRefusal(cfg); err != nil {
		t.Errorf("explicit non-reserved prefixes were refused: %v", err)
	}
	cfg.ResolvedWorkspacePrefix = "gcg"
	if err := ReservedPrefixSplitRefusal(cfg); err == nil {
		t.Error("a site-bound reserved workspace prefix was not refused")
	}
}

// A city that relocates nothing keeps today's advisory: no refusal.
func TestReservedPrefixSplitRefusalLeavesSingleStoreCitiesToTheWarning(t *testing.T) {
	single := &City{Workspace: Workspace{Name: "gas-city-gateway"}, Rigs: []Rig{{Name: "gas-city-ops"}}}
	if err := ReservedPrefixSplitRefusal(single); err != nil {
		t.Errorf("a single-store city was refused: %v", err)
	}
	allWork := splitCityNamed("gas-city-gateway")
	allWork.Storage = &StorageConfig{Classes: defaultStorageConfig().Classes}
	if err := ReservedPrefixSplitRefusal(allWork); err != nil {
		t.Errorf("a city whose [storage] keeps every class on work was refused: %v", err)
	}
	if len(ReservedPrefixWarnings(single.Rigs, EffectiveHQPrefix(single))) != 2 {
		t.Error("the single-store advisory no longer warns about both collisions")
	}
}

// Review finding 5: a prefix whose FIRST hyphen segment is reserved collides
// too. "gcg-alpha" mints "gcg-alpha-1", which is inside the binding's "gcg"
// namespace.
func TestReservedPrefixCollisionMatchesTheFirstSegment(t *testing.T) {
	for prefix, want := range map[string]string{
		"gcg": "gcg", "GCG": "gcg", "gcg-alpha": "gcg", "gco-x-y": "gco", "gcnq": "gcnq", "gcnq-q": "gcnq",
	} {
		if got, ok := ReservedPrefixCollision(prefix); !ok || got != want {
			t.Errorf("ReservedPrefixCollision(%q) = (%q, %v), want (%q, true)", prefix, got, ok, want)
		}
	}
	for _, prefix := range []string{"", "gc", "gcgx", "alpha-gcg", "mc", "gcx-alpha"} {
		if got, ok := ReservedPrefixCollision(prefix); ok {
			t.Errorf("ReservedPrefixCollision(%q) = %q, want no collision", prefix, got)
		}
	}
	err := ReservedPrefixSplitRefusal(splitCityNamed("my-city", Rig{Name: "alpha", Prefix: "gcg-alpha"}))
	if err == nil || !strings.Contains(err.Error(), `"gcg-alpha"`) || !strings.Contains(err.Error(), `"gcg"`) {
		t.Errorf("a split city with rig prefix gcg-alpha: refusal = %v", err)
	}
}

// The edit-time check sees a [storage] declared in an include fragment, which
// the raw config being written does not carry.
func TestReservedPrefixSplitRefusalForEditReadsFragmentStorage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "storage.toml"), []byte(`
[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("include = [\"storage.toml\"]\n[workspace]\nname = \"my-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := &City{Include: []string{"storage.toml"}, Workspace: Workspace{Name: "my-city"}, Rigs: []Rig{{Name: "gas-city-ops", Path: "/tmp/x"}}}
	if err := ReservedPrefixSplitRefusalForEdit(fsys.OSFS{}, tomlPath, next); err == nil || !strings.Contains(err.Error(), "gco") {
		t.Errorf("ForEdit with fragment [storage] = %v, want the gco refusal", err)
	}
	next.Include = nil
	if err := ReservedPrefixSplitRefusalForEdit(fsys.OSFS{}, tomlPath, next); err != nil {
		t.Errorf("ForEdit with no fragments and no [storage] refused: %v", err)
	}
}

// A city being created gets every rig its source composes, so a reserved
// prefix on a rig declared only in an include fragment is refused before the
// city exists, wherever the source declares its [storage].
func TestReservedPrefixSplitRefusalForNewCitySeesAFragmentRig(t *testing.T) {
	const storage = `
[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"
`
	for name, root := range map[string]string{
		"storage in the root":     "include = [\"rigs.toml\"]\n[workspace]\nname = \"my-city\"\n" + storage,
		"storage in the fragment": "include = [\"rigs.toml\", \"storage.toml\"]\n[workspace]\nname = \"my-city\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for file, body := range map[string]string{
				"rigs.toml":    "[[rigs]]\nname = \"gas-city-ops\"\npath = \"/tmp/x\"\n",
				"storage.toml": storage,
				"city.toml":    root,
			} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			tomlPath := filepath.Join(dir, "city.toml")
			next, err := Parse([]byte(root))
			if err != nil {
				t.Fatal(err)
			}
			if err := ReservedPrefixSplitRefusalForNewCity(fsys.OSFS{}, tomlPath, next); err == nil || !strings.Contains(err.Error(), `"gas-city-ops"`) || !strings.Contains(err.Error(), "gco") {
				t.Errorf("ForNewCity with the gco rig only in a fragment = %v, want the gco refusal naming the rig", err)
			}
		})
	}
}

func TestReservedPrefixSplitRefusalSeesARigPatchPrefix(t *testing.T) {
	cfg := splitCityNamed("my-city", Rig{Name: "alpha", Prefix: "al"})
	reserved := "gcs-alpha"
	cfg.Patches.Rigs = []RigPatch{{Name: "alpha", Prefix: &reserved}}
	if err := ReservedPrefixSplitRefusal(cfg); err == nil || !strings.Contains(err.Error(), "gcs-alpha") {
		t.Errorf("a rig patch setting gcs-alpha on a split city: refusal = %v", err)
	}
}

// A city being created is told to rename itself or change its source config,
// never to edit a file of the city that does not exist yet.
func TestReservedPrefixSplitRefusalForNewCityNamesTheRename(t *testing.T) {
	err := ReservedPrefixSplitRefusalForNewCity(fsys.OSFS{}, "", splitCityNamed("gas-city-gateway", Rig{Name: "gas-city-ops", Path: "/x"}))
	if err == nil {
		t.Fatal("no refusal")
	}
	for _, want := range []string{"name the city differently", "config the city is created from", "gcg", "gco"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "site.toml") || strings.Contains(err.Error(), "[[rigs]] entry") {
		t.Errorf("refusal names a file of the city being created: %v", err)
	}
}
