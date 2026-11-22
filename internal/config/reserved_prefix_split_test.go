package config

import (
	"errors"
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

// newCityNamed parses root, the city.toml a new city is created from, and
// gives it the name the new city takes.
func newCityNamed(t *testing.T, root, name string) *City {
	t.Helper()
	next, err := Parse([]byte(root))
	if err != nil {
		t.Fatal(err)
	}
	next.Workspace.Name, next.ResolvedWorkspaceName = name, name
	return next
}

// A new city is judged by the [storage] its city.toml and include fragments
// declare, which does not wait on its packs: a source whose remote import this
// host has not installed yet, which LoadWithIncludes cannot load, is refused
// for a reserved prefix as the split city its copy will be.
func TestReservedPrefixSplitRefusalForNewCityJudgesTheSplitBeforeItsPacksAreInstalled(t *testing.T) {
	const root = "include = [\"storage.toml\"]\n\n[workspace]\nname = \"template\"\n"
	tomlPath := writeCityWithUninstalledImport(t, map[string]string{"city.toml": root, "storage.toml": wholeSplitStorage})

	err := ReservedPrefixSplitRefusalForNewCity(fsys.OSFS{}, tomlPath, newCityNamed(t, root, "gas-city-ops"))
	if !errors.Is(err, ErrReservedBeadPrefix) || !strings.Contains(err.Error(), `"gco"`) {
		t.Fatalf("ForNewCity of gas-city-ops with its whole [storage] in a fragment = %v, want the gco refusal", err)
	}
}

// A new city whose include fragments do not compose cannot be judged, and
// could not load them either: the check refuses it with the reason rather than
// passing a split it cannot see.
func TestReservedPrefixSplitRefusalForNewCityRefusesFragmentsThatDoNotCompose(t *testing.T) {
	const root = "include = [\"storage.toml\"]\n\n[workspace]\nname = \"template\"\n"
	tomlPath := filepath.Join(t.TempDir(), "city.toml")
	if err := os.WriteFile(tomlPath, []byte(root), 0o644); err != nil {
		t.Fatal(err)
	}

	err := ReservedPrefixSplitRefusalForNewCity(fsys.OSFS{}, tomlPath, newCityNamed(t, root, "bright-lights"))
	if err == nil || errors.Is(err, ErrReservedBeadPrefix) || !strings.Contains(err.Error(), `"storage.toml"`) || !strings.Contains(err.Error(), tomlPath) {
		t.Fatalf("ForNewCity whose fragment is missing = %v, want an error naming %s and the missing fragment", err, tomlPath)
	}
}

// A new city's rigs mint under the prefixes its rig patches give them, which
// do not wait on its packs either: a fragment rig whose name derives gcg,
// patched to gql, is not refused, and one patched into a reserved namespace is.
func TestReservedPrefixSplitRefusalForNewCityJudgesPatchedRigPrefixes(t *testing.T) {
	for _, tc := range []struct {
		prefix  string
		refused bool
	}{
		{prefix: "gql"},
		{prefix: "gcs-gql", refused: true},
	} {
		t.Run(tc.prefix, func(t *testing.T) {
			root := "include = [\"rigs.toml\", \"storage.toml\"]\n\n[workspace]\nname = \"template\"\n\n[[patches.rigs]]\nname = \"graphql-code-generator\"\nprefix = \"" + tc.prefix + "\"\n"
			tomlPath := writeCityWithUninstalledImport(t, map[string]string{
				"city.toml":    root,
				"rigs.toml":    "[[rigs]]\nname = \"graphql-code-generator\"\npath = \"/tmp/gql\"\n",
				"storage.toml": wholeSplitStorage,
			})

			err := ReservedPrefixSplitRefusalForNewCity(fsys.OSFS{}, tomlPath, newCityNamed(t, root, "bright-lights"))
			if !tc.refused {
				if err != nil {
					t.Fatalf("ForNewCity with the gcg-derived rig patched to %s = %v, want no refusal", tc.prefix, err)
				}
				return
			}
			if !errors.Is(err, ErrReservedBeadPrefix) || !strings.Contains(err.Error(), `rig "graphql-code-generator" bead prefix "`+tc.prefix+`"`) {
				t.Fatalf("ForNewCity with the rig patched to %s = %v, want the refusal naming the patched prefix", tc.prefix, err)
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
// never to edit a file of the city that does not exist yet. gc init --file and
// --from, which create it from that config, refuse --prefix and --storage, so
// the refusal names neither.
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
	for _, flag := range []string{"--prefix", "--storage"} {
		if strings.Contains(err.Error(), flag) {
			t.Errorf("refusal names %s, which gc init --file and --from refuse: %v", flag, err)
		}
	}
}

// A plain gc init builds the city from a built-in template, so its fixes are
// a rename or gc init's own --prefix and --storage=single, not a source config.
func TestReservedPrefixSplitRefusalForPlainInitNamesGCInitsFlags(t *testing.T) {
	err := ReservedPrefixSplitRefusalForPlainInit(splitCityNamed("gas-city-gateway"))
	if err == nil {
		t.Fatal("no refusal")
	}
	for _, want := range []string{`"gcg"`, "name the city differently", "--prefix", "--storage=single"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
	for _, unusable := range []string{"config it is created from", "site.toml"} {
		if strings.Contains(err.Error(), unusable) {
			t.Errorf("refusal names %q, which a plain gc init has no use for: %v", unusable, err)
		}
	}
	if err := ReservedPrefixSplitRefusalForPlainInit(&City{Workspace: Workspace{Name: "gas-city-gateway"}}); err != nil {
		t.Errorf("a single-store plain init was refused: %v", err)
	}
}

// A fresh add (gc rig add, the API rig create) writes no [[rigs]] entry when it
// is refused, so the added rig's problem names the prefix the add takes; every
// other collision keeps its existing-city fix.
func TestReservedPrefixSplitRefusalForRigAddNamesTheAddPrefixForThatRigOnly(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"my-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := splitCityNamed("my-city",
		Rig{Name: "gas-city-ops", Path: "/tmp/ops"},
		Rig{Name: "graphql-code-generator", Path: "/tmp/gql"},
	)
	err := ReservedPrefixSplitRefusalForRigAdd(fsys.OSFS{}, tomlPath, next, "graphql-code-generator")
	if err == nil {
		t.Fatal("no refusal")
	}
	for _, want := range []string{
		`rig "graphql-code-generator" bead prefix "gcg" is in the reserved "gcg" namespace; add it with a non-reserved prefix: gc rig add --prefix <prefix>, or the "prefix" field of an API rig create`,
		`rig "gas-city-ops" bead prefix "gco" is in the reserved "gco" namespace; set an explicit prefix: prefix = "<prefix>" on that [[rigs]] entry`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
	if err := ReservedPrefixSplitRefusalForRigAdd(fsys.OSFS{}, tomlPath, &City{Workspace: Workspace{Name: "my-city"}, Rigs: next.Rigs}, "graphql-code-generator"); err != nil {
		t.Errorf("a rig add on a single-store city was refused: %v", err)
	}
}

// Every refusal is ErrReservedBeadPrefix to errors.Is, so the API answers it
// as the caller's error, and its text is the refusal alone: the sentinel's
// own text is not part of it.
func TestEveryReservedPrefixRefusalIsErrReservedBeadPrefix(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"gas-city-gateway\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := splitCityNamed("gas-city-gateway", Rig{Name: "gas-city-ops", Path: "/tmp/ops"})
	for name, refusal := range map[string]error{
		"split city":    ReservedPrefixSplitRefusal(next),
		"edit":          ReservedPrefixSplitRefusalForEdit(fsys.OSFS{}, tomlPath, next),
		"rig add":       ReservedPrefixSplitRefusalForRigAdd(fsys.OSFS{}, tomlPath, next, "gas-city-ops"),
		"adopt":         ReservedPrefixSplitRefusalForAdopt(fsys.OSFS{}, tomlPath, next, "gas-city-ops", "change the store's prefix"),
		"new city":      ReservedPrefixSplitRefusalForNewCity(fsys.OSFS{}, tomlPath, next),
		"plain gc init": ReservedPrefixSplitRefusalForPlainInit(next),
	} {
		t.Run(name, func(t *testing.T) {
			if !errors.Is(refusal, ErrReservedBeadPrefix) {
				t.Fatalf("refusal %v is not ErrReservedBeadPrefix", refusal)
			}
			if !strings.HasPrefix(refusal.Error(), "this city splits its coordination classes") || strings.Contains(refusal.Error(), ErrReservedBeadPrefix.Error()) {
				t.Errorf("refusal text %q is not the refusal alone", refusal)
			}
		})
	}
}

// An adopt (gc rig add --adopt) cannot give the adopted rig another prefix in
// city.toml, so only that rig's problem names the caller's remedy; every other
// collision keeps its existing-city fix.
func TestReservedPrefixSplitRefusalForAdoptNamesTheRemedyForThatRigOnly(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"gas-city-gateway\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := splitCityNamed("gas-city-gateway",
		Rig{Name: "gas-city-ops", Path: "/tmp/ops"},
		Rig{Name: "adopted", Path: "/tmp/adopted", Prefix: "gcs"},
	)
	reserved := "gcn-x"
	next.Patches.Rigs = []RigPatch{{Name: "other", Prefix: &reserved}}
	const fix = "change the store's prefix"

	err := ReservedPrefixSplitRefusalForAdopt(fsys.OSFS{}, tomlPath, next, "adopted", fix)
	if err == nil {
		t.Fatal("no refusal")
	}
	for _, want := range []string{
		`rig "adopted" bead prefix "gcs" is in the reserved "gcs" namespace; ` + fix,
		`rig "gas-city-ops" bead prefix "gco" is in the reserved "gco" namespace; set an explicit prefix: prefix = "<prefix>" on that [[rigs]] entry`,
		`workspace_prefix = "<prefix>" in .gc/site.toml`,
		`sets bead prefix "gcn-x", in the reserved "gcn" namespace; choose another prefix`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not contain %q", err, want)
		}
	}
}

func TestReservedPrefixSplitRefusalForAdoptReadsFragmentStorage(t *testing.T) {
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
	next := &City{Include: []string{"storage.toml"}, Workspace: Workspace{Name: "my-city"}, Rigs: []Rig{{Name: "adopted", Path: "/tmp/x", Prefix: "gco"}}}
	const fix = "change the store's prefix"
	if err := ReservedPrefixSplitRefusalForAdopt(fsys.OSFS{}, tomlPath, next, "adopted", fix); err == nil || !strings.Contains(err.Error(), "gco") || !strings.Contains(err.Error(), fix) {
		t.Errorf("ForAdopt with fragment [storage] = %v, want the gco refusal naming %q", err, fix)
	}
}

func TestReservedPrefixSplitRefusalForAdoptLeavesSingleStoreCitiesAlone(t *testing.T) {
	tomlPath := filepath.Join(t.TempDir(), "city.toml")
	if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"my-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	next := &City{Workspace: Workspace{Name: "my-city"}, Rigs: []Rig{{Name: "adopted", Path: "/tmp/x", Prefix: "gco"}}}
	if err := ReservedPrefixSplitRefusalForAdopt(fsys.OSFS{}, tomlPath, next, "adopted", "fix"); err != nil {
		t.Errorf("a single-store city was refused: %v", err)
	}
}
