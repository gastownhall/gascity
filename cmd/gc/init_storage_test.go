package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bdflags"
	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
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

// admitInitSplitBD makes the bd a new city's gc bd would run a pinned bd
// 1.3.4, which a split city admits, so a test of a plain gc init on a bd work
// store sees the born-split default rather than the test harness's fake bd
// held single-store.
func admitInitSplitBD(t *testing.T) {
	t.Helper()
	stubInitBDVersion(t, "bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)")
	stubInitSplitBDJudge(t, "bd-1.3.4", nil)
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
		{"", initStorageUnset},
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

// A plain gc init, with no --storage, is born split (the split-by-default
// flip, A1): every template gets the born-split block, and nothing is printed.
func TestInitDefaultsToBornSplit(t *testing.T) {
	for _, template := range []string{"", "minimal", "gastown", "gascity", "custom", "empty"} {
		t.Run("template="+template, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_SESSION", "")
			_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{configName: template})
			if code != 0 {
				t.Fatalf("doInit = %d: %s", code, stderr)
			}
			assertBornSplit(t, cfg)
			if strings.Contains(stderr, "single-store") {
				t.Errorf("stderr = %q, want no single-store notice for a born-split default", stderr)
			}
		})
	}
}

// A plain gc init on a bd the split city's gc bd would refuse starts
// single-store and says why, as an explicit split request does.
func TestInitDefaultHoldsABDBelowTheSplitFloorSingleStore(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_SESSION", "")
	bd := initStorageFakeBD(t)
	stubInitBDVersion(t, "bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)")
	stubInitSplitBDJudge(t, "bd-1.3.2", nil)
	_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{})
	if code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr)
	}
	if cfg.Storage != nil {
		t.Fatalf("a plain gc init on OSS bd 1.3.1 wrote [storage] %+v; want single-store", cfg.Storage)
	}
	for _, want := range []string{bd, "single-store", initSplitBDMinVersion, "upgrade bd", "gc storage migrate"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not mention %q", stderr, want)
		}
	}
}

// Remote session runtimes stay single-store until worker-site transport
// (CN-2). An explicit split request on one is refused; a plain gc init starts
// the city single-store and says why.
func TestInitDefaultStartsARemoteSessionRuntimeSingleStore(t *testing.T) {
	for _, tc := range []struct {
		name, env, configured, wantRuntime string
	}{
		{name: "GC_SESSION=k8s", env: "k8s", wantRuntime: "k8s"},
		{name: "GC_SESSION=hybrid", env: "hybrid", wantRuntime: "hybrid"},
		{name: "configured ssh", configured: "ssh:build-host", wantRuntime: "ssh:build-host"},
		{name: "configured exec", configured: "exec:/opt/runtime.sh", wantRuntime: "exec:/opt/runtime.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_SESSION", tc.env)
			if tc.configured != "" {
				prev := initCityConfigHook
				initCityConfigHook = func(cfg *config.City) { cfg.Session.Provider = tc.configured }
				t.Cleanup(func() { initCityConfigHook = prev })
				prevLocal := execRuntimeDeclaresLocal
				execRuntimeDeclaresLocal = func(string, string, string) bool { return false }
				t.Cleanup(func() { execRuntimeDeclaresLocal = prevLocal })
			}
			_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{})
			if code != 0 {
				t.Fatalf("doInit = %d: %s", code, stderr)
			}
			if cfg.Storage != nil {
				t.Fatalf("a plain gc init on a remote session runtime wrote [storage] %+v; want single-store", cfg.Storage)
			}
			for _, want := range []string{tc.wantRuntime, "single-store", "CN-2", "gc storage migrate"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not mention %q", stderr, want)
				}
			}
		})
	}
}

// A born-split city may not carry a reserved bead prefix, and a city whose
// name derives one (gas-city-gateway derives gcg) did not choose it, so a
// plain gc init starts it single-store and says why, naming the way to a
// split city, instead of refusing the name. The bd is not judged for a city
// that starts single-store anyway. --storage=single stays silent.
func TestInitDefaultStartsAReservedDerivedPrefixSingleStore(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_SESSION", "")
	initStorageFakeBD(t)
	stubInitBDVersion(t, "bd version 1.3.4 (7ef2fd05e: HEAD@7ef2fd05eb0c)")
	judged := stubInitSplitBDJudge(t, "bd-1.3.4", nil)
	_, cfg, code, stderr := initStorageTestCity(t, "/gas-city-gateway", wizardConfig{})
	if code != 0 {
		t.Fatalf("a plain gc init of gas-city-gateway = %d: %s", code, stderr)
	}
	if cfg.Storage != nil {
		t.Fatalf("a plain gc init of gas-city-gateway wrote [storage] %+v; its derived gcg prefix keeps it single-store", cfg.Storage)
	}
	for _, want := range []string{`"gcg"`, `"gas-city-gateway"`, "single-store", "--prefix"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the notice %q does not mention %s", stderr, want)
		}
	}
	if len(*judged) != 0 {
		t.Errorf("gc init judged bd %v for a city that starts single-store anyway", *judged)
	}

	t.Setenv("GC_BEADS", "file")
	_, cfg, code, stderr = initStorageTestCity(t, "/gas-city-gateway", wizardConfig{storage: initStorageSingle})
	if code != 0 {
		t.Fatalf("doInit --storage=single = %d: %s", code, stderr)
	}
	if cfg.Storage != nil {
		t.Fatalf("--storage=single wrote [storage] %+v", cfg.Storage)
	}
	if stderr != "" {
		t.Errorf("--storage=single printed %q, want nothing for an explicit opt-out", stderr)
	}
}

// A reserved prefix the city was given is its own choice: a plain gc init
// with --prefix gcm is refused, before anything is written, with fixes that
// include --storage=single, rather than started single-store.
func TestInitDefaultRefusesAGivenReservedPrefix(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "")
	f, _, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{prefix: "gcm"})
	if code == 0 {
		t.Fatal("a plain gc init with --prefix gcm was not refused")
	}
	for _, want := range []string{`"gcm"`, "--prefix", "--storage=single"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the refusal %q does not mention %s", stderr, want)
		}
	}
	if len(f.Files) != 0 {
		t.Errorf("the refused init wrote %d file(s)", len(f.Files))
	}
}

// The API's city create shares doInit and cannot pass --prefix or --storage,
// so a directory whose name derives a reserved bead prefix is created
// single-store, as before the split default, rather than refused (an untyped
// scaffold error the API answered with a 500). Any other directory is born
// split.
func TestAPIInitOfAReservedPrefixDirectoryStartsSingleStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	root := t.TempDir()
	apiInit := func(name string) *config.City {
		t.Helper()
		dir := filepath.Join(root, name)
		if err := cityInitDoInit(context.Background(), cityinit.InitRequest{Dir: dir, ConfigName: "tutorial", Provider: "claude"}); err != nil {
			t.Fatalf("API init of %s = %v", name, err)
		}
		data, err := os.ReadFile(filepath.Join(dir, "city.toml"))
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := config.Parse(data)
		if err != nil {
			t.Fatalf("parsing %s's city.toml: %v", name, err)
		}
		return cfg
	}
	for _, name := range []string{"gas-city-sandbox", "gas-city-main", "gas-city-ops"} {
		if cfg := apiInit(name); config.RelocatesAnyClass(cfg) {
			t.Errorf("API init of %s wrote a split [storage] %+v; the prefix its name derives is reserved", name, cfg.Storage)
		}
	}
	assertBornSplit(t, apiInit("bright-lights"))
}

// defaultInitCityOnDisk runs a plain gc init, with neither --storage nor
// --prefix, into a new directory on disk and returns its path. The city is
// born split.
func defaultInitCityOnDisk(t *testing.T) string {
	t.Helper()
	clearGCEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	var stdout, stderr bytes.Buffer
	wiz := wizardConfig{configName: "minimal", defaultProvider: "codex", providers: []string{"codex"}}
	if code := doInit(fsys.OSFS{}, cityPath, wiz, "", &stdout, &stderr, false); code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing the new city's city.toml: %v", err)
	}
	assertBornSplit(t, cfg)
	return cityPath
}

// Every new city is born split, so the reserved-prefix refusal at rig add
// (split-default R10) meets ordinary repository names: graphql-code-generator
// derives gcg. The refused add writes no [[rigs]] entry, so the refusal names
// the add's own --prefix rather than a [[rigs]] entry to edit, and the add
// with that --prefix succeeds.
func TestRigAddOnADefaultCityNamesTheAddPrefixForAReservedDerivedPrefix(t *testing.T) {
	cityPath := defaultInitCityOnDisk(t)
	tomlPath := filepath.Join(cityPath, "city.toml")
	before, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "graphql-code-generator")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, nil, "", "", "", false, false, &stdout, &stderr); code == 0 {
		t.Fatalf("gc rig add of graphql-code-generator on a born-split city exited 0; stdout=%s", stdout.String())
	}
	for _, want := range []string{`"gcg"`, "gc rig add --prefix <prefix>"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the refusal %q does not mention %s", stderr.String(), want)
		}
	}
	if strings.Contains(stderr.String(), "[[rigs]] entry") {
		t.Errorf("the refusal names a [[rigs]] entry the refused add never wrote: %q", stderr.String())
	}
	after, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the refused rig add rewrote city.toml:\n%s", after)
	}

	stdout.Reset()
	stderr.Reset()
	if code := doRigAdd(fsys.OSFS{}, cityPath, rigDir, nil, "", "gqlcg", "", false, false, &stdout, &stderr); code != 0 {
		t.Fatalf("gc rig add --prefix gqlcg, the fix the refusal names, = %d: %s", code, stderr.String())
	}
}

// The API's rig create provisions through the same refusal, and it reaches
// the API's error mapping as ErrReservedBeadPrefix, which the API answers 400
// rather than 500, with nothing written.
func TestAPIRigCreateOfAReservedDerivedPrefixOnADefaultCityIsTheCallersError(t *testing.T) {
	cityPath := defaultInitCityOnDisk(t)
	tomlPath := filepath.Join(cityPath, "city.toml")
	before, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	cs := newControllerState(context.Background(), &config.City{Workspace: config.Workspace{Name: "bright-lights"}}, runtime.NewFake(), events.NewFake(), "bright-lights", cityPath)

	err = cs.CreateRig(config.Rig{Name: "graphql-code-generator", Path: filepath.Join(cityPath, "graphql-code-generator")})
	if !errors.Is(err, config.ErrReservedBeadPrefix) {
		t.Fatalf("CreateRig of graphql-code-generator = %v, want the reserved-prefix refusal (ErrReservedBeadPrefix)", err)
	}
	if !strings.Contains(err.Error(), `the "prefix" field of an API rig create`) {
		t.Errorf("the refusal %q does not name the API's prefix field", err)
	}
	after, err := os.ReadFile(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("the refused create rewrote city.toml:\n%s", after)
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
		t.Errorf("--storage=single printed %q, want nothing for an explicit opt-out", stderr)
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
	if strings.Contains(stderr, "starts single-store") {
		t.Errorf("an explicit split request was answered with the default's single-store notice: %q", stderr)
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

// --from never adds the split (F3: a K8S pod inits with --from) and keeps a
// source's split wherever this host can serve it (initFromSplitStorageNotice),
// so --storage and --prefix are refused with it, as they are with --file,
// whose source config owns both.
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

// gc init --json reports the storage layout the new city was written with,
// read from its city.toml, so a caller sees a split request that started
// single-store.
func TestInitJSONReportsTheStorageLayout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		toml    string
		want    string
		present bool
	}{
		{name: "born split", toml: "[workspace]\nname = \"bright-lights\"\n", want: "split", present: true},
		{name: "single-store", toml: "[workspace]\nname = \"bright-lights\"\n", want: "single", present: true},
		{name: "no city.toml", present: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := filepath.Join(t.TempDir(), "city")
			if tc.toml != "" {
				cfg, perr := config.Parse([]byte(tc.toml))
				if perr != nil {
					t.Fatal(perr)
				}
				if tc.want == "split" {
					cfg.Storage = bornSplitStorageConfig()
				}
				data, merr := cfg.Marshal()
				if merr != nil {
					t.Fatal(merr)
				}
				if err := os.MkdirAll(cityPath, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var stdout bytes.Buffer
			if err := writeInitJSONOrExit(0, true, []string{cityPath}, "", "minimal", "", nil, "", "default", &stdout); err != nil {
				t.Fatalf("writeInitJSONOrExit: %v", err)
			}
			got := requireSingleJSONLine(t, &stdout)
			value, ok := got["storage"]
			if ok != tc.present || (tc.present && value != tc.want) {
				t.Fatalf("init JSON storage = %v (present %t), want %q (present %t): %+v", value, ok, tc.want, tc.present, got)
			}
		})
	}
}

// A born-split city runs the v2 session reconciler from its first boot unless
// its config carries a v2 gap, which keeps it on legacy with the reason named
// (design F11): the block gc init writes puts sessions and graph on the
// sqlite-beads binding v2 needs.
func TestBornSplitCityResolvesTheV2ReconcilerUnlessAGapIsConfigured(t *testing.T) {
	noEnv := func(string) (string, bool) { return "", false }
	bornSplit := &config.City{Storage: bornSplitStorageConfig()}
	res, err := resolveReconcilerMode(bornSplit, "", noEnv)
	if err != nil {
		t.Fatalf("resolveReconcilerMode: %v", err)
	}
	if res.Mode != reconcilerV2 {
		t.Fatalf("a born-split city resolves %v (%v), want v2", res.Mode, res.Fallbacks)
	}
	gap := &config.City{Storage: bornSplitStorageConfig(), Agents: []config.Agent{{Name: "worker", DependsOn: []string{"db"}}}}
	res, err = resolveReconcilerMode(gap, "", noEnv)
	if err != nil {
		t.Fatalf("resolveReconcilerMode with a v2 gap: %v", err)
	}
	if res.Mode != reconcilerLegacy || !strings.Contains(strings.Join(res.Fallbacks, "\n"), "depends_on") {
		t.Fatalf("a born-split city with depends_on resolves %v (%v), want legacy naming depends_on", res.Mode, res.Fallbacks)
	}
}

// initFromSplitSourceOnK8s is remoteStageSplitSource with a k8s session
// runtime: a split source a plain gc init would start single-store.
const initFromSplitSourceOnK8s = remoteStageSplitSource + `
[session]
provider = "k8s"
`

// gc init --from keeps the split a source declares exactly where a plain gc
// init is born split (resolveInitStorage): a bd a split city's gc bd refuses,
// or a remote session runtime, holds the copy single-store with plain init's
// notice. It never adds a split, and a remote stage copies its controller's
// [storage] as it is (F3).
func TestInitFromSplitStorageNoticeAppliesPlainInitAdmission(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		setup  func(t *testing.T) (bdPath string)
		// wantNotice lists what the notice names; nil means the copy keeps
		// the source's [storage].
		wantNotice []string
	}{
		{
			name:   "admitted bd keeps the split",
			source: remoteStageSplitSource,
			setup: func(t *testing.T) string {
				t.Setenv("GC_BEADS", "bd")
				bd := initStorageFakeBD(t)
				admitInitSplitBD(t)
				return bd
			},
		},
		{
			name:   "bd below the split floor",
			source: remoteStageSplitSource,
			setup: func(t *testing.T) string {
				t.Setenv("GC_BEADS", "bd")
				bd := initStorageFakeBD(t)
				stubInitBDVersion(t, "bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)")
				stubInitSplitBDJudge(t, "bd-1.3.2", nil)
				return bd
			},
			wantNotice: []string{"single-store", "v" + initSplitBDMinVersion, "upgrade bd", "gc storage migrate"},
		},
		{
			name:   "bd matching no pinned tree",
			source: remoteStageSplitSource,
			setup: func(t *testing.T) string {
				t.Setenv("GC_BEADS", "bd")
				bd := initStorageFakeBD(t)
				stubInitBDVersion(t, "bd version 1.3.4 (wrapped)")
				stubInitSplitBDJudge(t, "", nil)
				return bd
			},
			wantNotice: []string{"matches no pinned gc bd inventory", "single-store", "upgrade bd"},
		},
		{
			name:   "remote session runtime",
			source: initFromSplitSourceOnK8s,
			setup: func(t *testing.T) string {
				t.Setenv("GC_BEADS", "file")
				return ""
			},
			wantNotice: []string{`"k8s"`, "remote hosts", "single-store"},
		},
		{
			name:   "single-store source on a bd below the floor",
			source: "[workspace]\nname = \"template\"\n",
			setup: func(t *testing.T) string {
				t.Setenv("GC_BEADS", "bd")
				initStorageFakeBD(t)
				stubInitBDVersion(t, "bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)")
				stubInitSplitBDJudge(t, "bd-1.3.2", nil)
				return ""
			},
		},
		{
			name:   "pod remote stage on a bd below the floor",
			source: remoteStageSplitSource,
			setup: func(t *testing.T) string {
				t.Setenv("GC_BEADS", "bd")
				t.Setenv(remoteStageControllerEnv, "/city")
				initStorageFakeBD(t)
				stubInitBDVersion(t, "bd version 1.3.1 (c1c4b642a: HEAD@c1c4b642ac1c)")
				stubInitSplitBDJudge(t, "bd-1.3.2", nil)
				return ""
			},
		},
		{
			name:   "--remote-stage on a remote session runtime",
			source: initFromSplitSourceOnK8s,
			setup: func(t *testing.T) string {
				t.Setenv("GC_BEADS", "file")
				setInitRemoteStage(t, "/city")
				return ""
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_SESSION", "")
			t.Setenv(remoteStageControllerEnv, "")
			bd := tc.setup(t)
			srcToml := filepath.Join(t.TempDir(), "city.toml")
			if err := os.WriteFile(srcToml, []byte(tc.source), 0o644); err != nil {
				t.Fatal(err)
			}
			notice, err := initFromSplitStorageNotice(fsys.OSFS{}, srcToml, filepath.Join(t.TempDir(), "city"))
			if err != nil {
				t.Fatalf("initFromSplitStorageNotice: %v", err)
			}
			if tc.wantNotice == nil {
				if notice != "" {
					t.Fatalf("notice = %q; want the copy to keep the source's [storage]", notice)
				}
				return
			}
			if bd != "" {
				tc.wantNotice = append(tc.wantNotice, bd)
			}
			for _, want := range tc.wantNotice {
				if !strings.Contains(notice, want) {
					t.Errorf("notice %q does not mention %q", notice, want)
				}
			}
		})
	}
}

// On a file work store gc never judges a bd, for --from as for a plain init.
func TestInitFromSplitStorageNoticeOnAFileStoreNeverProbesBD(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "")
	t.Setenv(remoteStageControllerEnv, "")
	initStorageFakeBD(t)
	judged := stubInitSplitBDJudge(t, "", nil)
	srcToml := filepath.Join(t.TempDir(), "city.toml")
	if err := os.WriteFile(srcToml, []byte(remoteStageSplitSource), 0o644); err != nil {
		t.Fatal(err)
	}
	notice, err := initFromSplitStorageNotice(fsys.OSFS{}, srcToml, filepath.Join(t.TempDir(), "city"))
	if err != nil || notice != "" {
		t.Fatalf("initFromSplitStorageNotice = %q, %v; want the split kept", notice, err)
	}
	if len(*judged) != 0 {
		t.Fatalf("a file work store judged bd %v", *judged)
	}
}

// A copy held single-store is written without the source's [storage], says
// so, and leaves the source alone. It is judged as the single-store city it
// becomes, so a name whose derived prefix only a split city reserves (gco) is
// not refused.
func TestInitFromASplitSourceStartsSingleStoreWherePlainInitWould(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv(remoteStageControllerEnv, "")
	configureIsolatedRuntimeEnv(t)
	src := newRemoteStageSource(t)
	srcToml := filepath.Join(src, "city.toml")
	if err := os.WriteFile(srcToml, []byte(initFromSplitSourceOnK8s), 0o644); err != nil {
		t.Fatal(err)
	}
	cityPath := filepath.Join(t.TempDir(), "gas-city-ops")

	var stdout, stderr bytes.Buffer
	if code := doInitFromDirWithOptionsInternal(src, cityPath, "", &stdout, &stderr, true, true, hostedDoltInitOptions{}); code != 0 {
		t.Fatalf("gc init --from a split k8s source = %d; stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing the copy's city.toml: %v\n%s", err, data)
	}
	if cfg.Storage != nil || config.RelocatesAnyClass(cfg) {
		t.Fatalf("the copy kept the source's [storage] %+v:\n%s", cfg.Storage, data)
	}
	for _, want := range []string{`"k8s"`, "single-store", "leaves out the [storage] of " + srcToml} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not mention %q", stderr.String(), want)
		}
	}
	if layout, err := initStorageLayout(cityPath); err != nil || layout != string(initStorageSingle) {
		t.Errorf("initStorageLayout = %q, %v; want %q for gc init --json", layout, err, initStorageSingle)
	}
	after, err := os.ReadFile(srcToml)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != initFromSplitSourceOnK8s {
		t.Errorf("gc init --from rewrote its source:\n%s", after)
	}
}

// A remote stage copies its controller's split as it is, even where this
// host's own sessions would hold a new city single-store (F3): --remote-stage,
// or the pod environment initCityInPod runs gc init --from in.
func TestInitFromARemoteStageKeepsTheControllersSplit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T)
	}{
		{name: "--remote-stage", stage: func(t *testing.T) { setInitRemoteStage(t, "/city") }},
		{name: "pod environment", stage: func(t *testing.T) { t.Setenv(remoteStageControllerEnv, "/city") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_DOLT", "skip")
			t.Setenv(remoteStageControllerEnv, "")
			configureIsolatedRuntimeEnv(t)
			src := newRemoteStageSource(t)
			if err := os.WriteFile(filepath.Join(src, "city.toml"), []byte(initFromSplitSourceOnK8s), 0o644); err != nil {
				t.Fatal(err)
			}
			tc.stage(t)
			pod := filepath.Join(t.TempDir(), "workspace")

			var stdout, stderr bytes.Buffer
			if code := doInitFromDirWithOptionsInternal(src, pod, "", &stdout, &stderr, true, true, hostedDoltInitOptions{}); code != 0 {
				t.Fatalf("gc init --from a remote stage = %d; stderr=%s", code, stderr.String())
			}
			data, err := os.ReadFile(filepath.Join(pod, "city.toml"))
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Parse(data)
			if err != nil {
				t.Fatalf("parsing the stage's city.toml: %v\n%s", err, data)
			}
			if !config.RelocatesAnyClass(cfg) {
				t.Fatalf("the remote stage dropped its controller's [storage]:\n%s", data)
			}
			if strings.Contains(stderr.String(), "single-store") {
				t.Errorf("the remote stage printed a single-store notice: %s", stderr.String())
			}
		})
	}
}

// The shipped examples that say they carry the split gc init writes carry
// exactly bornSplitStorageConfig, so a change to that block moves them with
// it. hyperscale runs its sessions in pods, where gc init starts a city
// single-store, so it carries none.
func TestShippedExamplesCarryTheBornSplitStorage(t *testing.T) {
	t.Setenv("GC_SESSION", "")
	for _, tc := range []struct {
		example string
		split   bool
	}{
		{example: "gastown", split: true},
		{example: "lifecycle", split: true},
		{example: "swarm", split: true},
		{example: "t3bridge-gastown", split: true},
		{example: "hyperscale"},
	} {
		t.Run(tc.example, func(t *testing.T) {
			path := filepath.Join("..", "..", "examples", tc.example, "city.toml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			cfg, err := config.Parse(data)
			if err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}
			if tc.split {
				if !reflect.DeepEqual(cfg.Storage, bornSplitStorageConfig()) {
					t.Fatalf("%s [storage] = %+v, want the block gc init writes, %+v", path, cfg.Storage, bornSplitStorageConfig())
				}
				return
			}
			if cfg.Storage != nil {
				t.Fatalf("%s declares [storage] %+v; want none", path, cfg.Storage)
			}
			if runtime, remote := cityRunsRemoteSessions(cfg, t.TempDir()); !remote {
				t.Fatalf("%s runs sessions locally (%q), so gc init would split it; carry the born-split block", path, runtime)
			}
		})
	}
}
