package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// The launch-time guard refuses a start whose bead work_dir override resolves
// to another configured agent's role work_dir (FR4-T of ga-yq48qr). These tests
// drive buildPreparedStartWithWorkDirResolver so every override source reaches
// the guard through its real resolver.

const guardSessionName = "reviewer-session"

// guardFixture is a city with three agents: builder and reviewer own role
// work_dirs, polecat is a bounded pool (max 3) whose instances each get their
// own directory. The candidate session always belongs to reviewer.
type guardFixture struct {
	cityPath string
	cfg      *config.City
	store    beads.Store
}

func newGuardFixture(t *testing.T) *guardFixture {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "guard"},
		Agents: []config.Agent{
			{Name: "builder", WorkDir: ".gc/worktrees/builder"},
			{Name: "reviewer", WorkDir: ".gc/worktrees/reviewer"},
			{Name: "polecat", WorkDir: ".gc/worktrees/polecat/{{.AgentBase}}", MaxActiveSessions: intPtr(3)},
		},
	}
	for _, dir := range []string{
		".gc/worktrees/builder",
		".gc/worktrees/builder/ga-123",
		".gc/worktrees/reviewer",
		".gc/worktrees/polecat/polecat",
		".gc/worktrees/polecat/polecat-2",
		".gc/worktrees/polecat/polecat-4",
	} {
		if err := os.MkdirAll(filepath.Join(cityPath, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &guardFixture{cityPath: cityPath, cfg: cfg, store: beads.NewMemStore()}
}

func (f *guardFixture) abs(rel string) string { return filepath.Join(f.cityPath, rel) }

// overrideSource wires value as a work_dir override through one of the four
// ways a launch can learn a directory other than the configured one.
type overrideSource struct {
	name string
	// wire returns the session metadata and snapshot resolver that make value
	// the override.
	wire func(t *testing.T, f *guardFixture, value string) (map[string]string, taskWorkDirResolver)
}

func guardOverrideSources() []overrideSource {
	return []overrideSource{
		{name: "trigger-drain-source", wire: func(t *testing.T, f *guardFixture, value string) (map[string]string, taskWorkDirResolver) {
			source := mustCreateGuardBead(t, f.store, beads.Bead{
				Title:    "drain source anchor",
				Type:     "task",
				Metadata: map[string]string{beadmeta.LegacyWorkDirMetadataKey: value},
			})
			root := mustCreateGuardBead(t, f.store, beads.Bead{
				Title:    "drain item workflow",
				Type:     "task",
				Metadata: map[string]string{beadmeta.DrainMemberIDMetadataKey: source.ID},
			})
			trigger := mustCreateGuardBead(t, f.store, beads.Bead{
				Title:    "unclaimed step",
				Type:     "task",
				Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
			})
			return map[string]string{
				beadmeta.TriggerBeadIDMetadataKey:       trigger.ID,
				beadmeta.TriggerBeadStoreRefMetadataKey: "city",
			}, nil
		}},
		{name: "snapshot-resolver", wire: func(t *testing.T, f *guardFixture, value string) (map[string]string, taskWorkDirResolver) {
			task := guardInProgressTask(t, f.store, value)
			return nil, newAssignedTaskWorkDirResolver(f.cityPath, []beads.Bead{task})
		}},
		{name: "live-task-lookup", wire: func(t *testing.T, f *guardFixture, value string) (map[string]string, taskWorkDirResolver) {
			guardInProgressTask(t, f.store, value)
			return nil, nil
		}},
		{name: "session-record", wire: func(_ *testing.T, _ *guardFixture, value string) (map[string]string, taskWorkDirResolver) {
			return map[string]string{
				beadmeta.WorkDirMetadataKey:       value,
				beadmeta.LegacyWorkDirMetadataKey: value,
			}, nil
		}},
	}
}

func mustCreateGuardBead(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// guardInProgressTask creates an in-progress task assigned to the reviewer
// session, carrying value as its work_dir.
func guardInProgressTask(t *testing.T, store beads.Store, value string) beads.Bead {
	t.Helper()
	task := mustCreateGuardBead(t, store, beads.Bead{
		Title:    "task",
		Type:     "task",
		Metadata: map[string]string{"work_dir": value},
	})
	status, assignee := "in_progress", guardSessionName
	if err := store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}
	task, err := store.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// start builds the reviewer session's prepared start with value wired through
// src, returning the build error and the work_dir the launch settled on.
func (f *guardFixture) start(t *testing.T, src overrideSource, value string) (string, error) {
	t.Helper()
	meta := map[string]string{
		"template":     "reviewer",
		"session_name": guardSessionName,
	}
	extra, resolver := map[string]string(nil), taskWorkDirResolver(nil)
	if src.wire != nil {
		extra, resolver = src.wire(t, f, value)
	}
	for k, v := range extra {
		meta[k] = v
	}
	session := mustCreateGuardBead(t, f.store, beads.Bead{
		Title:    "reviewer",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "agent:reviewer"},
		Metadata: meta,
	})
	prepared, _, err := buildPreparedStartWithWorkDirResolver(startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp: TemplateParams{
			TemplateName: "reviewer",
			SessionName:  guardSessionName,
			WorkDir:      f.abs(".gc/worktrees/reviewer"),
		},
	}, f.cityPath, f.cfg, f.store, resolver, dispatchOptionSources{})
	if err != nil {
		return "", err
	}
	return prepared.cfg.WorkDir, nil
}

// 1 + 6: an override equal to another agent's role dir is refused, from every
// source, and the error names the session, the rejected path and the owner.
func TestWorkDirOverrideGuardRefusesAnotherAgentsRoleDir(t *testing.T) {
	for _, src := range guardOverrideSources() {
		t.Run(src.name, func(t *testing.T) {
			f := newGuardFixture(t)
			builderDir := f.abs(".gc/worktrees/builder")
			_, err := f.start(t, src, builderDir)
			if err == nil {
				t.Fatalf("start with work_dir override %q = nil error, want refusal", builderDir)
			}
			for _, want := range []string{guardSessionName, builderDir, "builder"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
}

// 2: the candidate's own role dir is not another agent's.
func TestWorkDirOverrideGuardAllowsOwnRoleDir(t *testing.T) {
	for _, src := range guardOverrideSources() {
		t.Run(src.name, func(t *testing.T) {
			f := newGuardFixture(t)
			own := f.abs(".gc/worktrees/reviewer")
			got, err := f.start(t, src, own)
			if err != nil {
				t.Fatalf("start with own role dir override: %v", err)
			}
			if got != own {
				t.Fatalf("launch work_dir = %q, want %q", got, own)
			}
		})
	}
}

// 3: per-bead worktrees nest under a role dir; nesting is not equality.
func TestWorkDirOverrideGuardAllowsWorktreeNestedUnderAnotherRoleDir(t *testing.T) {
	for _, src := range guardOverrideSources() {
		t.Run(src.name, func(t *testing.T) {
			f := newGuardFixture(t)
			nested := f.abs(".gc/worktrees/builder/ga-123")
			got, err := f.start(t, src, nested)
			if err != nil {
				t.Fatalf("start with nested worktree override: %v", err)
			}
			if got != nested {
				t.Fatalf("launch work_dir = %q, want %q", got, nested)
			}
		})
	}
}

// 4: the guard compares resolved paths, not the spelling of the override.
func TestWorkDirOverrideGuardResolvesCityRelativeTrailingSlashAndSymlink(t *testing.T) {
	live := guardOverrideSources()[2]
	for _, tc := range []struct {
		name  string
		value func(f *guardFixture) string
	}{
		{"city-relative", func(*guardFixture) string { return ".gc/worktrees/builder" }},
		{"city-relative-trailing-slash", func(*guardFixture) string { return ".gc/worktrees/builder/" }},
		{"absolute-trailing-slash", func(f *guardFixture) string { return f.abs(".gc/worktrees/builder") + "/" }},
		{"symlink", func(f *guardFixture) string {
			link := f.abs(".gc/builder-alias")
			if err := os.Symlink(f.abs(".gc/worktrees/builder"), link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
		{"symlink-trailing-slash", func(f *guardFixture) string {
			link := f.abs(".gc/builder-alias")
			if err := os.Symlink(f.abs(".gc/worktrees/builder"), link); err != nil {
				t.Fatal(err)
			}
			return link + "/"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardFixture(t)
			if _, err := f.start(t, live, tc.value(f)); err == nil {
				t.Fatal("start = nil error, want refusal of an override that resolves to the builder role dir")
			}
		})
	}
}

// 5: with no override, sharing a configured directory stays config-time
// validation's business; the launch is unchanged.
func TestWorkDirOverrideGuardIgnoresSharedConfiguredDir(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "guard"},
		Agents:    []config.Agent{{Name: "builder"}, {Name: "reviewer"}},
	}
	session := mustCreateGuardBead(t, store, beads.Bead{
		Title:    "reviewer",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "agent:reviewer"},
		Metadata: map[string]string{"template": "reviewer", "session_name": guardSessionName},
	})
	prepared, _, err := buildPreparedStartWithWorkDirResolver(startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp:   TemplateParams{TemplateName: "reviewer", SessionName: guardSessionName, WorkDir: cityPath},
	}, cityPath, cfg, store, nil, dispatchOptionSources{})
	if err != nil {
		t.Fatalf("start of a dir-less agent at the city root: %v", err)
	}
	if prepared.cfg.WorkDir != cityPath {
		t.Fatalf("launch work_dir = %q, want %q", prepared.cfg.WorkDir, cityPath)
	}
}

// 7: a bounded pool owns its template dir and each instance dir up to
// max_active_sessions; nothing past the bound is a role dir.
func TestWorkDirOverrideGuardCoversBoundedPoolDirs(t *testing.T) {
	live := guardOverrideSources()[2]
	for _, tc := range []struct {
		name    string
		rel     string
		refused bool
	}{
		{"template-dir", ".gc/worktrees/polecat/polecat", true},
		{"instance-2", ".gc/worktrees/polecat/polecat-2", true},
		{"past-the-bound", ".gc/worktrees/polecat/polecat-4", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGuardFixture(t)
			_, err := f.start(t, live, f.abs(tc.rel))
			if tc.refused && err == nil {
				t.Fatalf("start with override %q = nil error, want refusal naming the polecat pool", tc.rel)
			}
			if tc.refused && !strings.Contains(err.Error(), "polecat") {
				t.Fatalf("error %q does not name the polecat owner", err)
			}
			if !tc.refused && err != nil {
				t.Fatalf("start with override %q: %v", tc.rel, err)
			}
		})
	}
}
