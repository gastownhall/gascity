package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestMcpListCityCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "mcp/beads-health.toml", "city mcp")

	var stdout, stderr bytes.Buffer
	code := run([]string{"mcp", "list"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc mcp list exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"NAME", "beads-health", "city", "mcp/beads-health.toml"} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcp list output missing %q:\n%s", want, out)
		}
	}
}

func TestMcpListAgentCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "mcp/beads-health.toml", "city mcp")
	writeCatalogFile(t, cityDir, "agents/mayor/mcp/private-tool.template.toml", "agent mcp")

	var stdout, stderr bytes.Buffer
	code := run([]string{"mcp", "list", "--agent", "mayor"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc mcp list --agent exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"beads-health", "city", "private-tool", "agent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcp list --agent output missing %q:\n%s", want, out)
		}
	}
}

func TestMcpListSessionCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	writeNamedSessionCityTOML(t, cityDir)
	writeCatalogFile(t, cityDir, "mcp/beads-health.toml", "city mcp")
	writeCatalogFile(t, cityDir, "agents/mayor/mcp/private-tool.template.toml", "agent mcp")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title:  "mayor session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"template":     "mayor",
			"session_name": "s-mayor-1",
		},
	})
	if err != nil {
		t.Fatalf("store.Create(session bead): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"mcp", "list", "--session", bead.ID}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc mcp list --session exited %d: %s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{"beads-health", "city", "private-tool", "agent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("mcp list --session output missing %q:\n%s", want, out)
		}
	}
}
