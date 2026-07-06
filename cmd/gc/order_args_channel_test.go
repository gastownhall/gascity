package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// TestParseOrderRunVarFlags locks the `gc order run --var key=value` parsing:
// well-formed pairs land in the map (value may itself contain '='), and
// malformed or empty-key entries are rejected.
func TestParseOrderRunVarFlags(t *testing.T) {
	var stderr bytes.Buffer

	vars, ok := parseOrderRunVarFlags([]string{"repo=octo/demo", "token=a=b=c", "empty="}, &stderr)
	if !ok {
		t.Fatalf("parseOrderRunVarFlags rejected valid flags: %s", stderr.String())
	}
	if vars["repo"] != "octo/demo" {
		t.Fatalf("vars[repo] = %q, want octo/demo", vars["repo"])
	}
	if vars["token"] != "a=b=c" {
		t.Fatalf("vars[token] = %q, want a=b=c (split on first '=')", vars["token"])
	}
	if v, exists := vars["empty"]; !exists || v != "" {
		t.Fatalf("vars[empty] = (%q, %v), want present empty string", v, exists)
	}

	if got, ok := parseOrderRunVarFlags(nil, &stderr); !ok || got != nil {
		t.Fatalf("parseOrderRunVarFlags(nil) = (%v, %v), want (nil, true)", got, ok)
	}

	if _, ok := parseOrderRunVarFlags([]string{"noequals"}, &stderr); ok {
		t.Fatal("parseOrderRunVarFlags accepted a flag without '='")
	}
	if _, ok := parseOrderRunVarFlags([]string{"=value"}, &stderr); ok {
		t.Fatal("parseOrderRunVarFlags accepted an empty key")
	}
}

// TestPrepareOrderWispRecipeThreadsVarsToFormula proves the args channel reaches
// a formula order: dispatch vars flow through prepareOrderWispRecipe into
// PrepareInvocation/ExpandVars and drive compile-time range expansion. The
// range var `n` is required, so the recipe only compiles when the var is
// supplied, and the resulting step count reflects the supplied value.
func TestPrepareOrderWispRecipeThreadsVarsToFormula(t *testing.T) {
	dir := t.TempDir()
	formulaBody := `
formula = "e1-var-range"
version = 1

[vars.n]
description = "Loop count"
required = true

[[steps]]
id = "loop"
title = "Loop"

[steps.loop]
range = "1..{n}"

[[steps.loop.body]]
id = "work"
title = "Work {i}"
`
	if err := os.WriteFile(filepath.Join(dir, "e1-var-range.toml"), []byte(strings.TrimSpace(formulaBody)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	a := orders.Order{Name: "range-order", Trigger: "manual", Formula: "e1-var-range", FormulaLayer: dir}

	// Without the var the required compile-time range var is unresolved and
	// compilation fails — proving the var is what makes the recipe compile.
	if _, err := prepareOrderWispRecipe(context.Background(), store, a, []string{dir}, nil); err == nil {
		t.Fatal("prepareOrderWispRecipe with nil vars: expected failure for missing required range var n")
	}

	recipe, err := prepareOrderWispRecipe(context.Background(), store, a, []string{dir}, map[string]string{"n": "3"})
	if err != nil {
		t.Fatalf("prepareOrderWispRecipe with vars: %v", err)
	}
	if len(recipe.Steps) != 4 {
		t.Fatalf("len(recipe.Steps) = %d, want 4 (root + 3 loop iterations from n=3)", len(recipe.Steps))
	}
}

// TestOrderExecEnvWithErrorOverlaysVars proves the args channel reaches an exec
// order's process environment while leaving the static [order.env] reserved-key
// guard intact.
func TestOrderExecEnvWithErrorOverlaysVars(t *testing.T) {
	cityDir := t.TempDir()
	target := execStoreTarget{ScopeRoot: cityDir, ScopeKind: "city", Prefix: "ct"}
	a := orders.Order{Name: "hooked", Trigger: "manual", Exec: "true"}

	envSlice, err := orderExecEnvWithError(cityDir, nil, target, a, map[string]string{
		"repo": "octo/demo",
		"pr":   "42",
	})
	if err != nil {
		t.Fatalf("orderExecEnvWithError: %v", err)
	}

	got := map[string]string{}
	for _, entry := range envSlice {
		if key, value, ok := strings.Cut(entry, "="); ok {
			got[key] = value
		}
	}
	if got["repo"] != "octo/demo" {
		t.Fatalf("env[repo] = %q, want octo/demo; env=%v", got["repo"], envSlice)
	}
	if got["pr"] != "42" {
		t.Fatalf("env[pr] = %q, want 42; env=%v", got["pr"], envSlice)
	}

	// The static [order.env] reserved-key guard must still reject an order that
	// tries to override controller-owned env via [order.env].
	reserved := orders.Order{Name: "hooked", Trigger: "manual", Exec: "true", Env: map[string]string{"GC_CITY": "x"}}
	if _, err := orderExecEnvWithError(cityDir, nil, target, reserved, nil); err == nil {
		t.Fatal("orderExecEnvWithError: expected reserved [order.env] key GC_CITY to be rejected")
	}
}

// TestDispatchOneRefusesMissingRequiredParam proves a declared-required param
// absent from the dispatch vars is a hard error: dispatch records OrderFailed
// and never fires the order (no OrderFired, exec never runs).
func TestDispatchOneRefusesMissingRequiredParam(t *testing.T) {
	store := beads.NewMemStore()
	tracking, err := store.Create(beads.Bead{
		Title:  "order:needs-repo",
		Labels: []string{"order-run:needs-repo", labelOrderTracking},
	})
	if err != nil {
		t.Fatal(err)
	}

	var rec memRecorder
	ranExec := false
	execRun := func(context.Context, string, string, []string) ([]byte, error) {
		ranExec = true
		return nil, nil
	}
	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:     "needs-repo",
		Trigger:  "cooldown",
		Interval: "1h",
		Exec:     "echo hi",
		Params:   map[string]orders.OrderParam{"repo": {Required: true}},
	}}, store, nil, execRun, &rec)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	mad := ad.(*memoryOrderDispatcher)

	// nil vars → the required "repo" param is missing → dispatch must refuse.
	mad.addInflight()
	mad.dispatchOne(context.Background(), store, execStoreTarget{ScopeRoot: t.TempDir()}, mad.aa[0], t.TempDir(), tracking.ID, nil, nil)

	if !rec.hasType(events.OrderFailed) {
		t.Fatal("missing order.failed event for missing required param")
	}
	if rec.hasType(events.OrderFired) {
		t.Fatal("order.fired recorded; dispatch must refuse before firing")
	}
	if ranExec {
		t.Fatal("exec ran despite missing required param")
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	var failMsg string
	for _, e := range rec.events {
		if e.Type == events.OrderFailed {
			failMsg = e.Message
		}
	}
	if !strings.Contains(failMsg, "repo") || !strings.Contains(failMsg, "required") {
		t.Fatalf("order.failed message = %q, want it to name the missing required param repo", failMsg)
	}
}
