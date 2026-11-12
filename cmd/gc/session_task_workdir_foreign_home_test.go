package main

// A session must not launch into another session's home because a claimed
// work bead's legacy work_dir names it (ga-x329t4z; incident gm-25h32c).
//
// Incident: beads/pm claimed a bead the builder had worked. The builder's
// workspace-setup had stamped the legacy `work_dir` key with the builder's own
// persistent slot (.gc/worktrees/beads/builder). The controller later
// re-stamped gc.work_dir to the PM slot but, by design, never the legacy key,
// so every PM start after the claim launched in the builder's slot while the
// PM session bead still recorded the PM slot.
//
// The guard under test rejects a task bead's work dir when it is the home of a
// session other than the candidate: (a) the configured work_dir of another
// configured agent, counting pool instance names, or (b) the work_dir or
// gc.work_dir of another open session bead. The controls pin what must keep
// working.

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

type foreignHomeFixture struct {
	cityPath   string
	pmDir      string
	builderDir string
	cfg        *config.City
	store      beads.Store
	session    beads.Bead
	tp         TemplateParams
}

// newForeignHomeFixture builds the incident's shape: two rig-scoped role
// agents, each owning a persistent slot under .gc/worktrees/<rig>/<role>, a PM
// session bead recording the PM slot, and a work bead assigned to the bare PM
// role name (fleet bd identity is role-level) carrying taskMeta.
func newForeignHomeFixture(t *testing.T, taskMeta func(f *foreignHomeFixture) map[string]string) (*foreignHomeFixture, beads.Bead) {
	t.Helper()
	f := &foreignHomeFixture{cityPath: t.TempDir()}
	f.pmDir = filepath.Join(f.cityPath, ".gc", "worktrees", "beads", "pm")
	f.builderDir = filepath.Join(f.cityPath, ".gc", "worktrees", "beads", "builder")
	for _, dir := range []string{f.pmDir, f.builderDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.cfg = &config.City{
		// The rig is what {{.Rig}} in the agents' work_dir expands to.
		Rigs: []config.Rig{{Name: "beads", Path: filepath.Join(f.cityPath, "rigs", "beads")}},
		Agents: []config.Agent{
			{Name: "pm", Dir: "beads", WorkDir: ".gc/worktrees/{{.Rig}}/pm", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
			{Name: "builder", Dir: "beads", WorkDir: ".gc/worktrees/{{.Rig}}/builder", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1)},
		},
	}
	f.store = beads.NewMemStore()
	session, err := f.store.Create(beads.Bead{
		Title:  "beads/pm",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:beads/pm"},
		Metadata: map[string]string{
			"template":                  "beads/pm",
			"session_name":              "beads--pm",
			"alias":                     "beads/pm",
			"work_dir":                  f.pmDir,
			beadmeta.WorkDirMetadataKey: f.pmDir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	f.session = session
	task := f.assignTask(t, "be-ce3oxm", "beads/pm", taskMeta(f))
	// PreStart as resolveTemplate renders it: the pack's user-authored
	// worktree-setup hook with {{.WorkDir}} already expanded to the PM slot,
	// plus gc's own generated materialize-skills entry.
	f.tp = TemplateParams{
		TemplateName: "beads/pm",
		SessionName:  "beads--pm",
		WorkDir:      f.pmDir,
		Hints: agent.StartupHints{PreStart: append(
			[]string{"/city/packs/actual/pm/scripts/worktree-setup.sh /rigs/beads " + f.pmDir + " pm --sync --freshen-commit"},
			appendMaterializeSkillsPreStart(nil, "beads/pm", f.pmDir)...,
		)},
	}
	return f, task
}

// assignTask creates an in_progress work bead for assignee carrying meta.
func (f *foreignHomeFixture) assignTask(t *testing.T, title, assignee string, meta map[string]string) beads.Bead {
	t.Helper()
	task, err := f.store.Create(beads.Bead{Title: title, Metadata: meta})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	if err := f.store.Update(task.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}
	task, err = f.store.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// addSessionBead records an open session bead for another agent.
func (f *foreignHomeFixture) addSessionBead(t *testing.T, template, sessionName string, meta map[string]string) {
	t.Helper()
	all := map[string]string{"template": template, "session_name": sessionName, "alias": template}
	for k, v := range meta {
		all[k] = v
	}
	if _, err := f.store.Create(beads.Bead{
		Title:    template,
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "agent:" + template},
		Metadata: all,
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *foreignHomeFixture) candidate(t *testing.T) startCandidate {
	t.Helper()
	return startCandidate{info: sessiontest.SeedBead(t, f.session), tp: f.tp, order: 0}
}

func (f *foreignHomeFixture) mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func foreignHomeClock() clock.Clock {
	return &clock.Fake{Time: time.Date(2026, 10, 30, 11, 45, 47, 0, time.UTC)}
}

// launchLive prepares the candidate through the live-lookup path, in the
// fixture's city: configured work dirs resolve against its path.
func (f *foreignHomeFixture) launchLive(t *testing.T) *preparedStart {
	t.Helper()
	prepared, err := prepareStartCandidateForCity(f.candidate(t), f.cityPath, "", f.cfg, nil, f.store, foreignHomeClock(), io.Discard, nil, dispatchOptionSources{})
	if err != nil {
		t.Fatalf("prepareStartCandidateForCity: %v", err)
	}
	return prepared
}

// launchSnapshot prepares the candidate through the snapshot resolver built
// over tasks.
func (f *foreignHomeFixture) launchSnapshot(t *testing.T, tasks ...beads.Bead) *preparedStart {
	t.Helper()
	resolver := newAssignedTaskWorkDirResolver(f.cityPath, tasks)
	prepared, _, err := buildPreparedStartWithWorkDirResolver(f.candidate(t), f.cityPath, f.cfg, f.store, resolver, dispatchOptionSources{})
	if err != nil {
		t.Fatalf("buildPreparedStartWithWorkDirResolver: %v", err)
	}
	return prepared
}

func foreignHomeMeta(legacy, canonical string) map[string]string {
	return map[string]string{
		beadmeta.LegacyWorkDirMetadataKey: legacy,
		beadmeta.WorkDirMetadataKey:       canonical,
	}
}

// The incident, live-lookup path (resolveTaskWorkDir): the work bead's legacy
// work_dir names the BUILDER's configured slot. The PM session must not launch
// there.
func TestPrepareStartCandidate_ForeignAgentSlotDoesNotRedirectTaskWorkDir_LiveLookup(t *testing.T) {
	f, _ := newForeignHomeFixture(t, func(f *foreignHomeFixture) map[string]string {
		return foreignHomeMeta(f.builderDir, f.pmDir)
	})
	prepared := f.launchLive(t)
	if prepared.cfg.WorkDir != f.pmDir {
		t.Errorf("PM session launches in %q, want its own slot %q: a work bead's legacy work_dir naming another agent's configured work dir must not redirect the launch", prepared.cfg.WorkDir, f.pmDir)
	}
	// Split brain without the guard: the user-authored hook still prepares the
	// PM slot while gc's generated entry was retargeted to the launch dir.
	if len(prepared.cfg.PreStart) == 2 {
		user, generated := prepared.cfg.PreStart[0], prepared.cfg.PreStart[1]
		if strings.Contains(user, f.pmDir) && strings.Contains(generated, f.builderDir) {
			t.Errorf("split brain: user pre_start prepares %q but generated pre_start and the agent target %q", f.pmDir, f.builderDir)
		}
	}
}

// The incident, snapshot path (newAssignedTaskWorkDirResolver, which the
// reconciler tick and the v2 start effect pass when assigned work was read).
func TestPrepareStartCandidate_ForeignAgentSlotDoesNotRedirectTaskWorkDir_Snapshot(t *testing.T) {
	f, task := newForeignHomeFixture(t, func(f *foreignHomeFixture) map[string]string {
		return foreignHomeMeta(f.builderDir, f.pmDir)
	})
	prepared := f.launchSnapshot(t, task)
	if prepared.cfg.WorkDir != f.pmDir {
		t.Errorf("PM session launches in %q, want its own slot %q", prepared.cfg.WorkDir, f.pmDir)
	}
}

// Guard (a) counts pool instance names: another agent's pool slot is that
// agent's home even though no configured work_dir names it literally.
func TestPrepareStartCandidate_ForeignPoolInstanceSlotDoesNotRedirectTaskWorkDir(t *testing.T) {
	var slot string
	f, task := newForeignHomeFixture(t, func(f *foreignHomeFixture) map[string]string {
		slot = f.mkdir(t, filepath.Join(f.cityPath, ".gc", "worktrees", "beads", "worker-2"))
		return foreignHomeMeta(slot, f.pmDir)
	})
	f.cfg.Agents = append(f.cfg.Agents, config.Agent{
		Name: "worker", Dir: "beads", WorkDir: ".gc/worktrees/{{.Rig}}/{{.AgentBase}}",
		MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
	})
	if got := f.launchLive(t).cfg.WorkDir; got != f.pmDir {
		t.Errorf("live lookup: PM session launches in %q, want its own slot %q (pool instance slot %q belongs to worker)", got, f.pmDir, slot)
	}
	if got := f.launchSnapshot(t, task).cfg.WorkDir; got != f.pmDir {
		t.Errorf("snapshot: PM session launches in %q, want its own slot %q (pool instance slot %q belongs to worker)", got, f.pmDir, slot)
	}
}

// Guard (b): the dir is the recorded home of an open session bead whose agent
// is no longer in config, so only the session bead can say it is taken.
func TestPrepareStartCandidate_OpenSessionBeadHomeDoesNotRedirectTaskWorkDir(t *testing.T) {
	for _, key := range []string{"work_dir", beadmeta.WorkDirMetadataKey} {
		t.Run(key, func(t *testing.T) {
			var ghostDir string
			f, task := newForeignHomeFixture(t, func(f *foreignHomeFixture) map[string]string {
				ghostDir = f.mkdir(t, filepath.Join(f.cityPath, ".gc", "worktrees", "beads", "ghost"))
				return foreignHomeMeta(ghostDir, f.pmDir)
			})
			f.addSessionBead(t, "beads/ghost", "beads--ghost", map[string]string{key: ghostDir})
			if got := f.launchLive(t).cfg.WorkDir; got != f.pmDir {
				t.Errorf("live lookup: PM session launches in %q, want its own slot %q (%q is the home of an open session)", got, f.pmDir, ghostDir)
			}
			if got := f.launchSnapshot(t, task).cfg.WorkDir; got != f.pmDir {
				t.Errorf("snapshot: PM session launches in %q, want its own slot %q (%q is the home of an open session)", got, f.pmDir, ghostDir)
			}
		})
	}
}

// A rejected bead is skipped and resolution continues: the snapshot index
// keeps every candidate per assignee, so the next-newest bead's dir wins.
func TestNewAssignedTaskWorkDirResolver_SkipsForeignHomeToNextNewestBead(t *testing.T) {
	var perBead string
	f, newest := newForeignHomeFixture(t, func(f *foreignHomeFixture) map[string]string {
		perBead = f.mkdir(t, filepath.Join(f.cityPath, "rigs", "beads", "worktrees", "be-older"))
		return foreignHomeMeta(f.builderDir, f.pmDir)
	})
	newest.CreatedAt = time.Date(2026, 10, 30, 11, 0, 0, 0, time.UTC)
	older := beads.Bead{
		ID: "be-older", Status: "in_progress", Assignee: "beads/pm",
		CreatedAt: time.Date(2026, 10, 30, 7, 0, 0, 0, time.UTC),
		Metadata:  map[string]string{beadmeta.LegacyWorkDirMetadataKey: perBead},
	}
	if got := f.launchSnapshot(t, newest, older).cfg.WorkDir; got != perBead {
		t.Errorf("launch dir = %q, want the next-newest bead's per-bead worktree %q", got, perBead)
	}
}

// Live path: a rejected bead is skipped and the next assigned bead decides.
func TestPrepareStartCandidate_SkipsForeignHomeToNextAssignedBead(t *testing.T) {
	var perBead string
	f, _ := newForeignHomeFixture(t, func(f *foreignHomeFixture) map[string]string {
		perBead = f.mkdir(t, filepath.Join(f.cityPath, "rigs", "beads", "worktrees", "be-second"))
		return foreignHomeMeta(f.builderDir, f.pmDir)
	})
	f.assignTask(t, "be-second", "beads/pm", map[string]string{beadmeta.LegacyWorkDirMetadataKey: perBead})
	// Whichever bead lists first, the builder-slot one is skipped.
	if got := f.launchLive(t).cfg.WorkDir; got != perBead {
		t.Errorf("launch dir = %q, want the other assigned bead's per-bead worktree %q", got, perBead)
	}
}

// Control: the candidate's own configured slot is never rejected, even when
// the session row records a stale dir.
func TestPrepareStartCandidate_Control_OwnConfiguredSlotIsAccepted(t *testing.T) {
	f, _ := newForeignHomeFixture(t, func(f *foreignHomeFixture) map[string]string {
		return map[string]string{beadmeta.LegacyWorkDirMetadataKey: f.pmDir}
	})
	stale := f.mkdir(t, filepath.Join(f.cityPath, "stale"))
	if err := f.store.SetMetadataBatch(f.session.ID, map[string]string{"work_dir": stale, beadmeta.WorkDirMetadataKey: stale}); err != nil {
		t.Fatal(err)
	}
	f.session, _ = f.store.Get(f.session.ID)
	if got := f.launchLive(t).cfg.WorkDir; got != f.pmDir {
		t.Fatalf("WorkDir = %q, want the candidate's own configured slot %q", got, f.pmDir)
	}
}

// Control, the mayor's intervention: with the legacy key unset, the same bead
// (gc.work_dir still set) leaves the PM in its own slot. Passing proves the
// legacy key alone did the redirect, and that gc.work_dir is not launch
// authority (ga-rpvye5).
func TestPrepareStartCandidate_Control_LegacyKeyUnsetKeepsOwnSlot(t *testing.T) {
	f, _ := newForeignHomeFixture(t, func(f *foreignHomeFixture) map[string]string {
		return map[string]string{beadmeta.WorkDirMetadataKey: f.builderDir}
	})
	if got := f.launchLive(t).cfg.WorkDir; got != f.pmDir {
		t.Fatalf("WorkDir = %q, want %q", got, f.pmDir)
	}
}

// Control, the feature the guard must keep: a legacy work_dir naming a
// per-bead worktree that no session owns still redirects the launch.
func TestPrepareStartCandidate_Control_PerBeadWorktreeStillRedirects(t *testing.T) {
	var perBead string
	f, _ := newForeignHomeFixture(t, func(f *foreignHomeFixture) map[string]string {
		perBead = f.mkdir(t, filepath.Join(f.cityPath, "rigs", "beads", "worktrees", "be-ce3oxm"))
		return map[string]string{beadmeta.LegacyWorkDirMetadataKey: perBead}
	})
	if got := f.launchLive(t).cfg.WorkDir; got != perBead {
		t.Fatalf("WorkDir = %q, want per-bead worktree %q", got, perBead)
	}
}

// Control, disproving the alternative that the pool trigger binding put the PM
// in the builder slot: worktreeSpecForBead never yields the builder slot for
// this bead. Before PM's claim both keys named the builder slot with no
// ownership evidence (unmanaged: nil spec, so the binding uses the PM's
// configured dir); after the re-stamp the keys conflict and it errors (the
// request is refused).
func TestWorktreeSpecForBead_Control_PoolBindingCannotChooseBuilderSlot(t *testing.T) {
	builder := "/city/.gc/worktrees/beads/builder"
	pm := "/city/.gc/worktrees/beads/pm"
	before := beads.Bead{ID: "be-ce3oxm", Metadata: map[string]string{
		beadmeta.LegacyWorkDirMetadataKey: builder,
		beadmeta.WorkDirMetadataKey:       builder,
		beadmeta.WorkBranchMetadataKey:    "pm/be-7j3yps-valve-slot",
	}}
	if spec, err := worktreeSpecForBead(before, "rig:beads"); err != nil || spec != nil {
		t.Fatalf("before re-stamp: spec=%+v err=%v, want nil spec (unmanaged) and no error", spec, err)
	}
	after := beads.Bead{ID: "be-ce3oxm", Metadata: map[string]string{
		beadmeta.LegacyWorkDirMetadataKey: builder,
		beadmeta.WorkDirMetadataKey:       pm,
		beadmeta.WorkBranchMetadataKey:    "pm/be-7j3yps-valve-slot",
	}}
	if spec, err := worktreeSpecForBead(after, "rig:beads"); err == nil || spec != nil {
		t.Fatalf("after re-stamp: spec=%+v err=%v, want a conflicting-keys error", spec, err)
	}
}
