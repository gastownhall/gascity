//go:build integration

package integration

// End-to-end coverage for config drift on named sessions of a generic
// scope="rig" agent (the legacy reconciler, pinned via
// [daemon] session_reconciler = "legacy"): the session's template is the
// rig-stamped "<rig>/<name>" copy config.FindAgent synthesizes, which the
// configured-agents-only lookup misses, so the drift gates used to skip the
// session silently. Changing an agent env value moves the launch-half of the
// core fingerprint (OperatorEnv), so the awake sessions are relaunched with
// "agent relaunched (launch-only config change)" — that event per session is
// the restart evidence. `gc session wake` also resolves its will-not-start
// verdict through the configured-agents-only lookup, so waking the
// rig-stamped session exits 1 after recording the wake marker; the
// controller's own planning is rig-aware and serves it anyway.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestE2E_NamedSessionAgentEnvDrift_CityAndRigScope(t *testing.T) {
	cityDir, rigDirs := setupMultiRigCity(t, 1)

	// Both agents run the report script; each carries CUSTOM_DRIFT in its
	// agent env, which the core fingerprint covers as operator-authored
	// launch-tier identity (OperatorEnv).
	agentToml := func(name string, scopeRig bool, driftValue string) string {
		var b strings.Builder
		if scopeRig {
			b.WriteString("scope = \"rig\"\n")
		}
		b.WriteString("start_command = \"bash $GC_CITY/.gc/scripts/e2e-report.sh\"\n")
		b.WriteString("wake_mode = \"fresh\"\n")
		b.WriteString("idle_timeout = \"1h\"\n")
		fmt.Fprintf(&b, "\n[env]\nCUSTOM_DRIFT = %s\n", quote(driftValue))
		return b.String()
	}
	writeAgent := func(name string, scopeRig bool, driftValue string) {
		dir := filepath.Join(cityDir, "agents", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating agents/%s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "agent.toml"), []byte(agentToml(name, scopeRig, driftValue)), 0o644); err != nil {
			t.Fatalf("writing agents/%s/agent.toml: %v", name, err)
		}
	}
	// lead is a city agent; smith is a generic scope="rig" agent whose named
	// session (no dir) expands to rig-0/smith.
	writeAgent("lead", false, "v1")
	writeAgent("smith", true, "v1")
	copyE2EScripts(t, cityDir)

	cityName := filepath.Base(cityDir)
	var city strings.Builder
	fmt.Fprintf(&city, "[workspace]\nname = %s\n", quote(cityName))
	city.WriteString("\n[beads]\nprovider = \"file\"\n")
	city.WriteString("\n[daemon]\npatrol_interval = \"1s\"\nsession_reconciler = \"legacy\"\n")
	city.WriteString("\n[[rigs]]\nname = \"rig-0\"\n")
	city.WriteString("\n[[named_session]]\ntemplate = \"lead\"\nmode = \"on_demand\"\n")
	city.WriteString("\n[[named_session]]\ntemplate = \"smith\"\nscope = \"rig\"\nmode = \"on_demand\"\n")
	writeFileAtomic(t, filepath.Join(cityDir, "city.toml"), []byte(city.String()))

	var site strings.Builder
	fmt.Fprintf(&site, "workspace_name = %s\n", quote(cityName))
	fmt.Fprintf(&site, "\n[[rig]]\nname = \"rig-0\"\npath = %s\n", quote(rigDirs[0]))
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("creating .gc: %v", err)
	}
	writeFileAtomic(t, filepath.Join(cityDir, ".gc", "site.toml"), []byte(site.String()))

	if out, err := gc("", "restart", cityDir); err != nil {
		t.Fatalf("gc restart: %v\n%s", err, out)
	}
	waitForControllerReady(t, cityDir, 15*time.Second)

	for _, identity := range []string{"lead", "rig-0/smith"} {
		out, err := gc(cityDir, "session", "wake", identity)
		if err != nil && !strings.Contains(out, "wake recorded") {
			t.Fatalf("gc session wake %s: %v\n%s", identity, err, out)
		}
	}
	waitForReport(t, cityDir, "lead", e2eDefaultTimeout())
	waitForReport(t, cityDir, "rig-0/smith", e2eDefaultTimeout())

	// The drift edit: flip both agents' env value. The awake sessions must be
	// relaunched (launch-only config change) — the rig-scoped one included,
	// which the pre-fix reconciler skipped because no configured agent
	// matched the rig-stamped template. The relaunch may be deferred while
	// the session counts as recently active (report output), bounded at 30s.
	writeAgent("lead", false, "v2")
	writeAgent("smith", true, "v2")
	if out, err := gc(cityDir, "reload", "--timeout", "45s"); err != nil {
		t.Fatalf("gc reload: %v\n%s", err, out)
	}

	relaunched := map[string]bool{}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if !strings.Contains(line, "agent relaunched") {
					continue
				}
				var ev struct {
					Subject string `json:"subject"`
				}
				if json.Unmarshal([]byte(line), &ev) != nil {
					continue
				}
				if ev.Subject == "lead" || strings.Contains(ev.Subject, "smith") {
					relaunched[ev.Subject] = true
				}
			}
		}
		if len(relaunched) >= 2 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if len(relaunched) < 2 {
		sessions, _ := gc(cityDir, "session", "list", "--state", "all")
		events, _ := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
		t.Fatalf("no config-drift relaunch within 1m30s (relaunched: %v)\nsessions:\n%s\nevents:\n%s",
			relaunched, sessions, events)
	}
}
