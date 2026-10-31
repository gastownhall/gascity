//go:build windows

package config

import (
	"fmt"
	"testing"
)

func TestGeneratedProgramRegistryIsBounded(t *testing.T) {
	registry := newGeneratedProgramRegistry(2)
	registry.Store("first")
	registry.Store("second")
	registry.Store("second")
	registry.Store("third")

	if registry.Load("first") {
		t.Fatal("first entry survived past the registry limit")
	}
	if !registry.Load("second") || !registry.Load("third") {
		t.Fatalf("registry entries after eviction: second=%v third=%v, want both present", registry.Load("second"), registry.Load("third"))
	}
}

func TestGeneratedProgramRegistryRefreshesRecency(t *testing.T) {
	registry := newGeneratedProgramRegistry(generatedProgramRegistryLimit)
	commands := make([]string, generatedProgramRegistryLimit)
	for i := range commands {
		commands[i] = fmt.Sprintf("command-%04d", i)
		registry.Store(commands[i])
	}

	registry.Store(commands[0])
	registry.Store("newest")

	if !registry.Load(commands[0]) {
		t.Fatal("re-noted oldest entry was evicted")
	}
	if registry.Load(commands[1]) {
		t.Fatal("next-oldest entry survived; want it evicted after oldest recency refresh")
	}
	if !registry.Load("newest") {
		t.Fatal("newest entry was not stored")
	}
}
