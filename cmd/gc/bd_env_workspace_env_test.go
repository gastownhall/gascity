package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// workspaceEnvStamp is the fixed mtime the memo tests give city.toml so a
// rewrite is always observable through the stat stamp, independent of the
// filesystem's timestamp resolution.
var workspaceEnvStamp = time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)

func writeWorkspaceEnvCityTOML(t *testing.T, cityPath, body string, stamp time.Time) {
	t.Helper()
	path := filepath.Join(cityPath, "city.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
}

// writeWorkspaceEnvIncludedFragment writes a fragment that declares its own
// [workspace.env]. The composed load drops it, so the narrow reader must too.
func writeWorkspaceEnvIncludedFragment(t *testing.T, cityPath, pinDir string) {
	t.Helper()
	body := "[workspace]\n[workspace.env]\nPATH = " + strconv.Quote(pinDir) + "\nFRAGMENT_ONLY = \"1\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "fragment.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestWorkspaceEnvTableMatchesTheComposedLoad is the equivalence guard: for a
// city that needs no lock, the root-only read must agree with the composed
// load's Workspace.Env. If composition ever starts carrying env out of an
// included fragment, this fails and the narrow reader has to follow.
func TestWorkspaceEnvTableMatchesTheComposedLoad(t *testing.T) {
	pinDir := t.TempDir()
	cases := []struct {
		name string
		toml string
		frag bool
		want map[string]string
	}{
		{
			name: "root only",
			toml: "[workspace]\nname = \"demo\"\n[workspace.env]\nPATH = " + strconv.Quote(pinDir) + "\nOTHER = \"x\"\n",
			want: map[string]string{"PATH": pinDir, "OTHER": "x"},
		},
		{
			name: "none",
			toml: "[workspace]\nname = \"demo\"\n",
			want: nil,
		},
		{
			name: "fragment only",
			toml: "include = [\"fragment.toml\"]\n[workspace]\nname = \"demo\"\n",
			frag: true,
			want: nil,
		},
		{
			name: "root and fragment",
			toml: "include = [\"fragment.toml\"]\n[workspace]\nname = \"demo\"\n[workspace.env]\nROOT_ONLY = \"r\"\n",
			frag: true,
			want: map[string]string{"ROOT_ONLY": "r"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetWorkspaceEnvCache()
			t.Cleanup(resetWorkspaceEnvCache)
			cityPath := t.TempDir()
			writeWorkspaceEnvCityTOML(t, cityPath, tc.toml, workspaceEnvStamp)
			if tc.frag {
				writeWorkspaceEnvIncludedFragment(t, cityPath, pinDir)
			}

			got, err := workspaceEnvTable(cityPath)
			if err != nil {
				t.Fatalf("workspaceEnvTable: %v", err)
			}
			cfg, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)
			if err != nil {
				t.Fatalf("composed load: %v", err)
			}
			if len(got) != len(cfg.Workspace.Env) || (len(got) > 0 && !reflect.DeepEqual(got, cfg.Workspace.Env)) {
				t.Fatalf("workspaceEnvTable = %v, composed Workspace.Env = %v", got, cfg.Workspace.Env)
			}
			if len(got) != len(tc.want) || (len(got) > 0 && !reflect.DeepEqual(got, tc.want)) {
				t.Fatalf("workspaceEnvTable = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWorkspacePinResolvesWhenAnImportCannotBeResolved pins the §6 behavior
// change: the pin comes from the root file alone, so a composition failure
// (here an import that does not exist) no longer fails it.
func TestWorkspacePinResolvesWhenAnImportCannotBeResolved(t *testing.T) {
	resetWorkspaceEnvCache()
	t.Cleanup(resetWorkspaceEnvCache)
	cityPath := t.TempDir()
	pinDir := t.TempDir()
	pinned := filepath.Join(pinDir, "bd")
	writeExecutable(t, pinned, "#!/bin/sh\nexit 0\n")
	missing := filepath.Join(t.TempDir(), "no-such-pack")
	writeWorkspaceEnvCityTOML(t, cityPath,
		"[workspace]\nname = \"demo\"\n[workspace.env]\nPATH = "+strconv.Quote(pinDir)+"\n[imports.ghost]\nsource = "+strconv.Quote(missing)+"\n",
		workspaceEnvStamp)
	t.Setenv("PATH", t.TempDir())

	if _, err := loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard); err == nil {
		t.Fatal("fixture is vacuous: the composed load of an unresolvable import succeeded")
	}
	for name, resolve := range map[string]func(string) (string, error){
		"optional": workspacePinnedBdBinaryOptional,
		"strict":   workspacePinnedBdBinary,
	} {
		got, err := resolve(cityPath)
		if err != nil {
			t.Fatalf("%s pin: %v", name, err)
		}
		if got != pinned {
			t.Fatalf("%s pin = %q, want %q", name, got, pinned)
		}
	}
}

// TestWorkspaceStrictPinReportsPathWithoutBdFromTheMemo keeps the strict
// resolver's configuration error intact now that it answers from the table.
func TestWorkspaceStrictPinReportsPathWithoutBdFromTheMemo(t *testing.T) {
	resetWorkspaceEnvCache()
	t.Cleanup(resetWorkspaceEnvCache)
	cityPath := t.TempDir()
	emptyDir := t.TempDir()
	writeWorkspaceEnvCityTOML(t, cityPath,
		"[workspace]\nname = \"demo\"\n[workspace.env]\nPATH = "+strconv.Quote(emptyDir)+"\n", workspaceEnvStamp)

	loadsBefore := workspaceEnvLoads.Load()
	_, err := workspacePinnedBdBinary(cityPath)
	if err == nil || !strings.Contains(err.Error(), "workspace.env PATH is configured but contains no executable bd at an absolute path") {
		t.Fatalf("strict pin err = %v, want the PATH-without-bd configuration error", err)
	}
	if got := workspaceEnvLoads.Load() - loadsBefore; got != 1 {
		t.Fatalf("file reads for one strict pin = %d, want 1 (optional and strict must share the memo)", got)
	}
}

func TestWorkspaceEnvTableIsMemoizedByCityTOMLStat(t *testing.T) {
	resetWorkspaceEnvCache()
	t.Cleanup(resetWorkspaceEnvCache)
	cityPath := t.TempDir()
	writeWorkspaceEnvCityTOML(t, cityPath, "[workspace]\nname = \"demo\"\n[workspace.env]\nK = \"one\"\n", workspaceEnvStamp)

	loadsBefore := workspaceEnvLoads.Load()
	for i := 0; i < 3; i++ {
		got, err := workspaceEnvTable(cityPath)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got["K"] != "one" {
			t.Fatalf("read %d: K = %q, want one", i, got["K"])
		}
	}
	if got := workspaceEnvLoads.Load() - loadsBefore; got != 1 {
		t.Fatalf("file reads for three lookups = %d, want 1", got)
	}

	// The memo hands out clones: a caller that scribbles on its copy must not
	// poison the next caller's view.
	first, err := workspaceEnvTable(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	first["K"] = "scribbled"
	first["EXTRA"] = "x"
	second, err := workspaceEnvTable(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if second["K"] != "one" || len(second) != 1 {
		t.Fatalf("memo was mutated through a returned table: %v", second)
	}

	// A rewrite observed through the stat stamp invalidates the entry.
	writeWorkspaceEnvCityTOML(t, cityPath, "[workspace]\nname = \"demo\"\n[workspace.env]\nK = \"two\"\n", workspaceEnvStamp.Add(time.Second))
	loadsBefore = workspaceEnvLoads.Load()
	got, err := workspaceEnvTable(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if got["K"] != "two" {
		t.Fatalf("K after rewrite = %q, want two", got["K"])
	}
	if n := workspaceEnvLoads.Load() - loadsBefore; n != 1 {
		t.Fatalf("file reads after a rewrite = %d, want 1", n)
	}

	// Touching only the mtime (same size, same bytes) also invalidates.
	if err := os.Chtimes(filepath.Join(cityPath, "city.toml"), workspaceEnvStamp.Add(time.Hour), workspaceEnvStamp.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	loadsBefore = workspaceEnvLoads.Load()
	if _, err := workspaceEnvTable(cityPath); err != nil {
		t.Fatal(err)
	}
	if n := workspaceEnvLoads.Load() - loadsBefore; n != 1 {
		t.Fatalf("file reads after an mtime change = %d, want 1", n)
	}
}

func TestWorkspaceEnvTableDoesNotCacheErrorsOrMissingCities(t *testing.T) {
	resetWorkspaceEnvCache()
	t.Cleanup(resetWorkspaceEnvCache)
	cityPath := t.TempDir()

	// A missing city.toml is the empty answer, and caches nothing.
	loadsBefore := workspaceEnvLoads.Load()
	if got, err := workspaceEnvTable(cityPath); err != nil || len(got) != 0 {
		t.Fatalf("missing city.toml: table=%v err=%v, want empty,nil", got, err)
	}
	if got, err := workspaceEnvTable(cityPath); err != nil || len(got) != 0 {
		t.Fatalf("missing city.toml (second): table=%v err=%v, want empty,nil", got, err)
	}
	cityConfig := filepath.Join(cityPath, "city.toml")
	if _, cached := workspaceEnvCache.Load(normalizePathForCompare(cityConfig)); cached {
		t.Fatal("a missing city.toml was memoized")
	}

	// A malformed file fails and the failure is not memoized.
	if err := os.WriteFile(cityConfig, []byte("[workspace"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceEnvTable(cityPath); err == nil {
		t.Fatal("malformed city.toml did not fail")
	}
	if _, cached := workspaceEnvCache.Load(normalizePathForCompare(cityConfig)); cached {
		t.Fatal("a failed read was memoized")
	}

	// Repaired: read once, then served from the memo.
	writeWorkspaceEnvCityTOML(t, cityPath, "[workspace]\nname = \"demo\"\n[workspace.env]\nK = \"v\"\n", workspaceEnvStamp)
	loadsBefore = workspaceEnvLoads.Load()
	for i := 0; i < 2; i++ {
		got, err := workspaceEnvTable(cityPath)
		if err != nil || got["K"] != "v" {
			t.Fatalf("repaired read %d: table=%v err=%v", i, got, err)
		}
	}
	if n := workspaceEnvLoads.Load() - loadsBefore; n != 1 {
		t.Fatalf("file reads after repair = %d, want 1", n)
	}

	// The optional resolver treats a missing file as "no pin" without a read.
	missing := t.TempDir()
	loadsBefore = workspaceEnvLoads.Load()
	if pinned, err := workspacePinnedBdBinaryOptional(missing); err != nil || pinned != "" {
		t.Fatalf("pin for a city without city.toml = %q, %v; want empty,nil", pinned, err)
	}
	if n := workspaceEnvLoads.Load() - loadsBefore; n != 0 {
		t.Fatalf("file reads for a city without city.toml = %d, want 0", n)
	}
}

// TestWorkspacePinObservesTheAmbientEnvironmentOnEveryCall pins that the memo
// holds the RAW table: expandEnvMap reads the process environment, so a
// memoized expanded value would go stale when the environment changes.
func TestWorkspacePinObservesTheAmbientEnvironmentOnEveryCall(t *testing.T) {
	resetWorkspaceEnvCache()
	t.Cleanup(resetWorkspaceEnvCache)
	cityPath := t.TempDir()
	dirA := t.TempDir()
	dirB := t.TempDir()
	binA := filepath.Join(dirA, "bd")
	binB := filepath.Join(dirB, "bd")
	writeExecutable(t, binA, "#!/bin/sh\nexit 0\n")
	writeExecutable(t, binB, "#!/bin/sh\nexit 0\n")
	writeWorkspaceEnvCityTOML(t, cityPath,
		"[workspace]\nname = \"demo\"\n[workspace.env]\nBD_BIN = \"${SOME_TEST_DIR}/bd\"\n", workspaceEnvStamp)

	t.Setenv("SOME_TEST_DIR", dirA)
	got, err := workspacePinnedBdBinaryOptional(cityPath)
	if err != nil || got != binA {
		t.Fatalf("pin with SOME_TEST_DIR=A = %q, %v; want %q", got, err, binA)
	}
	loadsBefore := workspaceEnvLoads.Load()
	t.Setenv("SOME_TEST_DIR", dirB)
	got, err = workspacePinnedBdBinaryOptional(cityPath)
	if err != nil || got != binB {
		t.Fatalf("pin with SOME_TEST_DIR=B = %q, %v; want %q", got, err, binB)
	}
	if n := workspaceEnvLoads.Load() - loadsBefore; n != 0 {
		t.Fatalf("file reads after an ambient change = %d, want 0 (re-expansion must not re-read)", n)
	}
}
