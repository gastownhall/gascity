package config

import (
	"errors"
	"strings"
	"testing"
)

func basePtr(s string) *string { return &s }

// customs returns a fresh custom-providers map for tests.
func customs(providers map[string]ProviderSpec) map[string]ProviderSpec {
	m := make(map[string]ProviderSpec, len(providers))
	for k, v := range providers {
		m[k] = v
	}
	return m
}

func TestResolveProviderChain_NoBase(t *testing.T) {
	// leaf with no base — returns spec as-is with leaf-only chain.
	leaf := ProviderSpec{Command: "foo"}
	r, err := ResolveProviderChain("foo", leaf, customs(map[string]ProviderSpec{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.BuiltinAncestor != "" {
		t.Errorf("BuiltinAncestor = %q, want empty", r.BuiltinAncestor)
	}
	if len(r.Chain) != 1 || r.Chain[0].Kind != "custom" || r.Chain[0].Name != "foo" {
		t.Errorf("Chain = %+v, want [custom:foo]", r.Chain)
	}
	if r.Command != "foo" {
		t.Errorf("Command = %q, want foo", r.Command)
	}
}

func TestResolveProviderChain_ExplicitEmpty(t *testing.T) {
	// base = "" — explicit opt-out; no chain walk.
	leaf := ProviderSpec{Base: basePtr(""), Command: "foo"}
	r, err := ResolveProviderChain("foo", leaf, customs(map[string]ProviderSpec{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.BuiltinAncestor != "" {
		t.Errorf("BuiltinAncestor should be empty for base=\"\", got %q", r.BuiltinAncestor)
	}
	if len(r.Chain) != 1 {
		t.Errorf("Chain should be leaf-only for base=\"\", got %+v", r.Chain)
	}
}

func TestResolveProviderChain_BuiltinDirect(t *testing.T) {
	// base = "builtin:codex" on a leaf (aimux wrapper — needs resume_command).
	leaf := ProviderSpec{
		Base:          basePtr("builtin:codex"),
		Command:       "aimux",
		Args:          []string{"run", "codex"},
		ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
	}
	r, err := ResolveProviderChain("codex-mini", leaf, customs(map[string]ProviderSpec{}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.BuiltinAncestor != "codex" {
		t.Errorf("BuiltinAncestor = %q, want codex", r.BuiltinAncestor)
	}
	if len(r.Chain) != 2 || r.Chain[1].Kind != "builtin" || r.Chain[1].Name != "codex" {
		t.Errorf("Chain = %+v, want [custom:codex-mini, builtin:codex]", r.Chain)
	}
	// Inherited fields from built-in codex should propagate.
	if r.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want arg (inherited)", r.PromptMode)
	}
	if r.ReadyDelayMs != 3000 {
		t.Errorf("ReadyDelayMs = %d, want 3000 (inherited)", r.ReadyDelayMs)
	}
	// Leaf override preserved.
	if r.Command != "aimux" {
		t.Errorf("Command = %q, want aimux (leaf override)", r.Command)
	}
}

func TestResolveProviderChain_SelfExclusion(t *testing.T) {
	// Custom provider named "codex" shadows built-in codex; bare
	// base="codex" resolves via self-exclusion to built-in codex.
	leaf := ProviderSpec{
		Base:          basePtr("codex"),
		Command:       "aimux",
		ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
	}
	r, err := ResolveProviderChain("codex", leaf, customs(map[string]ProviderSpec{
		"codex": leaf, // the shadowing custom provider
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.BuiltinAncestor != "codex" {
		t.Errorf("BuiltinAncestor = %q, want codex (via self-exclusion)", r.BuiltinAncestor)
	}
}

func TestResolveProviderChain_ThreeLayer(t *testing.T) {
	// codex-max → codex → builtin:codex
	custom := map[string]ProviderSpec{
		"codex":     {Base: basePtr("builtin:codex"), Command: "aimux", Args: []string{"run", "codex", "--"}, ResumeCommand: "aimux run codex -- resume {{.SessionKey}}"},
		"codex-max": {Base: basePtr("codex"), ArgsAppend: []string{"-m", "gpt-5.4"}},
	}
	r, err := ResolveProviderChain("codex-max", custom["codex-max"], customs(custom))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.BuiltinAncestor != "codex" {
		t.Errorf("BuiltinAncestor = %q, want codex", r.BuiltinAncestor)
	}
	if len(r.Chain) != 3 {
		t.Errorf("Chain len = %d, want 3", len(r.Chain))
	}
	// Inherited from built-in codex.
	if r.PromptMode != "arg" {
		t.Errorf("PromptMode = %q, want arg (inherited)", r.PromptMode)
	}
	// Inherited from mid-layer (aimux wrapper).
	if r.Command != "aimux" {
		t.Errorf("Command = %q, want aimux (from codex mid)", r.Command)
	}
}

func TestResolveProviderChain_SelfCycleWithBareName(t *testing.T) {
	// base = "foo" inside [providers.foo] with no built-in foo.
	leaf := ProviderSpec{Base: basePtr("foo"), Command: "bar"}
	_, err := ResolveProviderChain("foo", leaf, customs(map[string]ProviderSpec{
		"foo": leaf,
	}))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var pcErr *ProviderChainError
	if !errors.As(err, &pcErr) || pcErr.Kind != "cycle" {
		t.Errorf("expected cycle error, got %T: %v", err, err)
	}
}

func TestResolveProviderChain_TransitiveCycle(t *testing.T) {
	// A → B → A
	custom := map[string]ProviderSpec{
		"A": {Base: basePtr("provider:B"), Command: "a"},
		"B": {Base: basePtr("provider:A"), Command: "b"},
	}
	_, err := ResolveProviderChain("A", custom["A"], customs(custom))
	if err == nil {
		t.Fatal("expected cycle error, got nil")
	}
	var pcErr *ProviderChainError
	if !errors.As(err, &pcErr) || pcErr.Kind != "cycle" {
		t.Errorf("expected cycle error, got %v", err)
	}
	if !strings.Contains(err.Error(), "→") {
		t.Errorf("error message should name the chain: %v", err)
	}
}

func TestResolveProviderChain_UnknownBase(t *testing.T) {
	leaf := ProviderSpec{Base: basePtr("builtin:nonexistent"), Command: "foo"}
	_, err := ResolveProviderChain("foo", leaf, customs(map[string]ProviderSpec{}))
	if err == nil {
		t.Fatal("expected error")
	}
	var pcErr *ProviderChainError
	if !errors.As(err, &pcErr) || pcErr.Kind != "unknown_base" {
		t.Errorf("expected unknown_base error, got %v", err)
	}
}

func TestResolveProviderChain_ProviderPrefix_SelfCycle(t *testing.T) {
	// base = "provider:foo" inside [providers.foo] — self-cycle via provider prefix.
	leaf := ProviderSpec{Base: basePtr("provider:foo"), Command: "bar"}
	_, err := ResolveProviderChain("foo", leaf, customs(map[string]ProviderSpec{"foo": leaf}))
	var pcErr *ProviderChainError
	if !errors.As(err, &pcErr) || pcErr.Kind != "cycle" {
		t.Errorf("expected cycle, got %v", err)
	}
}

func TestResolveProviderChain_BuiltinAncestorFromHopIdentity(t *testing.T) {
	// Chain where a custom provider is NAMED "codex" but the chain does
	// NOT reach the built-in (base = "provider:X"). BuiltinAncestor
	// must be empty — name-matching would falsely match built-in codex.
	custom := map[string]ProviderSpec{
		"codex":   {Base: basePtr("provider:my-root"), Command: "custom"},
		"my-root": {Command: "root"}, // no base
		"leaf":    {Base: basePtr("provider:codex")},
	}
	r, err := ResolveProviderChain("leaf", custom["leaf"], customs(custom))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.BuiltinAncestor != "" {
		t.Errorf("BuiltinAncestor = %q, want empty (no built-in in chain)", r.BuiltinAncestor)
	}
}

func TestResolveProviderChain_WrapperResumeMissing(t *testing.T) {
	// codex-mini wraps aimux around built-in codex (subcommand resume).
	// No resume_command declared → error.
	leaf := ProviderSpec{
		Base:    basePtr("builtin:codex"),
		Command: "aimux",
		Args:    []string{"run", "codex", "--"},
	}
	_, err := ResolveProviderChain("codex-mini", leaf, customs(map[string]ProviderSpec{}))
	if err == nil {
		t.Fatal("expected wrapper_resume_missing error")
	}
	var pcErr *ProviderChainError
	if !errors.As(err, &pcErr) || pcErr.Kind != "wrapper_resume_missing" {
		t.Errorf("expected wrapper_resume_missing, got %v", err)
	}
}

func TestResolveProviderChain_WrapperResumeProvided(t *testing.T) {
	// Same wrapper, but resume_command declared → no error.
	leaf := ProviderSpec{
		Base:          basePtr("builtin:codex"),
		Command:       "aimux",
		Args:          []string{"run", "codex", "--"},
		ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
	}
	_, err := ResolveProviderChain("codex-mini", leaf, customs(map[string]ProviderSpec{}))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestResolveProviderChain_NonWrapperResume(t *testing.T) {
	// Leaf inherits command from builtin codex — not a wrapper.
	leaf := ProviderSpec{Base: basePtr("builtin:codex")}
	r, err := ResolveProviderChain("codex-plain", leaf, customs(map[string]ProviderSpec{}))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if r.Command != "codex" {
		t.Errorf("Command = %q, want codex (inherited)", r.Command)
	}
}

func TestResolveProviderChain_UnknownPrefix(t *testing.T) {
	leaf := ProviderSpec{Base: basePtr("foo:bar"), Command: "x"}
	_, err := ResolveProviderChain("leaf", leaf, customs(map[string]ProviderSpec{}))
	var pcErr *ProviderChainError
	if !errors.As(err, &pcErr) || pcErr.Kind != "unknown_base" {
		t.Errorf("expected unknown_base for unknown prefix, got %v", err)
	}
}

func TestResolveProviderChain_EmptyBuiltinSuffix(t *testing.T) {
	leaf := ProviderSpec{Base: basePtr("builtin:"), Command: "x"}
	_, err := ResolveProviderChain("leaf", leaf, customs(map[string]ProviderSpec{}))
	if err == nil {
		t.Fatal("expected error for empty builtin suffix")
	}
}

func TestResolveProviderChain_SharedAncestorDAG(t *testing.T) {
	// A → C, B → C. Both resolve independently with their own visited sets.
	custom := map[string]ProviderSpec{
		"A": {Base: basePtr("provider:C")},
		"B": {Base: basePtr("provider:C")},
		"C": {
			Base: basePtr("builtin:codex"), Command: "aimux", Args: []string{"run", "codex"},
			ResumeCommand: "aimux run codex -- resume {{.SessionKey}}",
		},
	}
	rA, err := ResolveProviderChain("A", custom["A"], customs(custom))
	if err != nil {
		t.Fatalf("A: %v", err)
	}
	rB, err := ResolveProviderChain("B", custom["B"], customs(custom))
	if err != nil {
		t.Fatalf("B: %v", err)
	}
	if rA.BuiltinAncestor != "codex" || rB.BuiltinAncestor != "codex" {
		t.Errorf("both should have BuiltinAncestor=codex; got A=%q B=%q", rA.BuiltinAncestor, rB.BuiltinAncestor)
	}
}
