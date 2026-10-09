package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// These tests pin #6054: `gc sling <target> <formula> --formula` on a city whose
// graph class is relocated to a [storage] binding. The wisp root is minted in
// the graph binding, so the routing stamp must land there too; before the fix
// it targeted the work store and failed with "bead not found", leaking one
// open, unrouted wisp into the binding per attempt.

const splitSlingFormulaName = "split-root-only"

// writeSplitSlingFormulaCity adds a city-scoped pool agent and a root-only
// (Ready-visible) formula to a one-shot city, the minimal shape a pool sling
// of a --formula accepts.
func writeSplitSlingFormulaCity(t *testing.T, cityPath string) {
	t.Helper()
	formulaDir := filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("mkdir formulas: %v", err)
	}
	formulaBody := "formula = \"" + splitSlingFormulaName + "\"\nversion = 1\nphase = \"vapor\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n"
	if err := os.WriteFile(filepath.Join(formulaDir, splitSlingFormulaName+".toml"), []byte(formulaBody), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(cityPath, "city.toml"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open city.toml: %v", err)
	}
	defer f.Close() //nolint:errcheck
	if _, err := f.WriteString("\n[[agent]]\nname = \"worker\"\nmax_active_sessions = 3\nmin_active_sessions = 0\n"); err != nil {
		t.Fatalf("append agent: %v", err)
	}
}

// assertSplitFormulaSlingRoutesInTheBinding runs the real `gc sling --formula`
// command on cityPath and asserts the wisp root it minted is routed in the
// graph binding, absent from the work store, and served by `gc ready`.
func assertSplitFormulaSlingRoutesInTheBinding(t *testing.T, cityPath string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cmdSling(
		[]string{"worker", splitSlingFormulaName},
		true, false, false, // isFormula, doNudge, force
		"", nil, "",
		false, false, false, "", // noConvoy, owned, reassign, onFormula
		false, false, false, // noFormula, fromStdin, dryRun
		"", "",
		&stdout, &stderr,
	)
	if code != 0 {
		t.Fatalf("gc sling --formula on a split city exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}

	graph, relocated := cliStorageRoutes(cityPath).storeFor(coordclass.ClassGraph)
	if !relocated {
		t.Fatal("graph class is not relocated; the fixture is not a split city")
	}
	all, err := graph.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("list graph binding: %v", err)
	}
	var roots []beads.Bead
	for _, b := range all {
		if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWisp {
			roots = append(roots, b)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("graph binding holds %d wisp roots, want exactly 1: %+v", len(roots), roots)
	}
	root := roots[0]
	if got := root.Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
		t.Errorf("binding-resident wisp root %s gc.routed_to = %q, want worker (an unrouted wisp leaks into the binding)", root.ID, got)
	}

	work, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	if _, err := work.Get(root.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("wisp root %s resolves in the work store (err=%v); it must live only in the graph binding", root.ID, err)
	}

	resetCLIStorageRoutes(t)
	var readyOut, readyErr bytes.Buffer
	if rc := cmdReady(readyOpts{}, &readyOut, &readyErr); rc != 0 {
		t.Fatalf("gc ready exit = %d: %s", rc, readyErr.String())
	}
	var rows []struct {
		ID       string            `json:"id"`
		Metadata map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(readyOut.Bytes(), &rows); err != nil {
		t.Fatalf("decode gc ready: %v: %s", err, readyOut.String())
	}
	found := false
	for _, row := range rows {
		if row.ID == root.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("gc ready does not serve the slung wisp root %s; got %s", root.ID, readyOut.String())
	}
}

func TestCmdSlingFormulaRoutesTheWispInTheGraphBindingOnABornSplitCity(t *testing.T) {
	cityPath := oneShotCLICity(t, filepath.Join(t.TempDir(), "store"))
	writeSplitSlingFormulaCity(t, cityPath)
	assertSplitFormulaSlingRoutesInTheBinding(t, cityPath)
}

func TestCmdSlingFormulaRoutesTheWispInTheGraphBindingOnAMigratedCity(t *testing.T) {
	cityPath, _ := migratedOneShotCLICity(t)
	writeSplitSlingFormulaCity(t, cityPath)
	resetCLIStorageRoutes(t)
	assertSplitFormulaSlingRoutesInTheBinding(t, cityPath)
}

// TestSlingFormulaRootFeedsPoolDemandOnBothTopologies is the dispatch half of
// #6054: a --formula slung through the CLI sling deps (cliBeadRouter) onto a
// pool must become controller demand on both topologies. On a split city the
// demand scan reads the binding, which is where the root is minted and must
// therefore be routed.
func TestSlingFormulaRootFeedsPoolDemandOnBothTopologies(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		formulaDir := t.TempDir()
		formulaBody := "formula = \"" + splitSlingFormulaName + "\"\nversion = 1\nphase = \"vapor\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n"
		if err := os.WriteFile(filepath.Join(formulaDir, splitSlingFormulaName+".toml"), []byte(formulaBody), 0o644); err != nil {
			t.Fatalf("write formula: %v", err)
		}
		maxSess, minSess := 3, 0
		e.cfg.Agents = []config.Agent{{Name: "worker", MaxActiveSessions: &maxSess, MinActiveSessions: &minSess, Provider: "mock"}}
		e.cfg.Providers = map[string]config.ProviderSpec{"mock": {Command: "true"}}
		e.cfg.FormulaLayers.City = []string{formulaDir}

		deps := slingDeps{
			CityName:   "split-topology-city",
			CityPath:   e.cityPath,
			Cfg:        e.cfg,
			Runner:     newFakeRunner().run,
			Store:      e.work,
			GraphStore: e.graphStore(),
			StoreRef:   "city:split-topology-city",
		}
		var stdout, stderr bytes.Buffer
		opts := testOpts(e.cfg.Agents[0], splitSlingFormulaName)
		opts.IsFormula = true
		if code := doSling(opts, deps, nil, &stdout, &stderr); code != 0 {
			t.Fatalf("doSling --formula exit = %d; stderr: %s", code, stderr.String())
		}

		state := buildDesiredStateWithSessionBeads(
			"split-topology-city", e.cityPath, time.Now(), e.cfg, &localMockProvider{},
			e.sessionsStore(), nil, &sessionBeadSnapshot{}, nil, &stderr,
		)
		if got := state.ScaleCheckCounts["worker"]; got != 1 {
			t.Errorf("pool demand for worker = %d, want 1 (the slung --formula root is invisible to the controller demand scan)", got)
		}
	})
}
