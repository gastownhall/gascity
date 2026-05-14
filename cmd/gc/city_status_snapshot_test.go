package main

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func TestCityStatusCoalescesSessionLabelListCalls(t *testing.T) {
	sp := runtime.NewFake()
	dops := newFakeDrainOps()

	mem := beads.NewMemStore()
	counter := &sessionLabelListCounter{Store: mem}

	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.Store, error) { return counter, nil }
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })

	// Many agents -> each one would otherwise resolve via store.List(Label=gc:session).
	agents := make([]config.Agent, 0, 25)
	for i := 0; i < 25; i++ {
		name := "agent" + strconv.Itoa(i)
		agents = append(agents, config.Agent{Name: name, MaxActiveSessions: intPtr(1)})
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Agents:    agents,
	}

	cityPath := filepath.Join(t.TempDir(), "city")
	var stdout, stderr bytes.Buffer
	if code := doCityStatus(sp, dops, cfg, cityPath, &stdout, &stderr); code != 0 {
		t.Fatalf("doCityStatus code = %d, want 0; stderr=%q", code, stderr.String())
	}

	if got := counter.sessionLabelCalls(); got != 1 {
		t.Fatalf("session-label List calls = %d, want 1 (caching collapses redundant scans)", got)
	}
}

type sessionLabelListCounter struct {
	beads.Store
	mu    sync.Mutex
	count int
}

func (s *sessionLabelListCounter) List(q beads.ListQuery) ([]beads.Bead, error) {
	if isCanonicalSessionListQuery(q) || (q.Label == session.LabelSession && q.IncludeClosed) {
		s.mu.Lock()
		s.count++
		s.mu.Unlock()
	}
	return s.Store.List(q)
}

func (s *sessionLabelListCounter) sessionLabelCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.count
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
