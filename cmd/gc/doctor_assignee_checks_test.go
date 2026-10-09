package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/session"
)

func TestAssigneeResolvesCheckWarnsWhenRosterIsEmpty(t *testing.T) {
	result := newAssigneeResolvesCheck(nil, "/nonexistent", nil).Run(nil)
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning", result.Status)
	}
	if !strings.Contains(result.Message, "cannot be checked") {
		t.Errorf("message does not say the check could not answer: %q", result.Message)
	}
}

func TestAssigneeResolvesCheckReportsUnroutableAssignee(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{Title: "orphaned", Status: "open", Assignee: "phantom-owner"}); err != nil {
		t.Fatalf("seeding store: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gascity"},
		Agents:    []config.Agent{{Name: "polecat"}},
	}
	check := newAssigneeResolvesCheck(cfg, "/city", func(string) (beads.Store, error) { return store, nil })

	result := check.Run(nil)

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning", result.Status)
	}
	if !strings.Contains(result.Message, "1 open bead") {
		t.Errorf("message = %q, want it to report 1 unroutable bead", result.Message)
	}
	found := false
	for _, d := range result.Details {
		if strings.Contains(d, "phantom-owner") {
			found = true
		}
	}
	if !found {
		t.Errorf("details = %v, want an entry naming phantom-owner", result.Details)
	}
}

func TestAssigneeResolvesCheckScanScopeRecordsNilStoreConstructorAsSkipped(t *testing.T) {
	cfg := &config.City{Agents: []config.Agent{{Name: "polecat"}}}
	check := newAssigneeResolvesCheck(cfg, "/city", nil)

	result := check.Run(nil)

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning", result.Status)
	}
	if !strings.Contains(result.Message, "skipped") {
		t.Errorf("message = %q, want it to say scopes were skipped", result.Message)
	}
}

func TestIsOpenWorkStatus(t *testing.T) {
	for _, status := range []string{"open", "in_progress", "blocked", " open "} {
		if !isOpenWorkStatus(status) {
			t.Errorf("isOpenWorkStatus(%q) = false, want true", status)
		}
	}
	for _, status := range []string{"closed", "", "done"} {
		if isOpenWorkStatus(status) {
			t.Errorf("isOpenWorkStatus(%q) = true, want false", status)
		}
	}
}

func TestAssigneeResolvesCheckAcceptsLiveSessionAlias(t *testing.T) {
	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Title:    "scratch-lead",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"alias": "scratch-lead", "session_name": "scratch-lead"},
	}); err != nil {
		t.Fatalf("seeding session bead: %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "aliased work", Status: "open", Assignee: "scratch-lead"}); err != nil {
		t.Fatalf("seeding work bead: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gascity"},
		Agents:    []config.Agent{{Name: "polecat"}},
	}
	check := newAssigneeResolvesCheck(cfg, t.TempDir(), func(string) (beads.Store, error) { return store, nil })

	result := check.Run(nil)

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v (%s, %v), want ok for work assigned to a live session alias", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(result.Message, "every non-empty assignee on open beads in the scanned city and active rig stores resolves") {
		t.Errorf("message = %q, want the successful scan scope", result.Message)
	}
}

func TestAssigneeResolvesCheckNamesRelocatedClassStoresWithoutWarning(t *testing.T) {
	cityStore := beads.NewMemStore()
	relocatedStore := beads.NewMemStore()
	if _, err := relocatedStore.Create(beads.Bead{Title: "orphaned", Status: "open", Assignee: "phantom-owner"}); err != nil {
		t.Fatalf("seeding relocated store: %v", err)
	}
	cfg := infraSplitConfig("/city/.gc/infra")
	cfg.Agents = []config.Agent{{Name: "polecat"}}
	var opened []string
	check := newAssigneeResolvesCheck(cfg, "/city", func(path string) (beads.Store, error) {
		opened = append(opened, path)
		if path == "/city" {
			return cityStore, nil
		}
		return relocatedStore, nil
	})

	result := check.Run(nil)

	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v (%s, %v), want ok: a relocated class store is out of scope, not a defect", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(result.Message, "not scanned") {
		t.Errorf("message = %q, want it to say relocated stores were not scanned", result.Message)
	}
	if len(result.Details) != len(infraMigrationClasses) {
		t.Fatalf("details = %v, want one entry per relocated class", result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "graph") {
		t.Errorf("details = %v, want the relocated graph class named", result.Details)
	}
	for _, path := range opened {
		if path != "/city" {
			t.Errorf("store constructor opened %q; want only the city store", path)
		}
	}
	if strings.Contains(strings.Join(result.Details, "\n"), "phantom-owner") {
		t.Errorf("details = %v, relocated graph bead was scanned", result.Details)
	}
}

func TestAssigneeResolvesCheckStillFlagsCityBeadsWhenStoresAreRelocated(t *testing.T) {
	cityStore := beads.NewMemStore()
	if _, err := cityStore.Create(beads.Bead{Title: "orphaned", Status: "open", Assignee: "phantom-owner"}); err != nil {
		t.Fatalf("seeding city store: %v", err)
	}
	cfg := infraSplitConfig("/city/.gc/infra")
	cfg.Workspace.Name = "gascity"
	cfg.Agents = []config.Agent{{Name: "polecat"}}
	check := newAssigneeResolvesCheck(cfg, "/city", func(string) (beads.Store, error) { return cityStore, nil })

	result := check.Run(nil)

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (%s, %v), want warning", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(result.Message, "1 open bead") {
		t.Errorf("message = %q, want 1 unroutable bead", result.Message)
	}
}

func TestAssigneeResolvesCheckScansEphemeralBeads(t *testing.T) {
	store := beads.NewMemStore()
	phantom, err := store.Create(beads.Bead{
		Title:     "ephemeral orphan",
		Status:    "open",
		Assignee:  "goal-5-temporal",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("seeding ephemeral store: %v", err)
	}
	cfg := workOnlyStorageConfig()
	cfg.Agents = []config.Agent{{Name: "worker"}}
	check := newAssigneeResolvesCheck(cfg, "/city", func(string) (beads.Store, error) { return store, nil })

	result := check.Run(nil)

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (%s, %v), want warning", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), phantom.ID) {
		t.Fatalf("details = %v, want ephemeral bead %s", result.Details, phantom.ID)
	}
}

func TestAssigneeResolvesCheckReportsActiveRigWithoutPathAsSkipped(t *testing.T) {
	cfg := workOnlyStorageConfig()
	cfg.Agents = []config.Agent{{Name: "worker"}}
	cfg.Rigs = []config.Rig{{Name: "unresolved"}}
	store := beads.NewMemStore()
	check := newAssigneeResolvesCheck(cfg, "/city", func(string) (beads.Store, error) { return store, nil })

	result := check.Run(nil)

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (%s, %v), want warning", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "rig unresolved skipped: no store path resolved") {
		t.Errorf("details = %v, want unresolved rig path", result.Details)
	}
}

func TestAssigneeResolvesCheckReportsUnreadableSuspensionState(t *testing.T) {
	cityPath := t.TempDir()
	statePath := filepath.Join(cityPath, ".gc", "runtime", "suspension-state.json")
	if err := os.MkdirAll(statePath, 0o700); err != nil {
		t.Fatalf("creating unreadable suspension-state path: %v", err)
	}
	cfg := workOnlyStorageConfig()
	cfg.Agents = []config.Agent{{Name: "worker"}}
	cfg.Rigs = []config.Rig{{Name: "resumed", Path: filepath.Join(cityPath, "resumed"), SuspendedOnStart: true}}
	store := beads.NewMemStore()
	check := newAssigneeResolvesCheck(cfg, cityPath, func(string) (beads.Store, error) { return store, nil })

	result := check.Run(nil)

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (%s, %v), want warning", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "suspension state skipped") {
		t.Errorf("details = %v, want suspension-state read failure", result.Details)
	}
}

func TestAssigneeResolvesCheckReportsTmuxAliasResolutionErrors(t *testing.T) {
	cfg := workOnlyStorageConfig()
	cfg.Agents = []config.Agent{{Name: "worker", TmuxAlias: "{{.Unknown}}"}}
	store := beads.NewMemStore()
	check := newAssigneeResolvesCheck(cfg, "/city", func(string) (beads.Store, error) { return store, nil })

	result := check.Run(nil)

	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (%s, %v), want warning", result.Status, result.Message, result.Details)
	}
	if !strings.Contains(strings.Join(result.Details, "\n"), "tmux_alias") {
		t.Errorf("details = %v, want tmux_alias resolution error", result.Details)
	}
}
