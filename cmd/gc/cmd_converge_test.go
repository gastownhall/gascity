package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
)

func TestConvergeCreateGateTimeoutDefaultMatchesSharedDefault(t *testing.T) {
	cmd := newConvergeCreateCmd(io.Discard, io.Discard)
	flag := cmd.Flags().Lookup("gate-timeout")
	if flag == nil {
		t.Fatal("gate-timeout flag not found")
	}

	want := convergence.DefaultGateTimeout.String()
	if flag.DefValue != want {
		t.Fatalf("gate-timeout default = %q, want %q", flag.DefValue, want)
	}
	if got := flag.Value.String(); got != want {
		t.Fatalf("gate-timeout bound value = %q, want %q", got, want)
	}
}

func TestConvergeListAllRigsAggregatesCityAndRigStores(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	cityToml := "[workspace]\nname = \"convtest\"\n\n" +
		"[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"fake\"\n\n" +
		"[[rigs]]\nname = \"frontend\"\npath = " + strconv.Quote(rigDir) + "\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensuring scoped file store layout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensuring city file store: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("ensuring rig file store: %v", err)
	}
	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	createConvergeListTestBead(t, cityStore, "city loop", "")
	createConvergeListTestBead(t, rigStore, "rig loop", "frontend")

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newConvergeListCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--all-rigs", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr:\n%s", err, stderr.String())
	}

	var entries []struct {
		ID    string `json:"id"`
		Rig   string `json:"rig"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal output %q: %v", stdout.String(), err)
	}
	got := map[string]string{}
	for _, entry := range entries {
		got[entry.Title] = entry.Rig
	}
	if got["city loop"] != "" {
		t.Fatalf("city bead rig = %q, want empty", got["city loop"])
	}
	if got["rig loop"] != "frontend" {
		t.Fatalf("rig bead rig = %q, want frontend", got["rig loop"])
	}
}

func TestConvergeListAllRigsContinuesAfterRigStoreError(t *testing.T) {
	cityDir := t.TempDir()
	brokenRigPath := filepath.Join(cityDir, "rigs", "broken")
	frontendRigPath := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(filepath.Dir(brokenRigPath), 0o755); err != nil {
		t.Fatalf("creating rigs dir: %v", err)
	}
	if err := os.WriteFile(brokenRigPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("creating broken rig path: %v", err)
	}
	if err := os.MkdirAll(frontendRigPath, 0o755); err != nil {
		t.Fatalf("creating frontend rig dir: %v", err)
	}
	cityToml := "[workspace]\nname = \"convtest\"\n\n" +
		"[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"fake\"\n\n" +
		"[[rigs]]\nname = \"broken\"\npath = " + strconv.Quote(brokenRigPath) + "\n\n" +
		"[[rigs]]\nname = \"frontend\"\npath = " + strconv.Quote(frontendRigPath) + "\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensuring scoped file store layout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensuring city file store: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(frontendRigPath); err != nil {
		t.Fatalf("ensuring frontend rig file store: %v", err)
	}
	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	frontendStore, err := openStoreAtForCity(frontendRigPath, cityDir)
	if err != nil {
		t.Fatalf("open frontend store: %v", err)
	}
	createConvergeListTestBead(t, cityStore, "city loop", "")
	createConvergeListTestBead(t, frontendStore, "frontend loop", "frontend")

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newConvergeListCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--all-rigs", "--json"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("Execute returned nil, want non-zero error for broken rig")
	}
	if !strings.Contains(stderr.String(), `rig "broken"`) {
		t.Fatalf("stderr = %q, want per-rig error", stderr.String())
	}
	var entries []struct {
		Rig   string `json:"rig"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal output %q: %v", stdout.String(), err)
	}
	got := map[string]string{}
	for _, entry := range entries {
		got[entry.Title] = entry.Rig
	}
	if _, ok := got["city loop"]; !ok {
		t.Fatalf("city loop missing from output after broken rig: %#v", got)
	}
	if got["frontend loop"] != "frontend" {
		t.Fatalf("frontend loop rig = %q, want frontend", got["frontend loop"])
	}
}

func TestConvergeListAllRigsAppliesStateFilterAcrossScopes(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	cityToml := "[workspace]\nname = \"convtest\"\n\n" +
		"[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"fake\"\n\n" +
		"[[rigs]]\nname = \"frontend\"\npath = " + strconv.Quote(rigDir) + "\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensuring scoped file store layout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensuring city file store: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("ensuring rig file store: %v", err)
	}
	cityStore, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	rigStore, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	createConvergeListTestBeadWithState(t, cityStore, "city active", "", convergence.StateActive)
	createConvergeListTestBeadWithState(t, rigStore, "rig active", "frontend", convergence.StateActive)
	createConvergeListTestBeadWithState(t, cityStore, "city waiting", "", convergence.StateWaitingManual)
	createConvergeListTestBeadWithState(t, rigStore, "rig terminated", "frontend", convergence.StateTerminated)

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newConvergeListCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"--all-rigs", "--state", convergence.StateActive, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr:\n%s", err, stderr.String())
	}

	var entries []struct {
		Title string `json:"title"`
		State string `json:"state"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &entries); err != nil {
		t.Fatalf("unmarshal output %q: %v", stdout.String(), err)
	}
	got := map[string]string{}
	for _, entry := range entries {
		got[entry.Title] = entry.State
	}
	if len(got) != 2 {
		t.Fatalf("got entries %#v, want only two active loops", got)
	}
	for _, title := range []string{"city active", "rig active"} {
		if got[title] != convergence.StateActive {
			t.Fatalf("%s state = %q, want active; all entries %#v", title, got[title], got)
		}
	}
}

func createConvergeListTestBead(t *testing.T, store beads.Store, title, rig string) {
	t.Helper()
	createConvergeListTestBeadWithState(t, store, title, rig, convergence.StateActive)
}

func createConvergeListTestBeadWithState(t *testing.T, store beads.Store, title, rig, state string) {
	t.Helper()
	bead, err := store.Create(beads.Bead{Title: title, Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatalf("creating convergence bead: %v", err)
	}
	for key, value := range map[string]string{
		convergence.FieldState:         state,
		convergence.FieldIteration:     "1",
		convergence.FieldMaxIterations: "2",
		convergence.FieldGateMode:      convergence.GateModeManual,
		convergence.FieldFormula:       "test-formula",
		convergence.FieldTarget:        "test-agent",
		convergence.FieldRig:           rig,
	} {
		if err := store.SetMetadata(bead.ID, key, value); err != nil {
			t.Fatalf("setting %s: %v", key, err)
		}
	}
}

func TestConvergeStorePathForContext_RigErrors(t *testing.T) {
	cityDir := t.TempDir()
	cityToml := "[workspace]\nname = \"convtest\"\n\n" +
		"[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"fake\"\n\n" +
		"[[rigs]]\nname = \"unbound-rig\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	t.Run("unknown rig", func(t *testing.T) {
		_, err := convergeStorePathForContext(resolvedContext{CityPath: cityDir, RigName: "ghost-rig"})
		if err == nil {
			t.Fatal("expected error for an unregistered rig")
		}
		if !strings.Contains(err.Error(), "ghost-rig") || !strings.Contains(err.Error(), "not registered") {
			t.Errorf("error = %q, want it to name the rig and say it is not registered", err)
		}
	})

	t.Run("unbound rig", func(t *testing.T) {
		_, err := convergeStorePathForContext(resolvedContext{CityPath: cityDir, RigName: "unbound-rig"})
		if err == nil {
			t.Fatal("expected error for a registered but unbound rig")
		}
		if !strings.Contains(err.Error(), "unbound-rig") || !strings.Contains(err.Error(), "no bead store") {
			t.Errorf("error = %q, want it to name the rig and mention the missing bead store", err)
		}
	})
}

func TestConvergeTestGateUsesRigStorePath(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	cityToml := "[workspace]\nname = \"convtest\"\n\n" +
		"[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"fake\"\n\n" +
		"[[rigs]]\nname = \"frontend\"\npath = " + strconv.Quote(rigDir) + "\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensuring scoped file store layout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("ensuring rig file store: %v", err)
	}
	store, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	bead, err := store.Create(beads.Bead{Title: "gate", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatalf("creating convergence bead: %v", err)
	}

	outputPath := filepath.Join(t.TempDir(), "beads-dir.txt")
	scriptPath := filepath.Join(t.TempDir(), "gate.sh")
	script := "#!/bin/sh\nprintf '%s' \"$BEADS_DIR\" > " + strconv.Quote(outputPath) + "\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing gate script: %v", err)
	}
	for key, value := range map[string]string{
		convergence.FieldState:             convergence.StateActive,
		convergence.FieldGateMode:          convergence.GateModeCondition,
		convergence.FieldGateCondition:     scriptPath,
		convergence.FieldGateTimeout:       convergence.DefaultGateTimeout.String(),
		convergence.FieldGateTimeoutAction: convergence.TimeoutActionIterate,
		convergence.FieldIteration:         "1",
		convergence.FieldMaxIterations:     "2",
		convergence.FieldActiveWisp:        "wisp-1",
		convergence.FieldCityPath:          cityDir,
		convergence.FieldRig:               "frontend",
	} {
		if err := store.SetMetadata(bead.ID, key, value); err != nil {
			t.Fatalf("setting %s: %v", key, err)
		}
	}

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_RIG", "frontend")

	var stdout, stderr bytes.Buffer
	cmd := newConvergeTestGateCmd(&stdout, &stderr)
	cmd.SetArgs([]string{bead.ID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute: %v\nstderr:\n%s", err, stderr.String())
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("reading gate output: %v", err)
	}
	if got, want := string(data), filepath.Join(rigDir, ".beads"); got != want {
		t.Fatalf("BEADS_DIR = %q, want %q\nstdout:\n%s", got, want, stdout.String())
	}
}
