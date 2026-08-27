package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSessionReconcilerTraceStatusReportsLiveKeyedSnapshot(t *testing.T) {
	block := make(chan struct{})
	controller := startTraceStatusController(t, func(context.Context, sessionStartAdmission) error {
		<-block
		return nil
	})
	t.Cleanup(func() { close(block) })

	for _, id := range []string{"gcs-first1", "gcs-second2"} {
		if _, err := controller.Admit(id, sessionStartAdmissionSocket); err != nil {
			t.Fatalf("Admit(%q): %v", id, err)
		}
	}
	controller.RequestAudit()

	cr := &CityRuntime{
		sessionStartController: controller,
		sessionStartOwnership:  sessionStartOwnershipKeyed,
		sessionStartMode:       rollout.Auto,
	}
	got := cr.sessionReconcilerTraceStatus()
	want := sessionReconcilerTraceStatus{
		SchemaVersion:  "1",
		Available:      true,
		ConfiguredMode: "auto",
		EffectiveOwner: "keyed",
		PendingKeys:    2,
		AuditPending:   true,
	}
	if got != want {
		t.Fatalf("sessionReconcilerTraceStatus() = %+v, want %+v", got, want)
	}
	if !controller.TakeAuditRequest() {
		t.Fatal("status snapshot consumed audit pending request")
	}
	controller.RequestAudit()
	cr.sessionStartMode = rollout.Require
	if got, want := cr.sessionReconcilerTraceStatus(), (sessionReconcilerTraceStatus{
		SchemaVersion:  "1",
		Available:      true,
		ConfiguredMode: "require",
		EffectiveOwner: "keyed",
		PendingKeys:    2,
		AuditPending:   true,
	}); got != want {
		t.Fatalf("require keyed status = %+v, want %+v", got, want)
	}
}

func TestSessionReconcilerTraceStatusFallbacksAndProjectionMatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		cr   *CityRuntime
		want sessionReconcilerTraceStatus
	}{
		{
			name: "nil runtime",
			want: unavailableSessionReconcilerTraceStatus(),
		},
		{
			name: "off legacy",
			cr:   &CityRuntime{sessionStartMode: rollout.Off, sessionStartOwnership: sessionStartOwnershipLegacy},
			want: sessionReconcilerTraceStatus{SchemaVersion: "1", Available: true, ConfiguredMode: "off", EffectiveOwner: "legacy"},
		},
		{
			name: "required blocked",
			cr:   &CityRuntime{sessionStartMode: rollout.Require, sessionStartOwnership: sessionStartOwnershipRequiredBlocked},
			want: sessionReconcilerTraceStatus{SchemaVersion: "1", Available: true, ConfiguredMode: "require", EffectiveOwner: "required_blocked"},
		},
		{
			name: "startup before ownership installation",
			cr: &CityRuntime{cs: &controllerState{
				rolloutFlags: rollout.ForTest(rollout.WithSessionReconciler(rollout.Auto)),
			}},
			want: sessionReconcilerTraceStatus{SchemaVersion: "1", ConfiguredMode: "auto", EffectiveOwner: "unavailable"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cr.sessionReconcilerTraceStatus(); got != tc.want {
				t.Fatalf("sessionReconcilerTraceStatus() = %+v, want %+v", got, tc.want)
			}
		})
	}

	cityDir := t.TempDir()
	status, _, err := traceStatusLocal(cityDir)
	if err != nil {
		t.Fatalf("traceStatusLocal: %v", err)
	}
	if got, want := status.SessionReconciler, unavailableSessionReconcilerTraceStatus(); got != want {
		t.Fatalf("offline session reconciler status = %+v, want %+v", got, want)
	}
}

func TestSessionReconcilerTraceStatusOffLegacyRequiresNoKeyedEffectOwners(t *testing.T) {
	available := sessionReconcilerTraceStatus{
		SchemaVersion:  "1",
		Available:      true,
		ConfiguredMode: "off",
		EffectiveOwner: "legacy",
	}
	for _, tc := range []struct {
		name    string
		install func(*CityRuntime)
		want    sessionReconcilerTraceStatus
	}{
		{
			name: "clean off",
			want: available,
		},
		{
			name: "stale session start controller",
			install: func(cr *CityRuntime) {
				cr.sessionStartController = &sessionStartController{}
			},
			want: unavailableSessionReconcilerTraceStatus(),
		},
		{
			name: "stale session start event admission",
			install: func(cr *CityRuntime) {
				cr.cs = &controllerState{sessionStartEventAdmission: func(string) {}}
			},
			want: unavailableSessionReconcilerTraceStatus(),
		},
		{
			name: "stale nudge key controller",
			install: func(cr *CityRuntime) {
				cr.nudgeKeyController = &nudgeKeyController{}
			},
			want: unavailableSessionReconcilerTraceStatus(),
		},
		{
			name: "stale wait dependency producer",
			install: func(cr *CityRuntime) {
				cr.waitDependencyProducer = &sessionWaitDependencyProducer{}
			},
			want: unavailableSessionReconcilerTraceStatus(),
		},
		{
			name: "stale wait dependency producer admission",
			install: func(cr *CityRuntime) {
				cr.cs = &controllerState{
					sessionWaitShadowProducerAdmission: func(sessionWaitDependencyProducerRequest) {},
				}
			},
			want: unavailableSessionReconcilerTraceStatus(),
		},
		{
			name: "generic wait shadow admission is read only",
			install: func(cr *CityRuntime) {
				cr.cs = &controllerState{
					sessionWaitShadowAdmission: func() sessionWaitShadowRefreshResult {
						return sessionWaitShadowConverged
					},
				}
			},
			want: available,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr := &CityRuntime{
				sessionStartMode:      rollout.Off,
				sessionStartOwnership: sessionStartOwnershipLegacy,
			}
			if tc.install != nil {
				tc.install(cr)
			}
			if got := cr.sessionReconcilerTraceStatus(); got != tc.want {
				t.Fatalf("sessionReconcilerTraceStatus() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSessionReconcilerTraceStatusSocketProjection(t *testing.T) {
	cityDir := t.TempDir()
	cr := &CityRuntime{sessionStartMode: rollout.Off, sessionStartOwnership: sessionStartOwnershipLegacy}
	server, client := net.Pipe()
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		handleControllerConnWithSessionReconcilerStatus(
			server, cityDir, controllerHostingStandalone, nil, nil, nil, nil, nil, nil, nil, nil,
			cr.sessionReconcilerTraceStatus,
		)
	}()
	t.Cleanup(func() {
		client.Close() //nolint:errcheck
		awaitClose(t, handlerDone, "trace-status pipe handler")
	})

	if _, err := io.WriteString(client, "trace-status\n"); err != nil {
		t.Fatalf("write trace-status command: %v", err)
	}
	var reply traceControlReply
	if err := json.NewDecoder(client).Decode(&reply); err != nil {
		t.Fatalf("decode trace-status reply: %v", err)
	}
	if !reply.OK || reply.Status == nil {
		t.Fatalf("trace-status reply = %+v, want successful status", reply)
	}
	if got, want := reply.Status.SessionReconciler, cr.sessionReconcilerTraceStatus(); got != want {
		t.Fatalf("socket session reconciler status = %+v, want %+v", got, want)
	}
}

func TestRenderTraceStatusResultHumanJSONParity(t *testing.T) {
	result := traceStatusResultJSON{
		SchemaVersion:     "1",
		CityPath:          "/city",
		ControllerRunning: true,
		ControllerPID:     42,
		HeadSeq:           7,
		ActiveArms:        []TraceArm{},
		SessionReconciler: sessionReconcilerTraceStatus{
			SchemaVersion:  "1",
			Available:      true,
			ConfiguredMode: "auto",
			EffectiveOwner: "keyed",
			PendingKeys:    2,
			AuditPending:   true,
		},
	}
	var human, jsonOut bytes.Buffer
	if err := renderTraceStatusResult(&human, result, false); err != nil {
		t.Fatalf("render human trace status: %v", err)
	}
	if err := renderTraceStatusResult(&jsonOut, result, true); err != nil {
		t.Fatalf("render JSON trace status: %v", err)
	}
	var decoded traceStatusResultJSON
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal rendered trace status: %v", err)
	}
	if got, want := decoded.SchemaVersion, "1"; got != want {
		t.Fatalf("top-level schema version = %q, want %q", got, want)
	}
	if got, want := decoded.SessionReconciler, result.SessionReconciler; got != want {
		t.Fatalf("JSON session reconciler status = %+v, want %+v", got, want)
	}
	for _, line := range []string{
		fmt.Sprintf("Session reconciler schema version: %s", decoded.SessionReconciler.SchemaVersion),
		fmt.Sprintf("Session reconciler available: %t", decoded.SessionReconciler.Available),
		fmt.Sprintf("Session reconciler configured mode: %s", decoded.SessionReconciler.ConfiguredMode),
		fmt.Sprintf("Session reconciler effective owner: %s", decoded.SessionReconciler.EffectiveOwner),
		fmt.Sprintf("Session reconciler pending keys: %d", decoded.SessionReconciler.PendingKeys),
		fmt.Sprintf("Session reconciler audit pending: %t", decoded.SessionReconciler.AuditPending),
	} {
		if !strings.Contains(human.String(), line) {
			t.Fatalf("human trace status missing %q:\n%s", line, human.String())
		}
	}
}

func TestDecodeTraceStatusReplyFailsClosedOnInvalidSessionReconcilerStatus(t *testing.T) {
	if _, _, err := decodeTraceStatusReply([]byte(`{"ok":`)); err == nil {
		t.Fatal("decode malformed trace-status reply succeeded, want error")
	}

	oldControllerReply := []byte(`{"ok":true,"message":"ok","status":{"city_path":"/city","controller_running":true,"head_seq":0,"active_arms":[]}}`)
	status, _, err := decodeTraceStatusReply(oldControllerReply)
	if err != nil {
		t.Fatalf("decode old-controller reply: %v", err)
	}
	if got, want := status.SessionReconciler, unavailableSessionReconcilerTraceStatus(); got != want {
		t.Fatalf("old-controller session reconciler status = %+v, want %+v", got, want)
	}

	for _, tc := range []struct {
		name   string
		status sessionReconcilerTraceStatus
	}{
		{
			name:   "missing nested schema",
			status: sessionReconcilerTraceStatus{Available: true, ConfiguredMode: "auto", EffectiveOwner: "keyed"},
		},
		{
			name:   "unknown nested schema",
			status: sessionReconcilerTraceStatus{SchemaVersion: "2", Available: true, ConfiguredMode: "auto", EffectiveOwner: "keyed"},
		},
		{
			name:   "unknown mode",
			status: sessionReconcilerTraceStatus{SchemaVersion: "1", Available: true, ConfiguredMode: "unknown", EffectiveOwner: "keyed"},
		},
		{
			name:   "unknown owner",
			status: sessionReconcilerTraceStatus{SchemaVersion: "1", Available: true, ConfiguredMode: "auto", EffectiveOwner: "unknown"},
		},
		{
			name:   "negative pressure",
			status: sessionReconcilerTraceStatus{SchemaVersion: "1", Available: true, ConfiguredMode: "auto", EffectiveOwner: "keyed", PendingKeys: -1},
		},
		{
			name:   "impossible tuple",
			status: sessionReconcilerTraceStatus{SchemaVersion: "1", Available: true, ConfiguredMode: "off", EffectiveOwner: "keyed"},
		},
		{
			name:   "off with pending pressure",
			status: sessionReconcilerTraceStatus{SchemaVersion: "1", Available: true, ConfiguredMode: "off", EffectiveOwner: "legacy", PendingKeys: 1},
		},
		{
			name:   "off with audit pressure",
			status: sessionReconcilerTraceStatus{SchemaVersion: "1", Available: true, ConfiguredMode: "off", EffectiveOwner: "legacy", AuditPending: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(traceControlReply{
				OK: true,
				Status: &traceStatusJSON{
					CityPath:          "/city",
					ControllerRunning: true,
					ActiveArms:        []TraceArm{},
					SessionReconciler: tc.status,
				},
			})
			if err != nil {
				t.Fatalf("marshal trace status reply: %v", err)
			}
			status, _, err := decodeTraceStatusReply(payload)
			if err != nil {
				t.Fatalf("decode trace status reply: %v", err)
			}
			if got, want := status.SessionReconciler, unavailableSessionReconcilerTraceStatus(); got != want {
				t.Fatalf("decoded session reconciler status = %+v, want %+v", got, want)
			}
		})
	}
}

func TestSessionReconcilerTraceStatusReportsLegacyDuringAutoConfigHandoff(t *testing.T) {
	cs := &controllerState{rolloutFlags: rollout.ForTest(rollout.WithSessionReconciler(rollout.Auto))}
	cs.configMutationPending.Store(true)
	cr := &CityRuntime{
		cs:                     cs,
		sessionStartController: &sessionStartController{},
		sessionStartOwnership:  sessionStartOwnershipKeyed,
		sessionStartMode:       rollout.Auto,
	}
	if got, want := cr.sessionReconcilerTraceStatus().EffectiveOwner, "legacy"; got != want {
		t.Fatalf("effective owner during auto handoff = %q, want %q", got, want)
	}
}

func TestSessionReconcilerTraceStatusReportsRequiredBlockedDuringRequireConfigHandoff(t *testing.T) {
	cs := &controllerState{rolloutFlags: rollout.ForTest(rollout.WithSessionReconciler(rollout.Require))}
	cs.configMutationPending.Store(true)
	cr := &CityRuntime{
		cs:                     cs,
		sessionStartController: &sessionStartController{},
		sessionStartOwnership:  sessionStartOwnershipKeyed,
		sessionStartMode:       rollout.Require,
	}
	if got, want := cr.sessionReconcilerTraceStatus().EffectiveOwner, "required_blocked"; got != want {
		t.Fatalf("effective owner during require handoff = %q, want %q", got, want)
	}
}

func TestSessionReconcilerTraceStatusRejectsInvalidInternalTuples(t *testing.T) {
	controller := &sessionStartController{}
	for _, tc := range []struct {
		name string
		cr   *CityRuntime
	}{
		{
			name: "unknown mode",
			cr: &CityRuntime{
				sessionStartMode:      rollout.Mode("unknown"),
				sessionStartOwnership: sessionStartOwnershipLegacy,
			},
		},
		{
			name: "unknown owner",
			cr: &CityRuntime{
				sessionStartMode:       rollout.Auto,
				sessionStartOwnership:  sessionStartOwnership(255),
				sessionStartController: controller,
			},
		},
		{
			name: "keyed without controller",
			cr: &CityRuntime{
				sessionStartMode:      rollout.Auto,
				sessionStartOwnership: sessionStartOwnershipKeyed,
			},
		},
		{
			name: "off with keyed controller",
			cr: &CityRuntime{
				sessionStartMode:       rollout.Off,
				sessionStartOwnership:  sessionStartOwnershipKeyed,
				sessionStartController: controller,
			},
		},
		{
			name: "require with legacy owner",
			cr: &CityRuntime{
				sessionStartMode:      rollout.Require,
				sessionStartOwnership: sessionStartOwnershipLegacy,
			},
		},
		{
			name: "legacy owner with controller",
			cr: &CityRuntime{
				sessionStartMode:       rollout.Auto,
				sessionStartOwnership:  sessionStartOwnershipLegacy,
				sessionStartController: controller,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := tc.cr.sessionReconcilerTraceStatus(), unavailableSessionReconcilerTraceStatus(); got != want {
				t.Fatalf("sessionReconcilerTraceStatus() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestSessionReconcilerTraceStatusIsReadOnlyAndRaceSafe(t *testing.T) {
	var reconciles atomic.Int64
	provider := runtime.NewFake()
	controller := startTraceStatusController(t, func(context.Context, sessionStartAdmission) error {
		reconciles.Add(1)
		return nil
	})
	cr := &CityRuntime{
		sessionStartController: controller,
		sessionStartOwnership:  sessionStartOwnershipKeyed,
		sessionStartMode:       rollout.Require,
		sp:                     provider,
	}
	for range 1000 {
		_ = cr.sessionReconcilerTraceStatus()
	}
	if got := reconciles.Load(); got != 0 {
		t.Fatalf("1,000 status reads reconciled %d keys, want 0", got)
	}
	if calls := provider.SnapshotCalls(); len(calls) != 0 {
		t.Fatalf("1,000 status reads called provider: %+v", calls)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_, _ = controller.Admit("gcs-race-"+string(rune('a'+i%26))+string(rune('a'+(i/26)%26)), sessionStartAdmissionSocket)
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			_ = cr.sessionReconcilerTraceStatus()
		}
	}()
	wg.Wait()
}

func TestSessionReconcilerTraceStatusCapturesModeAndPressureAtOneLockPoint(t *testing.T) {
	controller := &sessionStartController{admissions: map[string]sessionStartAdmission{
		"gcs-auto": {},
	}}
	cr := &CityRuntime{
		sessionStartController: controller,
		sessionStartOwnership:  sessionStartOwnershipKeyed,
		sessionStartMode:       rollout.Auto,
	}

	controller.mu.Lock()
	controllerLocked := true
	defer func() {
		if controllerLocked {
			controller.mu.Unlock()
		}
	}()

	var got sessionReconcilerTraceStatus
	done := make(chan struct{})
	go func() {
		defer close(done)
		got = cr.sessionReconcilerTraceStatus()
	}()
	awaitCond(t, func() bool {
		if cr.sessionStartMu.TryLock() {
			cr.sessionStartMu.Unlock()
			return false
		}
		return true
	}, "trace status snapshot to retain the runtime lock while waiting for controller pressure")

	controller.mu.Unlock()
	controllerLocked = false
	awaitClose(t, done, "trace status snapshot")
	if got.PendingKeys != 1 {
		t.Fatalf("snapshot pending keys = %d, want 1", got.PendingKeys)
	}
}

func startTraceStatusController(t *testing.T, reconcile func(context.Context, sessionStartAdmission) error) *sessionStartController {
	t.Helper()
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 4096,
		MaxRetries:  0,
		Reconcile:   reconcile,
		Stderr:      io.Discard,
	})
	if err != nil {
		t.Fatalf("newSessionStartController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := controller.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		controller.Stop()
	})
	return controller
}
