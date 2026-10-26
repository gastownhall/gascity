package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// Regression tests from the review of #8332 (split-store fail-closed guards).

// A work bead an agent files with an order-run: label inherited from the
// patrol that found it (150 such beads in production single-store cities) is
// work, and gc bd may create it on a split city.
func TestSplitCityCreatesAPatrolFiledBugCarryingAnOrderRunLabel(t *testing.T) {
	cfg := infraSplitConfig(filepath.Join(t.TempDir(), "store"))
	for _, args := range [][]string{
		{"create", "Technical health finding", "--type", "bug", "--labels", "order-run:technical-health-patrol,technical-health"},
		{"create", "[handoff] backlog-review", "--labels", "order-run:backlog-review,product-lens"},
	} {
		if msg, refused := bdRelocatedClassCreateRefusal(cfg, t.TempDir(), args); refused {
			t.Errorf("gc bd %v refused on a split city: %s", args, msg)
		}
	}
}

// Review probe G: on a migrated split city, `gc rig add` of a directory named
// gas-city-gateway (derived prefix gcg) used to exit 0 and write the rig; then
// every CLI process refused the city and every hook claim failed closed. The
// add must refuse before writing anything.
func TestRigAddRefusesAReservedPrefixOnASplitCity(t *testing.T) {
	cityPath, _ := migratedOneShotCLICity(t)
	captureCLIStorageStderr(t)
	before, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "gas-city-gateway")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, nil, "", "", "", false, false, &stdout, &stderr); code == 0 {
		t.Fatalf("gc rig add accepted a gcg rig on a split city; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gcg") || !strings.Contains(stderr.String(), "reserved") {
		t.Errorf("stderr = %q, want the reserved-prefix refusal", stderr.String())
	}
	after, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the refused rig add rewrote city.toml:\n%s", after)
	}
	cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if refusal := config.ReservedPrefixSplitRefusal(cfg); refusal != nil {
		t.Errorf("the city now carries a reserved prefix: %v", refusal)
	}
}

// gc init --from a split source city into a name that derives a reserved
// prefix is refused BEFORE anything is copied, for the pack.toml layout and
// the legacy one alike (re-review probe P4: the legacy layout skipped the
// check), and leaves nothing behind, so the corrected retry succeeds.
func TestInitFromASplitCityRefusesAReservedDerivedPrefix(t *testing.T) {
	for _, withPack := range []bool{true, false} {
		t.Run(map[bool]string{true: "pack.toml", false: "legacy"}[withPack], func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			srcDir := t.TempDir()
			src := "[workspace]\nname = \"declared-city\"\n" + `
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
			if err := os.WriteFile(filepath.Join(srcDir, "city.toml"), []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			if withPack {
				if err := os.WriteFile(filepath.Join(srcDir, "pack.toml"), []byte("[pack]\nname = \"declared-city\"\nschema = 2\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cityPath := filepath.Join(t.TempDir(), "gas-city-gateway")
			var stdout, stderr bytes.Buffer
			if code := doInitFromDirWithOptionsFS(fsys.OSFS{}, srcDir, cityPath, "gas-city-gateway", &stdout, &stderr, true); code == 0 {
				t.Fatalf("gc init --from a split city named gas-city-gateway succeeded; stdout=%s", stdout.String())
			}
			if !strings.Contains(stderr.String(), "gcg") || !strings.Contains(stderr.String(), "workspace_prefix") {
				t.Errorf("stderr = %q, want the reserved-prefix refusal naming workspace_prefix", stderr.String())
			}
			if _, err := os.Stat(cityPath); err == nil {
				entries, _ := os.ReadDir(cityPath)
				t.Errorf("the refused init left %d entr(ies) behind at %s", len(entries), cityPath)
			}
			stdout.Reset()
			stderr.Reset()
			if code := doInitFromDirWithOptionsFS(fsys.OSFS{}, srcDir, cityPath, "my-city", &stdout, &stderr, true); code != 0 {
				t.Errorf("the corrected retry failed (%d): %s", code, stderr.String())
			}
		})
	}
}

// M25: `gc start` refuses a split city whose rig prefix is reserved, at its
// own validation step (beside ValidateRigs), before the storage gate.
func TestStartRefusesASplitCityWithAReservedRigPrefix(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "gas-city-gateway")
	clearInheritedBeadsEnv(t)
	t.Chdir(t.TempDir())
	for _, dir := range []string{filepath.Join(cityPath, ".gc"), rigPath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cityTOML := `[workspace]
name = "start-r10"

[beads]
provider = "file"

[storage.classes]
work = "work"
graph = "infra"
sessions = "infra"
messaging = "infra"
orders = "infra"
nudges = "infra"

[storage.bindings.infra]
provider = "sqlite-beads"

[[rigs]]
name = "gas-city-gateway"
`
	siteTOML := "workspace_name = \"start-r10\"\n\n[[rig]]\nname = \"gas-city-gateway\"\npath = " + strconv.Quote(rigPath) + "\n"
	for path, body := range map[string]string{
		filepath.Join(cityPath, "city.toml"):        cityTOML,
		filepath.Join(cityPath, ".gc", "site.toml"): siteTOML,
		filepath.Join(cityPath, "pack.toml"):        "[pack]\nname = \"start-r10\"\nschema = 2\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oldDry := dryRunMode
	dryRunMode = true
	t.Cleanup(func() { dryRunMode = oldDry })
	gateRan := false
	prevSource := openInfraMigrationSource
	openInfraMigrationSource = func(p string) (beads.Store, error) { gateRan = true; return prevSource(p) }
	t.Cleanup(func() { openInfraMigrationSource = prevSource })

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, false, &stdout, &stderr); code != 1 {
		t.Fatalf("gc start exit = %d, want 1\nstdout:\n%s\nstderr:\n%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), `rig "gas-city-gateway"`) || !strings.Contains(stderr.String(), "gcg") {
		t.Errorf("stderr = %q, want the reserved-prefix refusal naming the rig", stderr.String())
	}
	if gateRan {
		t.Error("gc start reached the storage gate; the refusal belongs to its own validation step")
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "store")); err == nil {
		t.Error("the refused start created a binding")
	}
}
