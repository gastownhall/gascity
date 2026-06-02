package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// versionCheckFormulaName is the formula name every version-check test
// uses. Inlined as a constant to avoid an unparam lint on the helper:
// no test in this file needs a different name today.
const versionCheckFormulaName = "deploy"

const versionCheckFormulaBody = `formula = "deploy"
description = "Deploy flow"

[[steps]]
id = "build"
title = "Build"

[[steps]]
id = "ship"
title = "Ship"
needs = ["build"]
`

// writeVersionCheckCity sets up a minimal city with one formula on disk
// and returns the city dir + the on-disk content hash of the formula
// (so the test can stamp matching or deliberately-mismatching metadata
// onto the bead it later creates).
//
// Mirrors writeTutorialFormulaCity but additionally exposes the recipe
// hash because the version-check command's whole job is comparing
// bead-metadata-recorded hash to current-disk hash. The formula name
// and body are constants because every version-check test uses the
// same fixture; vary the bead-side data instead.
func writeVersionCheckCity(t *testing.T) (cityDir, diskHash string) {
	t.Helper()

	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityDir = t.TempDir()
	writeFile := func(rel, body string) {
		t.Helper()
		path := filepath.Join(cityDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", path, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	writeFile("city.toml", "[workspace]\nname = \"my-city\"\nprovider = \"claude\"\n")
	writeFile("formulas/"+versionCheckFormulaName+".toml", versionCheckFormulaBody)

	recipe, err := formula.Compile(context.Background(), versionCheckFormulaName, []string{filepath.Join(cityDir, "formulas")}, nil)
	if err != nil {
		t.Fatalf("formula.Compile(%s): %v", versionCheckFormulaName, err)
	}
	if recipe.ContentHash == "" {
		t.Fatalf("formula.Compile(%s).ContentHash is empty; the version-check command relies on it being populated", versionCheckFormulaName)
	}
	return cityDir, recipe.ContentHash
}

// createVersionCheckBead opens the city's bead store and creates a
// molecule-like bead whose Ref points at formulaName and whose
// gc.formula_hash metadata is `hash`. Returns the created bead ID.
func createVersionCheckBead(t *testing.T, cityDir, formulaName, hash string) string {
	t.Helper()

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	bead := beads.Bead{
		Title:  "version-check fixture",
		Type:   "molecule",
		Status: "open",
		Ref:    formulaName,
	}
	created, err := store.Create(bead)
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	if hash != "" {
		if err := store.SetMetadata(created.ID, "gc.formula_hash", hash); err != nil {
			t.Fatalf("SetMetadata(gc.formula_hash): %v", err)
		}
	}
	return created.ID
}

// TestFormulaVersionCheck_MatchExitsZero covers the happy path: a bead
// whose gc.formula_hash matches the current on-disk formula. The
// command must print the "matches" line and return without error so
// the process exits 0.
func TestFormulaVersionCheck_MatchExitsZero(t *testing.T) {
	cityDir, diskHash := writeVersionCheckCity(t)
	beadID := createVersionCheckBead(t, cityDir, "deploy", diskHash)
	t.Chdir(cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaVersionCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{beadID})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute on match: err = %v; stderr=%s", err, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "matches on-disk version") {
		t.Errorf("stdout = %q, want 'matches on-disk version'", got)
	}
	if !strings.Contains(got, "deploy") {
		t.Errorf("stdout = %q, want it to name the formula", got)
	}
}

// TestFormulaVersionCheck_DivergeReturnsErrExit covers the unhappy
// path: bead hash differs from disk. The command must print the
// "DIVERGES" line and return errExit so the process exits non-zero.
func TestFormulaVersionCheck_DivergeReturnsErrExit(t *testing.T) {
	cityDir, _ := writeVersionCheckCity(t)
	beadID := createVersionCheckBead(t, cityDir, "deploy", "deadbeefdeadbeefdeadbeefdeadbeef")
	t.Chdir(cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaVersionCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{beadID})
	err := cmd.Execute()
	if !errors.Is(err, errExit) {
		t.Fatalf("Execute on diverge: err = %v, want errExit; stderr=%s", err, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "DIVERGES") {
		t.Errorf("stdout = %q, want 'DIVERGES'", got)
	}
	if !strings.Contains(got, "bead hash") || !strings.Contains(got, "disk hash") {
		t.Errorf("stdout = %q, want both bead/disk hashes shown to operators", got)
	}
}

// TestFormulaVersionCheck_DivergeShowsFormulaPath asserts the optional
// "formula path" line renders on divergence when the recipe was loaded
// from a real path. Without this, the diverge-path conditional Fprintf
// for the formula source goes uncovered.
func TestFormulaVersionCheck_DivergeShowsFormulaPath(t *testing.T) {
	cityDir, _ := writeVersionCheckCity(t)
	beadID := createVersionCheckBead(t, cityDir, "deploy", "deadbeefdeadbeefdeadbeefdeadbeef")
	t.Chdir(cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaVersionCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{beadID})
	if err := cmd.Execute(); !errors.Is(err, errExit) {
		t.Fatalf("Execute on diverge: err = %v, want errExit; stderr=%s", err, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "formula path:") {
		t.Errorf("stdout = %q, want formula-path line when recipe has a source", got)
	}
}

// TestFormulaVersionCheck_JSONOutput covers the --json branch of the
// switch. Asserts the structured payload contains every field the JSON
// schema promises so automated consumers can rely on it.
func TestFormulaVersionCheck_JSONOutput(t *testing.T) {
	cityDir, diskHash := writeVersionCheckCity(t)
	beadID := createVersionCheckBead(t, cityDir, "deploy", diskHash)
	t.Chdir(cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaVersionCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{beadID, "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute on json match: err = %v; stderr=%s", err, stderr.String())
	}

	var got formulaVersionCheckResult
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("decode JSON: %v\nstdout=%q", err, stdout.String())
	}
	if got.BeadID != beadID {
		t.Errorf("BeadID = %q, want %q", got.BeadID, beadID)
	}
	if got.FormulaName != "deploy" {
		t.Errorf("FormulaName = %q, want %q", got.FormulaName, "deploy")
	}
	if got.BeadHash != diskHash || got.DiskHash != diskHash {
		t.Errorf("hashes BeadHash=%q DiskHash=%q, both should equal %q", got.BeadHash, got.DiskHash, diskHash)
	}
	if !got.Match {
		t.Errorf("Match = false, want true (hashes are equal)")
	}
	if got.FormulaPath == "" {
		t.Errorf("FormulaPath empty; should carry the on-disk formula source for operator diagnostics")
	}
}

// TestFormulaVersionCheck_MissingFormulaHashErrors asserts the targeted
// error message when a bead was created before hash tracking. This
// guards the user-facing diagnostic that tells the operator the bead
// pre-dates the feature, rather than a confusing "compile failed".
func TestFormulaVersionCheck_MissingFormulaHashErrors(t *testing.T) {
	cityDir, _ := writeVersionCheckCity(t)
	// hash="" → don't set gc.formula_hash metadata.
	beadID := createVersionCheckBead(t, cityDir, "deploy", "")
	t.Chdir(cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaVersionCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{beadID})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("Execute on missing hash: err = nil, want a 'no gc.formula_hash metadata' error")
	}
	if !strings.Contains(err.Error(), "gc.formula_hash") {
		t.Errorf("err = %q, want it to mention gc.formula_hash", err)
	}
}

// TestFormulaVersionCheck_MissingRefErrors covers the bead-has-no-Ref
// guard. Without this branch under test, operators creating beads
// without a formula reference would see a generic compile error
// instead of the targeted "no Ref (formula name)" diagnostic.
func TestFormulaVersionCheck_MissingRefErrors(t *testing.T) {
	cityDir, _ := writeVersionCheckCity(t)
	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "no-ref fixture",
		Type:   "molecule",
		Status: "open",
		// Ref deliberately empty.
	})
	if err != nil {
		t.Fatalf("store.Create: %v", err)
	}
	if err := store.SetMetadata(created.ID, "gc.formula_hash", "abc123"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	t.Chdir(cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaVersionCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{created.ID})
	execErr := cmd.Execute()
	if execErr == nil {
		t.Fatalf("Execute on missing Ref: err = nil, want a 'no Ref (formula name)' error")
	}
	if !strings.Contains(execErr.Error(), "Ref") || !strings.Contains(execErr.Error(), "formula name") {
		t.Errorf("err = %q, want it to mention missing Ref / formula name", execErr)
	}
}

// TestFormulaVersionCheck_BeadNotFoundErrors covers the store.Get
// error path — bead ID doesn't exist. The diagnostic should name
// the bead ID and wrap the store error so the operator can correlate.
func TestFormulaVersionCheck_BeadNotFoundErrors(t *testing.T) {
	cityDir, _ := writeVersionCheckCity(t)
	t.Chdir(cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaVersionCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{"does-not-exist-1234"})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("Execute on missing bead: err = nil, want a 'reading bead' error")
	}
	if !strings.Contains(err.Error(), "reading bead") || !strings.Contains(err.Error(), "does-not-exist-1234") {
		t.Errorf("err = %q, want it to wrap the missing bead id", err)
	}
}

// TestFormulaVersionCheck_FormulaNotOnDiskErrors covers the
// formula.Compile error path — bead refers to a formula whose on-disk
// file has been deleted. The error must wrap the formula name so the
// operator knows which formula to restore.
func TestFormulaVersionCheck_FormulaNotOnDiskErrors(t *testing.T) {
	cityDir, _ := writeVersionCheckCity(t)
	beadID := createVersionCheckBead(t, cityDir, "ghost-formula", "abc123")
	t.Chdir(cityDir)

	var stdout, stderr bytes.Buffer
	cmd := newFormulaVersionCheckCmd(&stdout, &stderr)
	cmd.SetArgs([]string{beadID})
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("Execute with bead referring to absent formula: err = nil, want a compile error")
	}
	if !strings.Contains(err.Error(), "ghost-formula") {
		t.Errorf("err = %q, want it to name ghost-formula so operators can correlate", err)
	}
}

// TestNewFormulaCmd_RegistersVersionCheckSubcommand is a regression
// guard against the cmd-tree wiring. newFormulaCmd is currently
// uncovered (it is exercised only by the actual gc invocation path);
// covering the subcommand-registration line here also asserts that a
// future refactor doesn't silently drop the version-check subcommand
// while leaving the function definition intact.
func TestNewFormulaCmd_RegistersVersionCheckSubcommand(t *testing.T) {
	cmd := newFormulaCmd(&bytes.Buffer{}, &bytes.Buffer{})
	var found bool
	for _, sub := range cmd.Commands() {
		if sub.Name() == "version-check" {
			found = true
			break
		}
	}
	if !found {
		var names []string
		for _, sub := range cmd.Commands() {
			names = append(names, sub.Name())
		}
		t.Fatalf("newFormulaCmd subcommands = %v, want one named %q", names, "version-check")
	}
}
