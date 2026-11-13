//go:build integration && unix

package integration

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestE2E_Pool_InstanceNaming verifies that pool agents with max>1 get
// numbered instance names (worker-1, worker-2, etc.).
func TestE2E_Pool_InstanceNaming(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "worker",
				StartCommand: e2eReportScript(),
				Pool:         &e2ePool{Min: 2, Max: 2, Check: "echo 2"},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// The slot names are real and still stamped on the session bead (agent_name),
	// which is how the members are discovered here. What they are NOT is public
	// identity: an expanding-pool slot rebinds to a fresh session whenever a
	// holder dies, so each member owns work under its own session bead ID and
	// its GC_AGENT is that ID, not "worker-N".
	reports := waitForPoolMemberReports(t, cityDir, "worker", []string{"worker-1", "worker-2"}, e2eDefaultTimeout())
	for _, slot := range []string{"worker-1", "worker-2"} {
		if agent := reports[slot].get("GC_AGENT"); agent == slot {
			t.Errorf("%s GC_AGENT = %q: an expanding-pool member must not claim under its rebinding slot", slot, agent)
		} else if agent == "" {
			t.Errorf("%s has no GC_AGENT", slot)
		}
		if got := reports[slot].get("GC_ALIAS"); got != "" {
			t.Errorf("%s GC_ALIAS = %q, want empty for an unaliased pool slot", slot, got)
		}
	}
}

// TestE2E_Pool_MaxOneUsesCanonicalIdentity verifies that max=1 pool configs
// use the canonical singleton identity rather than concrete pool instance
// naming.
func TestE2E_Pool_MaxOneUsesCanonicalIdentity(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "singleton",
				StartCommand: e2eReportScript(),
				Pool:         &e2ePool{Min: 1, Max: 1, Check: "echo 1"},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	report := waitForReport(t, cityDir, "singleton", e2eDefaultTimeout())
	if !report.has("GC_AGENT", "singleton") {
		t.Errorf("singleton GC_AGENT: got %v, want [singleton]", report.getAll("GC_AGENT"))
	}
	// The canonical singleton identity never rebinds, so unlike an expanding
	// pool slot it stays a public alias and keeps answering to its own name.
	if got := report.get("GC_ALIAS"); got != "singleton" {
		t.Errorf("singleton GC_ALIAS = %q, want \"singleton\": a non-rebinding identity keeps its alias", got)
	}
}

// TestE2E_Pool_WithDir verifies that pool agents with a dir get the
// correct GC_DIR and working directory.
func TestE2E_Pool_WithDir(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "dirpool",
				StartCommand: e2eReportScript(),
				Dir:          "workdir",
				Pool:         &e2ePool{Min: 2, Max: 2, Check: "echo 2"},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// Pool instances with a dir: the qualified SLOT names include the dir prefix,
	// and remain the discovery key even though they are not the members' public
	// identity.
	reports := waitForPoolMemberReports(t, cityDir, "workdir/dirpool",
		[]string{"workdir/dirpool-1", "workdir/dirpool-2"}, e2eDefaultTimeout())

	// The e2e helper gives every pooled agent a per-instance work_dir, so each
	// instance runs in its own directory under the city's worktrees root
	// rather than sharing the configured dir.
	assertPerInstancePoolCWDs(t, cityDir, reports,
		map[string]string{"workdir/dirpool-1": "dirpool-1", "workdir/dirpool-2": "dirpool-2"})
}

// TestE2E_Pool_SharedDir verifies the pool work_dir isolation contract: a
// pooled agent that can run several sessions at once never shares one working
// directory. The e2e helper writes a per-instance work_dir for every pooled
// agent (a pool whose work_dir does not vary per instance is rejected at
// start, see workdir.ValidatePoolWorkDirIsolation), so the instances report
// different CWDs.
func TestE2E_Pool_SharedDir(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "shared",
				StartCommand: e2eReportScript(),
				Pool:         &e2ePool{Min: 2, Max: 2, Check: "echo 2"},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	reports := waitForPoolMemberReports(t, cityDir, "shared", []string{"shared-1", "shared-2"}, e2eDefaultTimeout())

	assertPerInstancePoolCWDs(t, cityDir, reports,
		map[string]string{"shared-1": "shared-1", "shared-2": "shared-2"})
}

// assertPerInstancePoolCWDs checks that each pool member (keyed by slot name,
// valued by its expected instance directory name) reports a CWD under the
// city's .gc/worktrees root whose last element is its own instance name, and
// that no two members share a CWD.
func assertPerInstancePoolCWDs(t *testing.T, cityDir string, reports map[string]*e2eReport, wantBase map[string]string) {
	t.Helper()
	root := filepath.Join(cityDir, ".gc", "worktrees")
	seen := map[string]string{}
	for slot, base := range wantBase {
		cwd := reports[slot].get("CWD")
		if cwd == "" {
			t.Errorf("%s reported no CWD", slot)
			continue
		}
		if filepath.Base(cwd) != base {
			t.Errorf("%s CWD = %q, want a per-instance directory named %q", slot, cwd, base)
		}
		if !strings.HasPrefix(normalizeE2EPath(t, cwd), normalizeE2EPath(t, root)+string(filepath.Separator)) {
			t.Errorf("%s CWD = %q, want it under %q", slot, cwd, root)
		}
		if other, dup := seen[cwd]; dup {
			t.Errorf("%s and %s share CWD %q; pool instances must not share a working directory", slot, other, cwd)
		}
		seen[cwd] = slot
	}
}

// TestE2E_Pool_EnvPerInstance verifies that each pool instance gets its own
// GC_AGENT env var with the correct instance name.
func TestE2E_Pool_EnvPerInstance(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "envpool",
				StartCommand: e2eReportScript(),
				Env:          map[string]string{"CUSTOM_SHARED": "yes"},
				Pool:         &e2ePool{Min: 2, Max: 2, Check: "echo 2"},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	reports := waitForPoolMemberReports(t, cityDir, "envpool", []string{"envpool-1", "envpool-2"}, e2eDefaultTimeout())
	r1, r2 := reports["envpool-1"], reports["envpool-2"]

	// Each instance still gets a UNIQUE GC_AGENT — it is now the member's own
	// session bead ID rather than its slot, which is the whole point: the identity
	// a pool member claims under has to be one that dies with it.
	a1, a2 := r1.get("GC_AGENT"), r2.get("GC_AGENT")
	if a1 == "" || a2 == "" {
		t.Errorf("pool members missing GC_AGENT: %q / %q", a1, a2)
	}
	if a1 == a2 {
		t.Errorf("pool members share GC_AGENT %q, want distinct per-session identities", a1)
	}
	if a1 == "envpool-1" || a2 == "envpool-2" {
		t.Errorf("pool members claim under their rebinding slots: %q / %q", a1, a2)
	}

	// Both share custom env.
	if !r1.has("CUSTOM_SHARED", "yes") {
		t.Error("envpool-1 missing CUSTOM_SHARED")
	}
	if !r2.has("CUSTOM_SHARED", "yes") {
		t.Error("envpool-2 missing CUSTOM_SHARED")
	}
}
