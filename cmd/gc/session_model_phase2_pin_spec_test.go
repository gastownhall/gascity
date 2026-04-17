package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// Phase 2 spec coverage from engdocs/design/session-model-unification.md:
// - explicit pin/unpin command surface
// - pin_awake as a durable wake reason
// - unpin removes only the durable pin reason and does not force stop

func TestPhase2CmdSessionPin_MaterializesNamedSessionAndSetsPinAwake(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_DIR", t.TempDir())

	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1

[[named_session]]
template = "worker"
mode = "on_demand"
`)
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdSessionPin([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionPin(worker) = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	b := onlySessionBead(t, cityDir)
	if got := b.Metadata["configured_named_identity"]; got != "worker" {
		t.Fatalf("configured_named_identity = %q, want worker", got)
	}
	if got := b.Metadata["pin_awake"]; got != "true" {
		t.Fatalf("pin_awake = %q, want true", got)
	}
	if got := b.Metadata["pending_create_claim"]; got != "" {
		t.Fatalf("pending_create_claim = %q, want no one-shot claim because pin_awake is the wake cause", got)
	}
}

func TestPhase2CmdSessionPin_ControllerMaterializesWithPinAsOnlyWakeCause(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_DIR", t.TempDir())

	cityDir := shortSocketTempDir(t, "gc-session-pin-")
	writePhase2PinCity(t, cityDir, true)
	t.Setenv("GC_CITY", cityDir)
	startPhase2PinControllerSocket(t, cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdSessionPin([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionPin(worker) = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	b := onlySessionBead(t, cityDir)
	if got := b.Metadata["pin_awake"]; got != "true" {
		t.Fatalf("pin_awake = %q, want true", got)
	}
	if got := b.Metadata["pending_create_claim"]; got != "" {
		t.Fatalf("pending_create_claim = %q, want no one-shot claim because pin_awake is the wake cause", got)
	}
}

func TestPhase2CmdSessionPin_DoesNotClearSuspendHold(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_DIR", t.TempDir())

	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	t.Setenv("GC_CITY", cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	heldUntil := "9999-12-31T23:59:59Z"
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":   "test-city--worker",
			"alias":          "worker",
			"template":       "worker",
			"state":          "suspended",
			"held_until":     heldUntil,
			"sleep_intent":   "user-hold",
			"session_origin": "manual",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionPin([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionPin(worker) = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(b.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", b.ID, err)
	}
	if got.Metadata["pin_awake"] != "true" {
		t.Fatalf("pin_awake = %q, want true", got.Metadata["pin_awake"])
	}
	if got.Metadata["held_until"] != heldUntil {
		t.Fatalf("held_until = %q, want preserved %q", got.Metadata["held_until"], heldUntil)
	}
	if got.Metadata["sleep_intent"] != "user-hold" {
		t.Fatalf("sleep_intent = %q, want user-hold", got.Metadata["sleep_intent"])
	}
}

func TestPhase2CmdSessionUnpin_ClearsOnlyPinAwake(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_DIR", t.TempDir())

	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	t.Setenv("GC_CITY", cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	b, err := store.Create(beads.Bead{
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":         "test-city--worker",
			"alias":                "worker",
			"template":             "worker",
			"state":                "active",
			"pin_awake":            "true",
			"pending_create_claim": "true",
		},
	})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdSessionUnpin([]string{"worker"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSessionUnpin(worker) = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	got, err := reopened.Get(b.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", b.ID, err)
	}
	if got.Metadata["pin_awake"] != "" {
		t.Fatalf("pin_awake = %q, want cleared", got.Metadata["pin_awake"])
	}
	if got.Metadata["pending_create_claim"] != "true" {
		t.Fatalf("pending_create_claim = %q, want preserved", got.Metadata["pending_create_claim"])
	}
	if got.Metadata["state"] != "active" {
		t.Fatalf("state = %q, want active", got.Metadata["state"])
	}
}

func TestPhase2CmdSessionUnpin_CancelsPinOnlyMaterializationStartClaim(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_DIR", t.TempDir())

	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1

[[named_session]]
template = "worker"
mode = "on_demand"
`)
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	if code := cmdSessionPin([]string{"worker"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionPin(worker) = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionUnpin([]string{"worker"}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionUnpin(worker) = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	b := onlySessionBead(t, cityDir)
	if got := b.Metadata["pin_awake"]; got != "" {
		t.Fatalf("pin_awake = %q, want cleared", got)
	}
	if got := b.Metadata["pending_create_claim"]; got != "" {
		t.Fatalf("pending_create_claim = %q, want canceled with pin-only materialization", got)
	}
}

func TestPhase2ComputeAwakeSet_PinnedSessionWakesAndSuppressesIdleSleep(t *testing.T) {
	result := ComputeAwakeSet(AwakeInput{
		Agents: []AwakeAgent{{QualifiedName: "worker", SleepAfterIdle: time.Minute}},
		SessionBeads: []AwakeSessionBead{{
			ID:          "mc-1",
			SessionName: "test-city--worker",
			Template:    "worker",
			State:       "active",
			Pinned:      true,
			IdleSince:   now.Add(-10 * time.Minute),
		}},
		Now: now,
	})

	assertAwake(t, result, "test-city--worker")
	assertReason(t, result, "test-city--worker", "pin")
}

func TestPhase2ComputeAwakeSet_PinRespectsHardBlockers(t *testing.T) {
	tests := []struct {
		name   string
		agents []AwakeAgent
		bead   AwakeSessionBead
	}{
		{
			name:   "agent_suspended",
			agents: []AwakeAgent{{QualifiedName: "worker", Suspended: true}},
			bead: AwakeSessionBead{
				State: "asleep",
			},
		},
		{
			name:   "config_missing",
			agents: nil,
			bead: AwakeSessionBead{
				State: "asleep",
			},
		},
		{
			name:   "per_session_suspended_state",
			agents: []AwakeAgent{{QualifiedName: "worker"}},
			bead: AwakeSessionBead{
				State: "suspended",
			},
		},
		{
			name:   "wait_hold",
			agents: []AwakeAgent{{QualifiedName: "worker"}},
			bead: AwakeSessionBead{
				State:    "asleep",
				WaitHold: true,
			},
		},
		{
			name:   "dependency_only",
			agents: []AwakeAgent{{QualifiedName: "worker"}},
			bead: AwakeSessionBead{
				State:          "asleep",
				DependencyOnly: true,
			},
		},
		{
			name:   "held_until",
			agents: []AwakeAgent{{QualifiedName: "worker"}},
			bead: AwakeSessionBead{
				State:     "asleep",
				HeldUntil: now.Add(time.Hour),
			},
		},
		{
			name:   "quarantine",
			agents: []AwakeAgent{{QualifiedName: "worker"}},
			bead: AwakeSessionBead{
				State:            "asleep",
				QuarantinedUntil: now.Add(time.Hour),
			},
		},
		{
			name:   "drained",
			agents: []AwakeAgent{{QualifiedName: "worker"}},
			bead: AwakeSessionBead{
				State:   "asleep",
				Drained: true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bead := tt.bead
			bead.ID = "mc-1"
			bead.SessionName = "test-city--worker"
			bead.Template = "worker"
			bead.Pinned = true

			result := ComputeAwakeSet(AwakeInput{
				Agents:       tt.agents,
				SessionBeads: []AwakeSessionBead{bead},
				Now:          now,
			})

			assertAsleep(t, result, "test-city--worker")
		})
	}
}

func TestPhase2ReconcileSessionBeads_PinWakesThroughSessionSleepSuppression(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{
			InteractiveResume: "60s",
		},
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "worker",
			StartCommand:      "true",
			MaxActiveSessions: intPtr(2),
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Mode:     "on_demand",
		}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	session := env.createSessionBead(sessionName, "worker")
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"pin_awake":                  "true",
	})
	policy := resolveSessionSleepPolicy(session, env.cfg, env.sp)
	if !policy.enabled() {
		t.Fatalf("test policy should be enabled: %+v", policy)
	}
	env.setSessionMetadata(&session, map[string]string{
		"state":                    "asleep",
		"sleep_reason":             "idle",
		"sleep_policy_fingerprint": policy.Fingerprint,
	})

	woken := env.reconcile([]beads.Bead{session})
	if woken != 1 {
		t.Fatalf("woken = %d, want pinned idle-slept session to wake", woken)
	}
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q is not running after pin wake", sessionName)
	}
}

func TestPhase2SessionListReason_ShowsWakeEligiblePin(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "true",
		}},
	}
	bead := beads.Bead{
		ID:     "mc-1",
		Status: "open",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "test-city--worker",
			"template":     "worker",
			"state":        "asleep",
			"pin_awake":    "true",
		},
	}
	info := session.Info{
		ID:          bead.ID,
		Template:    "worker",
		State:       session.StateAsleep,
		SessionName: "test-city--worker",
	}

	reason := sessionReason(info, map[string]beads.Bead{bead.ID: bead}, cfg, nil, nil, nil)
	if reason != string(WakePin) {
		t.Fatalf("sessionReason = %q, want %q", reason, WakePin)
	}
}

func TestPhase2SessionListReason_PinnedHoldStillShowsBlocker(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:         "worker",
			StartCommand: "true",
		}},
	}
	bead := beads.Bead{
		ID:     "mc-1",
		Status: "open",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "test-city--worker",
			"template":     "worker",
			"state":        "suspended",
			"held_until":   "9999-12-31T23:59:59Z",
			"sleep_intent": "user-hold",
			"sleep_reason": "user-hold",
			"pin_awake":    "true",
		},
	}
	info := session.Info{
		ID:          bead.ID,
		Template:    "worker",
		State:       session.StateSuspended,
		SessionName: "test-city--worker",
	}

	reason := sessionReason(info, map[string]beads.Bead{bead.ID: bead}, cfg, nil, nil, nil)
	if reason != "user-hold" {
		t.Fatalf("sessionReason = %q, want user-hold", reason)
	}
}

func TestPhase2SessionCmdRegistersPinSubcommands(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newSessionCmd(&stdout, &stderr)
	if _, _, err := cmd.Find([]string{"pin"}); err != nil {
		t.Fatalf("session pin command not registered: %v", err)
	}
	if _, _, err := cmd.Find([]string{"unpin"}); err != nil {
		t.Fatalf("session unpin command not registered: %v", err)
	}
}

func TestPhase2CmdSessionPin_RejectsTemplateFactoryTarget(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_DIR", t.TempDir())

	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	t.Setenv("GC_CITY", cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdSessionPin([]string{"template:worker"}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("cmdSessionPin(template:worker) = 0, want rejection; stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if count := phase0InterfaceSessionCount(t, cityDir); count != 0 {
		t.Fatalf("cmdSessionPin(template:worker) materialized %d session(s)", count)
	}
}

func writePhase2PinCity(t *testing.T, cityDir string, named bool) {
	t.Helper()
	namedSession := ""
	if named {
		namedSession = `
[[named_session]]
template = "worker"
mode = "on_demand"
`
	}
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`+namedSession)
}

func startPhase2PinControllerSocket(t *testing.T, cityDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	lis, err := net.Listen("unix", filepath.Join(cityDir, ".gc", "controller.sock"))
	if err != nil {
		t.Fatalf("Listen(controller.sock): %v", err)
	}
	t.Cleanup(func() {
		_ = lis.Close()
	})

	errCh := make(chan error, 1)
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 64)
			n, err := conn.Read(buf)
			if err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			reply := "ok\n"
			if string(buf[:n]) == "ping\n" {
				reply = "123\n"
			}
			if _, err := conn.Write([]byte(reply)); err != nil {
				conn.Close() //nolint:errcheck
				errCh <- err
				return
			}
			conn.Close() //nolint:errcheck
		}
	}()
	t.Cleanup(func() {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("controller socket: %v", err)
			}
		default:
		}
	})
}
