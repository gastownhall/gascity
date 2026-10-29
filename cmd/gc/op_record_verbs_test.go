package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
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

// restartAs brings the session back as generation gen, as the controller
// reviving it after a kill would.
func (c *cliOpCity) restartAs(t *testing.T, gen int) {
	t.Helper()
	if err := c.reload(t).SetMetadataBatch(c.bead.ID, map[string]string{
		"generation": strconv.Itoa(gen), "state": "awake", "state_reason": "",
	}); err != nil {
		t.Fatalf("reviving the session row: %v", err)
	}
	if err := c.sp.Start(context.Background(), c.name, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("restarting fake runtime: %v", err)
	}
}

// closeForReopen closes the session row as a suspend closes a configured
// named session's row: under the configured runtime name, so a wake of the
// named session reopens it.
func (c *cliOpCity) closeForReopen(t *testing.T) {
	t.Helper()
	cfg, err := loadCityConfig(c.dir, io.Discard)
	if err != nil {
		t.Fatalf("loading city config: %v", err)
	}
	spec, ok := findNamedSessionSpec(cfg, config.EffectiveCityName(cfg, filepath.Base(c.dir)), killPokeSessionIdentity)
	if !ok {
		t.Fatalf("no configured named session %q", killPokeSessionIdentity)
	}
	store := c.reload(t)
	if err := store.SetMetadataBatch(c.bead.ID, map[string]string{
		"session_name": spec.SessionName, "close_reason": "suspended",
	}); err != nil {
		t.Fatalf("readying the session row to reopen: %v", err)
	}
	if err := store.Close(c.bead.ID); err != nil {
		t.Fatalf("closing the session row: %v", err)
	}
}

// unreadableAtRead makes the session row unreadable to the kill's
// front-door read, after the target resolved, and readable again once the
// kill builds its handle, so only the read the op is scoped from fails.
func (c *cliOpCity) unreadableAtRead(t *testing.T) {
	t.Helper()
	oldBuild, oldHandle := buildSessionProviderByName, sessionKillHandle
	damaged := false
	buildSessionProviderByName = func(cfg *config.City, name string, sc config.SessionConfig, cityName, cityPath string) (runtime.Provider, error) {
		if !damaged {
			damaged = true
			task := "task"
			if err := c.reload(t).Update(c.bead.ID, beads.UpdateOpts{Type: &task, RemoveLabels: []string{session.LabelSession}}); err != nil {
				t.Fatalf("damaging the session row: %v", err)
			}
		}
		return oldBuild(cfg, name, sc, cityName, cityPath)
	}
	sessionKillHandle = func(cityPath string, store beads.Store, sp runtime.Provider, cfg *config.City, id string) (worker.Handle, error) {
		typ := session.BeadType
		if err := c.reload(t).Update(c.bead.ID, beads.UpdateOpts{Type: &typ, Labels: []string{session.LabelSession}}); err != nil {
			t.Fatalf("repairing the session row: %v", err)
		}
		return oldHandle(cityPath, store, sp, cfg, id)
	}
	t.Cleanup(func() { buildSessionProviderByName, sessionKillHandle = oldBuild, oldHandle })
}

// expectUntouchedAtOpen fails the test unless the session row reads, field
// for field, as it did before the command ran, and its runtime is as it was,
// when the recorder opens the op store, just before it proposes: no legacy
// write ran first.
func (c *cliOpCity) expectUntouchedAtOpen(t *testing.T, h *cliOpHarness) *bool {
	t.Helper()
	seen := new(bool)
	before := c.snap(t)
	h.onOpen = func() {
		*seen = true
		if d := c.snap(t).diff(before); d != "" {
			t.Errorf("legacy effect before the op was proposed: %s", d)
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

// A kill retried under its key after the controller revived the session (a
// new generation) reports the first kill, noting the restart, instead of
// killing the new run or refusing the key as another request.
func TestSessionKillOpKeyReplaysAcrossARestart(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, nil)
	var stdout, stderr bytes.Buffer
	if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "kill-1", &stdout, &stderr, true, false); code != 0 {
		t.Fatalf("kill = %d; stderr=%s", code, stderr.String())
	}
	first := cliOpOnly(t, h.opStore)
	c.restartAs(t, 5)
	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "kill-1", &stdout, &stderr, true, false); code != 0 {
		t.Fatalf("kill retried after a restart = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if m := cliOpJSON(t, stdout.Bytes()); m["replayed"] != true || m["op_id"] != first.ID || m["key"] != "kill-1" {
		t.Fatalf("retry JSON %v, want replayed op %s under key kill-1", m, first.ID)
	}
	validateJSONAgainstResultSchema(t, []string{"session", "kill"}, stdout.Bytes())
	if !strings.Contains(stderr.String(), "replayed op "+first.ID) || !strings.Contains(stderr.String(), "restarted as generation 5") {
		t.Fatalf("retry stderr = %q, want the replay of %s noting the restart as generation 5", stderr.String(), first.ID)
	}
	if !c.sp.IsRunning(c.name) {
		t.Fatal("a kill retried under its key killed the restarted session")
	}
	if ops := cliOpRows(t, h.opStore); len(ops) != 1 {
		t.Fatalf("op rows = %+v, want only the first kill's", ops)
	}
}

// A kill retried under its key across a restart answers from the earlier
// kill whatever its state: in progress, or closed failed. A key that names
// anything but an earlier kill of the session is still another request.
// Every refusal names the op the key belongs to.
func TestSessionKillOpKeyAcrossARestartAnswersFromTheEarlierOp(t *testing.T) {
	earlier := opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 3}
	for _, tc := range []struct {
		name      string
		plant     func(t *testing.T, store beads.Store, conv string) opjournal.Op
		code      string
		restarted bool
	}{{
		name: "earlier kill in progress",
		plant: func(t *testing.T, store beads.Store, conv string) opjournal.Op {
			return cliOpProposeOpen(t, store, opjournal.KindKill, conv, "kill-1", earlier, cliOpT0, opjournal.Policy{})
		},
		code: cliOpCodeInProgress, restarted: true,
	}, {
		name: "earlier kill failed",
		plant: func(t *testing.T, store beads.Store, conv string) opjournal.Op {
			return cliOpPlantClosed(t, store, opjournal.KindKill, conv, "kill-1", earlier, opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonOther})
		},
		code: cliOpCodeAlreadyFailed, restarted: true,
	}, {
		name: "the key names a close",
		plant: func(t *testing.T, store beads.Store, conv string) opjournal.Op {
			return cliOpProposeOpen(t, store, opjournal.KindClose, conv, "kill-1", opjournal.Scope{}, cliOpT0, opjournal.Policy{})
		},
		code: cliOpCodeKeyMismatch,
	}, {
		name: "the key names a kill of a later generation",
		plant: func(t *testing.T, store beads.Store, conv string) opjournal.Op {
			return cliOpProposeOpen(t, store, opjournal.KindKill, conv, "kill-1", opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 9}, cliOpT0, opjournal.Policy{})
		},
		code: cliOpCodeKeyMismatch,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
			c := newCLIOpCity(t, h, nil)
			planted := tc.plant(t, h.opStore, c.bead.ID)
			var stdout, stderr bytes.Buffer
			if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "kill-1", &stdout, &stderr, true, false); code != 1 {
				t.Fatalf("kill = %d, want 1; stderr=%s", code, stderr.String())
			}
			if got := cliOpFailureCode(t, stdout.Bytes()); got != tc.code {
				t.Fatalf("failure code = %q, want %s; stderr=%s", got, tc.code, stderr.String())
			}
			if m := cliOpJSON(t, stdout.Bytes()); m["op_id"] != planted.ID || m["key"] != "kill-1" {
				t.Fatalf("failure envelope %v, want op_id %s and key kill-1", m, planted.ID)
			}
			if got := strings.Contains(stderr.String(), "restarted as generation 4"); got != tc.restarted {
				t.Fatalf("stderr = %q; restart noted = %t, want %t", stderr.String(), got, tc.restarted)
			}
			if !c.sp.IsRunning(c.name) {
				t.Fatal("a refused kill killed the runtime")
			}
		})
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
		reader := installManualMetricReader(t)
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
		want := string(opjournal.KindKill) + "|cli|admit|refused|" + legacyop.ReasonNoGeneration
		if got := cliOpRecordCounts(t, reader); got[want] != 1 {
			t.Fatalf("counts = %v, want %s counted once", got, want)
		}
	})
}

// A kill whose session row cannot be read has no generation to scope its op
// to, and says why: unreadable_target, with the read error. require refuses
// it with no effect; auto runs the best-effort kill unrecorded.
func TestSessionKillOpUnreadableRow(t *testing.T) {
	t.Run("require refuses with no effect", func(t *testing.T) {
		reader := installManualMetricReader(t)
		h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "require"})
		c := newCLIOpCity(t, h, nil)
		c.unreadableAtRead(t)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "", &stdout, &stderr, true, false); code != 1 {
			t.Fatalf("kill = %d, want 1; stderr=%s", code, stderr.String())
		}
		if got := cliOpFailureCode(t, stdout.Bytes()); got != cliOpCodeUnavailable {
			t.Fatalf("failure code = %q, want %s", got, cliOpCodeUnavailable)
		}
		if !strings.Contains(stdout.String(), legacyop.ReasonUnreadableTarget+": ") {
			t.Fatalf("failure envelope %s, want the %s reason with its read error", stdout.String(), legacyop.ReasonUnreadableTarget)
		}
		if !c.sp.IsRunning(c.name) {
			t.Fatal("require killed the runtime it could not record")
		}
		if ops := cliOpRows(t, h.opStore); len(ops) != 0 {
			t.Fatalf("op rows = %+v, want none", ops)
		}
		want := string(opjournal.KindKill) + "|cli|admit|refused|" + legacyop.ReasonUnreadableTarget
		if got := cliOpRecordCounts(t, reader); got[want] != 1 {
			t.Fatalf("counts = %v, want %s counted once", got, want)
		}
	})
	t.Run("auto kills unrecorded", func(t *testing.T) {
		reader := installManualMetricReader(t)
		h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
		c := newCLIOpCity(t, h, nil)
		c.unreadableAtRead(t)
		var stdout, stderr bytes.Buffer
		if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "unread-1", &stdout, &stderr, true, false); code != 0 {
			t.Fatalf("kill = %d, want 0; stderr=%s", code, stderr.String())
		}
		if c.sp.IsRunning(c.name) {
			t.Fatal("auto did not run the best-effort kill")
		}
		if ops := cliOpRows(t, h.opStore); len(ops) != 0 {
			t.Fatalf("op rows = %+v, want none for an unreadable row", ops)
		}
		if !strings.Contains(stderr.String(), "--key not recorded ("+legacyop.ReasonUnreadableTarget+": ") {
			t.Fatalf("stderr = %q, want the --key warning naming %s and its read error", stderr.String(), legacyop.ReasonUnreadableTarget)
		}
		want := string(opjournal.KindKill) + "|cli|admit|unrecorded|" + legacyop.ReasonUnreadableTarget
		if got := cliOpRecordCounts(t, reader); got[want] != 1 {
			t.Fatalf("counts = %v, want %s counted once", got, want)
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

// A reset that rolled back closes its op via the rollback. It released the
// session's alias, so the run named by that alias says a same-key rerun
// replays it only by the session's ID, and one by the ID replays the rollback.
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
	if code := cmdSessionReset([]string{killPokeSessionIdentity}, "reset-1", &stdout, &stderr, true); code != 0 {
		t.Fatalf("reset = %d; stderr=%s", code, stderr.String())
	}
	op := cliOpOnly(t, h.opStore)
	cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", legacyop.ViaLegacyRollback)
	if !strings.Contains(stderr.String(), "replays this run only by the session ID "+c.bead.ID) {
		t.Fatalf("stderr = %q, want a note that a same-key rerun replays only by ID %s", stderr.String(), c.bead.ID)
	}

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

// cliOpFailingGetStore fails reads of one bead once armed.
type cliOpFailingGetStore struct {
	beads.Store
	id    string
	armed *bool
}

func (s cliOpFailingGetStore) Get(id string) (beads.Bead, error) {
	if *s.armed && id == s.id {
		return beads.Bead{}, errors.New("store read failed")
	}
	return s.Store.Get(id)
}

func TestSessionWakeOpFailureClosesFailed(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	armed := false
	h.opStore = f.rec
	h.onOpen = func() { armed = true }
	f.deps.store = cliOpFailingGetStore{Store: f.rec, id: f.id, armed: &armed}
	code, _, stderr := f.run(t, "", false)
	if code != 1 {
		t.Fatalf("wake over a failing store = %d, want 1; stderr=%s", code, stderr)
	}
	cliOpWantOutcome(t, cliOpOnly(t, f.rec), opjournal.ArmFailed, opjournal.ReasonOther, "")
}

func TestSessionKillOpLeaseBusyClosesRefused(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, nil)
	defer session.SetOperatorLeaseWaitForTest(100 * time.Millisecond)()
	held, err := session.TryRuntimeLease(sessionFrontDoor(c.store), session.RuntimeLeaseRequest{
		City: c.dir, Name: c.name, ID: c.bead.ID, TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("taking the runtime lease: %v", err)
	}
	defer held.Release()
	var stdout, stderr bytes.Buffer
	if code := cmdSessionKillWithForce([]string{c.bead.ID}, "", &stdout, &stderr, false, false); code != 1 {
		t.Fatalf("kill under a held lease = %d, want 1; stderr=%s", code, stderr.String())
	}
	cliOpWantOutcome(t, cliOpOnly(t, h.opStore), opjournal.ArmFailed, "refused:session_starting", "")
}

func TestSessionSuspendOpFailureClosesFailed(t *testing.T) {
	t.Run("direct stop fails", func(t *testing.T) {
		h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
		c := newCLIOpCity(t, h, nil)
		stubSessionSuspendController(t, false)
		c.sp.StopErrors[c.name] = errors.New("stop failed")
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSuspend([]string{c.bead.ID}, "", &stdout, &stderr, false); code != 1 {
			t.Fatalf("suspend = %d, want 1; stderr=%s", code, stderr.String())
		}
		cliOpWantOutcome(t, cliOpOnly(t, h.opStore), opjournal.ArmFailed, opjournal.ReasonOther, "")
	})
	t.Run("managed hold refused", func(t *testing.T) {
		h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
		c := newCLIOpCity(t, h, nil)
		stubSessionSuspendController(t, true)
		if err := c.store.Close(c.bead.ID); err != nil {
			t.Fatalf("closing session bead: %v", err)
		}
		var stdout, stderr bytes.Buffer
		if code := cmdSessionSuspend([]string{c.bead.ID}, "", &stdout, &stderr, false); code != 1 {
			t.Fatalf("suspend of a closed session = %d, want 1; stderr=%s", code, stderr.String())
		}
		cliOpWantOutcome(t, cliOpOnly(t, h.opStore), opjournal.ArmFailed, "refused:illegal_transition", "")
	})
}

// A kill of a named session whose runtime is already gone clears its
// circuit breaker and exits 0: the op closes skipped{not_needed}, which a
// same-key rerun replays as the kill's success.
func TestSessionKillOpNamedGoneRuntimeSkipsAndReplays(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, map[string]string{"state": "asleep"})
	if err := c.sp.Stop(c.name); err != nil {
		t.Fatalf("stopping fake runtime: %v", err)
	}
	c.useFreshReadingRuntime(t)
	var stdout, stderr bytes.Buffer
	if code := cmdSessionKillWithForce([]string{c.bead.ID}, "gone-1", &stdout, &stderr, false, false); code != 0 {
		t.Fatalf("kill = %d, want 0; stderr=%s", code, stderr.String())
	}
	first := stdout.String()
	op := cliOpOnly(t, h.opStore)
	if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmSkipped || op.Outcome.Reason != opjournal.ReasonNotNeeded {
		t.Fatalf("op = %+v, want closed skipped{not_needed}", op)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionKillWithForce([]string{c.bead.ID}, "gone-1", &stdout, &stderr, false, false); code != 0 {
		t.Fatalf("replayed kill = %d, want 0; stderr=%s", code, stderr.String())
	}
	if stdout.String() != first || !strings.Contains(stderr.String(), "replayed op "+op.ID) {
		t.Fatalf("replay stdout %q stderr %q, want %q and a replay note", stdout.String(), stderr.String(), first)
	}
}

// Front-door refusals write no op (I9): a kill refused for live subagents and
// a close refused for a config that did not load leave no op row.
func TestSessionVerbFrontDoorRefusalsWriteNoOp(t *testing.T) {
	t.Run("kill with live subagents", func(t *testing.T) {
		h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
		c := newCLIOpCity(t, h, nil)
		old := liveSubagentsForKill
		liveSubagentsForKill = func(context.Context, worker.Handle) ([]worker.InFlightSubagent, error) {
			return []worker.InFlightSubagent{{AgentID: "helper", Description: "still working", StartedAt: time.Now().Add(-time.Minute)}}, nil
		}
		t.Cleanup(func() { liveSubagentsForKill = old })
		var stdout, stderr bytes.Buffer
		if code := cmdSessionKillWithForce([]string{c.bead.ID}, "", &stdout, &stderr, false, false); code != 1 || !strings.Contains(stderr.String(), "helper") {
			t.Fatalf("kill = %d, stderr %q; want the live-subagent refusal", code, stderr.String())
		}
		if ops := cliOpRows(t, h.opStore); len(ops) != 0 || h.opens != 0 {
			t.Fatalf("op rows %+v, store opens %d: a front-door refusal must write no op", ops, h.opens)
		}
	})
	t.Run("close without config over claims", func(t *testing.T) {
		h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
		c := newCLIOpCity(t, h, nil)
		if _, err := c.store.Create(beads.Bead{Title: "claim", Type: "task", Assignee: c.bead.ID, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}}); err != nil {
			t.Fatalf("creating claim: %v", err)
		}
		appendCityTOML(t, c.dir, "\n[[[not toml\n")
		var stdout, stderr bytes.Buffer
		if code := cmdSessionClose([]string{c.bead.ID}, "", &stdout, &stderr, false); code != 1 || !strings.Contains(stderr.String(), "refusing to close") {
			t.Fatalf("close = %d, stderr %q; want the claims refusal", code, stderr.String())
		}
		if ops := cliOpRows(t, h.opStore); len(ops) != 0 || h.opens != 0 {
			t.Fatalf("op rows %+v, store opens %d: a front-door refusal must write no op", ops, h.opens)
		}
	})
}

// The production opener finds the op store's id prefix through the CLI's
// session-store wrappers and probes the store's conditional writes: a wake
// over a SQLite city store records its op there.
func TestSessionWakeOpThroughTheProductionOpener(t *testing.T) {
	production := cliOpenOpStore
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	cliOpenOpStore = production
	sqlite, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("sq"))
	if err != nil {
		t.Fatalf("opening sqlite store: %v", err)
	}
	f := newCLIOpWakeFixture(t)
	b, err := sqlite.Create(beads.Bead{
		Title: "wake op session", Type: session.BeadType, Labels: []string{session.LabelSession},
		Metadata: map[string]string{"session_name": "s-sq-wake-op", "template": "worker", "state": "asleep", "generation": "1"},
	})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}
	f.id = b.ID
	f.deps.store = sqlite
	f.deps.cityPath = t.TempDir()
	code, stdout, stderr := f.run(t, "", true)
	if code != 0 {
		t.Fatalf("wake = %d; stderr=%s", code, stderr)
	}
	m := cliOpJSON(t, []byte(stdout))
	opID, _ := m["op_id"].(string)
	if !strings.HasPrefix(opID, "sq-") {
		t.Fatalf("JSON %v, want an op id minted under the store's prefix sq", m)
	}
	row, err := sqlite.Get(opID)
	if err != nil || row.Type != opjournal.BeadType || row.Status != "closed" {
		t.Fatalf("op row %s = %+v (%v), want a closed op in the city store", opID, row, err)
	}
}

// A kill of an unnamed session whose runtime is already gone, or whose row
// is closed, exits 1; its op closes failed, so a same-key rerun replays the
// failure instead of reporting a kill.
func TestSessionKillOpUnnamedInactiveClosesFailedAndReplaysFailed(t *testing.T) {
	unnamed := map[string]string{"alias": "", namedSessionMetadataKey: "", namedSessionIdentityMetadata: "", "state": "asleep"}
	for _, tc := range []struct {
		name   string
		closed bool
		reason string
	}{
		{"runtime gone", false, "refused:session_inactive"},
		{"row closed", true, "refused:session_closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
			c := newCLIOpCity(t, h, unnamed)
			if err := c.sp.Stop(c.name); err != nil {
				t.Fatalf("stopping fake runtime: %v", err)
			}
			if tc.closed {
				if err := c.store.Close(c.bead.ID); err != nil {
					t.Fatalf("closing session bead: %v", err)
				}
			}
			c.useFreshReadingRuntime(t)
			var stdout, stderr bytes.Buffer
			if code := cmdSessionKillWithForce([]string{c.bead.ID}, "inactive-1", &stdout, &stderr, true, false); code != 1 {
				t.Fatalf("kill = %d, want 1; stderr=%s", code, stderr.String())
			}
			op := cliOpOnly(t, h.opStore)
			if op.Status != opjournal.StatusClosed || op.Outcome.Arm != opjournal.ArmFailed || op.Outcome.Reason != tc.reason {
				t.Fatalf("op outcome = %+v, want failed{%s}", op.Outcome, tc.reason)
			}
			stdout.Reset()
			stderr.Reset()
			if code := cmdSessionKillWithForce([]string{c.bead.ID}, "inactive-1", &stdout, &stderr, true, false); code != 1 {
				t.Fatalf("same-key rerun = %d, want 1 (the failure replayed); stderr=%s", code, stderr.String())
			}
			if got := cliOpFailureCode(t, stdout.Bytes()); got != cliOpCodeAlreadyFailed {
				t.Fatalf("failure code = %q, want %s", got, cliOpCodeAlreadyFailed)
			}
		})
	}
}

// cliOpCloseHookStore is an op store that runs onClose as an op row closes.
type cliOpCloseHookStore struct {
	*beads.MemStore
	onClose func()
}

func (s *cliOpCloseHookStore) UpdateIfMatch(id string, rev int64, opts beads.UpdateOpts) error {
	if opts.Status != nil && *opts.Status == "closed" && s.onClose != nil {
		s.onClose()
	}
	return s.MemStore.UpdateIfMatch(id, rev, opts)
}

func (s *cliOpCloseHookStore) CloseIfMatch(id string, rev int64) error {
	if s.onClose != nil {
		s.onClose()
	}
	return s.MemStore.CloseIfMatch(id, rev)
}

// A kill closes its op only once it has released the runtime lease, so the
// op store write never runs under the lease: on success, on a failed kill,
// and on a failed circuit-breaker reset. A kill that stopped the runtime has
// its lease ended by the fenced kill already; one that found the runtime
// gone still holds it when the kill returns.
func TestSessionKillOpClosesAfterTheLeaseIsReleased(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta map[string]string
		// failBreaker runs a managed controller that fails the reset.
		failBreaker bool
		wantCode    int
	}{
		{name: "named runtime gone", meta: map[string]string{"state": "asleep"}, wantCode: 0},
		{
			name:     "unnamed runtime gone fails",
			meta:     map[string]string{"alias": "", namedSessionMetadataKey: "", namedSessionIdentityMetadata: "", "state": "asleep"},
			wantCode: 1,
		},
		{name: "breaker reset fails", meta: map[string]string{"state": "asleep"}, failBreaker: true, wantCode: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
			c := newCLIOpCity(t, h, tc.meta)
			if err := c.sp.Stop(c.name); err != nil {
				t.Fatalf("stopping fake runtime: %v", err)
			}
			c.useFreshReadingRuntime(t)
			var ctl *cliOpController
			if tc.failBreaker {
				ctl = startCLIOpController(t, c.dir)
				ctl.failBreakerReset.Store(true)
			}
			hook := &cliOpCloseHookStore{MemStore: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}}
			h.opStore = hook
			closes := 0
			hook.onClose = func() {
				closes++
				lease, err := session.TryRuntimeLease(nil, session.RuntimeLeaseRequest{City: c.dir, Name: c.name})
				if err != nil {
					t.Errorf("the kill closed its op while holding the runtime lease: %v", err)
					return
				}
				lease.Release()
			}
			var stdout, stderr bytes.Buffer
			if code := cmdSessionKillWithForce([]string{c.bead.ID}, "", &stdout, &stderr, false, false); code != tc.wantCode {
				t.Fatalf("kill = %d, want %d; stderr=%s", code, tc.wantCode, stderr.String())
			}
			if closes == 0 {
				t.Fatal("the op was never closed")
			}
			if ctl != nil && ctl.breakerResets.Load() == 0 {
				t.Fatal("the kill never asked the controller to reset the circuit breaker")
			}
		})
	}
}

// A close closes its op only after its cleanup: the session's claims are
// released by the time the op reads succeeded.
func TestSessionCloseOpClosesAfterItsCleanup(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, nil)
	claim, err := c.store.Create(beads.Bead{Title: "claim", Type: "task", Assignee: c.bead.ID, Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "session-a"}})
	if err != nil {
		t.Fatalf("creating claim: %v", err)
	}
	inProgress := "in_progress"
	if err := c.store.Update(claim.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("claiming: %v", err)
	}
	hook := &cliOpCloseHookStore{MemStore: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}}
	h.opStore = hook
	closes := 0
	hook.onClose = func() {
		closes++
		got, err := c.reload(t).Get(claim.ID)
		if err != nil {
			t.Errorf("reading claim: %v", err)
			return
		}
		if got.Assignee == c.bead.ID {
			t.Errorf("the close closed its op before releasing the session's claim %s", claim.ID)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{c.bead.ID}, "", &stdout, &stderr, false); code != 0 {
		t.Fatalf("close = %d; stderr=%s", code, stderr.String())
	}
	if closes == 0 {
		t.Fatal("the op was never closed")
	}
}

// cliOpWakeCase is the wake verb case of the matrix.
func cliOpWakeCase(t *testing.T) cliOpVerbCase {
	t.Helper()
	for _, tc := range cliOpVerbCases() {
		if tc.kind == opjournal.KindWake {
			return tc
		}
	}
	t.Fatal("no wake case")
	return cliOpVerbCase{}
}

// A city config that does not load leaves its [ops] gates unread: the verb
// says so, each gate takes its builtin default, and a gate set in the
// environment still applies.
func TestSessionVerbOpConfigLoadFailure(t *testing.T) {
	const broken = "\n[ops]\nlifecycle = \"require\"\ncontent = \"bogus\"\n"
	t.Run("warns that the gates were not read", func(t *testing.T) {
		f := newCLIOpVerbFixture(t, cliOpWakeCase(t), nil)
		appendCityTOML(t, f.c.dir, broken)
		r := f.run("k-1", false)
		if r.code != 0 {
			t.Fatalf("wake = %d; stderr=%s", r.code, r.stderr)
		}
		if !strings.Contains(r.stderr, "[ops] gates were not read, so each takes its builtin default") {
			t.Fatalf("stderr = %q, want a warning that the [ops] gates were not read and take their builtin defaults", r.stderr)
		}
		// The lifecycle gate's builtin default is auto, which records only
		// over a SQLite sessions store: over this city's it runs unrecorded,
		// and the --key run says why.
		if !strings.Contains(r.stderr, "op journal open failed ("+legacyop.DegradeNotRecordedByDefault+")") {
			t.Fatalf("stderr = %q, want the builtin auto default's not-recorded warning for a --key run", r.stderr)
		}
		if ops := cliOpRows(t, f.h.opStore); len(ops) != 0 {
			t.Fatalf("op rows = %+v, want none under the builtin auto default over a non-SQLite store", ops)
		}
	})
	t.Run("an environment require still refuses", func(t *testing.T) {
		f := newCLIOpVerbFixture(t, cliOpWakeCase(t), map[string]string{"GC_OPS_LIFECYCLE": "require"})
		appendCityTOML(t, f.c.dir, broken)
		f.h.opStore = cliOpIncapableStore{f.h.opStore}
		r := f.run("", true)
		if r.code != 1 {
			t.Fatalf("wake = %d, want 1; stderr=%s", r.code, r.stderr)
		}
		if got := cliOpFailureCode(t, []byte(r.stdout)); got != cliOpCodeUnavailable {
			t.Fatalf("failure code = %q, want %s", got, cliOpCodeUnavailable)
		}
	})
	t.Run("a city that did not resolve warns too", func(t *testing.T) {
		newCLIOpHarness(t, nil)
		resolveErr := errors.New("not in a city directory")
		cfg, cfgErr := cliCityConfig("", resolveErr, io.Discard)
		if cfg != nil || !errors.Is(cfgErr, resolveErr) {
			t.Fatalf("cliCityConfig = %v, %v; want no config and the resolve error", cfg, cfgErr)
		}
		var stderr bytes.Buffer
		newCLIOp(cfg, &beads.MemStore{}, cliOpRequest{verb: "gc session wake", kind: opjournal.KindWake, cfgErr: cfgErr, stderr: &stderr})
		if !strings.Contains(stderr.String(), "[ops] gates were not read") || !strings.Contains(stderr.String(), resolveErr.Error()) {
			t.Fatalf("stderr = %q, want a warning that the [ops] gates were not read, naming %q", stderr.String(), resolveErr)
		}
	})
}

// A recorded run that fails names its op and key: on stderr, and under
// --json as the failure envelope.
func TestSessionWakeOpFailureNamesTheOpAndKey(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[asJSON], func(t *testing.T) {
			newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
			f := newCLIOpWakeFixture(t)
			if err := f.rec.SetMetadataBatch(f.id, map[string]string{
				"state":                     "creating",
				"pending_create_started_at": cliOpT0.Add(-time.Hour).Format(time.RFC3339),
			}); err != nil {
				t.Fatalf("seeding a stuck create: %v", err)
			}
			code, stdout, stderr := f.run(t, "", asJSON)
			if code != 1 {
				t.Fatalf("wake = %d, want 1; stderr=%s", code, stderr)
			}
			op := cliOpOnly(t, f.rec)
			if !strings.Contains(stderr, "op "+op.ID+" (key "+op.ClientKey+")") {
				t.Fatalf("stderr = %q, want it to name op %s and key %s", stderr, op.ID, op.ClientKey)
			}
			if !asJSON {
				if stdout != "" {
					t.Fatalf("stdout = %q, want nothing on a failed text run", stdout)
				}
				return
			}
			if got := cliOpFailureCode(t, []byte(stdout)); got != cliOpCodeFailed {
				t.Fatalf("failure code = %q, want %s", got, cliOpCodeFailed)
			}
			if m := cliOpJSON(t, []byte(stdout)); m["op_id"] != op.ID || m["key"] != op.ClientKey {
				t.Fatalf("failure envelope %v, want op_id %s and key %s", m, op.ID, op.ClientKey)
			}
		})
	}
}

// newCLIOpNamedCityWithoutRow builds a file-backed city whose configured
// named session has no row yet, on a fake runtime. It returns the city dir.
func newCLIOpNamedCityWithoutRow(t *testing.T) string {
	t.Helper()
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
	return dir
}

// cliOpSessionRows counts the city's session rows, closed ones included.
func cliOpSessionRows(t *testing.T, dir string) int {
	t.Helper()
	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	rows, err := store.List(beads.ListQuery{Label: session.LabelSession, IncludeClosed: true})
	if err != nil {
		t.Fatalf("listing session rows: %v", err)
	}
	return len(rows)
}

// Under require, a wake of a configured named session that has no row yet
// checks that the op store can record before it creates the row.
func TestSessionWakeOpRequireChecksTheStoreBeforeMaterializing(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "require"})
	dir := newCLIOpNamedCityWithoutRow(t)
	h.opStore = cliOpIncapableStore{&beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}}
	var stdout, stderr bytes.Buffer
	if code := cmdSessionWake([]string{killPokeSessionIdentity}, "", &stdout, &stderr, false); code != 1 {
		t.Fatalf("wake = %d, want 1; stderr=%s", code, stderr.String())
	}
	if n := cliOpSessionRows(t, dir); n != 0 {
		t.Fatalf("session rows = %d, want none: require refused, so nothing may be created", n)
	}
}

// Under require, a wake of a configured named session that has no row yet
// checks its --key before it creates the row: an invalid key refuses, and
// is counted, with nothing created.
func TestSessionWakeOpRequireRefusesAnInvalidKeyBeforeMaterializing(t *testing.T) {
	reader := installManualMetricReader(t)
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "require"})
	dir := newCLIOpNamedCityWithoutRow(t)
	h.opStore = &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	var stdout, stderr bytes.Buffer
	if code := cmdSessionWake([]string{killPokeSessionIdentity}, " padded", &stdout, &stderr, true); code != 1 {
		t.Fatalf("wake = %d, want 1; stderr=%s", code, stderr.String())
	}
	if got := cliOpFailureCode(t, stdout.Bytes()); got != cliOpCodeInvalidKey {
		t.Fatalf("failure code = %q, want %s; stderr=%s", got, cliOpCodeInvalidKey, stderr.String())
	}
	if n := cliOpSessionRows(t, dir); n != 0 {
		t.Fatalf("session rows = %d, want none: the key was refused, so nothing may be created", n)
	}
	want := string(opjournal.KindWake) + "|cli|admit|refused|" + legacyop.ReasonInvalidKey
	if got := cliOpRecordCounts(t, reader); got[want] != 1 {
		t.Fatalf("counts = %v, want %s counted once", got, want)
	}
}

// A wake of a configured named session whose row is closed is admitted
// before the row is reopened: a new key records the wake of the reopened
// row, a key the reopened row's conversation already holds for another
// request refuses with the row still closed, in every recording mode, and a
// key that names an earlier successful wake replays it without reopening
// anything.
func TestSessionWakeOpAdmitsBeforeReopeningANamedRow(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		t.Run(mode+" records the wake that reopens the row", func(t *testing.T) {
			h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": mode})
			c := newCLIOpCity(t, h, nil)
			c.closeForReopen(t)
			var stdout, stderr bytes.Buffer
			if code := cmdSessionWake([]string{killPokeSessionIdentity}, "reopen-2", &stdout, &stderr, true); code != 0 {
				t.Fatalf("wake = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if row := c.row(t); row.Status == "closed" {
				t.Fatal("session row still closed: an admitted wake reopens it")
			}
			op := cliOpOnly(t, h.opStore)
			if op.Kind != opjournal.KindWake || op.Conversation != c.bead.ID {
				t.Fatalf("op %+v, want the wake of the reopened row %s", op, c.bead.ID)
			}
			cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", legacyop.ViaLegacy)
			if m := cliOpJSON(t, stdout.Bytes()); m["op_id"] != op.ID || m["key"] != "reopen-2" || m["replayed"] == true {
				t.Fatalf("wake JSON %v, want op %s under key reopen-2, not replayed", m, op.ID)
			}
		})
		t.Run(mode+" refuses a key reused for a close", func(t *testing.T) {
			h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": mode})
			c := newCLIOpCity(t, h, nil)
			c.closeForReopen(t)
			planted := cliOpProposeOpen(t, h.opStore, opjournal.KindClose, c.bead.ID, "reopen-1", opjournal.Scope{}, cliOpT0, opjournal.Policy{})
			var stdout, stderr bytes.Buffer
			if code := cmdSessionWake([]string{killPokeSessionIdentity}, "reopen-1", &stdout, &stderr, true); code != 1 {
				t.Fatalf("wake = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if got := cliOpFailureCode(t, stdout.Bytes()); got != cliOpCodeKeyMismatch {
				t.Fatalf("failure code = %q, want %s; stderr=%s", got, cliOpCodeKeyMismatch, stderr.String())
			}
			if m := cliOpJSON(t, stdout.Bytes()); m["op_id"] != planted.ID || m["key"] != "reopen-1" {
				t.Fatalf("failure envelope %v, want op_id %s and key reopen-1", m, planted.ID)
			}
			if row := c.row(t); row.Status != "closed" {
				t.Fatalf("session row status = %q, want closed: the wake was refused, so nothing may be reopened", row.Status)
			}
		})
	}
	t.Run("auto replays an earlier wake without reopening", func(t *testing.T) {
		h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
		c := newCLIOpCity(t, h, nil)
		c.closeForReopen(t)
		planted := cliOpPlantClosed(t, h.opStore, opjournal.KindWake, c.bead.ID, "reopen-1", opjournal.Scope{},
			opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: legacyop.ViaLegacy}})
		var stdout, stderr bytes.Buffer
		if code := cmdSessionWake([]string{killPokeSessionIdentity}, "reopen-1", &stdout, &stderr, true); code != 0 {
			t.Fatalf("wake = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
		}
		if m := cliOpJSON(t, stdout.Bytes()); m["replayed"] != true || m["op_id"] != planted.ID {
			t.Fatalf("wake JSON %v, want the replay of op %s", m, planted.ID)
		}
		validateJSONAgainstResultSchema(t, []string{"session", "wake"}, stdout.Bytes())
		if row := c.row(t); row.Status != "closed" {
			t.Fatalf("session row status = %q, want closed: a replay acts on nothing", row.Status)
		}
		if ops := cliOpRows(t, h.opStore); len(ops) != 1 {
			t.Fatalf("op rows = %+v, want only the planted wake", ops)
		}
	})
}

// A kill replayed under --force says --force was not reapplied: it is outside
// the op's request digest.
func TestSessionKillOpReplayNamesUnreappliedForce(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	newCLIOpCity(t, h, nil)
	var stdout, stderr bytes.Buffer
	if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "force-1", &stdout, &stderr, false, false); code != 0 {
		t.Fatalf("kill = %d; stderr=%s", code, stderr.String())
	}
	stderr.Reset()
	if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "force-1", &stdout, &stderr, false, true); code != 0 {
		t.Fatalf("replayed kill = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "replayed op") || !strings.Contains(stderr.String(), "--force was not reapplied") {
		t.Fatalf("replay stderr = %q, want a note that --force was not reapplied", stderr.String())
	}
}

// The replay note is a stderr line, so it prints under --json too.
func TestSessionWakeOpJSONReplayNotesTheOpOnStderr(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	if code, _, stderr := f.run(t, "json-replay", true); code != 0 {
		t.Fatalf("wake = %d; stderr=%s", code, stderr)
	}
	code, _, stderr := f.run(t, "json-replay", true)
	op := cliOpOnly(t, f.rec)
	if code != 0 || !strings.Contains(stderr, "replayed op "+op.ID) {
		t.Fatalf("JSON replay = %d, stderr %q; want the replay note", code, stderr)
	}
}

// With the gate off --key is not read at all: an invalid one draws no
// warning.
func TestSessionWakeOpOffIgnoresAnInvalidKey(t *testing.T) {
	newCLIOpHarness(t, cliOpsOffEnv())
	f := newCLIOpWakeFixture(t)
	code, _, stderr := f.run(t, " padded", false)
	if code != 0 || stderr != "" {
		t.Fatalf("off wake with an invalid key = %d, stderr %q; want 0 and no output", code, stderr)
	}
}

// In auto, --key on a kill whose row has no generation cannot be honored:
// the kill says so rather than dropping it silently.
func TestSessionKillOpWithoutGenerationWarnsAboutTheKey(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	newCLIOpCity(t, h, map[string]string{"generation": ""})
	var stdout, stderr bytes.Buffer
	if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "nogen-1", &stdout, &stderr, false, false); code != 0 {
		t.Fatalf("kill = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "warning: --key") {
		t.Fatalf("stderr = %q, want a warning that --key was not recorded", stderr.String())
	}
}

// An op closed by someone else before this run closes it (the sweeper past
// finish_by) is reported, not dropped.
func TestSessionWakeOpLateCloseIsReported(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	f.deps.withdrawQueuedWaitNudges = func(string, []string) error {
		ops := cliOpRows(t, f.rec)
		if len(ops) != 1 {
			t.Fatalf("op rows = %d, want 1", len(ops))
		}
		st, err := opjournal.NewStore(beads.SessionStore{Store: f.rec}, &clock.Fake{Time: cliOpT0.Add(time.Hour)}, opjournal.WithIDPrefix("gc"))
		if err != nil {
			t.Fatalf("opening op store: %v", err)
		}
		if _, _, err := st.CloseExpired(ops[0], cliOpT0.Add(time.Hour), "sweeper"); err != nil {
			t.Fatalf("sweeping the op: %v", err)
		}
		return nil
	}
	code, _, stderr := f.run(t, "", false)
	if code != 0 {
		t.Fatalf("wake = %d; stderr=%s", code, stderr)
	}
	op := cliOpOnly(t, f.rec)
	if !strings.Contains(stderr, "op "+op.ID) || !strings.Contains(stderr, "already closed") {
		t.Fatalf("stderr = %q, want a note that op %s was already closed", stderr, op.ID)
	}
}

// require over a store that cannot record reports the store, even when the
// key is also invalid: the store is opened before the key is checked.
func TestSessionVerbOpRequireReportsTheStoreBeforeTheKey(t *testing.T) {
	for _, tc := range cliOpVerbCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIOpVerbFixture(t, tc, map[string]string{"GC_OPS_LIFECYCLE": "require"})
			f.h.opStore = cliOpIncapableStore{f.h.opStore}
			r := f.run(" padded", true)
			if r.code != 1 {
				t.Fatalf("%s = %d, want 1; stderr=%s", tc.name, r.code, r.stderr)
			}
			if got := tc.failureCode(t, r); got != cliOpCodeUnavailable {
				t.Fatalf("failure code = %q, want %s", got, cliOpCodeUnavailable)
			}
		})
	}
}

// cliOpRecordCounts is gc.ops.record.total by kind|surface|stage|result|reason.
func cliOpRecordCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	got := map[string]int64{}
	for _, dp := range collectCounterDataPoints(t, reader, "gc.ops.record.total") {
		var key []string
		for _, k := range []attribute.Key{"kind", "surface", "stage", "result", "unrecorded_reason"} {
			v, _ := dp.Attributes.Value(k)
			key = append(key, v.AsString())
		}
		got[strings.Join(key, "|")] += dp.Value
	}
	return got
}

// A require refusal over a store that cannot record is counted once as
// admit/refused for every verb, wake's early store check included, and
// prints no "running unrecorded" warning: nothing ran.
func TestSessionVerbOpRequireRefusalIsCounted(t *testing.T) {
	for _, tc := range cliOpVerbCases() {
		t.Run(tc.name, func(t *testing.T) {
			reader := installManualMetricReader(t)
			f := newCLIOpVerbFixture(t, tc, map[string]string{"GC_OPS_LIFECYCLE": "require"})
			f.h.opStore = cliOpIncapableStore{f.h.opStore}
			r := f.run("", false)
			if r.code != 1 {
				t.Fatalf("%s = %d, want 1; stderr=%s", tc.name, r.code, r.stderr)
			}
			if strings.Contains(r.stderr, "running unrecorded") {
				t.Fatalf("stderr = %q: a require refusal ran nothing, so it may not warn of running unrecorded", r.stderr)
			}
			got := cliOpRecordCounts(t, reader)
			want := string(tc.kind) + "|cli|admit|refused|" + legacyop.ReasonStoreUnavailable
			if got[want] != 1 {
				t.Fatalf("counts = %v, want %s counted once", got, want)
			}
		})
	}
}

// An outcome the journal refuses to store leaves the op open, and the run
// says so: the op sweeper closes it at its deadline.
func TestCLIOpRefusedOutcomeIsReported(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	var stdout, stderr bytes.Buffer
	op, start := startCLIOp(f.deps.cfg, f.rec, cliOpRequest{
		verb: "gc session wake", kind: opjournal.KindWake, conversation: f.id,
		stdout: &stdout, stderr: &stderr,
	})
	if start != cliOpExecute || !op.adm.Recorded {
		t.Fatalf("start = %v, recorded = %v; want a recorded execute", start, op.adm != nil && op.adm.Recorded)
	}
	op.finishOutcome(opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: opjournal.RefusedReasonPrefix + strings.Repeat("x", 300)})
	if !strings.Contains(stderr.String(), "op "+op.adm.Op.ID+" left open") {
		t.Fatalf("stderr = %q, want a note that op %s was left open", stderr.String(), op.adm.Op.ID)
	}
	if got := cliOpOnly(t, f.rec); got.Status == opjournal.StatusClosed {
		t.Fatalf("op = %+v, want it left open", got)
	}
}

// An op the command leaves without a result closes failed{other} when the
// command ends, and the run names the op and its key as failed.
func TestCLIOpEndClosesAnUnfinishedOpFailed(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	f := newCLIOpWakeFixture(t)
	var stdout, stderr bytes.Buffer
	op, start := startCLIOp(f.deps.cfg, f.rec, cliOpRequest{
		verb: "gc session wake", kind: opjournal.KindWake, conversation: f.id, key: "end-1",
		asJSON: true, stdout: &stdout, stderr: &stderr,
	})
	if start != cliOpExecute {
		t.Fatalf("start = %v, want execute", start)
	}
	op.end()
	cliOpWantOutcome(t, cliOpOnly(t, f.rec), opjournal.ArmFailed, opjournal.ReasonOther, "")
	if got := cliOpFailureCode(t, stdout.Bytes()); got != cliOpCodeFailed {
		t.Fatalf("failure code = %q, want %s", got, cliOpCodeFailed)
	}
	if !strings.Contains(stderr.String(), "key end-1") {
		t.Fatalf("stderr = %q, want the op's key named", stderr.String())
	}
}

// A malformed gate in the environment is ignored with a warning, not
// silently.
func TestSessionWakeOpWarnsAboutAMalformedEnvGate(t *testing.T) {
	newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "requir"})
	f := newCLIOpWakeFixture(t)
	code, _, stderr := f.run(t, "", false)
	if code != 0 || !strings.Contains(stderr, "GC_OPS_LIFECYCLE") {
		t.Fatalf("wake = %d, stderr %q; want a warning naming GC_OPS_LIFECYCLE", code, stderr)
	}
}

// A kill recorded for one generation stops only that generation. Here a
// controller start holds the runtime lease while the kill reads the row
// (generation 4), records its op and asks for the lease; while the kill
// waits, the start mints generation 5 and lets the lease go. The kill then
// stops nothing: it closes the op failed{refused:target_moved}, still scoped
// to generation 4, once it has released the lease. A same-key rerun replays
// that failure, noting the restart, and a rerun without the key kills
// generation 5.
func TestSessionKillOpRefusesAGenerationThatMovedBeforeTheLease(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, nil)
	hook := &cliOpCloseHookStore{MemStore: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}}
	h.opStore = hook
	hook.onClose = func() {
		lease, err := session.TryRuntimeLease(nil, session.RuntimeLeaseRequest{City: c.dir, Name: c.name})
		if err != nil {
			t.Errorf("the kill closed its op while holding the runtime lease: %v", err)
			return
		}
		lease.Release()
	}
	start, err := session.TryRuntimeLease(sessionFrontDoor(c.store), session.RuntimeLeaseRequest{
		City: c.dir, Name: c.name, ID: c.bead.ID, TTL: time.Minute,
	})
	if err != nil {
		t.Fatalf("taking the runtime lease: %v", err)
	}
	wait := sessionKillWaitLease
	t.Cleanup(func() { sessionKillWaitLease = wait })
	sessionKillWaitLease = func(ctx context.Context, front *session.Store, req session.RuntimeLeaseRequest) (*session.RuntimeLease, error) {
		sessionKillWaitLease = wait
		if held, err := session.TryRuntimeLease(front, req); !errors.Is(err, session.ErrRuntimeLeaseBusy) {
			if err == nil {
				held.Release()
			}
			t.Errorf("the kill asked for its runtime lease while the start did not hold it: %v", err)
		}
		if err := c.reload(t).SetMetadataBatch(c.bead.ID, map[string]string{"generation": "5"}); err != nil {
			t.Fatalf("minting generation 5: %v", err)
		}
		start.Release()
		return wait(ctx, front, req)
	}
	var stdout, stderr bytes.Buffer
	if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "kill-1", &stdout, &stderr, true, false); code != 1 {
		t.Fatalf("kill = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !c.sp.IsRunning(c.name) {
		t.Fatal("the kill stopped generation 5, a run its op does not name")
	}
	op := cliOpOnly(t, hook)
	cliOpWantOutcome(t, op, opjournal.ArmFailed, opjournal.RefusedReasonPrefix+cliOpRefusedTargetMoved, "")
	if op.Scope.Kind != opjournal.ScopeSession || op.Scope.Generation != 4 {
		t.Fatalf("op scope = %+v, want session generation 4, the one the kill recorded", op.Scope)
	}
	if !strings.Contains(stderr.String(), "generation 5, not generation 4") || !strings.Contains(stderr.String(), "nothing was killed") {
		t.Fatalf("stderr = %q, want the move from generation 4 to 5 and that nothing was killed", stderr.String())
	}
	if got := cliOpFailureCode(t, stdout.Bytes()); got != cliOpCodeFailed {
		t.Fatalf("failure code = %q, want %s", got, cliOpCodeFailed)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "kill-1", &stdout, &stderr, true, false); code != 1 {
		t.Fatalf("same-key rerun = %d, want 1; stderr=%s", code, stderr.String())
	}
	if got := cliOpFailureCode(t, stdout.Bytes()); got != cliOpCodeAlreadyFailed {
		t.Fatalf("same-key rerun failure code = %q, want %s", got, cliOpCodeAlreadyFailed)
	}
	if !strings.Contains(stderr.String(), "restarted as generation 5") {
		t.Fatalf("same-key rerun stderr = %q, want the restart as generation 5 noted", stderr.String())
	}
	if !c.sp.IsRunning(c.name) {
		t.Fatal("a same-key rerun killed generation 5")
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionKillWithForce([]string{killPokeSessionIdentity}, "", &stdout, &stderr, true, false); code != 0 {
		t.Fatalf("kill without the key = %d, want 0; stderr=%s", code, stderr.String())
	}
	if c.sp.IsRunning(c.name) {
		t.Fatal("a kill recorded for generation 5 did not stop it")
	}
	ops := cliOpRows(t, hook)
	if len(ops) != 2 {
		t.Fatalf("op rows = %+v, want the refused kill and the kill of generation 5", ops)
	}
	for _, o := range ops {
		if o.ID == op.ID {
			continue
		}
		cliOpWantOutcome(t, o, opjournal.ArmSucceeded, "", legacyop.ViaLegacy)
		if o.Scope.Generation != 5 {
			t.Fatalf("op %s scope = %+v, want generation 5", o.ID, o.Scope)
		}
	}
}

// A wake of a closed row named by its ID is refused before its op is
// proposed, in every recording mode: it writes no op, so the key it carried
// is still free for the wake of the configured named session by alias, which
// reopens that row and records the wake.
func TestSessionWakeOpByIDOfAClosedRowWritesNoOp(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		t.Run(mode, func(t *testing.T) {
			h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": mode})
			c := newCLIOpCity(t, h, nil)
			c.closeForReopen(t)
			var stdout, stderr bytes.Buffer
			if code := cmdSessionWake([]string{c.bead.ID}, "wake-1", &stdout, &stderr, true); code != 1 {
				t.Fatalf("wake by ID = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if !strings.Contains(stderr.String(), "session "+c.bead.ID+" is closed") || stdout.Len() != 0 {
				t.Fatalf("wake by ID stdout %q stderr %q, want only the closed refusal on stderr", stdout.String(), stderr.String())
			}
			if ops := cliOpRows(t, h.opStore); len(ops) != 0 {
				t.Fatalf("op rows = %+v, want none for a wake refused at the front door", ops)
			}
			if row := c.row(t); row.Status != "closed" {
				t.Fatalf("session row status = %q, want closed", row.Status)
			}

			stdout.Reset()
			stderr.Reset()
			if code := cmdSessionWake([]string{killPokeSessionIdentity}, "wake-1", &stdout, &stderr, true); code != 0 {
				t.Fatalf("wake by alias = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if row := c.row(t); row.Status == "closed" {
				t.Fatal("session row still closed: the wake by alias reopens it")
			}
			op := cliOpOnly(t, h.opStore)
			cliOpWantOutcome(t, op, opjournal.ArmSucceeded, "", legacyop.ViaLegacy)
			if op.Kind != opjournal.KindWake || op.Conversation != c.bead.ID || op.ClientKey != "wake-1" {
				t.Fatalf("op %+v, want the wake of the reopened row %s under key wake-1", op, c.bead.ID)
			}
			if m := cliOpJSON(t, stdout.Bytes()); m["op_id"] != op.ID || m["replayed"] == true {
				t.Fatalf("wake JSON %v, want op %s, not replayed", m, op.ID)
			}
		})
	}
}

// A same-key wake retried by the session's ID after the row it woke has
// closed again replays that wake, as the retry by alias does: the key's op on
// the row answers it, so it acts on nothing and writes no op.
func TestSessionWakeOpKeyReplaysByIDAfterTheRowCloses(t *testing.T) {
	for _, mode := range []string{"auto", "require"} {
		t.Run(mode, func(t *testing.T) {
			h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": mode})
			c := newCLIOpCity(t, h, nil)
			c.closeForReopen(t)
			var stdout, stderr bytes.Buffer
			if code := cmdSessionWake([]string{killPokeSessionIdentity}, "wake-1", &stdout, &stderr, true); code != 0 {
				t.Fatalf("wake by alias = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			first := cliOpOnly(t, h.opStore)
			c.closeForReopen(t)

			stdout.Reset()
			stderr.Reset()
			if code := cmdSessionWake([]string{c.bead.ID}, "wake-1", &stdout, &stderr, true); code != 0 {
				t.Fatalf("same-key wake by ID = %d, want 0 (the replay); stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if m := cliOpJSON(t, stdout.Bytes()); m["replayed"] != true || m["op_id"] != first.ID || m["session_id"] != c.bead.ID {
				t.Fatalf("same-key wake by ID JSON %v, want the replay of op %s on %s", m, first.ID, c.bead.ID)
			}
			validateJSONAgainstResultSchema(t, []string{"session", "wake"}, stdout.Bytes())
			if !strings.Contains(stderr.String(), "replayed op "+first.ID) {
				t.Fatalf("same-key wake by ID stderr = %q, want the replay of op %s noted", stderr.String(), first.ID)
			}
			if ops := cliOpRows(t, h.opStore); len(ops) != 1 {
				t.Fatalf("op rows = %+v, want only the first wake's", ops)
			}
			if row := c.row(t); row.Status != "closed" {
				t.Fatalf("session row status = %q, want closed: a replay acts on nothing", row.Status)
			}
		})
	}
}

// A wake of a closed row named by its ID whose --key already names an op on
// that row is answered from that op, as admission answers it by alias: a
// failed wake replays its failure, a wake still in progress is reported, and
// a key another request holds is refused as that request's. None of them
// writes an op or reopens the row.
func TestSessionWakeOpByIDOfAClosedRowAnswersFromItsKeysOp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(t *testing.T, store beads.Store, conv string) opjournal.Op
		code  string
	}{{
		name: "a failed wake",
		plant: func(t *testing.T, store beads.Store, conv string) opjournal.Op {
			return cliOpPlantClosed(t, store, opjournal.KindWake, conv, "wake-1", opjournal.Scope{}, cliOpRefusedOutcome(cliOpRefusedWillNotStart))
		},
		code: cliOpCodeAlreadyFailed,
	}, {
		name: "a wake in progress",
		plant: func(t *testing.T, store beads.Store, conv string) opjournal.Op {
			return cliOpProposeOpen(t, store, opjournal.KindWake, conv, "wake-1", opjournal.Scope{}, cliOpT0, opjournal.Policy{})
		},
		code: cliOpCodeInProgress,
	}, {
		name: "a close under the key",
		plant: func(t *testing.T, store beads.Store, conv string) opjournal.Op {
			return cliOpPlantClosed(t, store, opjournal.KindClose, conv, "wake-1", opjournal.Scope{},
				opjournal.Outcome{Arm: opjournal.ArmSucceeded, Result: opjournal.Result{Via: legacyop.ViaLegacy}})
		},
		code: cliOpCodeKeyMismatch,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
			c := newCLIOpCity(t, h, nil)
			c.closeForReopen(t)
			planted := tc.plant(t, h.opStore, c.bead.ID)
			var stdout, stderr bytes.Buffer
			if code := cmdSessionWake([]string{c.bead.ID}, "wake-1", &stdout, &stderr, true); code != 1 {
				t.Fatalf("wake by ID = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			if got := cliOpFailureCode(t, stdout.Bytes()); got != tc.code {
				t.Fatalf("failure code = %q, want %s; stderr=%s", got, tc.code, stderr.String())
			}
			if m := cliOpJSON(t, stdout.Bytes()); m["op_id"] != planted.ID || m["key"] != "wake-1" {
				t.Fatalf("failure envelope %v, want op_id %s and key wake-1", m, planted.ID)
			}
			if ops := cliOpRows(t, h.opStore); len(ops) != 1 {
				t.Fatalf("op rows = %+v, want only the planted op", ops)
			}
			if row := c.row(t); row.Status != "closed" {
				t.Fatalf("session row status = %q, want closed", row.Status)
			}
		})
	}
}

// cliOpFailingOpReadStore is an op store whose reads of one op id fail.
type cliOpFailingOpReadStore struct {
	*beads.MemStore
	id string
}

func (s *cliOpFailingOpReadStore) Get(id string) (beads.Bead, error) {
	if id == s.id {
		return beads.Bead{}, errors.New("op store read failed")
	}
	return s.MemStore.Get(id)
}

// A wake of a closed row named by its ID whose --key names an op that does
// not read is refused at the front door, as for a key that names none, and
// says the key's op was not checked: it writes no op.
func TestSessionWakeOpByIDOfAClosedRowReportsAnUnreadableKeyOp(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, nil)
	c.closeForReopen(t)
	opID, err := opjournal.DeriveID("gc", c.bead.ID, legacyop.PrincipalOperator, "wake-1")
	if err != nil {
		t.Fatalf("deriving the key's op id: %v", err)
	}
	h.opStore = &cliOpFailingOpReadStore{MemStore: &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}, id: opID}
	var stdout, stderr bytes.Buffer
	if code := cmdSessionWake([]string{c.bead.ID}, "wake-1", &stdout, &stderr, true); code != 1 {
		t.Fatalf("wake by ID = %d, want 1; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "op store read failed") || !strings.Contains(stderr.String(), "--key wake-1 was not checked") {
		t.Fatalf("stderr = %q, want a warning that the read of the key's op failed", stderr.String())
	}
	if !strings.Contains(stderr.String(), "session "+c.bead.ID+" is closed") || stdout.Len() != 0 {
		t.Fatalf("stdout %q stderr %q, want the closed refusal on stderr only", stdout.String(), stderr.String())
	}
	if ops := cliOpRows(t, h.opStore); len(ops) != 0 {
		t.Fatalf("op rows = %+v, want none", ops)
	}
}

// A same-key rerun of a close replays it only by the session's ID. Closed by
// the alias of a configured named session, the session says so on stderr;
// once a wake has created the named session again under a new ID, a rerun by
// the alias closes that new session under an op of its own, and a rerun by
// the first session's ID replays the first close.
func TestSessionCloseOpKeyReplaysOnlyByID(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, nil)
	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{killPokeSessionIdentity}, "close-1", &stdout, &stderr, true); code != 0 {
		t.Fatalf("close = %d; stderr=%s", code, stderr.String())
	}
	first := cliOpOnly(t, h.opStore)
	if !strings.Contains(stderr.String(), "replays this run only by the session ID "+c.bead.ID) {
		t.Fatalf("stderr = %q, want a note that a same-key rerun replays only by ID %s", stderr.String(), c.bead.ID)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionWake([]string{killPokeSessionIdentity}, "", &stdout, &stderr, true); code != 0 {
		t.Fatalf("wake = %d; stderr=%s", code, stderr.String())
	}
	replacement, _ := cliOpJSON(t, stdout.Bytes())["session_id"].(string)
	if replacement == "" || replacement == c.bead.ID {
		t.Fatalf("wake JSON %s, want the named session created again under a new ID", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionClose([]string{killPokeSessionIdentity}, "close-1", &stdout, &stderr, true); code != 0 {
		t.Fatalf("same-key close by alias = %d; stderr=%s", code, stderr.String())
	}
	if m := cliOpJSON(t, stdout.Bytes()); m["session_id"] != replacement || m["replayed"] == true || m["op_id"] == first.ID {
		t.Fatalf("same-key close by alias JSON %v, want a new close of %s", m, replacement)
	}
	if got, err := c.reload(t).Get(replacement); err != nil || got.Status != "closed" {
		t.Fatalf("session %s = %+v (%v), want it closed", replacement, got, err)
	}

	stdout.Reset()
	stderr.Reset()
	if code := cmdSessionClose([]string{c.bead.ID}, "close-1", &stdout, &stderr, true); code != 0 {
		t.Fatalf("same-key close by ID = %d; stderr=%s", code, stderr.String())
	}
	if m := cliOpJSON(t, stdout.Bytes()); m["replayed"] != true || m["op_id"] != first.ID {
		t.Fatalf("same-key close by ID JSON %v, want the replay of op %s", m, first.ID)
	}
}

// cliOpReopenFailStore fails the write that reopens a closed configured named
// session's row, as a write error would.
type cliOpReopenFailStore struct{ beads.Store }

func (s cliOpReopenFailStore) Tx(commitMsg string, fn func(tx beads.Tx) error) error {
	if strings.HasPrefix(commitMsg, "gc: reopen configured named session ") {
		return errors.New("reopen write failed")
	}
	return s.Store.Tx(commitMsg, fn)
}

// A wake whose op was admitted for the closed named row its resolution meant
// to reopen, but whose reopen fell through to a new row, closes that op
// failed{refused:target_moved} and records the wake under an op for the row
// it acts on, which its JSON names.
func TestSessionWakeOpRetargetsWhenTheReopenFallsThrough(t *testing.T) {
	h := newCLIOpHarness(t, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
	c := newCLIOpCity(t, h, nil)
	c.closeForReopen(t)
	cfg, err := loadCityConfig(c.dir, io.Discard)
	if err != nil {
		t.Fatalf("loading city config: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := doSessionWake(killPokeSessionIdentity, &stdout, &stderr, true, sessionWakeDeps{
		opKey:                     "move-1",
		store:                     cliOpReopenFailStore{Store: c.reload(t)},
		cfg:                       cfg,
		cityPath:                  c.dir,
		cityResolved:              true,
		now:                       time.Now,
		withdrawQueuedWaitNudges:  func(string, []string) error { return nil },
		cityUsesManagedReconciler: func(string) bool { return false },
		pokeController:            func(string, reconcilekey.Key) error { return nil },
	})
	if code != 0 {
		t.Fatalf("wake = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	m := cliOpJSON(t, stdout.Bytes())
	id, _ := m["session_id"].(string)
	if id == "" || id == c.bead.ID {
		t.Fatalf("wake JSON %v, want a new row: the reopen of %s failed", m, c.bead.ID)
	}
	if row := c.row(t); row.Status != "closed" {
		t.Fatalf("row %s status = %q, want closed: its reopen failed", c.bead.ID, row.Status)
	}
	ops := cliOpRows(t, h.opStore)
	if len(ops) != 2 {
		t.Fatalf("op rows = %+v, want the op admitted for the closed row and the wake's", ops)
	}
	byConversation := map[string]opjournal.Op{}
	for _, o := range ops {
		byConversation[o.Conversation] = o
	}
	moved, ok := byConversation[c.bead.ID]
	if !ok {
		t.Fatalf("op rows = %+v, want one for the closed row %s", ops, c.bead.ID)
	}
	cliOpWantOutcome(t, moved, opjournal.ArmFailed, opjournal.RefusedReasonPrefix+cliOpRefusedTargetMoved, "")
	woke, ok := byConversation[id]
	if !ok {
		t.Fatalf("op rows = %+v, want one for the row the wake acted on, %s", ops, id)
	}
	cliOpWantOutcome(t, woke, opjournal.ArmSucceeded, "", legacyop.ViaLegacy)
	if m["op_id"] != woke.ID || m["key"] != "move-1" {
		t.Fatalf("wake JSON %v, want op %s under key move-1", m, woke.ID)
	}
}
