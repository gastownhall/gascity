package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/opjournal/legacyop"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
)

// cliOpRowSnap is what a legacy write would change: the session row's status
// and metadata, and whether its runtime runs.
type cliOpRowSnap struct {
	status  string
	meta    map[string]string
	running bool
}

// snap reads the session row and runtime fresh.
func (c *cliOpCity) snap(t *testing.T) cliOpRowSnap {
	t.Helper()
	b := c.row(t)
	return cliOpRowSnap{status: b.Status, meta: maps.Clone(b.Metadata), running: c.sp.IsRunning(c.name)}
}

// diff describes how s differs from want, or "" when they match.
func (s cliOpRowSnap) diff(want cliOpRowSnap) string {
	var out []string
	if s.status != want.status {
		out = append(out, fmt.Sprintf("status %q -> %q", want.status, s.status))
	}
	if s.running != want.running {
		out = append(out, fmt.Sprintf("runtime running %t -> %t", want.running, s.running))
	}
	for k, v := range s.meta {
		if want.meta[k] != v {
			out = append(out, fmt.Sprintf("%s %q -> %q", k, want.meta[k], v))
		}
	}
	for k, v := range want.meta {
		if _, ok := s.meta[k]; !ok && v != "" {
			out = append(out, fmt.Sprintf("%s %q -> unset", k, v))
		}
	}
	return strings.Join(out, ", ")
}

// useFreshReadingRuntime serves the city's runtime through a tmux-profile
// fake, which reads liveness fresh, so a kill can confirm a runtime gone.
func (c *cliOpCity) useFreshReadingRuntime(t *testing.T) {
	t.Helper()
	profiled := runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: c.sp})
	oldBuild := buildSessionProviderByName
	buildSessionProviderByName = func(*config.City, string, config.SessionConfig, string, string) (runtime.Provider, error) {
		return profiled, nil
	}
	t.Cleanup(func() { buildSessionProviderByName = oldBuild })
}

// cliOpController is a fake managed controller on the city's socket that
// counts the circuit-breaker resets it is asked for and answers every other
// command ok. With failBreakerReset set it answers a reset with a reply that
// does not decode, so the reset fails.
type cliOpController struct {
	breakerResets    atomic.Int32
	failBreakerReset atomic.Bool
}

func startCLIOpController(t *testing.T, cityDir string) *cliOpController {
	t.Helper()
	ctl := &cliOpController{}
	sockPath := controllerSocketPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(controller socket dir): %v", err)
	}
	os.Remove(sockPath) //nolint:errcheck
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(%q): %v", sockPath, err)
	}
	t.Cleanup(func() {
		lis.Close()         //nolint:errcheck
		os.Remove(sockPath) //nolint:errcheck
	})
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck
				scanner := bufio.NewScanner(conn)
				if !scanner.Scan() {
					return
				}
				line := scanner.Text()
				switch {
				case line == "ping":
					fmt.Fprintf(conn, "%d\n", os.Getpid()) //nolint:errcheck
				case strings.HasPrefix(line, sessionCircuitResetCommandPrefix):
					ctl.breakerResets.Add(1)
					if ctl.failBreakerReset.Load() {
						conn.Write([]byte("not json\n")) //nolint:errcheck
						return
					}
					conn.Write([]byte(`{"outcome":"ok"}` + "\n")) //nolint:errcheck
				default:
					conn.Write([]byte("ok\n")) //nolint:errcheck
				}
			}(conn)
		}
	}()
	return ctl
}

// cliOpVerbCase is one recorded session verb, run against a cliOpCity.
type cliOpVerbCase struct {
	name   string
	kind   opjournal.Kind
	scope  opjournal.Scope
	policy opjournal.Policy
	// meta overrides the city row's metadata, so the verb has work to do.
	meta map[string]string
	// setup stubs the verb's collaborators; it may return the controller.
	setup func(t *testing.T, c *cliOpCity) *cliOpController
	run   func(c *cliOpCity, key string, asJSON bool, stdout, stderr io.Writer) int
	// noJSON marks a verb with no --json: its refusals are checked by exit
	// code and effect alone.
	noJSON bool
	// replayStdout, when set, is what a replay prints instead of the first
	// run's output: a restart request's first run reports the poke, which a
	// replay does not repeat.
	replayStdout string
	// schema is the verb's result schema path; empty is session <name>.
	schema []string
	// keyNamespace is the verb's --key namespace (cliOpRequest.keyNamespace):
	// its op is stored under keyNamespace + --key.
	keyNamespace string
}

// resultSchema is the verb's result schema path.
func (tc cliOpVerbCase) resultSchema() []string {
	if tc.schema != nil {
		return tc.schema
	}
	return []string{"session", tc.name}
}

// cliOpRestartToken is the instance token the restart verbs' row records and
// their caller runs under (SG-3's fence).
const cliOpRestartToken = "tok-cli-op-restart"

// inSessionRestart runs a restart verb from inside the city's session: its
// alias, runtime name and incarnation.
func inSessionRestart(t *testing.T, c *cliOpCity) *cliOpController {
	t.Helper()
	t.Setenv("GC_ALIAS", killPokeSessionIdentity)
	t.Setenv("GC_TMUX_SESSION", "")
	t.Setenv("GC_SESSION_NAME", c.name)
	t.Setenv("GC_INSTANCE_TOKEN", cliOpRestartToken)
	return nil
}

// runHandoffVerb runs a self handoff as gc handoff [--key] [--json] would.
func runHandoffVerb(_ *cliOpCity, key string, asJSON bool, stdout, stderr io.Writer) int {
	out := stdout
	if asJSON {
		out = io.Discard
	}
	return cmdHandoff(handoffRun{args: []string{"context cycle"}, key: key, asJSON: asJSON, stdout: out, jsonOut: stdout, stderr: stderr})
}

// cliOpVerbCases are the recorded lifecycle verbs (DES-O0API §8.4), the
// in-session restart requests (OJ-6) included.
func cliOpVerbCases() []cliOpVerbCase {
	return []cliOpVerbCase{
		{
			name: "wake", kind: opjournal.KindWake,
			meta: map[string]string{"state": "suspended", "held_until": cliOpT0.Add(24 * time.Hour).Format(time.RFC3339), "sleep_reason": "user-hold"},
			run: func(c *cliOpCity, key string, asJSON bool, stdout, stderr io.Writer) int {
				return cmdSessionWake([]string{c.bead.ID}, key, stdout, stderr, asJSON)
			},
		},
		{
			name: "suspend", kind: opjournal.KindSuspend,
			setup: func(t *testing.T, _ *cliOpCity) *cliOpController {
				stubSessionSuspendController(t, false)
				return nil
			},
			run: func(c *cliOpCity, key string, asJSON bool, stdout, stderr io.Writer) int {
				return cmdSessionSuspend([]string{c.bead.ID}, key, stdout, stderr, asJSON)
			},
		},
		{
			name: "close", kind: opjournal.KindClose,
			run: func(c *cliOpCity, key string, asJSON bool, stdout, stderr io.Writer) int {
				return cmdSessionClose([]string{c.bead.ID}, key, stdout, stderr, asJSON)
			},
		},
		{
			name: "kill", kind: opjournal.KindKill, scope: opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 4},
			run: func(c *cliOpCity, key string, asJSON bool, stdout, stderr io.Writer) int {
				return cmdSessionKillWithForce([]string{c.bead.ID}, key, stdout, stderr, asJSON, false)
			},
		},
		{
			name: "reset", kind: opjournal.KindRestart,
			policy: opjournal.Policy{Restart: opjournal.RestartPolicy{Mode: opjournal.RestartFresh}},
			setup: func(t *testing.T, c *cliOpCity) *cliOpController {
				return startCLIOpController(t, c.dir)
			},
			run: func(c *cliOpCity, key string, asJSON bool, stdout, stderr io.Writer) int {
				return cmdSessionReset([]string{c.bead.ID}, key, stdout, stderr, asJSON)
			},
		},
		{
			name: "request-restart", kind: opjournal.KindRestart, policy: restartOpPolicy,
			meta:  map[string]string{"instance_token": cliOpRestartToken},
			setup: inSessionRestart, noJSON: true, replayStdout: "Restart requested.\n",
			keyNamespace: legacyop.KeyNamespaceRequestRestart,
			run: func(_ *cliOpCity, key string, _ bool, stdout, stderr io.Writer) int {
				return cmdRuntimeRequestRestart(key, stdout, stderr)
			},
		},
		{
			name: "handoff", kind: opjournal.KindRestart, policy: restartOpPolicy,
			meta:  map[string]string{"instance_token": cliOpRestartToken, namedSessionModeMetadata: "always"},
			setup: inSessionRestart, replayStdout: "Handoff: restart requested.\n", schema: []string{"handoff"},
			keyNamespace: legacyop.KeyNamespaceHandoff,
			run:          runHandoffVerb,
		},
	}
}

// stubSessionSuspendController pins the suspend path: managed (held_until
// metadata, enqueue) or direct (the worker handle's Stop).
func stubSessionSuspendController(t *testing.T, managed bool) {
	t.Helper()
	oldManaged, oldPoke, oldEnqueue := sessionSuspendManagedReconciler, sessionSuspendPokeController, sessionSuspendEnqueueController
	sessionSuspendManagedReconciler = func(string) bool { return managed }
	sessionSuspendPokeController = func(string) error { return nil }
	sessionSuspendEnqueueController = func(string, reconcilekey.Key) error { return nil }
	t.Cleanup(func() {
		sessionSuspendManagedReconciler, sessionSuspendPokeController, sessionSuspendEnqueueController = oldManaged, oldPoke, oldEnqueue
	})
}

// failureCode is a refused run's failure code: its --json envelope's, or,
// for a verb with no --json, the code its stderr line implies.
func (tc cliOpVerbCase) failureCode(t *testing.T, r cliOpVerbRun) string {
	t.Helper()
	if !tc.noJSON {
		return cliOpFailureCode(t, []byte(r.stdout))
	}
	switch {
	case strings.Contains(r.stderr, "in progress"):
		return cliOpCodeInProgress
	case strings.Contains(r.stderr, " closed failed"):
		return cliOpCodeAlreadyFailed
	case strings.Contains(r.stderr, "op journal cannot record this request"):
		return cliOpCodeUnavailable
	case strings.Contains(r.stderr, "invalid --key"):
		return cliOpCodeInvalidKey
	}
	t.Fatalf("stderr %q names no op refusal", r.stderr)
	return ""
}

// cliOpVerbRun is one verb run's result.
type cliOpVerbRun struct {
	code           int
	stdout, stderr string
}

// cliOpVerbFixture is a city set up for one verb case.
type cliOpVerbFixture struct {
	tc  cliOpVerbCase
	h   *cliOpHarness
	c   *cliOpCity
	ctl *cliOpController
}

func newCLIOpVerbFixture(t *testing.T, tc cliOpVerbCase, env map[string]string) *cliOpVerbFixture {
	t.Helper()
	h := newCLIOpHarness(t, env)
	c := newCLIOpCity(t, h, tc.meta)
	f := &cliOpVerbFixture{tc: tc, h: h, c: c}
	if tc.setup != nil {
		f.ctl = tc.setup(t, c)
	}
	return f
}

func (f *cliOpVerbFixture) run(key string, asJSON bool) cliOpVerbRun {
	var stdout, stderr bytes.Buffer
	code := f.tc.run(f.c, key, asJSON, &stdout, &stderr)
	return cliOpVerbRun{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

// proposeOpen writes an open op for the verb under --key key, as another CLI
// process mid-run would leave it.
func (f *cliOpVerbFixture) proposeOpen(t *testing.T, key string, at time.Time) opjournal.Op {
	t.Helper()
	return cliOpProposeOpen(t, f.h.opStore, f.tc.kind, f.c.bead.ID, legacyop.NamespacedKey(f.tc.keyNamespace, key), f.tc.scope, at, f.tc.policy)
}

// TestSessionVerbOpOrderAtPropose: every verb proposes its op before its
// first legacy effect. When the recorder opens the op store the row reads as
// it did before the command ran, field for field, the runtime is as it was,
// and the controller has been asked for no circuit-breaker reset.
func TestSessionVerbOpOrderAtPropose(t *testing.T) {
	for _, tc := range cliOpVerbCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIOpVerbFixture(t, tc, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
			before := f.c.snap(t)
			opened := false
			f.h.onOpen = func() {
				opened = true
				if d := f.c.snap(t).diff(before); d != "" {
					t.Errorf("legacy effect before the op was proposed: %s", d)
				}
				if f.ctl != nil && f.ctl.breakerResets.Load() != 0 {
					t.Errorf("the controller reset the circuit breaker before the op was proposed")
				}
			}
			if r := f.run("", true); r.code != 0 {
				t.Fatalf("%s = %d; stderr=%s", tc.name, r.code, r.stderr)
			}
			if !opened {
				t.Fatal("the op store was never opened")
			}
			op := cliOpOnly(t, f.h.opStore)
			if op.Kind != tc.kind || op.Status != opjournal.StatusClosed {
				t.Fatalf("op = %+v, want a closed %s", op, tc.kind)
			}
		})
	}
}

// TestSessionResetOpRollbackOrderAtPropose: the rollback path proposes its op
// before it rolls the pending create back.
func TestSessionResetOpRollbackOrderAtPropose(t *testing.T) {
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
	ctl := startCLIOpController(t, c.dir)
	before := c.snap(t)
	opened := false
	h.onOpen = func() {
		opened = true
		if d := c.snap(t).diff(before); d != "" {
			t.Errorf("legacy effect before the op was proposed: %s", d)
		}
		if ctl.breakerResets.Load() != 0 {
			t.Errorf("the controller reset the circuit breaker before the op was proposed")
		}
	}
	var stdout, stderr bytes.Buffer
	if code := cmdSessionReset([]string{c.bead.ID}, "", &stdout, &stderr, true); code != 0 {
		t.Fatalf("reset = %d; stderr=%s", code, stderr.String())
	}
	if !opened {
		t.Fatal("the op store was never opened")
	}
	if m := cliOpJSON(t, stdout.Bytes()); m["mode"] != "rollback" {
		t.Fatalf("JSON %v, want a rollback", m)
	}
}

// TestSessionVerbOpOffWritesNothing: with the gate off a verb opens no op
// store, writes no op row and adds nothing to its JSON.
func TestSessionVerbOpOffWritesNothing(t *testing.T) {
	for _, tc := range cliOpVerbCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIOpVerbFixture(t, tc, cliOpsOffEnv())
			r := f.run("", true)
			if r.code != 0 {
				t.Fatalf("%s = %d; stderr=%s", tc.name, r.code, r.stderr)
			}
			if f.h.opens != 0 {
				t.Fatalf("op store opened %d times with the gate off", f.h.opens)
			}
			if ops := cliOpRows(t, f.h.opStore); len(ops) != 0 {
				t.Fatalf("op rows = %+v, want none", ops)
			}
			if tc.noJSON {
				return
			}
			m := cliOpJSON(t, []byte(r.stdout))
			for _, k := range []string{"op_id", "key", "replayed"} {
				if _, ok := m[k]; ok {
					t.Fatalf("JSON %v carries %q with the gate off", m, k)
				}
			}
		})
	}
}

// TestSessionVerbOpKeyReplaysWithoutActing: a same-key rerun of a successful
// run prints the first run's output, names the op, and does not act again.
func TestSessionVerbOpKeyReplaysWithoutActing(t *testing.T) {
	for _, tc := range cliOpVerbCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIOpVerbFixture(t, tc, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
			first := f.run("replay-"+tc.name, false)
			if first.code != 0 {
				t.Fatalf("first %s = %d; stderr=%s", tc.name, first.code, first.stderr)
			}
			after := f.c.snap(t)
			second := f.run("replay-"+tc.name, false)
			op := cliOpOnly(t, f.h.opStore)
			wantStdout := first.stdout
			if tc.replayStdout != "" {
				wantStdout = tc.replayStdout
			}
			if second.code != 0 || second.stdout != wantStdout {
				t.Fatalf("replay = %d, stdout %q; want 0 and %q (stderr %s)", second.code, second.stdout, wantStdout, second.stderr)
			}
			if !strings.Contains(second.stderr, "replayed op "+op.ID) {
				t.Fatalf("replay stderr = %q, want it to name op %s", second.stderr, op.ID)
			}
			if d := f.c.snap(t).diff(after); d != "" {
				t.Fatalf("a replay acted again: %s", d)
			}
			if tc.noJSON {
				return
			}
			js := f.run("replay-"+tc.name, true)
			m := cliOpJSON(t, []byte(js.stdout))
			if js.code != 0 || m["replayed"] != true || m["op_id"] != op.ID || m["key"] != "replay-"+tc.name {
				t.Fatalf("JSON replay = %d %v, want replayed op %s", js.code, m, op.ID)
			}
			validateJSONAgainstResultSchema(t, tc.resultSchema(), []byte(js.stdout))
		})
	}
}

// TestSessionVerbOpInProgressRefuses: an open op under the key, inside its
// bound, is another run in progress: exit 1 with no effect, the op untouched.
func TestSessionVerbOpInProgressRefuses(t *testing.T) {
	for _, tc := range cliOpVerbCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIOpVerbFixture(t, tc, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
			open := f.proposeOpen(t, "busy", cliOpT0.Add(-time.Minute))
			before := f.c.snap(t)
			r := f.run("busy", true)
			if r.code != 1 {
				t.Fatalf("%s = %d, want 1 for an op in progress; stderr=%s", tc.name, r.code, r.stderr)
			}
			if got := tc.failureCode(t, r); got != cliOpCodeInProgress {
				t.Fatalf("failure code = %q, want %s", got, cliOpCodeInProgress)
			}
			if d := f.c.snap(t).diff(before); d != "" {
				t.Fatalf("an op in progress acted: %s", d)
			}
			if op := cliOpOnly(t, f.h.opStore); op.ID != open.ID || op.Status == opjournal.StatusClosed {
				t.Fatalf("op = %+v, want %s left open", op, open.ID)
			}
		})
	}
}

// TestSessionVerbOpExpiredOpenOpReplaysFailed: an open op past its bound
// closes failed{deadline_in_flight} and replays as exit 1 with no effect.
func TestSessionVerbOpExpiredOpenOpReplaysFailed(t *testing.T) {
	for _, tc := range cliOpVerbCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIOpVerbFixture(t, tc, map[string]string{"GC_OPS_LIFECYCLE": "auto"})
			open := f.proposeOpen(t, "stale", cliOpT0)
			f.h.clk.Time = cliOpT0.Add(legacyop.ExecutionBound + time.Second)
			before := f.c.snap(t)
			r := f.run("stale", true)
			if r.code != 1 {
				t.Fatalf("%s = %d, want 1; stderr=%s", tc.name, r.code, r.stderr)
			}
			if got := tc.failureCode(t, r); got != cliOpCodeAlreadyFailed {
				t.Fatalf("failure code = %q, want %s", got, cliOpCodeAlreadyFailed)
			}
			if d := f.c.snap(t).diff(before); d != "" {
				t.Fatalf("a failed replay acted: %s", d)
			}
			op := cliOpOnly(t, f.h.opStore)
			if op.ID != open.ID || op.Outcome.Reason != opjournal.ReasonDeadlineInFlight {
				t.Fatalf("op = %+v, want %s closed failed{deadline_in_flight}", op, open.ID)
			}
		})
	}
}

// TestSessionVerbOpAgentPrincipal: run from inside an agent session, the op's
// producer is that session's conversation.
func TestSessionVerbOpAgentPrincipal(t *testing.T) {
	for _, tc := range cliOpVerbCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIOpVerbFixture(t, tc, map[string]string{"GC_OPS_LIFECYCLE": "auto", "GC_SESSION_ID": "gc-agent-7"})
			if r := f.run("", false); r.code != 0 {
				t.Fatalf("%s = %d; stderr=%s", tc.name, r.code, r.stderr)
			}
			op := cliOpOnly(t, f.h.opStore)
			if op.Producer.Principal != "conversation:gc-agent-7" || op.Producer.Actor != opjournal.ActorAgent {
				t.Fatalf("producer = %+v, want conversation:gc-agent-7 / agent", op.Producer)
			}
		})
	}
}

// TestSessionVerbOpRequireRefusesAnIncapableStore: require over a store that
// cannot record refuses before any effect.
func TestSessionVerbOpRequireRefusesAnIncapableStore(t *testing.T) {
	for _, tc := range cliOpVerbCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newCLIOpVerbFixture(t, tc, map[string]string{"GC_OPS_LIFECYCLE": "require"})
			f.h.opStore = cliOpIncapableStore{f.h.opStore}
			before := f.c.snap(t)
			r := f.run("", true)
			if r.code != 1 {
				t.Fatalf("%s = %d, want 1; stderr=%s", tc.name, r.code, r.stderr)
			}
			if got := tc.failureCode(t, r); got != cliOpCodeUnavailable {
				t.Fatalf("failure code = %q, want %s", got, cliOpCodeUnavailable)
			}
			if d := f.c.snap(t).diff(before); d != "" {
				t.Fatalf("require acted without recording: %s", d)
			}
		})
	}
}
