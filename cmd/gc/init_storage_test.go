package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// stubInitBDVersion makes every `bd version` probe gc init runs report line
// (or fail, when line is "").
func stubInitBDVersion(t *testing.T, line string) {
	t.Helper()
	prev := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		if binary != "bd" {
			return prev(binary)
		}
		if line == "" {
			return "", errors.New("bd: not found")
		}
		return line, nil
	}
	t.Cleanup(func() { initRunVersion = prev })
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

// The split floor is bd 1.3.2 (maintainer ruling 2026-11-04): a split request
// on an older bd starts the city single-store, with a hint to upgrade.
func TestInitStorageSplitHonoursTheBDFloor(t *testing.T) {
	for _, tc := range []struct {
		version   string
		wantSplit bool
	}{
		{"bd version 1.3.0 (Homebrew)", false},
		{"bd version 1.3.1 (3ac028bf: main)", false},
		{"bd version 1.3.2 (dev)", true},
		{"bd version 1.3.4", true},
		{"bd version 1.4.0", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			t.Setenv("GC_BEADS", "bd")
			t.Setenv("GC_SESSION", "")
			stubInitBDVersion(t, tc.version)
			_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSplit})
			if code != 0 {
				t.Fatalf("doInit = %d: %s", code, stderr)
			}
			if tc.wantSplit {
				assertBornSplit(t, cfg)
				if stderr != "" {
					t.Errorf("stderr = %q, want nothing on a bd at or above the floor", stderr)
				}
				return
			}
			if cfg.Storage != nil {
				t.Fatalf("a split request on %s wrote [storage] %+v; want single-store", tc.version, cfg.Storage)
			}
			for _, want := range []string{"1.3.2", "single-store", "upgrade bd", "gc storage migrate"} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q does not mention %q", stderr, want)
				}
			}
		})
	}
}

// A bd gc cannot version (missing, or a version line it cannot parse) is not
// judged here: the dependency check and gc bd's own trust check own it.
func TestInitStorageSplitKeepsTheSplitWhenBDCannotBeVersioned(t *testing.T) {
	for _, line := range []string{"", "bd (development build)"} {
		t.Run(line, func(t *testing.T) {
			t.Setenv("GC_BEADS", "bd")
			t.Setenv("GC_SESSION", "")
			stubInitBDVersion(t, line)
			_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSplit})
			if code != 0 {
				t.Fatalf("doInit = %d: %s", code, stderr)
			}
			assertBornSplit(t, cfg)
		})
	}
}

// The floor is a bd floor: a city whose work store does not run bd never asks.
func TestInitStorageSplitOnAFileStoreNeverProbesBD(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "")
	prev := initRunVersion
	initRunVersion = func(binary string) (string, error) {
		if binary == "bd" {
			t.Errorf("gc init probed bd for a file-store city")
		}
		return prev(binary)
	}
	t.Cleanup(func() { initRunVersion = prev })
	_, cfg, code, stderr := initStorageTestCity(t, "/bright-lights", wizardConfig{storage: initStorageSplit})
	if code != 0 {
		t.Fatalf("doInit = %d: %s", code, stderr)
	}
	assertBornSplit(t, cfg)
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
