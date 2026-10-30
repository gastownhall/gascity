//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/stretchr/testify/require"
)

// namedDriftRestartBudget bounds how long a drifted named session may take to
// restart after a reload. Named sessions defer config drift while recent
// activity cannot be ruled out, so the budget covers that deferral window on
// top of the patrol cadence.
const namedDriftRestartBudget = 4 * time.Minute

// TestE2E_NamedSessionProviderEnvDrift_CityAndRigScope verifies that, on the
// legacy session reconciler, changing a provider env value in city.toml
// restarts every on-demand named session running on that provider, whether
// the named session is city-scoped or rig-scoped (a generic scope = "rig"
// city agent, stamped per rig). Both sessions run a stub agent that the
// provider's process_names match and use wake_mode = "fresh".
func TestE2E_NamedSessionProviderEnvDrift_CityAndRigScope(t *testing.T) {
	if usingSubprocess() {
		t.Skip("named-session drift restart is exercised on the tmux provider")
	}

	cityDir, rigDirs := setupMultiRigCity(t, 1)
	// Keep the scaffold's name: a reload refuses a workspace rename.
	cityName := filepath.Base(cityDir)
	copyE2EScripts(t, cityDir)
	writeNamedDriftCity(t, cityDir, cityName, rigDirs[0], "v1")

	out, err := gc("", "restart", cityDir)
	require.NoError(t, err, "gc restart: %s", out)
	waitForControllerReady(t, cityDir, 30*time.Second)

	// A pin materializes each on-demand session's canonical bead and wakes it.
	// Drop the pin once the session runs: a pinned named session is exempt
	// from config-drift restarts by design, and the canonical bead alone keeps
	// an on-demand session desired.
	sessions := []string{"lead", "rig-0/smith"}
	for _, name := range sessions {
		out, err := gc(cityDir, "session", "pin", name)
		require.NoError(t, err, "gc session pin %s: %s", name, out)
	}
	got := waitForNamedDriftReports(t, cityDir, sessions, e2eDefaultTimeout())
	for _, name := range sessions {
		if got[name] != "v1" {
			namedDriftFatal(t, cityDir, fmt.Sprintf("%s initial CUSTOM_DRIFT = %q, want v1 (all: %v)", name, got[name], got))
		}
	}
	for _, name := range sessions {
		out, err := gc(cityDir, "session", "unpin", name)
		require.NoError(t, err, "gc session unpin %s: %s", name, out)
	}

	require.NoError(t, os.RemoveAll(filepath.Join(cityDir, ".gc-reports")))
	writeNamedDriftCityToml(t, cityDir, cityName, "v2")
	out, err = gc(cityDir, "reload", "--timeout", "45s")
	require.NoError(t, err, "gc reload after provider env change: %s", out)

	// The reports were removed before the reload, so a report now means the
	// agent was started again. Only the restart is asserted: the value it came
	// back with depends on how the runtime relaunches a launch-only change.
	got = waitForNamedDriftReports(t, cityDir, sessions, namedDriftRestartBudget)
	var missed []string
	for _, name := range sessions {
		if _, ok := got[name]; !ok {
			missed = append(missed, name)
		}
	}
	if len(missed) > 0 {
		namedDriftFatal(t, cityDir, fmt.Sprintf("no config-drift restart within %s for: %s (restarted: %v)",
			namedDriftRestartBudget, strings.Join(missed, ", "), got))
	}
	t.Logf("restarted after the provider env change: %v", got)
}

// namedDriftFatal fails the test with msg and the city's sessions, beads and
// supervisor log.
func namedDriftFatal(t *testing.T, cityDir, msg string) {
	t.Helper()
	list, _ := gc(cityDir, "session", "list", "--state", "all")
	t.Fatalf("%s\nsessions:\n%s\nbeads:\n%s\nsupervisor log:\n%s",
		msg, list, namedDriftBeads(cityDir), namedDriftSupervisorLog(cityDir))
}

// waitForNamedDriftReports polls the city's complete e2e-report.sh reports
// until one exists for every session alias, or timeout passes, and returns the
// CUSTOM_DRIFT value each alias's report holds. A report is matched by the
// GC_ALIAS it records, not by its file name: the script names its file after
// GC_AGENT, which need not be the alias.
func waitForNamedDriftReports(t *testing.T, cityDir string, aliases []string, timeout time.Duration) map[string]string {
	t.Helper()
	got := make(map[string]string, len(aliases))
	pollUntil(timeout, time.Second, func() bool {
		paths, _ := filepath.Glob(filepath.Join(cityDir, ".gc-reports", "*.report"))
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if err != nil || !strings.Contains(string(data), "STATUS=complete") {
				continue
			}
			report := parseReport(t, data)
			if alias := report.get("GC_ALIAS"); alias != "" {
				got[alias] = report.get("CUSTOM_DRIFT")
			}
		}
		for _, alias := range aliases {
			if _, ok := got[alias]; !ok {
				return false
			}
		}
		return true
	})
	return got
}

// writeNamedDriftCity writes the city pack, its two agents and the rig
// binding, then city.toml with the given provider env value.
func writeNamedDriftCity(t *testing.T, cityDir, cityName, rigDir, value string) {
	t.Helper()

	pack := packTomlWithCoreImport(t, cityName) + `
[[named_session]]
template = "lead"
mode = "on_demand"

[[named_session]]
template = "smith"
scope = "rig"
mode = "on_demand"
`
	require.NoError(t, os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte(pack), 0o644))
	agents := map[string]string{
		"lead":  "provider = \"stub\"\nwake_mode = \"fresh\"\nidle_timeout = \"1h\"\nmax_active_sessions = 1\n",
		"smith": "scope = \"rig\"\nprovider = \"stub\"\nwake_mode = \"fresh\"\nidle_timeout = \"1h\"\nmax_active_sessions = 1\n",
	}
	for name, body := range agents {
		dir := filepath.Join(cityDir, "agents", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "agent.toml"), []byte(body), 0o644))
	}
	site := fmt.Sprintf("workspace_name = %s\n\n[[rig]]\nname = \"rig-0\"\npath = %s\n", quote(cityName), quote(rigDir))
	require.NoError(t, os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte(site), 0o644))
	writeNamedDriftCityToml(t, cityDir, cityName, value)
}

// writeNamedDriftCityToml writes city.toml, pinned to the legacy session
// reconciler, with the stub provider's CUSTOM_DRIFT env set to value. The stub
// runs e2e-report.sh under bash, which the provider's process_names match.
func writeNamedDriftCityToml(t *testing.T, cityDir, cityName, value string) {
	t.Helper()

	script := filepath.Join(cityDir, ".gc", "scripts", "e2e-report.sh")
	var b strings.Builder
	fmt.Fprintf(&b, "[workspace]\nname = %s\n", quote(cityName))
	b.WriteString("\n[beads]\nprovider = \"file\"\n")
	b.WriteString(e2eDaemonSection)
	b.WriteString("session_reconciler = \"legacy\"\n")
	fmt.Fprintf(&b, "\n[providers.stub]\ncommand = \"bash\"\nargs = [%s]\nprompt_mode = \"none\"\nprocess_names = [\"bash\"]\n", quote(script))
	fmt.Fprintf(&b, "\n[providers.stub.env]\nCUSTOM_DRIFT = %s\n", quote(value))
	b.WriteString("\n[[rigs]]\nname = \"rig-0\"\n")
	writeFileAtomic(t, filepath.Join(cityDir, "city.toml"), []byte(b.String()))
}

// namedDriftSupervisorLog returns the isolated supervisor log's drift lines
// and its tail, for a failure message.
func namedDriftSupervisorLog(cityDir string) string {
	env := parseEnvList(commandEnvForDir(cityDir, false))
	if env["GC_HOME"] == "" {
		return "(no GC_HOME)"
	}
	data, err := os.ReadFile(filepath.Join(env["GC_HOME"], "supervisor.log"))
	if err != nil {
		return fmt.Sprintf("(reading supervisor.log: %v)", err)
	}
	var drift []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, "drift") {
			drift = append(drift, line)
		}
	}
	return "drift lines:\n" + strings.Join(drift, "\n") + "\ntail:\n" + tailText(string(data), 80)
}

// namedDriftBeads returns each bead in the city's file store with its
// assignee and the metadata the config-drift gate reads, for a failure
// message.
func namedDriftBeads(cityDir string) string {
	store, recorder, err := openFileStoreBeads(cityDir)
	if err != nil {
		return fmt.Sprintf("(opening file store: %v)", err)
	}
	defer recorder.Close() //nolint:errcheck // best-effort diagnostic
	items, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		return fmt.Sprintf("(listing beads: %v)", err)
	}
	var b strings.Builder
	for _, item := range items {
		fmt.Fprintf(&b, "%s %s %q status=%s assignee=%q", item.ID, item.Type, item.Title, item.Status, item.Assignee)
		for _, key := range []string{"template", "session_name", "state", "configured_named_identity", "started_config_hash", "config_drift_deferred_at", "pin_awake"} {
			if v := item.Metadata[key]; v != "" {
				fmt.Fprintf(&b, " %s=%q", key, v)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}
