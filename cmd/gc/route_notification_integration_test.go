//go:build integration

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	corepack "github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// routeClaimRuntime represents an already-running agent that executes the hook
// when the provider receives a claim turn. Only the model is replaced; claim
// selection and the SQLite ownership mutation are production code.
type routeClaimRuntime struct {
	*runtime.Fake
	claim func() error
}

func (r *routeClaimRuntime) Nudge(name string, content []runtime.ContentBlock) error {
	if err := r.Fake.Nudge(name, content); err != nil {
		return err
	}
	if !strings.Contains(runtime.FlattenText(content), "gc hook --claim") {
		return fmt.Errorf("received turn has no claim instruction")
	}
	return r.claim()
}

// A CLI subprocess cannot own the supervisor's ACP connection. Its wait-idle
// request must enter the durable queue, then be delivered by the owner.
type routeCLIACP struct{ runtime.Provider }

func (r routeCLIACP) Nudge(string, []runtime.ContentBlock) error { return runtime.ErrSessionNotFound }

// TestRouteNotificationDispatchToIdleNamedClaim owns the composition missing
// from script-only tests: actual condition dispatcher -> shell -> CLI delivery
// function -> durable queue -> supervisor nudge dispatcher -> runtime -> claim.
// A loopback CLI adapter supplies deterministic read commands and forwards
// nudge requests to the real delivery function; it is not a supervisor API mock.
func TestRouteNotificationDispatchToIdleNamedClaim(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	clearInheritedCityRoutingEnv(t)
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	work, err := beads.OpenSQLiteStore(filepath.Join(dir, "work"))
	if err != nil {
		t.Fatal(err)
	}
	if closer, ok := work.(interface{ CloseStore() error }); ok {
		defer closer.CloseStore()
	}
	claimStore, ok := work.(interface {
		Claim(string, string) (beads.Bead, bool, error)
	})
	if !ok {
		t.Fatal("SQLite store lacks claim support")
	}
	request, err := work.Create(beads.Bead{Title: "root-only routed request", Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	nudges := openNudgeBeadStore(dir)
	if nudges.Store == nil {
		t.Fatal("no nudge store")
	}
	seat, err := nudges.Create(beads.Bead{Title: "Session: worker", Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession}, Metadata: map[string]string{
		"session_name": "worker-session", "agent_name": "worker", "template": "worker", "transport": "acp", "session_origin": "named",
	}})
	if err != nil {
		t.Fatal(err)
	}
	provider := &routeClaimRuntime{Fake: runtime.NewFake()}
	if err := provider.Start(t.Context(), "worker-session", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	provider.SetActivity("worker-session", time.Now().Add(-time.Minute))
	provider.claim = func() error {
		var stdout, stderr bytes.Buffer
		code := doHookClaim("ready", dir, hookClaimOptions{Assignee: seat.ID, SessionID: seat.ID, IdentityCandidates: []string{seat.ID, "worker"}, RouteTargets: []string{"worker"}, JSON: true}, hookClaimOps{
			Runner: func(string, string) (string, error) {
				rows, err := work.List(beads.ListQuery{Status: "open"})
				if err != nil {
					return "", err
				}
				data, err := json.Marshal(rows)
				return string(data), err
			},
			Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
				return hookClaimThroughStore(id, assignee, func() (beads.Bead, bool, error) { return claimStore.Claim(id, assignee) }, work.Get)
			},
		}, &stdout, &stderr)
		if code != 0 {
			return fmt.Errorf("hook claim: %s", stderr.String())
		}
		return nil
	}
	target := nudgeTarget{cityPath: dir, agent: config.Agent{Name: "worker", Session: "acp"}, resolved: &config.ResolvedProvider{Name: "opencode"}, sessionID: seat.ID, sessionName: "worker-session", cfg: supervisorCfg()}
	bus := events.NewFake()
	bus.Record(events.Event{Type: "city.started"})
	payload, err := json.Marshal(struct {
		Bead beads.Bead `json:"bead"`
	}{request})
	if err != nil {
		t.Fatal(err)
	}
	bus.Record(events.Event{Type: events.BeadCreated, Subject: request.ID, Payload: payload})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/head":
			head, _ := bus.LatestSeq()
			_, _ = fmt.Fprintf(w, "%d\n", head)
		case "/events":
			after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
			if err != nil || after != 1 {
				http.Error(w, "expected replay after seeded cursor 1", 400)
				return
			}
			rows, _ := bus.List(events.Filter{AfterSeq: after})
			for _, event := range rows {
				if event.Seq > 1 {
					_ = json.NewEncoder(w).Encode(event)
				}
			}
		case "/work":
			rows, err := work.List(beads.ListQuery{Status: "open"})
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			_ = json.NewEncoder(w).Encode(rows)
		case "/nudge":
			if r.URL.Query().Get("target") != "worker" {
				http.Error(w, "wrong target", 400)
				return
			}
			var stdout, stderr bytes.Buffer
			code := deliverSessionNudgeWithWorker(target, nudges.Store, routeCLIACP{provider}, r.URL.Query().Get("message"), nudgeDeliveryWaitIdle, true, &stdout, &stderr)
			if code != 0 {
				http.Error(w, stderr.String(), 500)
				return
			}
			_, _ = w.Write(stdout.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	packDir := filepath.Join(dir, "core")
	stateDir := citylayout.PackStateDir(dir, "core")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "nudge-on-route-delivery.json"), []byte(`{"version":1,"cursor":1,"pending":{},"notified":{},"retry":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nudge-on-route.sh"} {
		data, err := fs.ReadFile(corepack.PackFS, "assets/scripts/"+name)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(packDir, "assets/scripts", name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	adapter := `#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$ROUTE_TEST_CALLS"
case "$1 $2" in
  'events --seq') curl -fsS "$ROUTE_TEST_URL/head" ;;
  'events --watch')
    [ "$3" = --after ]
    curl -fsS --get --data-urlencode "after=$4" "$ROUTE_TEST_URL/events" ;;
  'rig list') echo '{"rigs":[]}' ;;
  'agent list') echo '{"agents":[{"qualified_name":"worker","pool":{"min":0,"max":1},"routes_to_pool":false}]}' ;;
  ready*) curl -fsS "$ROUTE_TEST_URL/work" ;;
  'session list') echo '{"sessions":[]}' ;;
  'session nudge') curl -fsS --get --data-urlencode "target=$3" --data-urlencode "message=$4" "$ROUTE_TEST_URL/nudge" ;;
  *) echo "unexpected command: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "gc"), []byte(adapter), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))
	t.Setenv("ROUTE_TEST_URL", server.URL)
	t.Setenv("ROUTE_TEST_CALLS", filepath.Join(dir, "calls"))
	data, err := fs.ReadFile(corepack.PackFS, "orders/nudge-on-route.toml")
	if err != nil {
		t.Fatal(err)
	}
	order, err := orders.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	order.Name, order.FormulaLayer = "nudge-on-route", filepath.Join(packDir, "formulas")
	dispatcher := buildOrderDispatcherFromListExec([]orders.Order{order}, beads.NewMemStore(), bus, shellExecRunner, bus).(*memoryOrderDispatcher)
	defer dispatcher.cancel()
	dispatcher.dispatch(t.Context(), dir, time.Now())
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if !dispatcher.drain(ctx) {
		t.Fatal("order did not finish")
	}
	stateBytes, err := os.ReadFile(filepath.Join(stateDir, "nudge-on-route-delivery.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct{ Cursor uint64 }
	if err := json.Unmarshal(stateBytes, &state); err != nil || state.Cursor < 2 {
		t.Fatalf("pack-scoped cursor did not advance: %s, %v", stateBytes, err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil || !strings.Contains(string(calls), "events --watch --after 1") {
		t.Fatalf("dispatcher did not replay the seeded cursor: %s, %v", calls, err)
	}
	before, err := work.Get(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Assignee != "" {
		t.Fatalf("work claimed before the runtime received a turn: %+v", before)
	}
	delivered, err := dispatchAllQueuedNudges(dir, supervisorCfg(), nudges, nudges, provider, newSessionBeadSnapshot([]beads.Bead{seat}), nil)
	if err != nil || delivered != 1 {
		t.Fatalf("delivery = %d, %v; events: %+v", delivered, err, bus.Events)
	}
	after, err := work.Get(request.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Assignee != seat.ID || after.Status != "in_progress" {
		t.Fatalf("runtime did not claim routed root: %+v", after)
	}
	starts := 0
	for _, call := range provider.Calls {
		if call.Method == "Start" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("claim required a runtime restart: starts=%d", starts)
	}
}
