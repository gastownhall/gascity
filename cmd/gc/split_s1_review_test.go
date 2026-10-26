package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
// prefix is refused before the workspace identity is persisted.
func TestInitFromASplitCityRefusesAReservedDerivedPrefix(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(srcDir, "pack.toml"), []byte("[pack]\nname = \"declared-city\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cityPath := filepath.Join(t.TempDir(), "target")
	var stdout, stderr bytes.Buffer
	if code := doInitFromDirWithOptionsFS(fsys.OSFS{}, srcDir, cityPath, "gas-city-gateway", &stdout, &stderr, true); code == 0 {
		t.Fatalf("gc init --from a split city named gas-city-gateway succeeded; stdout=%s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gcg") || !strings.Contains(stderr.String(), "workspace_prefix") {
		t.Errorf("stderr = %q, want the reserved-prefix refusal naming workspace_prefix", stderr.String())
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "site.toml")); err == nil {
		if data, _ := os.ReadFile(filepath.Join(cityPath, ".gc", "site.toml")); strings.Contains(string(data), "gas-city-gateway") {
			t.Errorf("the refused init persisted the reserved identity:\n%s", data)
		}
	}
}
