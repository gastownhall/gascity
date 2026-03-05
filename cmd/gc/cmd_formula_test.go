package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
)

// --- gc formula list ---

func TestFormulaListEmpty(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/city/.gc/formulas"] = true

	var stdout, stderr bytes.Buffer
	code := doFormulaList(fs, "/city/.gc/formulas", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doFormulaList = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "No formulas found") {
		t.Errorf("stdout = %q, want 'No formulas found'", stdout.String())
	}
}

func TestFormulaListMissingDir(t *testing.T) {
	fs := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	code := doFormulaList(fs, "/city/.gc/formulas", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doFormulaList = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "No formulas found") {
		t.Errorf("stdout = %q, want 'No formulas found'", stdout.String())
	}
}

func TestFormulaListSuccess(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/city/.gc/formulas"] = true
	fs.Files["/city/.gc/formulas/pancakes.formula.toml"] = []byte(`formula = "pancakes"`)
	fs.Files["/city/.gc/formulas/deploy.formula.toml"] = []byte(`formula = "deploy"`)
	// Non-formula file should be ignored.
	fs.Files["/city/.gc/formulas/notes.txt"] = []byte("not a formula")

	var stdout, stderr bytes.Buffer
	code := doFormulaList(fs, "/city/.gc/formulas", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doFormulaList = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "deploy") {
		t.Errorf("stdout missing 'deploy': %q", out)
	}
	if !strings.Contains(out, "pancakes") {
		t.Errorf("stdout missing 'pancakes': %q", out)
	}
	if strings.Contains(out, "notes") {
		t.Errorf("stdout should not contain 'notes': %q", out)
	}
}

// --- gc formula show ---

func TestFormulaShowSuccess(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/formulas"] = true
	fs.Files["/formulas/pancakes.formula.toml"] = []byte(`
formula = "pancakes"
description = "Make pancakes"

[[steps]]
id = "dry"
title = "Mix dry"
description = "Flour, sugar, salt"

[[steps]]
id = "wet"
title = "Mix wet"
description = "Eggs, milk, butter"

[[steps]]
id = "combine"
title = "Combine"
description = "Fold together"
needs = ["dry", "wet"]
`)

	var stdout, stderr bytes.Buffer
	code := doFormulaShow(fs, "/formulas", "pancakes", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doFormulaShow = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	for _, want := range []string{"pancakes", "Make pancakes", "Steps:       3", "dry", "wet", "combine", "needs: dry, wet"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// --- embedded formula validation ---

func TestEmbeddedMolDoWorkParses(t *testing.T) {
	data, err := defaultFormulas.ReadFile("formulas/mol-do-work.formula.toml")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	f, err := formula.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := formula.Validate(f); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if f.Name != "mol-do-work" {
		t.Errorf("Name = %q, want %q", f.Name, "mol-do-work")
	}
	if len(f.Steps) != 1 {
		t.Fatalf("len(Steps) = %d, want 1", len(f.Steps))
	}
	if f.Steps[0].ID != "do-work" {
		t.Errorf("Steps[0].ID = %q, want %q", f.Steps[0].ID, "do-work")
	}
}

func TestFormulaShowMissing(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/formulas"] = true

	var stdout, stderr bytes.Buffer
	code := doFormulaShow(fs, "/formulas", "nonexistent", &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doFormulaShow = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want 'not found'", stderr.String())
	}
}
