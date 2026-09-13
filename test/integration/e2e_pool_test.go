//go:build integration

package integration

import (
	"path/filepath"
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
	// holder dies, so each member owns work under its own session name and its
	// GC_AGENT is that name, not "worker-N".
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

	wantDir := filepath.Join(cityDir, "workdir")

	// Both instances share the same workdir (no template expansion).
	for _, slot := range []string{"workdir/dirpool-1", "workdir/dirpool-2"} {
		if cwd := reports[slot].get("CWD"); !sameE2EPath(t, cwd, wantDir) {
			t.Errorf("%s CWD = %q, want %q", slot, cwd, wantDir)
		}
	}
}

// TestE2E_Pool_SharedDir verifies that without a template dir, all pool
// instances share the same working directory.
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

	cwd1 := reports["shared-1"].get("CWD")
	cwd2 := reports["shared-2"].get("CWD")

	if cwd1 != cwd2 {
		t.Errorf("shared pool instances have different CWDs: %q vs %q", cwd1, cwd2)
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
	// session name rather than its slot, which is the whole point: the identity
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
