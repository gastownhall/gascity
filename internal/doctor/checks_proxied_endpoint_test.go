package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/config"
)

// proxiedScopeFixture writes the on-disk shape of one bd-owned proxied scope:
// bd's metadata binding, the sidecar, and — when pid is non-zero — a valid
// schema-2 proxy record naming that pid.
type proxiedScopeFixture struct {
	scopeRoot string
	root      string
	record    proxyendpoint.Record
}

func writeProxiedScope(t *testing.T, scopeRoot, sidecarBody string, pid, port int) proxiedScopeFixture {
	t.Helper()
	beadsDir := filepath.Join(scopeRoot, ".beads")
	root := filepath.Join(beadsDir, "dolt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"backend":"dolt","dolt_mode":"proxied-server","dolt_database":"beads"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	if sidecarBody != "" {
		if err := os.WriteFile(filepath.Join(beadsDir, proxyendpoint.SidecarFileName), []byte(sidecarBody), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fixture := proxiedScopeFixture{scopeRoot: scopeRoot, root: root}
	if pid == 0 {
		return fixture
	}
	rootID, err := proxyendpoint.RootID(root)
	if err != nil {
		t.Fatalf("RootID(%s): %v", root, err)
	}
	fixture.record = proxyendpoint.Record{
		PID:         pid,
		Port:        port,
		UpstreamID:  "upstream",
		Schema:      proxyendpoint.SchemaV2,
		Kind:        proxyendpoint.RecordKind,
		Birth:       proxyendpoint.BirthToken("boot-fixture", "44556677"),
		RootID:      rootID,
		ControlPort: port + 1,
	}
	body, err := json.Marshal(fixture.record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyendpoint.PIDPath(root), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// stubProxyProcess makes the recorded pid look like bd's live supervisor for
// this root, with the given --idle-timeout spelling.
func stubProxyProcess(t *testing.T, fixture proxiedScopeFixture, idleFlag string) {
	t.Helper()
	argv := []string{"/opt/beads/bd", proxyendpoint.ChildVerb, proxyendpoint.RootFlag, fixture.root}
	if idleFlag != "" {
		argv = append(argv, proxyendpoint.IdleTimeoutFlag, idleFlag)
	}
	stubProxyProcessTable(t, proxyendpoint.ProcessTable{
		Alive: func(pid int) bool { return pid == fixture.record.PID },
		Argv: func(pid int) ([]string, error) {
			if pid != fixture.record.PID {
				return nil, fmt.Errorf("no process %d", pid)
			}
			return argv, nil
		},
		Birth: func(pid int) (string, error) {
			if pid != fixture.record.PID {
				return "", fmt.Errorf("no process %d", pid)
			}
			return fixture.record.Birth, nil
		},
	})
}

func stubProxyProcessTable(t *testing.T, table proxyendpoint.ProcessTable) {
	t.Helper()
	previous := proxiedEndpointProcessTable
	proxiedEndpointProcessTable = func() proxyendpoint.ProcessTable { return table }
	t.Cleanup(func() { proxiedEndpointProcessTable = previous })
}

// stubProbe replaces the one operation that would touch bd's Dolt child.
func stubProbe(t *testing.T, result proxyendpoint.ProbeResult, calls *int) {
	t.Helper()
	previous := probeProxiedEndpoint
	probeProxiedEndpoint = func(context.Context, proxyendpoint.Endpoint, string) proxyendpoint.ProbeResult {
		if calls != nil {
			*calls++
		}
		return result
	}
	t.Cleanup(func() { probeProxiedEndpoint = previous })
}

// proxiedTarget is the connection target contract resolves for a proxied scope:
// a mode and a database, and deliberately no host or port, because bd owns the
// endpoint.
func proxiedTarget() contract.DoltConnectionTarget {
	return contract.DoltConnectionTarget{DoltMode: "proxied-server", Database: "beads"}
}

// TestBeadsStorePayloadOnAHealthyProxiedScope is PR1's acceptance shape: a
// proxied scope reports the endpoint record, evidence argv+birth, its idle
// policy and both cursors — while the store it opened is still BdStore, because
// this slice changes no routing at all.
func TestBeadsStorePayloadOnAHealthyProxiedScope(t *testing.T) {
	scope := t.TempDir()
	fixture := writeProxiedScope(t, scope, `{"root_path":"dolt","idle_timeout":-1}`, 6001, 45123)
	stubProxyProcess(t, fixture, "-1ns")
	probes := 0
	stubProbe(t, proxyendpoint.ProbeResult{
		Outcome: proxyendpoint.ProbeServed,
		Cursors: proxyendpoint.Cursors{Main: beads.SchemaCursorMain, Ignored: beads.SchemaCursorIgnored},
	}, &probes)

	payload := newBeadsStorePayload(scope, proxiedTarget(), beadsStoreDiagnostic{
		Store:         beads.BeadsStoreNameBdStore,
		PreflightGate: beads.BeadsGateProxiedProvider,
	})

	if payload.Store != beads.BeadsStoreNameBdStore {
		t.Errorf("store = %q, want %q — PR1 reports the endpoint, it does not route to it", payload.Store, beads.BeadsStoreNameBdStore)
	}
	ep := payload.Endpoint
	if ep == nil {
		t.Fatal("a bd-owned proxied scope reported no endpoint")
	}
	if ep.Verdict != "live" {
		t.Errorf("verdict = %q, want live (%s)", ep.Verdict, ep.Detail)
	}
	if ep.Evidence != "argv+birth" {
		t.Errorf("evidence = %q, want argv+birth", ep.Evidence)
	}
	if ep.BirthEvidence != "match" {
		t.Errorf("birth_evidence = %q, want match", ep.BirthEvidence)
	}
	if ep.IdlePolicy != "never" || ep.IdlePolicySource != "argv" {
		t.Errorf("idle policy = %s(%s), want never(argv)", ep.IdlePolicy, ep.IdlePolicySource)
	}
	if ep.IdleTimeout != "" {
		t.Errorf("idle_timeout = %q, want it omitted for a never policy", ep.IdleTimeout)
	}
	if ep.Host != proxyendpoint.Host || ep.Port != 45123 || ep.PID != 6001 || ep.ControlPort != 45124 {
		t.Errorf("endpoint = %s:%d pid=%d control=%d, want 127.0.0.1:45123 pid=6001 control=45124", ep.Host, ep.Port, ep.PID, ep.ControlPort)
	}
	if ep.Root != fixture.root {
		t.Errorf("root = %q, want %q", ep.Root, fixture.root)
	}
	if ep.Generation == "" || strings.Contains(ep.Generation, "boot-fixture") {
		t.Errorf("generation = %q, want a fingerprint that does not carry the boot id", ep.Generation)
	}
	if ep.Probe != "served" {
		t.Errorf("probe = %q, want served", ep.Probe)
	}
	if ep.Cursors == nil {
		t.Fatal("a served probe reported no cursors")
	}
	if *ep.Cursors != expectedCursors() {
		t.Errorf("cursors = %v, want the pinned %v", *ep.Cursors, expectedCursors())
	}
	if ep.ExpectedCursors != expectedCursors() {
		t.Errorf("expected_cursors = %v, want %v", ep.ExpectedCursors, expectedCursors())
	}
	if probes != 1 {
		t.Errorf("probes = %d, want exactly 1 — a probe costs bd's Dolt child a session", probes)
	}
}

// TestBeadsStorePayloadNeverProbesAnUnprovenEndpoint is the safety property of
// the whole payload: the port named by a record gc cannot vouch for belongs to
// whatever happens to be listening there, and gc must not speak to it.
func TestBeadsStorePayloadNeverProbesAnUnprovenEndpoint(t *testing.T) {
	cases := []struct {
		name        string
		pid         int
		table       func(t *testing.T, f proxiedScopeFixture)
		wantVerdict string
	}{
		{
			name: "no record at all",
			pid:  0,
			table: func(t *testing.T, _ proxiedScopeFixture) {
				stubProxyProcessTable(t, proxyendpoint.ProcessTable{})
			},
			wantVerdict: "no_record",
		},
		{
			name: "a dead pid",
			pid:  6002,
			table: func(t *testing.T, _ proxiedScopeFixture) {
				stubProxyProcessTable(t, proxyendpoint.ProcessTable{
					Alive: func(int) bool { return false },
					Argv:  func(int) ([]string, error) { return nil, nil },
					Birth: func(int) (string, error) { return "", nil },
				})
			},
			wantVerdict: "dead",
		},
		{
			name: "a recycled pid",
			pid:  6003,
			table: func(t *testing.T, _ proxiedScopeFixture) {
				stubProxyProcessTable(t, proxyendpoint.ProcessTable{
					Alive: func(int) bool { return true },
					Argv:  func(int) ([]string, error) { return []string{"/usr/bin/sleep", "infinity"}, nil },
					Birth: func(int) (string, error) { return "", nil },
				})
			},
			wantVerdict: "foreign_process",
		},
		{
			name: "a proxy bd restarted without gc seeing it",
			pid:  6004,
			table: func(t *testing.T, f proxiedScopeFixture) {
				stubProxyProcessTable(t, proxyendpoint.ProcessTable{
					Alive: func(int) bool { return true },
					Argv: func(int) ([]string, error) {
						return []string{"bd", proxyendpoint.ChildVerb, proxyendpoint.RootFlag, f.root}, nil
					},
					Birth: func(int) (string, error) { return proxyendpoint.BirthToken("boot-fixture", "999"), nil },
				})
			},
			wantVerdict: "birth_mismatch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := t.TempDir()
			fixture := writeProxiedScope(t, scope, `{"root_path":"dolt","idle_timeout":-1}`, tc.pid, 45200)
			tc.table(t, fixture)
			probes := 0
			stubProbe(t, proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeServed}, &probes)

			payload := newBeadsStorePayload(scope, proxiedTarget(), beadsStoreDiagnostic{Store: beads.BeadsStoreNameBdStore})
			ep := payload.Endpoint
			if ep == nil {
				t.Fatal("a proxied scope reported no endpoint")
			}
			if ep.Verdict != tc.wantVerdict {
				t.Fatalf("verdict = %q, want %q (%s)", ep.Verdict, tc.wantVerdict, ep.Detail)
			}
			if probes != 0 {
				t.Fatalf("gc probed a port it could not prove belongs to bd's proxy (%d probe(s))", probes)
			}
			if ep.Probe != "" {
				t.Fatalf("probe = %q, want it absent when no probe ran", ep.Probe)
			}
			if ep.Cursors != nil {
				t.Fatal("cursors reported without a served probe")
			}
			// The sidecar still answers when the process table cannot: the
			// policy a stopped proxy will come back with is exactly what an
			// operator wants to see next to "dead".
			if ep.IdlePolicy != "never" || ep.IdlePolicySource != "sidecar" {
				t.Errorf("idle policy = %s(%s), want never(sidecar)", ep.IdlePolicy, ep.IdlePolicySource)
			}
		})
	}
}

// TestBeadsStorePayloadReportsAnUnservedProbe pins the two states a live proxy
// can be in besides serving, and that both are reported rather than smoothed
// into "down".
func TestBeadsStorePayloadReportsAnUnservedProbe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome proxyendpoint.ProbeOutcome
		want    string
	}{
		{"a draining proxy refuses", proxyendpoint.ProbeRefused, "refused"},
		{"a proxy whose Dolt child is gone accepts and closes", proxyendpoint.ProbeAcceptedNoGreeting, "accepted_no_greeting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := t.TempDir()
			fixture := writeProxiedScope(t, scope, `{"root_path":"dolt","idle_timeout":-1}`, 6005, 45300)
			stubProxyProcess(t, fixture, "-1ns")
			stubProbe(t, proxyendpoint.ProbeResult{Outcome: tc.outcome, Err: io.EOF}, nil)

			payload := newBeadsStorePayload(scope, proxiedTarget(), beadsStoreDiagnostic{Store: beads.BeadsStoreNameBdStore})
			ep := payload.Endpoint
			if ep.Verdict != "live" {
				t.Fatalf("verdict = %q, want live — the record and the process are both fine here", ep.Verdict)
			}
			if ep.Probe != tc.want {
				t.Fatalf("probe = %q, want %q", ep.Probe, tc.want)
			}
			if ep.Cursors != nil {
				t.Fatal("cursors reported for an unserved probe")
			}
			if ep.Detail == "" {
				t.Error("an unserved probe reported no detail")
			}
		})
	}
}

// TestBeadsStorePayloadIdleSourcesCrossed pins the two-source resolution as the
// payload reports it: a live proxy's argv is the truth, and the sidecar answers
// only when no argv did.
func TestBeadsStorePayloadIdleSourcesCrossed(t *testing.T) {
	cases := []struct {
		name       string
		sidecar    string
		idleFlag   string
		wantPolicy string
		wantSource string
		wantWindow string
	}{
		{
			name:       "gc's own scope",
			sidecar:    `{"root_path":"dolt","idle_timeout":-1}`,
			idleFlag:   "-1ns",
			wantPolicy: "never",
			wantSource: "argv",
		},
		{
			name:       "an operator's scope with no flag at init",
			sidecar:    `{"root_path":"dolt"}`,
			idleFlag:   "30s",
			wantPolicy: "finite",
			wantSource: "argv",
			wantWindow: "30s",
		},
		{
			// The sidecar was edited under a live proxy. Only the argv
			// describes the process that is running.
			name:       "a sidecar that disagrees with the live proxy",
			sidecar:    `{"root_path":"dolt","idle_timeout":-1}`,
			idleFlag:   "45s",
			wantPolicy: "finite",
			wantSource: "argv",
			wantWindow: "45s",
		},
		{
			name:       "a proxy whose argv carries no flag",
			sidecar:    `{"root_path":"dolt"}`,
			idleFlag:   "",
			wantPolicy: "finite",
			wantSource: "bd-default",
			wantWindow: proxyendpoint.BdDefaultIdleTimeout.String(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scope := t.TempDir()
			fixture := writeProxiedScope(t, scope, tc.sidecar, 6006, 45400)
			stubProxyProcess(t, fixture, tc.idleFlag)
			stubProbe(t, proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeServed}, nil)

			ep := newBeadsStorePayload(scope, proxiedTarget(), beadsStoreDiagnostic{Store: beads.BeadsStoreNameBdStore}).Endpoint
			if ep.IdlePolicy != tc.wantPolicy || ep.IdlePolicySource != tc.wantSource {
				t.Fatalf("idle policy = %s(%s), want %s(%s)", ep.IdlePolicy, ep.IdlePolicySource, tc.wantPolicy, tc.wantSource)
			}
			if ep.IdleTimeout != tc.wantWindow {
				t.Fatalf("idle_timeout = %q, want %q", ep.IdleTimeout, tc.wantWindow)
			}
		})
	}
}

// TestBeadsStorePayloadSkipsNonProxiedScopes pins that nothing here runs for a
// direct scope: no endpoint block, and above all no process-table read or dial
// for a topology that has no bd proxy at all.
func TestBeadsStorePayloadSkipsNonProxiedScopes(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	metadata := `{"backend":"dolt","dolt_mode":"server","dolt_database":"beads"}`
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o600); err != nil {
		t.Fatal(err)
	}
	consulted := false
	stubProxyProcessTable(t, proxyendpoint.ProcessTable{
		Alive: func(int) bool { consulted = true; return false },
	})
	probes := 0
	stubProbe(t, proxyendpoint.ProbeResult{Outcome: proxyendpoint.ProbeServed}, &probes)

	payload := newBeadsStorePayload(scope, contract.DoltConnectionTarget{DoltMode: "server", Database: "beads"}, beadsStoreDiagnostic{
		Store: "NativeDoltStore",
	})
	if payload.Endpoint != nil {
		t.Fatalf("a direct scope reported a proxy endpoint: %+v", payload.Endpoint)
	}
	if consulted || probes != 0 {
		t.Fatal("a direct scope reached the process table or the probe")
	}
	if payload.Store != "NativeDoltStore" {
		t.Errorf("store = %q, want NativeDoltStore", payload.Store)
	}
}

// TestBeadsStorePayloadMarshalsTheShapeAutomationReads pins the JSON keys. They
// are a wire format the acceptance matrix and the perf gate read, so a rename
// has to fail here rather than in whatever consumes it next.
func TestBeadsStorePayloadMarshalsTheShapeAutomationReads(t *testing.T) {
	scope := t.TempDir()
	fixture := writeProxiedScope(t, scope, `{"root_path":"dolt","idle_timeout":-1}`, 6007, 45500)
	stubProxyProcess(t, fixture, "-1ns")
	stubProbe(t, proxyendpoint.ProbeResult{
		Outcome: proxyendpoint.ProbeServed,
		Cursors: proxyendpoint.Cursors{Main: 66, Ignored: 26},
	}, nil)

	payload := newBeadsStorePayload(scope, proxiedTarget(), beadsStoreDiagnostic{
		Store:         beads.BeadsStoreNameBdStore,
		PreflightGate: beads.BeadsGateProxiedProvider,
	})
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	for _, key := range []string{"store", "preflight_gate", "endpoint"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("payload has no %q key: %s", key, encoded)
		}
	}
	endpoint, ok := decoded["endpoint"].(map[string]any)
	if !ok {
		t.Fatalf("endpoint is not an object: %s", encoded)
	}
	for _, key := range []string{
		"root", "host", "port", "pid", "generation", "verdict", "evidence",
		"birth_evidence", "idle_policy", "idle_policy_source", "probe",
		"cursors", "expected_cursors",
	} {
		if _, ok := endpoint[key]; !ok {
			t.Errorf("endpoint has no %q key: %s", key, encoded)
		}
	}
	cursors, ok := endpoint["cursors"].(map[string]any)
	if !ok {
		t.Fatalf("cursors is not an object: %s", encoded)
	}
	if cursors["main"] != float64(66) || cursors["ignored"] != float64(26) {
		t.Errorf("cursors = %v, want main=66 ignored=26", cursors)
	}
	if strings.Contains(string(encoded), "boot-fixture") {
		t.Errorf("the payload carries the host's boot id: %s", encoded)
	}
}

// --- proxied-idle-timeout ---

// writeScopeOwnership records scopeRoot as gc-owned and settled in the city's
// journal.
func writeScopeOwnership(t *testing.T, cityPath string, scopes map[string]string) {
	t.Helper()
	writeScopeOwnershipInState(t, cityPath, scopes, "ready")
}

// writeScopeOwnershipInState records gc ownership of every scope in one chosen
// journal state, so a test can drive the pending-initialisation lens.
func writeScopeOwnershipInState(t *testing.T, cityPath string, scopes map[string]string, state string) {
	t.Helper()
	rows := make([]scopeOwnershipRow, 0, len(scopes))
	for key, path := range scopes {
		rows = append(rows, scopeOwnershipRow{key: key, path: path, state: state})
	}
	writeScopeOwnershipRows(t, cityPath, rows...)
}

// scopeOwnershipRow is one journal entry: the scope's key, its path, and the
// state gc last recorded for it.
type scopeOwnershipRow struct {
	key   string
	path  string
	state string
}

// writeScopeOwnershipRows records gc ownership of several scopes in states that
// may differ, which is the shape the pending-initialisation lens is for: one
// scope settled, another still being initialized.
func writeScopeOwnershipRows(t *testing.T, cityPath string, rows ...scopeOwnershipRow) {
	t.Helper()
	gcDir := filepath.Join(cityPath, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	entries := make([]string, 0, len(rows))
	for _, row := range rows {
		entries = append(entries, fmt.Sprintf(`%q:{"scope_path":%q,"lifecycle_owner":"provider","state":%q}`, row.key, row.path, row.state))
	}
	body := fmt.Sprintf(`{"version":1,"scopes":{%s}}`, strings.Join(entries, ","))
	if err := os.WriteFile(filepath.Join(gcDir, "scope-ownership.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProxiedIdleTimeoutCheck(t *testing.T) {
	cases := []struct {
		name       string
		sidecar    string
		wantStatus CheckStatus
		wantIn     string
	}{
		{
			// What gc's own `bd init` produces: the script passes
			// --proxied-server-idle-timeout 0, which bd maps to
			// IdleTimeoutNever before persisting.
			name:       "gc's own scope pins the proxy resident",
			sidecar:    `{"root_path":"dolt","idle_timeout":-1}`,
			wantStatus: StatusOK,
			wantIn:     "pin their proxy resident",
		},
		{
			// bd elides a zero, so this is what a scope initialized without
			// the flag looks like — and bd's provider substitutes 30s for it.
			name:       "an absent key is bd's 30s default",
			sidecar:    `{"root_path":"dolt"}`,
			wantStatus: StatusWarning,
			wantIn:     "finite(30s, bd-default)",
		},
		{
			name:       "an explicit window is still a window",
			sidecar:    `{"root_path":"dolt","idle_timeout":300000000000}`,
			wantStatus: StatusWarning,
			wantIn:     "finite(5m0s, sidecar)",
		},
		{
			name:       "no sidecar at all",
			sidecar:    "",
			wantStatus: StatusWarning,
			wantIn:     "bd-default",
		},
		{
			name:       "an unreadable sidecar is reported, not assumed",
			sidecar:    `{"root_path":`,
			wantStatus: StatusWarning,
			wantIn:     "could not read",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			writeProxiedScope(t, city, tc.sidecar, 0, 0)
			writeScopeOwnership(t, city, map[string]string{"city": city})

			check := NewProxiedIdleTimeoutCheckForConfig(city, &config.City{}, nil)
			if check == nil {
				t.Fatal("the check was not registered for a city with a gc-owned proxied scope")
			}
			got := check.Run(&CheckContext{CityPath: city})
			if got.Status != tc.wantStatus {
				t.Fatalf("status = %v, want %v: %s", got.Status, tc.wantStatus, got.Message)
			}
			if !strings.Contains(got.Message, tc.wantIn) && !containsAny(got.Details, tc.wantIn) {
				t.Fatalf("message %q / details %v do not mention %q", got.Message, got.Details, tc.wantIn)
			}
			if got.Severity != SeverityAdvisory {
				t.Errorf("severity = %v, want advisory — the scope works, it is only slower than gc's topology intends", got.Severity)
			}
			if got.Status != StatusOK && got.FixHint == "" {
				t.Error("a warning with no fix hint")
			}
			if check.CanFix() {
				t.Error("the check offers to fix a file bd owns")
			}
		})
	}
}

// TestProxiedIdleTimeoutCheckUsesThePendingInitLens pins that a scope the
// journal records as still initializing is not reported as a misconfigured one.
//
// The crashed-init state is the one that persists: bd writes metadata.json and
// the sidecar in one batch, so what is left behind is a proxied scope with no
// sidecar — indistinguishable here from an operator who initialized without the
// flag, and reported with a hint to re-initialize a scope that is already
// mid-initialization.
func TestProxiedIdleTimeoutCheckUsesThePendingInitLens(t *testing.T) {
	t.Run("a scope mid-initialisation is not an offender", func(t *testing.T) {
		city := t.TempDir()
		// No sidecar at all: the shape a crashed `bd init` leaves behind.
		writeProxiedScope(t, city, "", 0, 0)
		writeScopeOwnershipInState(t, city, map[string]string{"city": city}, "provider_initializing")

		check := NewProxiedIdleTimeoutCheckForConfig(city, &config.City{}, nil)
		if check == nil {
			t.Fatal("the check was not registered for a journaled proxied scope")
		}
		got := check.Run(&CheckContext{CityPath: city})
		if got.Message != pendingScopeInitMessage {
			t.Fatalf("message = %q, want the pending-initialisation message %q", got.Message, pendingScopeInitMessage)
		}
		if strings.Contains(got.Message, "do not pin") || containsAny(got.Details, "bd-default") {
			t.Fatalf("a scope mid-initialisation was reported as a sidecar misconfiguration: %q %v", got.Message, got.Details)
		}
		if got.Severity != SeverityAdvisory {
			t.Errorf("severity = %v, want advisory", got.Severity)
		}
	})

	t.Run("a settled offender is still reported beside a pending scope", func(t *testing.T) {
		city := t.TempDir()
		rig := filepath.Join(city, "rigs", "alpha")
		writeProxiedScope(t, city, `{"root_path":"dolt"}`, 0, 0)
		writeProxiedScope(t, rig, "", 0, 0)
		// BOTH scopes have to be in the config and in the journal or the check
		// never covers the rig: managedDoltScopeRootsFromConfig enumerates
		// cfg.Rigs, and the constructor keeps only journaled scopes. The rig is
		// journaled as still initializing, the city as settled and offending.
		writeScopeOwnershipRows(t, city,
			scopeOwnershipRow{key: "city", path: city, state: "ready"},
			scopeOwnershipRow{key: "rigs/alpha", path: rig, state: "provider_initializing"},
		)
		cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: rig}}}

		check := NewProxiedIdleTimeoutCheckForConfig(city, cfg, nil)
		if check == nil {
			t.Fatal("the check was not registered for a journaled proxied scope")
		}
		if len(check.scopeRoots) != 2 {
			t.Fatalf("the check covers %d scope(s) (%v), want the city and the rig: a pending scope that is not covered cannot be reported beside anything", len(check.scopeRoots), check.scopeRoots)
		}
		got := check.Run(&CheckContext{CityPath: city})
		if got.Status != StatusWarning || !strings.Contains(got.Message, "do not pin their proxy resident") {
			t.Fatalf("status/message = %v / %q, want the settled scope reported", got.Status, got.Message)
		}
		// The offender is counted alone: a scope mid-initialisation is not one.
		if !strings.Contains(got.Message, "1 gc-owned proxied scope(s) do not pin") {
			t.Errorf("message %q counts the pending scope as an offender", got.Message)
		}
		// And the pending scope is still SAID, in a detail a reader can tell
		// apart from an offender's `label (policy)` line.
		if !containsAny(got.Details, pendingScopeDetailSuffix) {
			t.Fatalf("details %v do not name the pending scope as pending", got.Details)
		}
		if !containsAny(got.Details, "rigs/alpha") {
			t.Fatalf("details %v do not mention the pending rig at all", got.Details)
		}
	})
}

// TestProxiedIdleTimeoutCheckRegistration pins that the check appears exactly
// for the cities it has an opinion about. A line telling an operator their own
// proxied workspace is misconfigured would be gc asserting a preference as a
// defect.
func TestProxiedIdleTimeoutCheckRegistration(t *testing.T) {
	t.Run("a direct city has no such scope", func(t *testing.T) {
		city := t.TempDir()
		beadsDir := filepath.Join(city, ".beads")
		if err := os.MkdirAll(beadsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		metadata := `{"backend":"dolt","dolt_mode":"server"}`
		if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(metadata), 0o600); err != nil {
			t.Fatal(err)
		}
		writeScopeOwnership(t, city, map[string]string{"city": city})
		if c := NewProxiedIdleTimeoutCheckForConfig(city, &config.City{}, nil); c != nil {
			t.Fatal("the check registered on a direct city")
		}
	})

	t.Run("a proxied workspace gc does not own", func(t *testing.T) {
		city := t.TempDir()
		writeProxiedScope(t, city, `{"root_path":"dolt"}`, 0, 0)
		if c := NewProxiedIdleTimeoutCheckForConfig(city, &config.City{}, nil); c != nil {
			t.Fatal("the check registered on a proxied workspace with no gc ownership journal")
		}
	})
}

// containsAny reports whether any detail line contains needle.
func containsAny(details []string, needle string) bool {
	for _, detail := range details {
		if strings.Contains(detail, needle) {
			return true
		}
	}
	return false
}
