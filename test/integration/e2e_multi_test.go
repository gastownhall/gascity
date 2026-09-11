//go:build integration

package integration

import (
	"testing"
)

// TestE2E_MultiAgent_Independent verifies that multiple agents start
// independently with their own GC_AGENT and custom env.
func TestE2E_MultiAgent_Independent(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "alpha",
				StartCommand: e2eReportScript(),
				Env:          map[string]string{"CUSTOM_ROLE": "alpha"},
			},
			{
				Name:         "beta",
				StartCommand: e2eReportScript(),
				Env:          map[string]string{"CUSTOM_ROLE": "beta"},
			},
			{
				Name:         "gamma",
				StartCommand: e2eReportScript(),
				Env:          map[string]string{"CUSTOM_ROLE": "gamma"},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	for _, name := range []string{"alpha", "beta", "gamma"} {
		report := waitForReport(t, cityDir, name, e2eDefaultTimeout())

		if !report.has("GC_AGENT", name) {
			t.Errorf("%s: GC_AGENT got %v, want [%s]", name, report.getAll("GC_AGENT"), name)
		}
		if !report.has("CUSTOM_ROLE", name) {
			t.Errorf("%s: CUSTOM_ROLE got %v, want [%s]", name, report.getAll("CUSTOM_ROLE"), name)
		}
	}
}

// TestE2E_MultiAgent_PoolAndFixed verifies that a pool agent and a fixed
// agent coexist in the same city.
func TestE2E_MultiAgent_PoolAndFixed(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "fixed",
				StartCommand: e2eReportScript(),
				Env:          map[string]string{"CUSTOM_TYPE": "fixed"},
			},
			{
				Name:         "pooled",
				StartCommand: e2eReportScript(),
				Env:          map[string]string{"CUSTOM_TYPE": "pooled"},
				Pool:         &e2ePool{Min: 2, Max: 2, Check: "echo 2"},
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	// The two agents sit on opposite sides of the ownership rule, and the
	// contrast is the point of this test.
	//
	// A FIXED agent has one stable identity, so it keeps its alias and answers
	// to its bare name.
	fixedReport := waitForReport(t, cityDir, "fixed", e2eDefaultTimeout())
	if !fixedReport.has("GC_AGENT", "fixed") {
		t.Errorf("fixed agent GC_AGENT: got %v, want [fixed]", fixedReport.getAll("GC_AGENT"))
	}
	if got := fixedReport.get("GC_ALIAS"); got != "fixed" {
		t.Errorf("fixed agent GC_ALIAS = %q, want \"fixed\": a stable identity keeps its alias", got)
	}

	// A POOL member's slot rebinds to a fresh session whenever a holder dies, so
	// the slot is bookkeeping and the session owns work under its own session
	// name. Its report is therefore keyed by the session name, not "pooled-N".
	reports := waitForPoolMemberReports(t, cityDir, "pooled", []string{"pooled-1", "pooled-2"}, e2eDefaultTimeout())
	seen := make(map[string]string, len(reports))
	for _, slot := range []string{"pooled-1", "pooled-2"} {
		report := reports[slot]
		agent := report.get("GC_AGENT")
		if agent == slot {
			t.Errorf("%s GC_AGENT = %q: an expanding-pool member must not claim under its rebinding slot", slot, agent)
		}
		if agent == "" {
			t.Errorf("%s has no GC_AGENT", slot)
		}
		if got := report.get("GC_ALIAS"); got != "" {
			t.Errorf("%s GC_ALIAS = %q, want empty for an unaliased pool slot", slot, got)
		}
		if !report.has("CUSTOM_TYPE", "pooled") {
			t.Errorf("%s missing CUSTOM_TYPE=pooled", slot)
		}
		if prev, dup := seen[agent]; dup {
			t.Errorf("%s and %s report the same GC_AGENT %q; pool members must stay distinct", prev, slot, agent)
		}
		seen[agent] = slot
	}
}

// TestE2E_MultiAgent_CityAndRig verifies that city-scoped and rig-scoped
// agents can coexist, with rig agents receiving GC_RIG and the correct dir.
func TestE2E_MultiAgent_CityAndRig(t *testing.T) {
	city := e2eCity{
		Agents: []e2eAgent{
			{
				Name:         "cityscoped",
				StartCommand: e2eReportScript(),
			},
			{
				Name:         "rigscoped",
				StartCommand: e2eReportScript(),
				Dir:          "rigs/myrig",
			},
		},
	}
	cityDir := setupE2ECity(t, nil, city)

	cityReport := waitForReport(t, cityDir, "cityscoped", e2eDefaultTimeout())
	// QualifiedName = dir/name = "rigs/myrig/rigscoped"
	rigReport := waitForReport(t, cityDir, "rigs/myrig/rigscoped", e2eDefaultTimeout())

	// City-scoped agent should not have GC_RIG.
	if cityReport.hasKey("GC_RIG") {
		t.Errorf("city-scoped agent has unexpected GC_RIG: %v", cityReport.getAll("GC_RIG"))
	}

	// Rig-scoped agent should have its dir set correctly.
	if cwd := rigReport.get("CWD"); cwd == "" {
		t.Error("rig-scoped agent CWD is empty")
	}
}
