package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// cliOpCity is a file-backed city with one awake named session running on a
// fake runtime.
type cliOpCity struct {
	dir   string
	store beads.Store
	sp    *runtime.Fake
	bead  beads.Bead
	name  string
}

// newCLIOpCity builds the city. Its op rows live in h's opStore, a store
// that pins ids.
func newCLIOpCity(t *testing.T, h *cliOpHarness, extra map[string]string) *cliOpCity {
	t.Helper()
	h.opStore = &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	dir := shortSocketTempDir(t, "gc-cli-op-")
	t.Setenv("GC_CITY", dir)
	writeGenericNamedSessionCityTOML(t, dir)

	sp := runtime.NewFake()
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return sp, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })

	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	const name = "s-gc-cli-op"
	meta := map[string]string{
		"alias":                      killPokeSessionIdentity,
		"template":                   "worker",
		"agent_name":                 "gascity/gc.worker",
		"session_name":               name,
		"state":                      "awake",
		"generation":                 "4",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: killPokeSessionIdentity,
	}
	for k, v := range extra {
		if v == "" {
			delete(meta, k)
			continue
		}
		meta[k] = v
	}
	b, err := store.Create(beads.Bead{
		Title:    "named session",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession, "template:worker"},
		Metadata: meta,
	})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}
	if err := sp.Start(context.Background(), name, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("starting fake runtime: %v", err)
	}
	if err := sp.SetMeta(name, "GC_SESSION_ID", b.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	oldPoke := sessionKillPokeController
	sessionKillPokeController = func(string, reconcilekey.Key) error { return nil }
	t.Cleanup(func() { sessionKillPokeController = oldPoke })
	return &cliOpCity{dir: dir, store: store, sp: sp, bead: b, name: name}
}

// reload reopens the city store, so a read sees what a command wrote.
func (c *cliOpCity) reload(t *testing.T) beads.Store {
	t.Helper()
	s, err := openCityStoreAt(c.dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	return s
}

// row reads the session bead fresh.
func (c *cliOpCity) row(t *testing.T) beads.Bead {
	t.Helper()
	b, err := c.reload(t).Get(c.bead.ID)
	if err != nil {
		t.Fatalf("reading session bead: %v", err)
	}
	return b
}

// expectUntouchedAtOpen fails the test unless the session row still reads as
// before when the recorder opens the op store, just before it proposes: no
// legacy write ran first.
func (c *cliOpCity) expectUntouchedAtOpen(t *testing.T, h *cliOpHarness) *bool {
	t.Helper()
	seen := new(bool)
	h.onOpen = func() {
		*seen = true
		b := c.row(t)
		if b.Status == "closed" || b.Metadata["state"] != "awake" || b.Metadata["held_until"] != "" || b.Metadata["state_reason"] != "" {
			t.Errorf("session row %+v was written before the op was proposed", b.Metadata)
		}
		if !c.sp.IsRunning(c.name) {
			t.Errorf("runtime stopped before the op was proposed")
		}
	}
	return seen
}

func TestSessionKillOpRecordsTheGenerationAndCloses(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, nil)
	seen := c.expectUntouchedAtOpen(t, h)
	var stdout, stderr bytes.Buffer
	if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "", &stdout, &stderr, true, false); code != 0 {
		t.Fatalf("kill = %d; stderr=%s", code, stderr.String())
	}
	if !*seen {
		t.Fatal("the op store was never opened")
	}
	op := cliOpOnly(t, h.opStore)
	cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", legacyop.ViaLegacy)
	if op.Kind != opjournal.KindKill || op.Scope.Kind != opjournal.ScopeSession || op.Scope.Generation != 4 {
		t.Fatalf("op = %+v, want a kill scoped to session generation 4", op)
	}
	m := cliOpJSON(t, stdout.Bytes())
	if m["op_id"] != op.ID || m["key"] != op.ClientKey {
		t.Fatalf("JSON %v, want op_id %s and key %s", m, op.ID, op.ClientKey)
	}
	validateJSONAgainstResultSchema(t, []string{"session", "kill"}, stdout.Bytes())
}

func TestSessionKillOpKeyReplaysWithoutKillingAgain(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, nil)
	var stdout, stderr bytes.Buffer
	if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "kill-1", &stdout, &stderr, false, false); code != 0 {
		t.Fatalf("kill = %d; stderr=%s", code, stderr.String())
	}
	first := stdout.String()
	// The runtime comes back, as the controller would restart it.
	if err := c.sp.Start(context.Background(), c.name, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("restarting fake runtime: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "kill-1", &stdout, &stderr, false, false); code != 0 {
		t.Fatalf("replayed kill = %d; stderr=%s", code, stderr.String())
	}
	if stdout.String() != first || !strings.Contains(stderr.String(), "replayed op") {
		t.Fatalf("replay stdout %q stderr %q, want %q and a replay note", stdout.String(), stderr.String(), first)
	}
	if !c.sp.IsRunning(c.name) {
		t.Fatal("a replayed kill killed the runtime again")
	}
}

func TestSessionKillOpWithoutGeneration(t *testing.T) {
	t.Run("auto runs unrecorded", func(t *testing.T) {
		h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
		newCLIOpCity(t, h, map[string]string{"generation": ""})
		var stdout, stderr bytes.Buffer
		if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "", &stdout, &stderr, true, false); code != 0 {
			t.Fatalf("kill = %d; stderr=%s", code, stderr.String())
		}
		if ops := cliOpRows(t, h.opStore); len(ops) != 0 {
			t.Fatalf("op rows = %+v, want none for a row with no generation", ops)
		}
		if _, ok := cliOpJSON(t, stdout.Bytes())["op_id"]; ok {
			t.Fatalf("JSON %s carries op_id for an unrecorded kill", stdout.String())
		}
	})
	t.Run("require refuses with no effect", func(t *testing.T) {
		h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "require"})
		c := newCLIOpCity(t, h, map[string]string{"generation": ""})
		var stdout, stderr bytes.Buffer
		if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "", &stdout, &stderr, true, false); code != 1 {
			t.Fatalf("kill = %d, want 1; stderr=%s", code, stderr.String())
		}
		if got := cliOpFailureCode(t, stdout.Bytes()); got != cliOpCodeUnavailable {
			t.Fatalf("failure code = %q, want %s", got, cliOpCodeUnavailable)
		}
		if !c.sp.IsRunning(c.name) {
			t.Fatal("require killed the runtime it could not record")
		}
	})
}

func TestSessionCloseOpRecordsBeforeTheCloseAndReplays(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, nil)
	seen := c.expectUntouchedAtOpen(t, h)
	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{killPokeSessionIdentity}, "close-1", &stdout, &stderr, true); code != 0 {
		t.Fatalf("close = %d; stderr=%s", code, stderr.String())
	}
	if !*seen {
		t.Fatal("the op store was never opened")
	}
	h.onOpen = nil
	op := cliOpOnly(t, h.opStore)
	cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", legacyop.ViaLegacy)
	if op.Kind != opjournal.KindClose || op.ClientKey != "close-1" {
		t.Fatalf("op = %+v, want a close under key close-1", op)
	}
	validateJSONAgainstResultSchema(t, []string{"session", "close"}, stdout.Bytes())

	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionClose([]string{c.bead.ID}, "close-1", &stdout, &stderr, true); code != 0 {
		t.Fatalf("replayed close = %d; stderr=%s", code, stderr.String())
	}
	if m := cliOpJSON(t, stdout.Bytes()); m["replayed"] != true || m["op_id"] != op.ID {
		t.Fatalf("replay JSON %v, want replayed op %s", m, op.ID)
	}
	validateJSONAgainstResultSchema(t, []string{"session", "close"}, stdout.Bytes())
}

func TestSessionSuspendOpRecordsBothPaths(t *testing.T) {
	for _, tc := range []struct {
		name    string
		managed bool
		mode    string
	}{
		{"direct", false, "direct"},
		{"managed", true, "managed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
			c := newCLIOpCity(t, h, nil)
			oldManaged, oldPoke, oldEnqueue := sessionSuspendManagedReconciler, sessionSuspendPokeController, sessionSuspendEnqueueController
			sessionSuspendManagedReconciler = func(string) bool { return tc.managed }
			sessionSuspendPokeController = func(string) error { return nil }
			sessionSuspendEnqueueController = func(string, reconcilekey.Key) error { return nil }
			t.Cleanup(func() {
				sessionSuspendManagedReconciler, sessionSuspendPokeController, sessionSuspendEnqueueController = oldManaged, oldPoke, oldEnqueue
			})
			seen := c.expectUntouchedAtOpen(t, h)
			var stdout, stderr bytes.Buffer
			if code := cmdSessionSuspend([]string{killPokeSessionIdentity}, "", &stdout, &stderr, true); code != 0 {
				t.Fatalf("suspend = %d; stderr=%s", code, stderr.String())
			}
			if !*seen {
				t.Fatal("the op store was never opened")
			}
			op := cliOpOnly(t, h.opStore)
			cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", legacyop.ViaLegacy)
			m := cliOpJSON(t, stdout.Bytes())
			if op.Kind != opjournal.KindSuspend || m["mode"] != tc.mode || m["op_id"] != op.ID {
				t.Fatalf("op %+v JSON %v, want a %s suspend", op, m, tc.mode)
			}
			validateJSONAgainstResultSchema(t, []string{"session", "suspend"}, stdout.Bytes())
		})
	}
}

func TestSessionResetOpRecordsARestartFresh(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, nil)
	startSessionResetTestController(t, c.dir)
	seen := c.expectUntouchedAtOpen(t, h)
	var stdout, stderr bytes.Buffer
	if code := cmdSessionReset([]string{killPokeSessionIdentity}, "", &stdout, &stderr, true); code != 0 {
		t.Fatalf("reset = %d; stderr=%s", code, stderr.String())
	}
	if !*seen {
		t.Fatal("the op store was never opened")
	}
	op := cliOpOnly(t, h.opStore)
	cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", legacyop.ViaLegacy)
	if op.Kind != opjournal.KindRestart || op.Policy.Restart.Mode != opjournal.RestartFresh {
		t.Fatalf("op = %+v, want restart{fresh}", op)
	}
	validateJSONAgainstResultSchema(t, []string{"session", "reset"}, stdout.Bytes())
}

func TestSessionResetOpRollbackClosesViaRollbackAndReplays(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	now := time.Now().UTC()
	c := newCLIOpCity(t, h, map[string]string{
		"state":                     "creating",
		"generation":                "3",
		"instance_token":            "tok-stuck",
		"pending_create_claim":      "true",
		"pending_create_started_at": now.Add(-2 * time.Hour).Format(time.RFC3339),
		"last_woke_at":              now.Add(-time.Hour).Format(time.RFC3339),
	})
	if err := c.sp.Stop(c.name); err != nil {
		t.Fatalf("stopping fake runtime: %v", err)
	}
	startSessionResetTestController(t, c.dir)
	var stdout, stderr bytes.Buffer
	if code := cmdSessionReset([]string{c.bead.ID}, "reset-1", &stdout, &stderr, true); code != 0 {
		t.Fatalf("reset = %d; stderr=%s", code, stderr.String())
	}
	op := cliOpOnly(t, h.opStore)
	cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", legacyop.ViaLegacyRollback)

	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionReset([]string{c.bead.ID}, "reset-1", &stdout, &stderr, true); code != 0 {
		t.Fatalf("replayed reset = %d; stderr=%s", code, stderr.String())
	}
	m := cliOpJSON(t, stdout.Bytes())
	if m["mode"] != "rollback" || m["replayed"] != true {
		t.Fatalf("replay JSON %v, want a replayed rollback", m)
	}
}
