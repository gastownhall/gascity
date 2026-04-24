package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestCityStatusNamedSessionsUseProvidedStore(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	store := beads.NewMemStore()

	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })

	if _, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"configured_named_session":  "true",
			"configured_named_identity": "refinery",
			"configured_named_mode":     "on_demand",
		},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents:    []config.Agent{{Name: "refinery"}},
		NamedSessions: []config.NamedSession{{
			Template: "refinery",
		}},
	}
	var stdout, stderr bytes.Buffer
	cityPath := filepath.Join(t.TempDir(), "city")
	snapshot := collectCityStatusSnapshot(sp, cfg, cityPath, store, &stderr)
	if len(snapshot.NamedSessions) != 1 {
		t.Fatalf("named sessions = %d, want 1", len(snapshot.NamedSessions))
	}
	if snapshot.NamedSessions[0].Status != "materialized" {
		t.Fatalf("named session status = %q, want materialized", snapshot.NamedSessions[0].Status)
	}
	code := doCityStatus(sp, dops, cfg, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Named sessions:") {
		t.Fatalf("stdout missing named sessions section, got:\n%s", out)
	}
	if !strings.Contains(out, "materialized (on_demand)") {
		t.Fatalf("stdout = %q, want materialized named session status", out)
	}
}

func TestCityStatusSnapshotNilConfigUsesCityPathName(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "city")
	snapshot := collectCityStatusSnapshot(runtime.NewFake(), nil, cityPath, nil, io.Discard)
	if snapshot.CityName != "city" {
		t.Fatalf("CityName = %q, want city", snapshot.CityName)
	}
}

func TestCityStatusJSONPreservesNilAgentsWhenEmpty(t *testing.T) {
	status := cityStatusJSONFromSnapshot(cityStatusSnapshot{CityName: "city"}, StatusSummaryJSON{})
	if status.Agents != nil {
		t.Fatalf("Agents = %#v, want nil slice", status.Agents)
	}
}

type failingStatusStore struct {
	*beads.MemStore
	failID string
	err    error
}

func (s *failingStatusStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, s.err
	}
	return s.MemStore.Get(id)
}

func TestCityStatusNamedSessionLookupErrorsAreSurfaced(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	store := &failingStatusStore{
		MemStore: beads.NewMemStore(),
		failID:   "refinery",
		err:      errors.New("store offline"),
	}

	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		NamedSessions: []config.NamedSession{{
			Template: "refinery",
		}},
	}

	var stdout, stderr bytes.Buffer
	snapshot := collectCityStatusSnapshot(sp, cfg, "/home/user/city", store, &stderr)
	if len(snapshot.NamedSessions) != 1 {
		t.Fatalf("named sessions = %d, want 1", len(snapshot.NamedSessions))
	}
	if got := snapshot.NamedSessions[0].Status; !strings.HasPrefix(got, "lookup error:") {
		t.Fatalf("snapshot named session status = %q, want lookup error", got)
	}

	code := doCityStatus(sp, dops, cfg, "/home/user/city", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "lookup error:") || !strings.Contains(out, "store offline") {
		t.Fatalf("stdout = %q, want surfaced store error", out)
	}
}
