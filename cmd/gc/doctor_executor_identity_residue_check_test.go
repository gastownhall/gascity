package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestExecutorIdentityResidueCheckFlagsAndFixesStaleStamp(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "stale stamp", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
			"gc.work_dir":     "/worktrees/gascity/builder-1",
			"work_dir":        "/legacy/worktrees/gascity/builder-1",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "CITY-1") {
		t.Fatalf("details missing CITY-1:\n%s", details)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}

	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for _, key := range []string{"gc.session_name", "gc.work_dir", "work_dir"} {
		if v := bd.Metadata[key]; v != "" {
			t.Fatalf("expected %s cleared, got %q", key, v)
		}
	}
	if bd.Metadata["gc.routed_to"] != "gascity/reviewer" {
		t.Fatalf("Fix must not touch gc.routed_to, got %q", bd.Metadata["gc.routed_to"])
	}

	result = check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status after fix = %v, want ok: %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckSkipsInProgressBead(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "in flight", Type: "task", Status: "in_progress", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
			"gc.work_dir":     "/worktrees/gascity/builder-1",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (in_progress beads must never be flagged): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}

	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder" || bd.Metadata["gc.work_dir"] != "/worktrees/gascity/builder-1" {
		t.Fatalf("Fix must not touch an in_progress bead's stamps, got %+v", bd.Metadata)
	}
}

func TestExecutorIdentityResidueCheckFixIsIdempotent(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := &residueSetMetadataBatchSpyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "stale stamp", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
		}},
	}, nil)}

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("first Fix returned error: %v", err)
	}
	if cityStore.calls != 1 {
		t.Fatalf("expected exactly 1 write after first Fix, got %d", cityStore.calls)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("second Fix returned error: %v", err)
	}
	if cityStore.calls != 1 {
		t.Fatalf("expected zero additional writes on second Fix, got %d total", cityStore.calls)
	}
}

func TestExecutorIdentityResidueCheckAllowsCanonicalSessionNameEncoding(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "still current", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (session_name canonicalizes to current routed_to, not drift): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckSkipsSessionBeadWithoutSessionName(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "session bead", Type: "session", Status: "open", Metadata: map[string]string{
			"work_dir": "/worktrees/gascity/builder-1",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a session bead's bare work_dir must not be flagged without gc.session_name): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckSkipsDrainStepWithoutSessionName(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "drain step", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.work_dir": "/worktrees/gascity/builder-1",
			"work_dir":    "/worktrees/gascity/builder-1",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a drain step/member carries both work_dir keys but never gc.session_name, so it must not be flagged): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckSkipsOpenPoolBeadWithoutSessionName(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "pool ready", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.work_dir": "/worktrees/gascity/builder-1",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (an open pool-ready bead has gc.work_dir but no gc.session_name, so it must not be flagged): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckSkipsWorkflowRunRootDespiteSessionNameStamp(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "workflow run root", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.kind":         "workflow",
			"gc.session_name": "gascity--builder",
			"gc.work_dir":     "/worktrees/gascity/builder-1",
			"gc.routed_to":    "gascity/reviewer",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a workflow run root is topology, never itself claimed; a completed step's visibility stamp on gc.session_name/gc.work_dir must not read as residue even though it mismatches gc.routed_to): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder" || bd.Metadata["gc.work_dir"] != "/worktrees/gascity/builder-1" {
		t.Fatalf("Fix must not clear a workflow run root's visibility stamp, got %+v", bd.Metadata)
	}
}

func TestExecutorIdentityResidueCheckAllowsCustomSessionTemplateEncoding(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{
		Name:            "acmecity",
		SessionTemplate: "{{.City}}-{{.Name}}",
	}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "custom template session", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "acmecity-builder",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (gc.session_name minted via a custom session_template forward-encodes from the current gc.routed_to, so it is current, not drift): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckDescribeNamesTriggeringKeys(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "session-name residue", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
		}},
		{ID: "CITY-2", Title: "work_dir residue", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.work_dir": "/worktrees/gascity/builder-1",
			"work_dir":    "/legacy/worktrees/gascity/builder-1",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning: %#v", result.Status, result)
	}

	sessionDetail := residueDetailFor(t, result.Details, "CITY-1")
	if !strings.Contains(sessionDetail, "(gc.session_name)") {
		t.Fatalf("describe() must name the triggering key for a session-name finding, got:\n%s", sessionDetail)
	}
	if strings.Contains(sessionDetail, "work_dir") {
		t.Fatalf("describe() must not name work_dir keys for a session-name-only finding, got:\n%s", sessionDetail)
	}

	workDirDetail := residueDetailFor(t, result.Details, "CITY-2")
	if !strings.Contains(workDirDetail, "(gc.work_dir, work_dir)") {
		t.Fatalf("describe() must name both triggering keys for a work_dir finding, got:\n%s", workDirDetail)
	}
	if strings.Contains(workDirDetail, "session_name") {
		t.Fatalf("describe() must not name gc.session_name for a work_dir-only finding, got:\n%s", workDirDetail)
	}
}

// residueDetailFor returns the single detail line naming beadID, failing the
// test if zero or more than one line matches.
func residueDetailFor(t *testing.T, details []string, beadID string) string {
	t.Helper()
	var match string
	count := 0
	for _, d := range details {
		if strings.Contains(d, beadID) {
			match = d
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 detail line for bead %s, got %d:\n%s", beadID, count, strings.Join(details, "\n"))
	}
	return match
}

type residueSetMetadataBatchSpyStore struct {
	beads.Store
	calls int
}

func (s *residueSetMetadataBatchSpyStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.calls++
	return s.Store.SetMetadataBatch(id, kvs)
}

// Round 2 (ga-p5eymu, amended ruling ga-6af29d decision 1) test cases below.

func TestExecutorIdentityResidueCheckSkipsOpenBeadWithEmptyRoutedTo(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "detached handoff orphan", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.session_name": "gascity--builder",
			"gc.work_branch":  "builder/ga-abc123",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (empty gc.routed_to is the ordinary post-claim/detached-orphan state, never residue): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckDistinguishesLegitimatePoolInstanceFromStaleReroute(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SESSION-1", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "gascity--builder-2",
			"template":     "gascity/builder",
		}},
		{ID: "CITY-1", Title: "legitimate pool instance", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder-2",
		}},
		{ID: "CITY-2", Title: "stale re-route", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--deployer",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (CITY-2 is a genuine re-route hazard): %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if strings.Contains(details, "CITY-1") {
		t.Fatalf("CITY-1 carries a legitimate pool-instance identity (session bead SESSION-1 records gascity--builder-2 as a real member of gascity/builder's route) and must not be flagged:\n%s", details)
	}
	if !strings.Contains(details, "CITY-2") {
		t.Fatalf("CITY-2's session name matches no session bead and no route encoding; it must still be flagged as a true positive:\n%s", details)
	}
}

func TestExecutorIdentityResidueCheckScansWithLiveOpenQuery(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	store := &residueListQuerySpyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "warrant", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder",
		}},
	}, nil)}

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return store, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok: %#v", result.Status, result)
	}

	found := false
	for _, q := range store.queries {
		if q.Status == "open" && q.Live {
			found = true
			if !q.AllowScan {
				t.Fatalf("live open scan query %+v must set AllowScan", q)
			}
			if q.IncludeClosed {
				t.Fatalf("live open scan query %+v should not also request IncludeClosed (Status=open already excludes closed; requesting both wastes the backing-store fetch)", q)
			}
		}
	}
	if !found {
		t.Fatalf("expected a Status=%q Live=true scan query (gc-4zb pattern: mapBdStatus folds bd's raw review/testing/blocked into \"open\", so only a Live query reaches the backing store's own raw --status=open filter that actually excludes them), got queries: %+v", "open", store.queries)
	}
}

func TestExecutorIdentityResidueCheckNeverFlagsClosedBead(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "closed with stale stamp", Type: "task", Status: "closed", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a stamp surviving close is documented behavior -- stampRunSessionIdentity keeps the completed-run->session link durable; closed beads are out of scope entirely): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder" {
		t.Fatalf("Fix must not clear a closed bead's stamp, got %+v", bd.Metadata)
	}
}

func TestExecutorIdentityResidueCheckFlagsLegacyCanonicalWorkDirDisagreement(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "work_dir disagreement, session_name current", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder",
			"gc.work_dir":     "/worktrees/gascity/builder-1",
			"work_dir":        "/legacy/worktrees/gascity/builder-1",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (legacy work_dir and canonical gc.work_dir disagree -- the ga-6af29d 20-bead class -- independent of gc.session_name, which is current): %#v", result.Status, result)
	}
	details := strings.Join(result.Details, "\n")
	if !strings.Contains(details, "CITY-1") {
		t.Fatalf("details missing CITY-1:\n%s", details)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.work_dir"] != "" || bd.Metadata["work_dir"] != "" {
		t.Fatalf("Fix must clear the disagreeing work_dir keys, got %+v", bd.Metadata)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder" {
		t.Fatalf("Fix must not clear gc.session_name when only the work_dir trigger fired (it is current, not a separate finding), got %q", bd.Metadata["gc.session_name"])
	}
}

type residueListQuerySpyStore struct {
	beads.Store
	queries []beads.ListQuery
}

func (s *residueListQuerySpyStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, q)
	return s.Store.List(q)
}

// Round 3 (ga-dkfmdu) test cases below.

func TestExecutorIdentityResidueCheckPreservesWorkDirOnWorktreeOwningBead(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "worktree-owning bead, retired-slot session_name", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":      "gascity/reviewer",
			"gc.session_name":   "gascity--builder-9",
			"gc.work_dir":       "/worktrees/gascity/builder-1",
			"work_dir":          "/legacy/worktrees/gascity/builder-1",
			"gc.worktree_root":  "/worktrees/gascity/builder-1",
			"gc.worktree_repo":  "gascity",
			"gc.worktree_owner": "builder-1",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (the retired-slot gc.session_name is a genuine independent finding): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	bd, err := cityStore.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.work_dir"] != "/worktrees/gascity/builder-1" || bd.Metadata["work_dir"] != "/legacy/worktrees/gascity/builder-1" {
		t.Fatalf("Fix must not clear work_dir keys on a bead carrying worktree-ownership evidence (clearing them defeats worktreeSpecForBead's fail-closed legacy/canonical conflict check by erasing the evidence it inspects), got %+v", bd.Metadata)
	}
	if bd.Metadata["gc.session_name"] != "" {
		t.Fatalf("Fix must still clear the independently-stale gc.session_name, got %q", bd.Metadata["gc.session_name"])
	}
}

func TestExecutorIdentityResidueCheckDefersToPoolSlotWorkDirRepair(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	bd := beads.Bead{ID: "CITY-1", Title: "canonical clobbered with pool-slot label", Type: "task", Status: "open", Metadata: map[string]string{
		"gc.routed_to": "gascity/builder",
		"gc.work_dir":  ".gc/worktrees/gascity/builder-1",
		"work_dir":     "/legacy/worktrees/gascity/builder-1",
	}}
	if poolSlotWorkDirRepairFor(cfg, bd) == nil {
		t.Fatalf("fixture premise broken: bead must match poolSlotWorkDirRepairFor's precondition (canonical pool-slot-shaped, legacy differs and is not)")
	}

	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{bd}, nil)
	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a bead matching poolSlotWorkDirRepairFor's precondition is a repair candidate, not residue -- flagging it races the repair sweep and risks destroying the same evidence the repair exists to restore): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckAllowsAliasOnlySessionIdentity(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SESSION-1", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"alias":    "mayor",
			"template": "gascity/mayor",
		}},
		{ID: "CITY-1", Title: "named session identity", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/mayor",
			"gc.session_name": "mayor",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (SESSION-1 is a named session identified by alias, not session_name -- sessionBeadIdentifier falls back to alias, and CITY-1's gc.session_name matches that legitimate identity for gascity/mayor): %#v", result.Status, result)
	}
}

// Round 4 (#6135 fix-merge) test cases below: session beads live in the
// session coordination class (the city store by default), never in a rig
// store, so a rig scope must resolve executor identities from there.

func TestExecutorIdentityResidueCheckHonorsCitySessionIdentityForRigScopePoolSlot(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "gascity", Path: rigDir}}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SESSION-1", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "gascity--builder-2",
			"template":     "gascity/builder",
		}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RIG-1", Title: "legitimate pool instance in a rig", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder-2",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, residueStoreFactory(t, cityDir, cityStore, rigDir, rigStore))

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (SESSION-1 lives in the city store because session beads are session-class; a rig scope must still see gascity--builder-2 as a legitimate identity for gascity/builder): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	bd, err := rigStore.Get("RIG-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder-2" {
		t.Fatalf("Fix must not clear a legitimate pool-slot identity on a rig bead, got %q", bd.Metadata["gc.session_name"])
	}
}

func TestExecutorIdentityResidueCheckHonorsCityAliasSessionIdentityForRigScope(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "gascity", Path: rigDir}}}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SESSION-1", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"alias":    "mayor",
			"template": "gascity/mayor",
		}},
	}, nil)
	rigStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "RIG-1", Title: "named session identity in a rig", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/mayor",
			"gc.session_name": "mayor",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, residueStoreFactory(t, cityDir, cityStore, rigDir, rigStore))

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (an alias-only named session in the city store is a legitimate identity for a rig bead routed to gascity/mayor): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckHonorsLabelOnlySessionBeadIdentity(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "SESSION-1", Status: "open", Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
			"session_name": "gascity--builder-2",
			"template":     "gascity/builder",
		}},
		{ID: "CITY-1", Title: "pool instance", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/builder",
			"gc.session_name": "gascity--builder-2",
		}},
	}, nil)

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok (a crash/migration-damaged session bead keeps the gc:session label with an empty type; ListAllSessionBeads unions type and label, so its identity still counts): %#v", result.Status, result)
	}
}

func TestExecutorIdentityResidueCheckFixSkipsBeadClaimedSinceCollection(t *testing.T) {
	cityDir := t.TempDir()
	cfg := &config.City{}
	cityStore := &residueClaimedOnGetSpyStore{Store: beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "CITY-1", Title: "claimed between collect and write", Type: "task", Status: "open", Metadata: map[string]string{
			"gc.routed_to":    "gascity/reviewer",
			"gc.session_name": "gascity--builder",
		}},
	}, nil)}

	check := newExecutorIdentityResidueCheck(cfg, cityDir, func(path string) (beads.Store, error) {
		if path != cityDir {
			return nil, fmt.Errorf("unexpected store path %q", path)
		}
		return cityStore, nil
	})

	result := check.Run(&doctor.CheckContext{})
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want warning (the listed snapshot is still open): %#v", result.Status, result)
	}

	if err := check.Fix(&doctor.CheckContext{}); err != nil {
		t.Fatalf("Fix returned error: %v", err)
	}
	if cityStore.writes != 0 {
		t.Fatalf("expected zero writes when the live bead is in_progress at write time (a claim consumes gc.routed_to and puts the stamp back in service; clearing it would strip identity off work in flight), got %d", cityStore.writes)
	}
	bd, err := cityStore.Store.Get("CITY-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bd.Metadata["gc.session_name"] != "gascity--builder" {
		t.Fatalf("Fix must leave the stamp on a bead claimed since collection, got %q", bd.Metadata["gc.session_name"])
	}
}

// residueStoreFactory returns a newStore func serving cityStore at cityDir and
// rigStore at rigDir, failing the test on any other path.
func residueStoreFactory(t *testing.T, cityDir string, cityStore beads.Store, rigDir string, rigStore beads.Store) func(string) (beads.Store, error) {
	t.Helper()
	return func(path string) (beads.Store, error) {
		switch path {
		case cityDir:
			return cityStore, nil
		case rigDir:
			return rigStore, nil
		}
		return nil, fmt.Errorf("unexpected store path %q", path)
	}
}

// residueClaimedOnGetSpyStore lists beads as open but returns them in_progress
// from Get, standing in for a worker that claimed the bead in the window
// between collection and the write.
type residueClaimedOnGetSpyStore struct {
	beads.Store
	writes int
}

func (s *residueClaimedOnGetSpyStore) Get(id string) (beads.Bead, error) {
	bd, err := s.Store.Get(id)
	if err != nil {
		return bd, err
	}
	bd.Status = "in_progress"
	return bd, nil
}

func (s *residueClaimedOnGetSpyStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.writes++
	return s.Store.SetMetadataBatch(id, kvs)
}
