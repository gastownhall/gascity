package worker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestFactorySessionAndCatalogShareWorkerBoundary(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	searchPaths := []string{"/tmp/worker-a", "/tmp/worker-b"}

	factory, err := NewFactory(FactoryConfig{
		Store:       store,
		Provider:    sp,
		SearchPaths: searchPaths,
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	handle, err := factory.Session(SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})
	if err != nil {
		t.Fatalf("factory.Session: %v", err)
	}
	if !reflect.DeepEqual(handle.adapter.SearchPaths, searchPaths) {
		t.Fatalf("handle adapter search paths = %#v, want %#v", handle.adapter.SearchPaths, searchPaths)
	}

	info, err := handle.Create(context.Background(), CreateModeDeferred)
	if err != nil {
		t.Fatalf("Create(deferred): %v", err)
	}

	catalog, err := factory.Catalog()
	if err != nil {
		t.Fatalf("factory.Catalog: %v", err)
	}
	got, err := catalog.Get(info.ID)
	if err != nil {
		t.Fatalf("catalog.Get(%q): %v", info.ID, err)
	}
	if got.ID != info.ID {
		t.Fatalf("catalog.Get(%q).ID = %q, want %q", info.ID, got.ID, info.ID)
	}
	if got.Template != "probe" {
		t.Fatalf("catalog.Get(%q).Template = %q, want probe", info.ID, got.Template)
	}
}

func TestFactoryAdapterUsesConfiguredSearchPaths(t *testing.T) {
	factory, err := NewFactory(FactoryConfig{
		Store:       beads.NewMemStore(),
		SearchPaths: []string{"/tmp/factory-search"},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	adapter := factory.Adapter()
	if !reflect.DeepEqual(adapter.SearchPaths, []string{"/tmp/factory-search"}) {
		t.Fatalf("Adapter().SearchPaths = %#v, want %#v", adapter.SearchPaths, []string{"/tmp/factory-search"})
	}
}

func TestFactoryTranscriptMethodsUseConfiguredSearchPaths(t *testing.T) {
	searchBase := t.TempDir()
	workDir := t.TempDir()
	slug := strings.ReplaceAll(workDir, "/", "-")
	slug = strings.ReplaceAll(slug, ".", "-")
	transcriptDir := filepath.Join(searchBase, slug)
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", transcriptDir, err)
	}
	transcriptPath := filepath.Join(transcriptDir, "session.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(
		"{\"uuid\":\"1\",\"type\":\"assistant\",\"message\":{\"role\":\"assistant\",\"model\":\"claude-opus-4-5-20251101\",\"usage\":{\"input_tokens\":1000}},\"timestamp\":\"2025-01-01T00:00:00Z\"}\n",
	), 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", transcriptPath, err)
	}

	factory, err := NewFactory(FactoryConfig{
		Store:       beads.NewMemStore(),
		SearchPaths: []string{searchBase},
	})
	if err != nil {
		t.Fatalf("NewFactory: %v", err)
	}

	gotPath := factory.DiscoverTranscript("claude", workDir, "")
	if gotPath != transcriptPath {
		t.Fatalf("DiscoverTranscript() = %q, want %q", gotPath, transcriptPath)
	}
	meta, err := factory.TailMeta(transcriptPath)
	if err != nil {
		t.Fatalf("TailMeta(%q): %v", transcriptPath, err)
	}
	if meta == nil || meta.Model != "claude-opus-4-5-20251101" {
		t.Fatalf("TailMeta(%q) = %#v, want model claude-opus-4-5-20251101", transcriptPath, meta)
	}
}
