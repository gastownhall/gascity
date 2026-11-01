//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
)

// This file proves, against real bd and the built gc, that a city-scoped
// agent's rendered discovery queries can find work that lives in a rig store
// (issue #1689's routed half): an open rig bead routed to the agent, an
// in_progress rig bead assigned to the agent's session identity, and that
// in_progress bead when gated claims sort ahead of it.

const (
	promptQueryWorkBeginMarker             = "__PROMPT_QUERY_WORK_BEGIN__"
	promptQueryWorkEndMarker               = "__PROMPT_QUERY_WORK_END__"
	promptQueryInProgressBeginMarker       = "__PROMPT_QUERY_ASSIGNED_IN_PROGRESS_BEGIN__"
	promptQueryInProgressEndMarker         = "__PROMPT_QUERY_ASSIGNED_IN_PROGRESS_END__"
	promptQueryFederationTemplateStartName = "watcher"
)

// setUpPromptQueryFederationCity creates a real-dolt city with one rig and a
// schema-2 (city-scoped) watcher agent whose prompt template prints the
// WorkQuery and AssignedInProgressQuery placeholders between sentinels.
func setUpPromptQueryFederationCity(t *testing.T) (cityDir, rigDir, qualifiedName string) {
	t.Helper()

	env := newIsolatedCommandEnv(t, true)
	cityName := uniqueCityName()
	cityDir = filepath.Join(t.TempDir(), cityName)
	registerIntegrationDoltSQLServerCleanup(t, cityDir)

	out, err := retryOnDoltDirtyTableMigrationRace(func() (string, error) {
		return runGCDoltWithEnv(env, "", "init",
			"--skip-provider-readiness",
			"--providers", "codex",
			"--default-provider", "codex",
			cityDir,
		)
	})
	if err != nil {
		t.Fatalf("gc init failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)                //nolint:errcheck // best-effort cleanup
		runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
	})

	rigName := "watcher-rig-alpha"
	rigDir = filepath.Join(t.TempDir(), rigName)
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("creating rig dir: %v", err)
	}
	out, err = retryOnDoltDirtyTableMigrationRace(func() (string, error) {
		return gcDolt(cityDir, "rig", "add", rigDir)
	})
	if err != nil {
		t.Fatalf("gc rig add failed: %v\noutput: %s", err, out)
	}

	// The sentinel template the scaffold copies into agents/watcher/.
	tmpl := fmt.Sprintf("%s\n{{ .WorkQuery }}\n%s\n%s\n{{ .AssignedInProgressQuery }}\n%s\n",
		promptQueryWorkBeginMarker, promptQueryWorkEndMarker,
		promptQueryInProgressBeginMarker, promptQueryInProgressEndMarker)
	tmplPath := filepath.Join(t.TempDir(), "watcher.template.md")
	if err := os.WriteFile(tmplPath, []byte(tmpl), 0o644); err != nil {
		t.Fatalf("writing template: %v", err)
	}
	out, err = gcDolt(cityDir, "agent", "add",
		"--name", promptQueryFederationTemplateStartName,
		"--prompt-template", tmplPath,
		"--json",
	)
	if err != nil {
		t.Fatalf("gc agent add failed: %v\noutput: %s", err, out)
	}
	m := regexp.MustCompile(`"qualified_name"\s*:\s*"([^"]+)"`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("gc agent add --json output has no qualified_name: %s", out)
	}

	// The scaffold registers the agent with no explicit scope, but the routed
	// discovery contract belongs to city-scoped agents: pin the binding.
	agentToml := filepath.Join(cityDir, "agents", promptQueryFederationTemplateStartName, "agent.toml")
	if err := os.WriteFile(agentToml, []byte("scope = \"city\"\n"), 0o644); err != nil {
		t.Fatalf("writing agent scope: %v", err)
	}
	return cityDir, rigDir, m[1]
}

// renderPromptQueryFederationQueries renders the watcher agent's prompt
// through the real `gc prime` and returns its WorkQuery and
// AssignedInProgressQuery.
func renderPromptQueryFederationQueries(t *testing.T, cityDir string) (workQuery, inProgressQuery string) {
	t.Helper()
	out, err := gcDolt(cityDir, "prime", "--strict", promptQueryFederationTemplateStartName)
	if err != nil {
		t.Fatalf("gc prime failed: %v\noutput: %s", err, out)
	}
	extract := func(begin, end string) string {
		i := strings.Index(out, begin)
		if i < 0 {
			t.Fatalf("rendered prompt lacks %s; prompt:\n%s", begin, out)
		}
		rest := out[i+len(begin):]
		j := strings.Index(rest, end)
		if j < 0 {
			t.Fatalf("rendered prompt lacks %s after %s; prompt:\n%s", end, begin, out)
		}
		return strings.TrimSpace(rest[:j])
	}
	workQuery = extract(promptQueryWorkBeginMarker, promptQueryWorkEndMarker)
	inProgressQuery = extract(promptQueryInProgressBeginMarker, promptQueryInProgressEndMarker)
	if workQuery == "" || inProgressQuery == "" {
		t.Fatalf("rendered prompt has an empty discovery query; work=%q in_progress=%q", workQuery, inProgressQuery)
	}
	return workQuery, inProgressQuery
}

// promptQueryNamedSessionEnv builds the environment a named session of the
// agent runs in: the city's managed Dolt endpoint, GC_CITY pointing at the
// city, and exactly one session identity (GC_SESSION_ORIGIN=named,
// GC_ALIAS=<qualified name>).
func promptQueryNamedSessionEnv(t *testing.T, cityDir, qualifiedName string) []string {
	t.Helper()
	env := commandEnvForDir(cityDir, true)
	env = filterEnvMany(env,
		"GC_SESSION_ID", "GC_SESSION_NAME", "GC_ALIAS", "GC_AGENT", "GC_SESSION_ORIGIN",
		"GC_CITY", "GC_CITY_PATH", "GC_CITY_RUNTIME_DIR",
		"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT",
	)
	env = append(env,
		"GC_SESSION_ORIGIN=named",
		"GC_ALIAS="+qualifiedName,
		"GC_CITY="+cityDir,
		"GC_CITY_PATH="+cityDir,
		"GC_CITY_RUNTIME_DIR="+filepath.Join(cityDir, ".gc", "runtime"),
	)
	if port, ok := ensureManagedDoltPortForTest(cityDir); ok {
		env = appendManagedDoltEndpointEnv(env, port)
	}
	return env
}

// runPromptQueryAsNamedSession runs a rendered discovery query the way the
// agent's own shell would: sh -c in the agent's work_dir, with the named
// session identity (GC_SESSION_ORIGIN=named, GC_ALIAS=<qualified name>) and
// GC_CITY pointing at the city, against the city's managed Dolt server.
func runPromptQueryAsNamedSession(t *testing.T, cityDir, qualifiedName, query string) string {
	t.Helper()

	workDir := filepath.Join(cityDir, ".gc", "agents", promptQueryFederationTemplateStartName)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("creating agent work dir: %v", err)
	}
	env := promptQueryNamedSessionEnv(t, cityDir, qualifiedName)
	out, err := runCommand(workDir, env, 2*time.Minute, "sh", "-c", query)
	if err != nil {
		t.Fatalf("discovery query failed: %v\noutput: %s", err, out)
	}
	return out
}

// promptQueryBeadID extracts the bead id a `bd create` printed, anchored on
// the store's issue prefix so unrelated output noise cannot win.
func promptQueryBeadID(t *testing.T, out, wantPrefix string) string {
	t.Helper()
	for _, field := range strings.Fields(out) {
		if strings.HasPrefix(field, wantPrefix+"-") {
			return field
		}
	}
	t.Fatalf("no bead id with prefix %q in output: %s", wantPrefix, out)
	return ""
}

func TestCityScopedAgentPromptQueriesFindRigWork(t *testing.T) {
	t.Run("case R: routed rig bead appears in WorkQuery", func(t *testing.T) {
		cityDir, rigDir, qualifiedName := setUpPromptQueryFederationCity(t)

		// The rig-store bead under test: open, unassigned, routed to the agent.
		rigID := promptQueryBeadID(t, mustCreateRigBead(t, cityDir, rigDir, "routed rig work for the watcher"), config.DeriveBeadsPrefix("watcher-rig-alpha"))
		out, err := bdDoltInRig(cityDir, rigDir, "update", rigID, "--set-metadata", "gc.routed_to="+qualifiedName)
		if err != nil {
			t.Fatalf("routing rig bead to %s failed: %v\noutput: %s", qualifiedName, err, out)
		}

		// Control: gc hook's cross-store fan-out already lists the routed rig
		// bead, so the bead is well-formed and routed (passes at base). The
		// hook needs the same named-session identity as the query, or its
		// routed tier has no self to match against.
		hookOut, err := runGCDoltWithEnv(promptQueryNamedSessionEnv(t, cityDir, qualifiedName), cityDir,
			"hook", qualifiedName, "--json")
		if err != nil {
			t.Fatalf("gc hook failed: %v\noutput: %s", err, hookOut)
		}
		if !strings.Contains(hookOut, rigID) {
			t.Errorf("control: gc hook does not list routed rig bead %s — bead malformed; output:\n%s", rigID, hookOut)
		}

		workQuery, _ := renderPromptQueryFederationQueries(t, cityDir)

		// Control: a city-store bead routed to the same agent is returned by
		// the rendered WorkQuery, proving the query runs (and would have
		// passed at base, so the case-R red is not vacuous).
		cityName := filepath.Base(cityDir)
		cityID := promptQueryBeadID(t, mustCreateCityBead(t, cityDir, "routed city work for the watcher"), config.DeriveBeadsPrefix(cityName))
		out, err = bdDolt(cityDir, "update", cityID, "--set-metadata", "gc.routed_to="+qualifiedName)
		if err != nil {
			t.Fatalf("routing city bead failed: %v\noutput: %s", err, out)
		}

		got := runPromptQueryAsNamedSession(t, cityDir, qualifiedName, workQuery)
		if !strings.Contains(got, cityID) {
			t.Errorf("control: WorkQuery does not list the routed city bead %s; output:\n%s", cityID, got)
		}
		if !strings.Contains(got, rigID) {
			t.Errorf("AC-2: WorkQuery of city-scoped agent %s does not list routed rig bead %s; output:\n%s", qualifiedName, rigID, got)
		}
	})

	t.Run("case I: in-progress rig bead appears in AssignedInProgressQuery", func(t *testing.T) {
		cityDir, rigDir, qualifiedName := setUpPromptQueryFederationCity(t)

		// The rig-store bead under test: in_progress, assigned to the agent's
		// session identity.
		rigID := promptQueryBeadID(t, mustCreateRigBead(t, cityDir, rigDir, "claimed rig work for the watcher"), config.DeriveBeadsPrefix("watcher-rig-alpha"))
		out, err := bdDoltInRig(cityDir, rigDir, "update", rigID,
			"--assignee", qualifiedName, "--status", "in_progress")
		if err != nil {
			t.Fatalf("claiming rig bead for %s failed: %v\noutput: %s", qualifiedName, err, out)
		}

		_, inProgressQuery := renderPromptQueryFederationQueries(t, cityDir)
		got := runPromptQueryAsNamedSession(t, cityDir, qualifiedName, inProgressQuery)
		if !strings.Contains(got, rigID) {
			t.Errorf("AC-3: AssignedInProgressQuery of city-scoped agent %s does not list its in-progress rig bead %s; output:\n%s", qualifiedName, rigID, got)
		}
	})

	t.Run("case G: gated in-progress claims do not hide a servable rig claim", func(t *testing.T) {
		cityDir, rigDir, qualifiedName := setUpPromptQueryFederationCity(t)
		rigPrefix := config.DeriveBeadsPrefix("watcher-rig-alpha")
		claim := func(id string, inRig bool) {
			t.Helper()
			args := []string{"update", id, "--assignee", qualifiedName, "--status", "in_progress"}
			var out string
			var err error
			if inRig {
				out, err = bdDoltInRig(cityDir, rigDir, args...)
			} else {
				out, err = bdDolt(cityDir, args...)
			}
			if err != nil {
				t.Fatalf("claiming %s for %s failed: %v\noutput: %s", id, qualifiedName, err, out)
			}
		}

		// Three in_progress claims held by the agent's session identity, in the
		// federated reader's (priority, created_at, id) order:
		//   - P0 in the CITY store, parked on a dispatch hold;
		//   - P1 in the rig store, blocked by an open rig gate;
		//   - P2 in the rig store, servable: the one claim the session can resume.
		// The two gated claims sort first, so a one-row read of the merged answer
		// sees only the held one and the servable claim is never read.
		cityName := filepath.Base(cityDir)
		heldID := promptQueryBeadID(t, mustCreateCityBead(t, cityDir, "held city claim", "--priority", "0", "--labels", beadmeta.HoldExternalLabel), config.DeriveBeadsPrefix(cityName))
		claim(heldID, false)
		gateID := promptQueryBeadID(t, mustCreateRigBead(t, cityDir, rigDir, "gate the blocked claim waits on"), rigPrefix)
		blockedID := promptQueryBeadID(t, mustCreateRigBead(t, cityDir, rigDir, "blocked rig claim", "--priority", "1"), rigPrefix)
		if out, err := bdDoltInRig(cityDir, rigDir, "dep", "add", blockedID, gateID); err != nil {
			t.Fatalf("blocking %s on %s failed: %v\noutput: %s", blockedID, gateID, err, out)
		}
		claim(blockedID, true)
		servableID := promptQueryBeadID(t, mustCreateRigBead(t, cityDir, rigDir, "servable rig claim", "--priority", "2"), rigPrefix)
		claim(servableID, true)

		workQuery, inProgressQuery := renderPromptQueryFederationQueries(t, cityDir)
		for _, q := range []struct{ name, query string }{
			{"AssignedInProgressQuery", inProgressQuery},
			{"WorkQuery", workQuery},
		} {
			got := runPromptQueryAsNamedSession(t, cityDir, qualifiedName, q.query)
			if !strings.Contains(got, servableID) {
				t.Errorf("%s of city-scoped agent %s does not list its servable in-progress rig bead %s behind the held %s and the blocked %s; output:\n%s", q.name, qualifiedName, servableID, heldID, blockedID, got)
			}
			for _, gated := range []string{heldID, blockedID} {
				if strings.Contains(got, gated) {
					t.Errorf("%s of city-scoped agent %s serves gated in-progress bead %s; output:\n%s", q.name, qualifiedName, gated, got)
				}
			}
		}
	})
}

func mustCreateRigBead(t *testing.T, cityDir, rigDir, title string, args ...string) string {
	t.Helper()
	out, err := bdDoltInRig(cityDir, rigDir, append([]string{"create", title}, args...)...)
	if err != nil {
		t.Fatalf("bd create in rig failed: %v\noutput: %s", err, out)
	}
	return out
}

func mustCreateCityBead(t *testing.T, cityDir, title string, args ...string) string {
	t.Helper()
	out, err := bdDolt(cityDir, append([]string{"create", title}, args...)...)
	if err != nil {
		t.Fatalf("bd create in city failed: %v\noutput: %s", err, out)
	}
	return out
}
