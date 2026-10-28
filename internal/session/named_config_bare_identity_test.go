package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// requalifiedLaneCity is a city whose named session golden-hon arrives
// through the import binding "lane" in rig dir "contentbuild", as
// `gc rig add --adopt` requalifies a top-level golden-hon (#5957). With
// ambiguous set, a second binding "ops" maps the same bare leaf.
func requalifiedLaneCity(ambiguous bool) *config.City {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "golden-hon", Dir: "contentbuild", BindingName: "lane"}},
		NamedSessions: []config.NamedSession{
			{Template: "golden-hon", Dir: "contentbuild", BindingName: "lane", Mode: "always"},
		},
	}
	if ambiguous {
		cfg.Agents = append(cfg.Agents, config.Agent{Name: "golden-hon", Dir: "contentbuild", BindingName: "ops"})
		cfg.NamedSessions = append(cfg.NamedSessions, config.NamedSession{Template: "golden-hon", Dir: "contentbuild", BindingName: "ops", Mode: "always"})
	}
	return cfg
}

// preAdoptBareRow is the live row a top-level golden-hon session left before
// its requalification: identity, session_name and alias all bare.
func preAdoptBareRow() beads.Bead {
	return beads.Bead{
		Title:  "golden-hon",
		Type:   BeadType,
		Labels: []string{LabelSession},
		Metadata: map[string]string{
			NamedSessionMetadataKey:      "true",
			NamedSessionIdentityMetadata: "golden-hon",
			NamedSessionModeMetadata:     "always",
			"session_name":               "golden-hon",
			"alias":                      "golden-hon",
			"template":                   "golden-hon",
			"state":                      "active",
		},
	}
}

func requalifiedLaneSpec(t *testing.T, cfg *config.City) NamedSessionSpec {
	t.Helper()
	spec, ok := FindNamedSessionSpec(cfg, "test-city", "contentbuild/lane.golden-hon")
	if !ok {
		t.Fatal("FindNamedSessionSpec(contentbuild/lane.golden-hon) found no spec")
	}
	return spec
}

// AC-6 (#5957): the qualified spec's canonical resolvers adopt the unique
// pre-qualification bare row, and never a bare row that is not unique.
func TestNamedSessionResolversAdoptUniqueBareIdentityRow(t *testing.T) {
	resolve := func(t *testing.T, store beads.Store, spec NamedSessionSpec) (lookup, canonical, candidate beads.Bead) {
		t.Helper()
		l, err := LookupConfiguredNamedSession(store, spec)
		if err != nil {
			t.Fatalf("LookupConfiguredNamedSession: %v", err)
		}
		if l.HasConflict {
			t.Fatalf("LookupConfiguredNamedSession reported conflict %s", l.Conflict.ID)
		}
		lookup = l.Canonical
		c, ok, err := FindCanonicalConfiguredNamedSessionBead(store, spec)
		if err != nil {
			t.Fatalf("FindCanonicalConfiguredNamedSessionBead: %v", err)
		}
		if ok {
			canonical = c
		}
		candidates, err := NamedSessionResolutionCandidates(store, spec)
		if err != nil {
			t.Fatalf("NamedSessionResolutionCandidates: %v", err)
		}
		if b, ok := FindCanonicalNamedSessionBead(candidates, spec); ok {
			candidate = b
		}
		return lookup, canonical, candidate
	}

	t.Run("unique bare row", func(t *testing.T) {
		cfg := requalifiedLaneCity(false)
		spec := requalifiedLaneSpec(t, cfg)
		if spec.BareIdentity != "golden-hon" {
			t.Fatalf("spec.BareIdentity = %q, want golden-hon", spec.BareIdentity)
		}
		store := beads.NewMemStore()
		row, err := store.Create(preAdoptBareRow())
		if err != nil {
			t.Fatal(err)
		}
		lookup, canonical, candidate := resolve(t, store, spec)
		for name, got := range map[string]beads.Bead{"LookupConfiguredNamedSession": lookup, "FindCanonicalConfiguredNamedSessionBead": canonical, "NamedSessionResolutionCandidates": candidate} {
			if got.ID != row.ID {
				t.Errorf("%s resolved %q, want the bare row %s", name, got.ID, row.ID)
			}
		}
	})

	t.Run("exact identity beats bare", func(t *testing.T) {
		cfg := requalifiedLaneCity(false)
		spec := requalifiedLaneSpec(t, cfg)
		store := beads.NewMemStore()
		if _, err := store.Create(preAdoptBareRow()); err != nil {
			t.Fatal(err)
		}
		exact := preAdoptBareRow()
		exact.Metadata[NamedSessionIdentityMetadata] = spec.Identity
		exact.Metadata["session_name"] = spec.SessionName
		exact.Metadata["alias"] = spec.Identity
		exact.Metadata["template"] = NamedSessionBackingTemplate(spec)
		want, err := store.Create(exact)
		if err != nil {
			t.Fatal(err)
		}
		lookup, canonical, candidate := resolve(t, store, spec)
		for name, got := range map[string]beads.Bead{"LookupConfiguredNamedSession": lookup, "FindCanonicalConfiguredNamedSessionBead": canonical, "NamedSessionResolutionCandidates": candidate} {
			if got.ID != want.ID {
				t.Errorf("%s resolved %q, want the qualified row %s", name, got.ID, want.ID)
			}
		}
	})

	t.Run("two bare rows", func(t *testing.T) {
		cfg := requalifiedLaneCity(false)
		spec := requalifiedLaneSpec(t, cfg)
		store := beads.NewMemStore()
		for range 2 {
			if _, err := store.Create(preAdoptBareRow()); err != nil {
				t.Fatal(err)
			}
		}
		lookup, canonical, candidate := resolve(t, store, spec)
		for name, got := range map[string]beads.Bead{"LookupConfiguredNamedSession": lookup, "FindCanonicalConfiguredNamedSessionBead": canonical, "NamedSessionResolutionCandidates": candidate} {
			if got.ID != "" {
				t.Errorf("%s resolved %q, want no row of two bare claimants", name, got.ID)
			}
		}
	})

	t.Run("ambiguous leaf", func(t *testing.T) {
		cfg := requalifiedLaneCity(true)
		spec := requalifiedLaneSpec(t, cfg)
		if spec.BareIdentity != "" {
			t.Fatalf("spec.BareIdentity = %q, want empty for a leaf two bindings share", spec.BareIdentity)
		}
		store := beads.NewMemStore()
		if _, err := store.Create(preAdoptBareRow()); err != nil {
			t.Fatal(err)
		}
		lookup, canonical, candidate := resolve(t, store, spec)
		for name, got := range map[string]beads.Bead{"LookupConfiguredNamedSession": lookup, "FindCanonicalConfiguredNamedSessionBead": canonical, "NamedSessionResolutionCandidates": candidate} {
			if got.ID != "" {
				t.Errorf("%s resolved %q, want no row for an ambiguous leaf", name, got.ID)
			}
		}
	})

	t.Run("top-level entry shadows the leaf", func(t *testing.T) {
		cfg := requalifiedLaneCity(false)
		cfg.Agents = append(cfg.Agents, config.Agent{Name: "golden-hon"})
		cfg.NamedSessions = append(cfg.NamedSessions, config.NamedSession{Template: "golden-hon"})
		spec := requalifiedLaneSpec(t, cfg)
		if spec.BareIdentity != "" {
			t.Fatalf("spec.BareIdentity = %q, want empty when a top-level entry owns the leaf", spec.BareIdentity)
		}
	})
}
