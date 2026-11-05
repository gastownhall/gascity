package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// stubInitBDVersion makes every `bd version` probe gc init runs report line
// (or fail, when line is ""), whatever path names the bd.
func stubInitBDVersion(t *testing.T, line string) {
	t.Helper()
	prev := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		if filepath.Base(binary) != "bd" {
			return prev(binary)
		}
		if line == "" {
			return "", errors.New("bd: not found")
		}
		return line, nil
	}
	t.Cleanup(func() { initRunVersion = prev })
}

// initStorageFakeBD puts an executable bd first on PATH, with BD_BIN unset, and
// returns its path: the bd a new city's gc bd would run. It never runs; tests
// stub the command-tree judgment and the version probe.
func initStorageFakeBD(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bd := filepath.Join(dir, "bd")
	if err := os.WriteFile(bd, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // stands in for an executable
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_BIN", "")
	return bd
}

// stubInitSplitBDJudge makes the split floor's command-tree judgment answer the
// pinned inventory named inventory ("" for none) or err, and returns the
// binaries it was asked to judge.
func stubInitSplitBDJudge(t *testing.T, inventory string, err error) *[]string {
	t.Helper()
	var judged []string
	prev := initJudgeSplitBD
	initJudgeSplitBD = func(bdPath string) (*bdflags.Inventory, error) {
		judged = append(judged, bdPath)
		if err != nil {
			return nil, err
		}
		if inventory == "" {
			return nil, nil
		}
		inv := bdflags.InventoryByName(inventory)
		if inv == nil {
			t.Fatalf("no pinned inventory %q", inventory)
		}
		return inv, nil
	}
	t.Cleanup(func() { initJudgeSplitBD = prev })
	return &judged
}

// initStorageTestCity runs doInit on a fake filesystem and returns the written
// city.toml parsed, or nil when doInit wrote none.
func initStorageTestCity(t *testing.T, cityPath string, wiz wizardConfig) (*fsys.Fake, *config.City, int, string) {
	t.Helper()
	f := fsys.NewFake()
	var stdout, stderr bytes.Buffer
	code := doInit(f, cityPath, wiz, "", &stdout, &stderr, false)
	data, ok := f.Files[filepath.Join(cityPath, "city.toml")]
	if !ok {
		return f, nil, code, stderr.String()
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing the written city.toml: %v\n%s", err, data)
	}
	return f, cfg, code, stderr.String()
}

// assertBornSplit fails unless cfg carries the born-split block of the design
// (§2.1): work on work, the five infrastructure classes on one sqlite-beads
// binding at .gc/store.
func assertBornSplit(t *testing.T, cfg *config.City) {
	t.Helper()
	if cfg.Storage == nil {
		t.Fatal("city.toml has no [storage]; want the born-split block")
	}
	c := cfg.Storage.Classes
	if c.Work != "work" {
		t.Errorf("storage.classes.work = %q, want work", c.Work)
	}
	for name, got := range map[string]string{"graph": c.Graph, "sessions": c.Sessions, "messaging": c.Messaging, "orders": c.Orders, "nudges": c.Nudges} {
		if got != initSplitBindingName {
			t.Errorf("storage.classes.%s = %q, want %q", name, got, initSplitBindingName)
		}
	}
	binding, ok := cfg.Storage.Bindings[initSplitBindingName]
	if !ok {
		t.Fatalf("no [storage.bindings.%s]: %+v", initSplitBindingName, cfg.Storage.Bindings)
	}
	if binding.Provider != "sqlite-beads" || binding.Path != ".gc/store" {
		t.Errorf("binding = %+v, want provider sqlite-beads at .gc/store", binding)
	}
	if !config.RelocatesAnyClass(cfg) {
		t.Error("config.RelocatesAnyClass = false for the born-split block")
	}
}

func TestParseInitStorageChoice(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want initStorageChoice
	}{
		{"", initStorageSingle},
		{"single", initStorageSingle},
		{"split", initStorageSplit},
		{" split ", initStorageSplit},
	} {
		got, err := parseInitStorageChoice(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("parseInitStorageChoice(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{"Split", "work", "infra", "both"} {
		if _, err := parseInitStorageChoice(raw); err == nil || !strings.Contains(err.Error(), "single") || !strings.Contains(err.Error(), "split") {
			t.Errorf("parseInitStorageChoice(%q) error = %v, want a refusal naming single and split", raw, err)
		}
	}
}

// Until the flip (A1), a plain gc init stays single-store.
func TestInitDefaultsToSingleStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "")
	_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{})
	if code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr)
	}
	if cfg.Storage != nil {
		t.Fatalf("a plain gc init wrote [storage] %+v; the default stays single-store", cfg.Storage)
	}
}

func TestInitStorageSingleWritesNoStorage(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "")
	_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSingle})
	if code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr)
	}
	if cfg.Storage != nil {
		t.Fatalf("--storage=single wrote [storage] %+v", cfg.Storage)
	}
	if stderr != "" {
		t.Errorf("--storage=single printed %q, want nothing while single is the default", stderr)
	}
}

func TestInitStorageSplitWritesTheBornSplitBlock(t *testing.T) {
	for _, template := range []string{"", "minimal", "gastown", "gascity", "custom", "empty"} {
		t.Run("template="+template, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_SESSION", "")
			_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{configName: template, storage: initStorageSplit})
			if code != 0 {
				t.Fatalf("doInit = %d: %s", code, stderr)
			}
			assertBornSplit(t, cfg)
		})
	}
}

// The split floor (maintainer ruling 2026-11-04, design D3(b)): a split city's
// gc bd runs only a bd whose command tree matches a pinned inventory, so gc
// init judges the bd that city would run the same way, by tree and not by
// version string, and keeps OSS releases before 1.3.2 single-store. Every
// Enterprise build reports 1.3.0, and an OSS build from main reports 1.3.2
// with another tree, so a version floor alone misjudges both.
func TestInitStorageSplitJudgesTheBDByItsCommandTree(t *testing.T) {
	for _, tc := range []struct {
		name, version, inventory string
		wantSplit                bool
	}{
		{name: "OSS 1.3.0 has no pinned tree", version: "bd version 1.3.0 (f45b249ce: HEAD@f45b249ce6b4)"},
		{name: "OSS 1.3.1 has 1.3.2's tree but predates the floor", version: "bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)", inventory: "bd-1.3.2"},
		{name: "OSS 1.3.2", version: "bd version 1.3.2 (9b65b9e19: HEAD@9b65b9e198ac)", inventory: "bd-1.3.2", wantSplit: true},
		{name: "OSS 1.3.3 has 1.3.4's tree", version: "bd version 1.3.3 (e86e46fd4: HEAD@e86e46fd462c)", inventory: "bd-1.3.4", wantSplit: true},
		{name: "OSS 1.3.4", version: "bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)", inventory: "bd-1.3.4", wantSplit: true},
		{name: "the pinned Enterprise 1.3.0 build", version: "bd version 1.3.0 (Enterprise dev, Observer unknown; 77612097cec0: enterprise@77612097cec0)", inventory: "bd-1.3.0-enterprise-77612097cec0", wantSplit: true},
		{name: "an Enterprise build with no pinned tree", version: "bd version 1.3.0 (Enterprise dev, Observer unknown; d4c35678ec5e: enterprise@d4c35678ec5e)"},
		{name: "an OSS main build reporting 1.3.2 with another tree", version: "bd version 1.3.2 (dev)"},
		{name: "a release gc pins no tree for yet", version: "bd version 1.4.0"},
		{name: "an unversioned build with a pinned tree", version: "bd (development build)", inventory: "bd-1.3.4", wantSplit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "bd")
			t.Setenv("GC_SESSION", "")
			bd := initStorageFakeBD(t)
			stubInitBDVersion(t, tc.version)
			judged := stubInitSplitBDJudge(t, tc.inventory, nil)
			_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSplit})
			if code != 0 {
				t.Fatalf("doInit = %d: %s", code, stderr)
			}
			if len(*judged) != 1 || (*judged)[0] != bd {
				t.Errorf("judged %v, want only the bd on PATH %s", *judged, bd)
			}
			if tc.wantSplit {
				assertBornSplit(t, cfg)
				if stderr != "" {
					t.Errorf("stderr = %q, want nothing for a bd a split city admits", stderr)
				}
				return
			}
			if cfg.Storage != nil {
				t.Fatalf("a split request on %s wrote [storage] %+v; want single-store", tc.version, cfg.Storage)
			}
			for _, want := range []string{bd, "single-store", "1.3.2", "upgrade bd", "gc storage migrate"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not mention %q", stderr, want)
				}
			}
		})
	}
}

// A bd whose tree gc cannot check (a walk that fails or times out, as a mise
// or asdf shim does) would be refused by a split city's gc bd as well, so the
// city starts single-store and the notice says why.
func TestInitStorageSplitStartsSingleStoreWhenTheBDTreeCannotBeChecked(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_SESSION", "")
	initStorageFakeBD(t)
	stubInitBDVersion(t, "bd version 1.3.4")
	stubInitSplitBDJudge(t, "", errors.New("checking its command tree: the walk exceeded 2m0s"))
	_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSplit})
	if code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr)
	}
	if cfg.Storage != nil {
		t.Fatalf("wrote [storage] %+v for a bd gc could not check", cfg.Storage)
	}
	for _, want := range []string{"the walk exceeded 2m0s", "single-store", "gc storage migrate"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not mention %q", stderr, want)
		}
	}
}

// With no bd at all there is nothing to judge: the dependency check reports
// the missing bd and stops init, so the request stands.
func TestInitStorageSplitWithNoBDLeavesItToTheDependencyCheck(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_SESSION", "")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("BD_BIN", "")
	judged := stubInitSplitBDJudge(t, "", nil)
	_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSplit})
	if code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr)
	}
	assertBornSplit(t, cfg)
	if len(*judged) != 0 {
		t.Errorf("judged %v with no bd installed", *judged)
	}
}

// gc bd on a city whose workspace.env pins no bd (gc init writes none) runs an
// ambient BD_BIN that names an absolute executable ahead of PATH, so that is
// the bd the floor judges.
func TestInitStorageSplitJudgesTheAmbientBDBin(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_SESSION", "")
	initStorageFakeBD(t)
	pinned := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(pinned, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil { //nolint:gosec // stands in for an executable
		t.Fatal(err)
	}
	t.Setenv("BD_BIN", pinned)
	stubInitBDVersion(t, "bd version 1.3.4")
	judged := stubInitSplitBDJudge(t, "bd-1.3.4", nil)
	_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSplit})
	if code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr)
	}
	assertBornSplit(t, cfg)
	if len(*judged) != 1 || (*judged)[0] != pinned {
		t.Errorf("judged %v, want only BD_BIN %s", *judged, pinned)
	}
}

// The floor is a bd floor: a city whose work store does not run bd never asks.
func TestInitStorageSplitOnAFileStoreNeverProbesBD(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "")
	prev := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		if filepath.Base(binary) == "bd" {
			t.Errorf("gc init probed bd for a file-store city")
		}
		return prev(binary)
	}
	t.Cleanup(func() { initRunVersion = prev })
	judged := stubInitSplitBDJudge(t, "", nil)
	_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSplit})
	if code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr)
	}
	assertBornSplit(t, cfg)
	if len(*judged) != 0 {
		t.Errorf("judged %v for a file-store city", *judged)
	}
}

// Remote session runtimes stay single-store until worker-site transport
// (CN-2): a split request on one is refused before anything is written.
func TestInitStorageSplitRefusesARemoteSessionRuntime(t *testing.T) {
	for _, tc := range []struct {
		name, env, configured string
	}{
		{name: "GC_SESSION=k8s", env: "k8s"},
		{name: "GC_SESSION=hybrid", env: "hybrid"},
		{name: "configured ssh", configured: "ssh:build-host"},
		{name: "configured exec", configured: "exec:/opt/runtime.sh"},
		{name: "configured k8s", configured: "k8s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_SESSION", tc.env)
			if tc.configured != "" {
				prev := initCityConfigHook
				initCityConfigHook = func(cfg *config.City) { cfg.Session.Provider = tc.configured }
				t.Cleanup(func() { initCityConfigHook = prev })
			}
			f, _, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSplit})
			if code == 0 {
				t.Fatal("gc init --storage=split succeeded on a remote session runtime")
			}
			for _, want := range []string{"CN-2", "--storage=single", "remote"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not mention %q", stderr, want)
				}
			}
			if len(f.Files) != 0 {
				t.Errorf("the refused init wrote %d file(s)", len(f.Files))
			}
		})
	}
}

// An exec runtime whose handshake declares locality=local runs its sessions
// on this host, so a split request on it is born split; the handshake is read
// for the new city's path.
func TestInitStorageSplitAdmitsAnExecRuntimeDeclaringLocal(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "")
	prevHook := initCityConfigHook
	initCityConfigHook = func(cfg *config.City) { cfg.Session.Provider = "exec:/opt/runtime.sh" }
	t.Cleanup(func() { initCityConfigHook = prevHook })
	var judged []string
	prevLocal := execRuntimeDeclaresLocal
	execRuntimeDeclaresLocal = func(script, _, cityPath string) bool {
		judged = append(judged, script+"@"+cityPath)
		return true
	}
	t.Cleanup(func() { execRuntimeDeclaresLocal = prevLocal })
	_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSplit})
	if code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr)
	}
	assertBornSplit(t, cfg)
	if len(judged) == 0 || judged[0] != "/opt/runtime.sh@/bright-lights" {
		t.Errorf("locality judged as %v, want /opt/runtime.sh@/bright-lights", judged)
	}
}

func TestInitStorageSingleAcceptsARemoteSessionRuntime(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "k8s")
	_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSingle})
	if code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr)
	}
	if cfg.Storage != nil {
		t.Fatalf("wrote [storage] %+v on a single-store request", cfg.Storage)
	}
}

func TestInitPrefixSetsTheWorkspacePrefix(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "")
	f, _, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{prefix: "bx"})
	if code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr)
	}
	binding, err := config.LoadSiteBinding(f, "/bright-lights")
	if err != nil {
		t.Fatal(err)
	}
	if binding.WorkspacePrefix != "bx" {
		t.Fatalf("workspace prefix = %q, want bx", binding.WorkspacePrefix)
	}
}

// --prefix is the fix for a city whose name derives a reserved prefix
// (gas-city-gateway derives gcg).
func TestInitPrefixFixesAReservedDerivedPrefixOnASplitCity(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "")
	_, _, code, stderr := initStorageTestCity(t, "/gas-city-gateway", wizardConfig{storage: initStorageSplit})
	if code == 0 {
		t.Fatal("a split gas-city-gateway with its derived gcg prefix was not refused")
	}
	if !strings.Contains(stderr, "--prefix") {
		t.Errorf("the refusal %q does not name --prefix", stderr)
	}
	f, cfg, code, stderr := initStorageTestCity(t, "/gas-city-gateway", wizardConfig{storage: initStorageSplit, prefix: "gw"})
	if code != 0 {
		t.Fatalf("doInit with --prefix gw = %d: %s", code, stderr)
	}
	assertBornSplit(t, cfg)
	binding, err := config.LoadSiteBinding(f, "/gas-city-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if binding.WorkspacePrefix != "gw" {
		t.Fatalf("workspace prefix = %q, want gw", binding.WorkspacePrefix)
	}
}

func TestInitPrefixRefusesAReservedPrefixOnASplitCity(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "")
	f, _, code, _ := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSplit, prefix: "gcm"})
	if code == 0 {
		t.Fatal("--prefix gcm on a split city was not refused")
	}
	if len(f.Files) != 0 {
		t.Errorf("the refused init wrote %d file(s)", len(f.Files))
	}
}

func TestParseInitPrefix(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{"", ""}, {"bx", "bx"}, {" gw ", "gw"}, {"my_city-1", "my_city-1"}} {
		got, err := parseInitPrefix(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("parseInitPrefix(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
	for _, raw := range []string{"   ", "a b", "x/y", "-x", "x.y", "café"} {
		if _, err := parseInitPrefix(raw); err == nil {
			t.Errorf("parseInitPrefix(%q) accepted a malformed prefix", raw)
		}
	}
}

// gc init never edits a city.toml it did not write: --storage=split or
// --prefix over an existing one is refused rather than silently ignored.
func TestInitStorageAndPrefixRefuseAnExistingCityToml(t *testing.T) {
	for name, wiz := range map[string]wizardConfig{
		"split":  {storage: initStorageSplit},
		"prefix": {prefix: "bx"},
	} {
		t.Run(name, func(t *testing.T) {
			f := fsys.NewFake()
			f.Files["/bright-lights/city.toml"] = []byte("[workspace]\nname = \"bright-lights\"\n")
			var stdout, stderr bytes.Buffer
			if code := doInit(f, "/bright-lights", wiz, "", &stdout, &stderr, false); code == 0 {
				t.Fatalf("doInit over an existing city.toml with %s succeeded: %s", name, stdout.String())
			}
		})
	}
}

// --from copies its source verbatim and never adds the split (F3: a K8S pod
// inits with --from), so --storage and --prefix are refused with it, as they
// are with --file, whose source config owns both.
func TestInitStorageAndPrefixFlagsRefuseFromAndFile(t *testing.T) {
	for _, args := range [][]string{
		{"--from", "/tmp/src", "--storage", "split"},
		{"--from", "/tmp/src", "--storage", "single"},
		{"--from", "/tmp/src", "--prefix", "bx"},
		{"--file", "/tmp/city.toml", "--storage", "split"},
		{"--file", "/tmp/city.toml", "--prefix", "bx"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			cmd := newInitCmd(io.Discard, io.Discard)
			cmd.SetArgs(append(args, filepath.Join(t.TempDir(), "city")))
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "none of the others can be") {
				t.Fatalf("gc init %v = %v, want a mutually-exclusive flag refusal", args, err)
			}
		})
	}
}

func TestInitStorageFlagRefusesAnUnknownValueBeforeScaffolding(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	var stderr bytes.Buffer
	cmd := newInitCmd(io.Discard, &stderr)
	cmd.SetArgs([]string{"--template", "custom", "--storage", "both", cityPath})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.Execute(); err == nil {
		t.Fatal("gc init --storage=both succeeded")
	}
	if !strings.Contains(stderr.String(), "--storage") {
		t.Errorf("stderr %q does not name --storage", stderr.String())
	}
	if _, err := fsys.OSFS.Stat(fsys.OSFS{}, cityPath); err == nil {
		t.Errorf("the refused init created %s", cityPath)
	}
}

// inventoryIsEnterprise reports whether a pinned inventory was dumped from an
// Enterprise bd build (its build tags carry "enterprise").
func inventoryIsEnterprise(inv *bdflags.Inventory) bool {
	for _, tag := range strings.Split(inv.Provenance.BuildTags, ",") {
		if strings.TrimSpace(tag) == "enterprise" {
			return true
		}
	}
	return false
}

// The split admission ruling (maintainer, 2026-11-05) over every pinned
// inventory, not just stubbed ones: a bd whose tree matches a pinned
// inventory gets the split, and the v1.3.2 floor applies only to OSS
// builds. An Enterprise build below the floor is admitted, and an OSS build
// below it is held single-store with the upgrade hint. The floor exempts a
// sub-floor bd only when its version is the matched inventory's own version,
// so pinning an OSS 1.3.0 or 1.3.1 tree would admit that release. This test
// fails if that happens, and the floor would then need to tell Enterprise and
// OSS apart some other way.
func TestInitSplitFloorAppliesToOSSPinnedInventoriesOnly(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_SESSION", "")
	initStorageFakeBD(t)
	sawEnterpriseBelowFloor := false
	for _, inv := range bdflags.Inventories() {
		t.Run(inv.Name, func(t *testing.T) {
			stubInitBDVersion(t, "bd version "+inv.Version)
			stubInitSplitBDJudge(t, inv.Name, nil)
			belowFloor := compareVersions(inv.Version, initSplitBDMinVersion) < 0
			enterprise := inventoryIsEnterprise(inv)
			if belowFloor && enterprise {
				sawEnterpriseBelowFloor = true
			}
			notice := splitBDFloorNotice()
			if wantAdmit := enterprise || !belowFloor; wantAdmit {
				if notice != "" {
					t.Errorf("bd v%s matching %s (enterprise=%t) held single-store: %s", inv.Version, inv.Name, enterprise, notice)
				}
				return
			}
			if notice == "" {
				t.Fatalf("OSS bd v%s matching pinned %s admitted to split; the v%s floor holds OSS releases below it single-store", inv.Version, inv.Name, initSplitBDMinVersion)
			}
			for _, want := range []string{"single-store", "upgrade bd", initSplitBDMinVersion} {
				if !strings.Contains(notice, want) {
					t.Errorf("notice %q does not mention %q", notice, want)
				}
			}
		})
	}
	if !sawEnterpriseBelowFloor {
		t.Errorf("no pinned Enterprise inventory below v%s; the ruling's Enterprise 1.3.0 case is no longer exercised", initSplitBDMinVersion)
	}
}

// The ruling's three named cases, end to end through resolveInitStorage
// against the real pinned inventories: the Enterprise 1.3.0 tree is admitted,
// OSS 1.3.0 (no pinned tree) and OSS 1.3.1 (1.3.2's tree, below the floor)
// start single-store with the upgrade hint, and OSS 1.3.2 is admitted.
func TestResolveInitStorageSplitAdmissionRuling(t *testing.T) {
	for _, tc := range []struct {
		name, version, inventory string
		wantSplit                bool
	}{
		{name: "enterprise 1.3.0 pinned tree", version: "bd version 1.3.0 (Enterprise dev, Observer unknown; 77612097cec0: enterprise@77612097cec0)", inventory: "bd-1.3.0-enterprise-77612097cec0", wantSplit: true},
		{name: "OSS 1.3.0", version: "bd version 1.3.0 (f45b249ce: HEAD@f45b249ce6b4)"},
		{name: "OSS 1.3.1", version: "bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)", inventory: "bd-1.3.2"},
		{name: "OSS 1.3.2", version: "bd version 1.3.2 (9b65b9e19: HEAD@9b65b9e198ac)", inventory: "bd-1.3.2", wantSplit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "bd")
			t.Setenv("GC_SESSION", "")
			initStorageFakeBD(t)
			stubInitBDVersion(t, tc.version)
			stubInitSplitBDJudge(t, tc.inventory, nil)
			storage, notice, err := resolveInitStorage(&config.City{}, t.TempDir(), initStorageSplit)
			if err != nil {
				t.Fatalf("resolveInitStorage: %v", err)
			}
			if tc.wantSplit {
				if storage == nil || notice != "" {
					t.Fatalf("storage=%+v notice=%q; want the born-split block and no notice", storage, notice)
				}
				return
			}
			if storage != nil {
				t.Fatalf("storage = %+v; want single-store", storage)
			}
			for _, want := range []string{"single-store", "upgrade bd", "v" + initSplitBDMinVersion, "gc storage migrate"} {
				if !strings.Contains(notice, want) {
					t.Errorf("notice %q does not mention %q", notice, want)
				}
			}
		})
	}
}
