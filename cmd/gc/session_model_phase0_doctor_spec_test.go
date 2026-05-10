package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - Diagnostics
// - Doctor contract

func TestPhase0DoctorReportsClosedBeadOwner(t *testing.T) {
	cityPath, store := newPhase0DoctorCity(t)

	closed, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-closed",
			"template":     "worker",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close(%s): %v", closed.ID, err)
	}
	if _, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Title:    "stale owner",
		Assignee: closed.ID,
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	t.Setenv("GC_CITY", cityPath)
	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "closed-bead-owner") {
		t.Fatalf("doctor output missing closed-bead-owner finding:\n%s", out)
	}
}

func TestPhase0DoctorReportsStaleRoutedConfig(t *testing.T) {
	cityPath, store := newPhase0DoctorCity(t)

	if _, err := store.Create(beads.Bead{
		Type:   "task",
		Status: "open",
		Title:  "stale route",
		Metadata: map[string]string{
			"gc.routed_to": "missing-config",
		},
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	t.Setenv("GC_CITY", cityPath)
	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "stale-routed-config") {
		t.Fatalf("doctor output missing stale-routed-config finding:\n%s", out)
	}
}

func TestPhase0DoctorReportsMissingBeadOwner(t *testing.T) {
	cityPath, store := newPhase0DoctorCity(t)

	if _, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Title:    "missing owner",
		Assignee: "gc-missing-session",
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	t.Setenv("GC_CITY", cityPath)
	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "missing-bead-owner") {
		t.Fatalf("doctor output missing missing-bead-owner finding:\n%s", out)
	}
}

func TestPhase0DoctorReportsRetiredBeadOwner(t *testing.T) {
	cityPath, store := newPhase0DoctorCity(t)

	retired, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":             "",
			"template":                 "worker",
			"state":                    "archived",
			"continuity_eligible":      "false",
			"configured_named_session": "true",
		},
	})
	if err != nil {
		t.Fatalf("create retired session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Title:    "retired owner",
		Assignee: retired.ID,
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	t.Setenv("GC_CITY", cityPath)
	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "retired-bead-owner") {
		t.Fatalf("doctor output missing retired-bead-owner finding:\n%s", out)
	}
}

func TestPhase0DoctorDoesNotReportContinuityEligibleArchivedOwnerAsRetired(t *testing.T) {
	cityPath, store := newPhase0DoctorCity(t)

	owner, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":             "s-worker",
			"template":                 "worker",
			"state":                    "archived",
			"continuity_eligible":      "true",
			"configured_named_session": "true",
		},
	})
	if err != nil {
		t.Fatalf("create archived session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Title:    "continuity owner",
		Assignee: owner.ID,
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	t.Setenv("GC_CITY", cityPath)
	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if strings.Contains(out, "retired-bead-owner") {
		t.Fatalf("doctor output reported continuity-eligible archived owner as retired:\n%s", out)
	}
}

func TestPhase0DoctorReportsAmbiguousLegacySessionToken(t *testing.T) {
	cityPath, store := newPhase0DoctorCity(t)

	for _, name := range []string{"s-gc-one", "s-gc-two"} {
		if _, err := store.Create(beads.Bead{
			Type:   session.BeadType,
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"alias":        "mayor",
				"session_name": name,
				"template":     "worker",
			},
		}); err != nil {
			t.Fatalf("create session bead %s: %v", name, err)
		}
	}
	if _, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Title:    "ambiguous legacy owner",
		Assignee: "mayor",
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	t.Setenv("GC_CITY", cityPath)
	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "ambiguous-legacy-session-token") {
		t.Fatalf("doctor output missing ambiguous-legacy-session-token finding:\n%s", out)
	}
}

func TestPhase0DoctorReportsLegacyTokenMatchesConfigOnly(t *testing.T) {
	cityPath, store := newPhase0DoctorCityWithConfig(t, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
`)

	if _, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Title:    "legacy template owner",
		Assignee: "worker",
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	t.Setenv("GC_CITY", cityPath)
	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "legacy-token-matches-config-only") {
		t.Fatalf("doctor output missing legacy-token-matches-config-only finding:\n%s", out)
	}
}

func TestPhase0DoctorReportsHistoricalAliasOwner(t *testing.T) {
	cityPath, store := newPhase0DoctorCity(t)

	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":         "mayor",
			"alias_history": "sky",
			"session_name":  "s-gc-mayor",
			"template":      "worker",
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Title:    "historical owner",
		Assignee: "sky",
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	t.Setenv("GC_CITY", cityPath)
	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "historical-alias-owner") {
		t.Fatalf("doctor output missing historical-alias-owner finding:\n%s", out)
	}
}

func TestPhase0DoctorReportsConfiguredNamedConflict(t *testing.T) {
	cityPath, store := newPhase0DoctorCityWithConfig(t, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
start_command = "true"

[[named_session]]
template = "mayor"
mode = "on_demand"
`)

	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"alias":        "mayor",
			"session_name": "s-gc-squatter",
			"template":     "other",
		},
	}); err != nil {
		t.Fatalf("create squatter session: %v", err)
	}

	t.Setenv("GC_CITY", cityPath)
	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, &stdout, &stderr)

	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "configured-named-conflict") {
		t.Fatalf("doctor output missing configured-named-conflict finding:\n%s", out)
	}
}

type sessionModelDoctorQuerySpyStore struct {
	*beads.MemStore
	queries []beads.ListQuery
}

func (s *sessionModelDoctorQuerySpyStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	if query.AllowScan && query.IncludeClosed && query.Label == "" && query.Type == "" {
		return nil, errors.New("unfiltered closed scan")
	}
	return s.MemStore.List(query)
}

func TestPhase0DoctorSessionModelAvoidsUnfilteredClosedScan(t *testing.T) {
	store := &sessionModelDoctorQuerySpyStore{MemStore: beads.NewMemStore()}
	closed, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "s-gc-closed",
			"template":     "worker",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close(%s): %v", closed.ID, err)
	}
	if _, err := store.Create(beads.Bead{
		Type:     "task",
		Status:   "open",
		Title:    "stale owner",
		Assignee: closed.ID,
	}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	check := &sessionModelDoctorCheck{
		cfg:      &config.City{},
		cityPath: "/city",
		newStore: func(string) (beads.Store, error) {
			return store, nil
		},
	}
	result := check.Run(&doctor.CheckContext{Verbose: true})
	if result.Status != doctor.StatusWarning || !strings.Contains(strings.Join(result.Details, "\n"), "closed-bead-owner") {
		t.Fatalf("session-model result = %#v, want closed-bead-owner warning", result)
	}
	for _, query := range store.queries {
		if query.AllowScan && query.IncludeClosed && query.Label == "" && query.Type == "" {
			t.Fatalf("session-model used unfiltered closed scan: %#v", query)
		}
	}
}

func newPhase0DoctorCity(t *testing.T) (string, *beads.FileStore) {
	t.Helper()

	return newPhase0DoctorCityWithConfig(t, `[workspace]
name = "test-city"

[beads]
provider = "file"
`)
}

func newPhase0DoctorCityWithConfig(t *testing.T, configText string) (string, *beads.FileStore) {
	t.Helper()

	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(configText), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityPath, ".gc", "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	return cityPath, store
}
