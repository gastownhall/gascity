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

	r1 := waitForReport(t, cityDir, "worker-1", e2eDefaultTimeout())
	r2 := waitForReport(t, cityDir, "worker-2", e2eDefaultTimeout())

	if !r1.has("GC_AGENT", "worker-1") {
		t.Errorf("worker-1 GC_AGENT: got %v, want [worker-1]", r1.getAll("GC_AGENT"))
	}
	if !r2.has("GC_AGENT", "worker-2") {
		t.Errorf("worker-2 GC_AGENT: got %v, want [worker-2]", r2.getAll("GC_AGENT"))
	}
}

// TestE2E_Pool_MaxOneStillUsesPoolIdentity verifies that max=1 pool configs
// still use concrete pool instance naming rather than collapsing into a
// singleton template identity.
func TestE2E_Pool_MaxOneStillUsesPoolIdentity(t *testing.T) {
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

	report := waitForReport(t, cityDir, "singleton-1", e2eDefaultTimeout())
	if !report.has("GC_AGENT", "singleton-1") {
		t.Errorf("singleton-1 GC_AGENT: got %v, want [singleton-1]", report.getAll("GC_AGENT"))
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

	// Pool instances with dir: qualified names include dir prefix.
	r1 := waitForReport(t, cityDir, "workdir/dirpool-1", e2eDefaultTimeout())
	r2 := waitForReport(t, cityDir, "workdir/dirpool-2", e2eDefaultTimeout())

	wantDir := filepath.Join(cityDir, "workdir")

	// Both instances share the same workdir (no template expansion).
	if cwd := r1.get("CWD"); cwd != wantDir {
		t.Errorf("dirpool-1 CWD = %q, want %q", cwd, wantDir)
	}
	if cwd := r2.get("CWD"); cwd != wantDir {
		t.Errorf("dirpool-2 CWD = %q, want %q", cwd, wantDir)
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

	r1 := waitForReport(t, cityDir, "shared-1", e2eDefaultTimeout())
	r2 := waitForReport(t, cityDir, "shared-2", e2eDefaultTimeout())

	cwd1 := r1.get("CWD")
	cwd2 := r2.get("CWD")

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

	r1 := waitForReport(t, cityDir, "envpool-1", e2eDefaultTimeout())
	r2 := waitForReport(t, cityDir, "envpool-2", e2eDefaultTimeout())

	// Each instance gets unique GC_AGENT.
	if !r1.has("GC_AGENT", "envpool-1") {
		t.Errorf("envpool-1 GC_AGENT: got %v", r1.getAll("GC_AGENT"))
	}
	if !r2.has("GC_AGENT", "envpool-2") {
		t.Errorf("envpool-2 GC_AGENT: got %v", r2.getAll("GC_AGENT"))
	}

	// Both share custom env.
	if !r1.has("CUSTOM_SHARED", "yes") {
		t.Error("envpool-1 missing CUSTOM_SHARED")
	}
	if !r2.has("CUSTOM_SHARED", "yes") {
		t.Error("envpool-2 missing CUSTOM_SHARED")
	}
}
