package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// TestResolveMaterializingNamed_RetiredPoolSlotsDoNotHoldConfiguredName pins
// a real-world incident, seen in production, through the resolver `gc session
// submit` uses. A pool slot took the bare runtime name of a configured named
// session, the reconciler retired it, and the closed bead — no
// configured_named_* marker — refused every later submit with "session name
// already exists". Two such beads held the name; clearing one by hand only
// surfaced the other. Upstream #4742 (c94e921f6) releases a closed
// pool_managed+ephemeral bead's name; this proves the release reaches the
// materializing resolve for both beads at once.
func TestResolveMaterializingNamed_RetiredPoolSlotsDoNotHoldConfiguredName(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := shortSocketTempDir(t, "gc-retired-slot-")
	t.Setenv("GC_CITY", cityDir)
	writeNamedSessionCityTOML(t, cityDir)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig(%q): %v", cityDir, err)
	}
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt(%q): %v", cityDir, err)
	}

	runtimeName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "mayor")
	retired := map[string]bool{}
	for i := 0; i < 2; i++ {
		b, err := store.Create(beads.Bead{
			Title:  "mayor",
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"session_name":          runtimeName,
				"session_name_explicit": "true",
				"alias":                 "mayor",
				"agent_name":            "mayor",
				"template":              "mayor",
				"pool_managed":          "true",
				"session_origin":        "ephemeral",
			},
		})
		if err != nil {
			t.Fatalf("Create(retired pool slot %d): %v", i, err)
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("Close(retired pool slot %d): %v", i, err)
		}
		retired[b.ID] = true
	}

	id, err := resolveSessionIDMaterializingNamed(cityDir, cfg, store, "mayor")
	if err != nil {
		t.Fatalf("resolveSessionIDMaterializingNamed(mayor) with two retired pool slots holding %q: %v", runtimeName, err)
	}
	if id == "" {
		t.Fatal("resolveSessionIDMaterializingNamed(mayor) returned an empty id")
	}
}
