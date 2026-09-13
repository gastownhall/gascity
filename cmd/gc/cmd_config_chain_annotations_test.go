package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestRenderProviderChainAnnotations_Empty(t *testing.T) {
	if got := renderProviderChainAnnotations(nil); got != "" {
		t.Errorf("nil city: expected empty, got %q", got)
	}
	cfg := &config.City{}
	if got := renderProviderChainAnnotations(cfg); got != "" {
		t.Errorf("empty city: expected empty, got %q", got)
	}
}

func TestRenderProviderChainAnnotations_BuiltinChain(t *testing.T) {
	cfg := &config.City{
		ResolvedProviders: map[string]config.ResolvedProvider{
			"codex-max": {
				Name:            "codex-max",
				BuiltinAncestor: "codex",
				Chain: []config.HopIdentity{
					{Kind: "custom", Name: "codex-max"},
					{Kind: "builtin", Name: "codex"},
				},
			},
		},
	}
	got := renderProviderChainAnnotations(cfg)
	if !strings.Contains(got, "codex-max") {
		t.Errorf("missing provider name: %q", got)
	}
	if !strings.Contains(got, "builtin:codex") {
		t.Errorf("missing builtin prefix: %q", got)
	}
	if !strings.Contains(got, "→") {
		t.Errorf("missing chain arrow: %q", got)
	}
}

func TestRenderProviderChainAnnotations_NoInheritance(t *testing.T) {
	cfg := &config.City{
		ResolvedProviders: map[string]config.ResolvedProvider{
			"standalone": {
				Name:            "standalone",
				BuiltinAncestor: "",
				Chain: []config.HopIdentity{
					{Kind: "custom", Name: "standalone"},
				},
			},
		},
	}
	got := renderProviderChainAnnotations(cfg)
	if !strings.Contains(got, "(no inheritance)") {
		t.Errorf("expected '(no inheritance)', got %q", got)
	}
}

func TestRenderProviderChainAnnotations_CustomRootedNoBuiltin(t *testing.T) {
	cfg := &config.City{
		ResolvedProviders: map[string]config.ResolvedProvider{
			"leaf": {
				Name:            "leaf",
				BuiltinAncestor: "",
				Chain: []config.HopIdentity{
					{Kind: "custom", Name: "leaf"},
					{Kind: "custom", Name: "root"},
				},
			},
		},
	}
	got := renderProviderChainAnnotations(cfg)
	if !strings.Contains(got, "(no built-in ancestor)") {
		t.Errorf("expected '(no built-in ancestor)', got %q", got)
	}
}

func TestRenderProviderChainAnnotations_SortedDeterministic(t *testing.T) {
	cfg := &config.City{
		ResolvedProviders: map[string]config.ResolvedProvider{
			"zebra": {Name: "zebra", Chain: []config.HopIdentity{{Kind: "custom", Name: "zebra"}}},
			"alpha": {Name: "alpha", Chain: []config.HopIdentity{{Kind: "custom", Name: "alpha"}}},
			"mid":   {Name: "mid", Chain: []config.HopIdentity{{Kind: "custom", Name: "mid"}}},
		},
	}
	got := renderProviderChainAnnotations(cfg)
	ai := strings.Index(got, "alpha")
	mi := strings.Index(got, "mid")
	zi := strings.Index(got, "zebra")
	if ai >= mi || mi >= zi {
		t.Errorf("expected alphabetical ordering, got: %q", got)
	}
}
