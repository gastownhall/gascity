//go:build integration

package core

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// routeScriptFixture owns the real order/check/script boundary; the gc double
// supplies CLI responses without touching a developer's city or supervisor.
type routeScriptFixture struct {
	t     *testing.T
	dir   string
	env   []string
	order orders.Order
	bus   *events.Fake
}

func newRouteScriptFixture(t *testing.T) *routeScriptFixture {
	t.Helper()
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatal("route notification integration tests require jq")
	}
	dir := t.TempDir()
	f := &routeScriptFixture{t: t, dir: dir, order: readOrder(t, "nudge-on-route.toml"), bus: events.NewFake()}
	for _, name := range []string{"nudge-on-route.sh"} {
		body, err := fs.ReadFile(PackFS, "assets/scripts/"+name)
		if err != nil {
			t.Fatal(err)
		}
		f.write("assets/scripts/"+name, string(body), 0o755)
	}
	f.write("bin/gc", `#!/usr/bin/env bash
set -eu
[ ! -e /dev/fd/9 ] || { echo 'delivery lock leaked to a child process' >&2; exit 2; }
printf '%s\n' "$*" >> "$FIXTURE/calls"
case "$1 $2" in
  'events --seq')
    if [ -p "$FIXTURE/blocked-seq" ]; then
      echo entered > "$FIXTURE/seq-entered"
      read -r release < "$FIXTURE/blocked-seq"
    fi
    jq -s 'map(.seq) | max // 0' "$FIXTURE/events" ;;
  events*)
    [ ! -f "$FIXTURE/fail-read" ] || exit 1
    after=0
    while [ "$#" -gt 0 ]; do
      if [ "$1" = --after ]; then after="$2"; shift; fi
      shift
    done
    jq -c --argjson after "$after" 'select(.seq > $after)' "$FIXTURE/events"
    if [ -f "$FIXTURE/truncated" ]; then echo 'gc events: truncated event walk' >&2; fi
    ;;
  'rig list') echo '{"rigs":[]}' ;;
  'bd show') jq --arg id "$3" '[.[] | select(.id == $id)]' "$FIXTURE/beads" ;;
  ready*)
    [ ! -f "$FIXTURE/fail-scan" ] || exit 1
    case "$*" in
      *--status=open*) ;;
      *) if [ -f "$FIXTURE/blocked" ]; then echo '[]'; exit 0; fi ;;
    esac
    jq '[.[] | select(.status == "open" and (.assignee // "") == "" and (.metadata."gc.routed_to" // "") != "")]' "$FIXTURE/beads"
    ;;
  'agent list')
    [ ! -f "$FIXTURE/fail-agent-list" ] || exit 1
    cat "$FIXTURE/agents" ;;
  'session list')
    [ ! -f "$FIXTURE/fail-session-list" ] || exit 1
    cat "$FIXTURE/sessions" ;;
  'session nudge')
    [ ! -f "$FIXTURE/fail-nudge" ] || exit 1
    if [ -p "$FIXTURE/entered" ]; then
      echo entered > "$FIXTURE/entered"
      read -r release < "$FIXTURE/release"
    fi
    printf '%s\n' "$3" >> "$FIXTURE/nudges"
    ;;
  *) echo "unexpected gc command: $*" >&2; exit 2 ;;
esac
`, 0o755)
	f.write("events", "", 0o600)
	f.write("beads", "[]", 0o600)
	f.write("sessions", `{"sessions":[]}`, 0o600)
	f.write("agents", `{"agents":[{"qualified_name":"worker","pool":{"min":0,"max":1},"routes_to_pool":false}]}`, 0o600)
	f.env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "FIXTURE="+dir,
		"PACK_DIR="+dir, "GC_CITY="+dir, "GC_PACK_STATE_DIR="+filepath.Join(dir, "state"))
	return f
}

func (f *routeScriptFixture) write(name, body string, mode os.FileMode) {
	f.t.Helper()
	path := filepath.Join(f.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *routeScriptFixture) seed(eventType string, nested bool) {
	f.t.Helper()
	bead := `{"id":"test-work","status":"open","assignee":"","metadata":{"gc.routed_to":"worker"}}`
	f.write("beads", "["+bead+"]", 0o600)
	payload := bead
	if nested {
		payload = `{"bead":` + bead + `}`
	}
	e := events.Event{Type: eventType, Subject: "test-work", Payload: json.RawMessage(payload)}
	f.bus.Record(e)
	f.syncEvents()
}

func (f *routeScriptFixture) syncEvents() {
	f.t.Helper()
	var lines strings.Builder
	for _, event := range f.bus.Events {
		body, err := json.Marshal(event)
		if err != nil {
			f.t.Fatal(err)
		}
		lines.Write(body)
		lines.WriteByte('\n')
	}
	f.write("events", lines.String(), 0o600)
}

func (f *routeScriptFixture) due() bool {
	f.t.Helper()
	result := orders.CheckTriggerWithOptions(f.order, time.Now(), nil, f.bus, nil, orders.TriggerOptions{
		ConditionCtx: f.t.Context(), ConditionDir: f.dir, ConditionEnv: f.env, ConditionTimeout: f.order.CheckTimeoutOrDefault(),
	})
	return result.Due
}

func (f *routeScriptFixture) run() error {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", "-c", f.order.Exec)
	cmd.Dir, cmd.Env = f.dir, f.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Logf("order exec: %v\n%s", err, out)
	}
	return err
}

func (f *routeScriptFixture) nudges() string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, "nudges"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func TestRouteNotificationCreationTriggersDelivery(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "flat", true: "nested"}[nested], func(t *testing.T) {
			f := newRouteScriptFixture(t)
			f.bus.Record(events.Event{Type: "city.started"})
			f.write("state/nudge-on-route-delivery.json", `{"version":1,"cursor":1,"pending":{},"notified":{},"retry":{}}`, 0o600)
			f.seed("bead.created", nested)
			if !f.due() {
				t.Fatal("a creation-only route did not trigger the shipped order")
			}
			if err := f.run(); err != nil {
				t.Fatal(err)
			}
			if got := f.nudges(); got != "worker\n" {
				t.Fatalf("nudges = %q, want worker", got)
			}
			if err := f.run(); err != nil {
				t.Fatal(err)
			}
			if got := f.nudges(); got != "worker\n" {
				t.Fatalf("replay duplicated notification: %q", got)
			}
		})
	}
}

// A killed probe must hand recovery to exec on the next pass, even if the
// event service remains slow and no additional event arrives.
func TestRouteNotificationInterruptedCheckSchedulesRecovery(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.bus.Record(events.Event{Type: "city.started"})
	f.write("state/nudge-on-route-delivery.json", `{"version":1,"cursor":1,"pending":{},"notified":{},"retry":{}}`, 0o600)
	f.seed("bead.created", true)
	for _, name := range []string{"blocked-seq", "seq-entered"} {
		if err := unix.Mkfifo(filepath.Join(f.dir, name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entered, err := os.OpenFile(filepath.Join(f.dir, "seq-entered"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer entered.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan orders.TriggerResult, 1)
	go func() {
		results <- orders.CheckTriggerWithOptions(f.order, time.Now(), nil, f.bus, nil, orders.TriggerOptions{
			ConditionCtx: ctx, ConditionDir: f.dir, ConditionEnv: f.env, ConditionTimeout: 30 * time.Second,
		})
	}()
	ready := make(chan error, 1)
	go func() { _, err := bufio.NewReader(entered).ReadString('\n'); ready <- err }()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("condition never reached the event probe")
	}
	cancel() // same process-group cancellation path as the check deadline
	if result := <-results; result.Due {
		t.Fatalf("interrupted probe result = %+v, want not due", result)
	}
	before, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.due() {
		t.Fatal("interrupted probe did not schedule recovery without another API read")
	}
	after, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil || string(before) != string(after) {
		t.Fatalf("recovery check repeated remote reads: before=%s after=%s err=%v", before, after, err)
	}
	if err := os.Remove(filepath.Join(f.dir, "blocked-seq")); err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\n" {
		t.Fatalf("recovery nudges = %q", got)
	}
	if f.due() {
		t.Fatal("successful recovery left a continuously due probe")
	}
}

func TestRouteNotificationRetriesWithoutNewEvents(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.updated", true)
	f.write("fail-nudge", "", 0o600)
	if err := f.run(); err == nil {
		t.Fatal("failed delivery was reported as success")
	}
	if err := os.Remove(filepath.Join(f.dir, "fail-nudge")); err != nil {
		t.Fatal(err)
	}
	if f.due() {
		t.Fatal("failed target retries without backoff")
	}
	f.expireRetry()
	if !f.due() {
		t.Fatal("failed delivery has no retry without unrelated events")
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\n" {
		t.Fatalf("retry nudges = %q", got)
	}
}

func TestRouteNotificationIncompleteSnapshotUsesLiveWork(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.bus.Record(events.Event{Type: "city.started"})
	f.write("state/nudge-on-route-delivery.json", `{"version":1,"cursor":1,"pending":{"test-work":"worker"},"notified":{},"retry":{}}`, 0o600)
	f.write("beads", `[{"id":"test-work","status":"open","metadata":{"gc.routed_to":"worker"}}]`, 0o600)
	f.bus.Record(events.Event{Type: "bead.updated", Subject: "test-work", Payload: json.RawMessage(`{"id":"test-work"}`)})
	f.syncEvents()
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\n" {
		t.Fatalf("incomplete event discarded pending live work: %q", got)
	}
}

func TestRouteNotificationSkipsClaimedAndClosed(t *testing.T) {
	for _, current := range []string{
		`{"status":"closed","assignee":""}`,
		`{"status":"open","assignee":"another-session"}`,
	} {
		t.Run(current, func(t *testing.T) {
			f := newRouteScriptFixture(t)
			f.seed("bead.updated", true)
			f.write("beads", "["+strings.TrimSuffix(current, "}")+`,"id":"test-work","metadata":{"gc.routed_to":"worker"}}]`, 0o600)
			if err := f.run(); err != nil {
				t.Fatal(err)
			}
			if got := f.nudges(); got != "" {
				t.Fatalf("ineligible work was nudged: %q", got)
			}
		})
	}
}

func TestRouteNotificationFailedReadKeepsCursor(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.seed("bead.updated", false)
	f.write("fail-read", "", 0o600)
	f.write("fail-scan", "", 0o600)
	before, err := os.ReadFile(filepath.Join(f.dir, "state/nudge-on-route-delivery.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !f.due() {
		t.Fatal("read failure must reach exec so the controller records it")
	}
	if err := f.run(); err == nil {
		t.Fatal("event read failure was swallowed")
	}
	after, err := os.ReadFile(filepath.Join(f.dir, "state/nudge-on-route-delivery.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oldState, newState struct {
		Cursor int `json:"cursor"`
	}
	if err := json.Unmarshal(before, &oldState); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(after, &newState); err != nil {
		t.Fatal(err)
	}
	if oldState.Cursor != newState.Cursor {
		t.Fatal("failed reads advanced the event cursor")
	}
	if err := os.Remove(filepath.Join(f.dir, "fail-read")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.dir, "fail-scan")); err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\n" {
		t.Fatalf("read recovery duplicated delivery: %q", got)
	}
}

func TestRouteNotificationRetriesFailedBootstrap(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	f.write("fail-scan", "", 0o600)
	if err := f.run(); err == nil {
		t.Fatal("failed snapshot was accepted")
	}
	if got := f.nudges(); got != "" {
		t.Fatalf("failed snapshot delivered %q", got)
	}
	if err := os.Remove(filepath.Join(f.dir, "fail-scan")); err != nil {
		t.Fatal(err)
	}
	f.expireRetry()
	if !f.due() {
		t.Fatal("incomplete bootstrap must remain due")
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\n" {
		t.Fatalf("bootstrap recovery = %q", got)
	}
}

func (f *routeScriptFixture) expireRetry() {
	f.t.Helper()
	path := filepath.Join(f.dir, "state/nudge-on-route-delivery.json")
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		f.t.Fatal(err)
	}
	for _, retry := range state["retry"].(map[string]any) {
		retry.(map[string]any)["next_at"] = 0
	}
	state["read_after"] = 0
	data, err = json.Marshal(state)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write("state/nudge-on-route-delivery.json", string(data), 0o600)
}

func TestRouteNotificationDelayedReplayAndRouteChanges(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	// Already-established cursors replay by sequence, even when the event is
	// older than any former lookback. Live state wins over the event's route.
	f.bus.Record(events.Event{
		Type: "bead.updated", Subject: "test-work", Ts: time.Now().Add(-24 * time.Hour),
		Payload: json.RawMessage(`{"id":"test-work","status":"open","metadata":{"gc.routed_to":"stale-worker"}}`),
	})
	f.syncEvents()
	f.write("beads", `[{"id":"test-work","status":"open","metadata":{"gc.routed_to":"new-worker"}}]`, 0o600)
	if !f.due() {
		t.Fatal("delayed route change did not trigger")
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.seed("bead.updated", true)
	if !f.due() {
		t.Fatal("return to original route was suppressed")
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\nnew-worker\nworker\n" {
		t.Fatalf("route changes = %q", got)
	}
	calls, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "events --watch --after 1") {
		t.Fatal("delayed replay did not use persisted sequence")
	}
}

func TestRouteNotificationReturningRouteAfterFailedReroute(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	other := `{"id":"test-work","status":"open","metadata":{"gc.routed_to":"other"}}`
	f.bus.Record(events.Event{Type: "bead.updated", Subject: "test-work", Payload: json.RawMessage(other)})
	f.syncEvents()
	f.write("beads", "["+other+"]", 0o600)
	f.write("fail-nudge", "", 0o600)
	if err := f.run(); err == nil {
		t.Fatal("rerouted delivery unexpectedly succeeded")
	}
	if err := os.Remove(filepath.Join(f.dir, "fail-nudge")); err != nil {
		t.Fatal(err)
	}
	f.seed("bead.updated", true)
	if !f.due() {
		t.Fatal("returning route retained the first route's obsolete success")
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\nworker\n" {
		t.Fatalf("returning route notifications = %q", got)
	}
}

func TestRouteNotificationReturningRouteBeforeDelivery(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "one replay batch", true: "blocked intermediate route"}[blocked], func(t *testing.T) {
			f := newRouteScriptFixture(t)
			f.seed("bead.created", true)
			if err := f.run(); err != nil {
				t.Fatal(err)
			}
			other := `{"id":"test-work","status":"open","metadata":{"gc.routed_to":"other"}}`
			f.bus.Record(events.Event{Type: "bead.updated", Subject: "test-work", Payload: json.RawMessage(other)})
			f.syncEvents()
			if blocked {
				f.write("beads", "["+other+"]", 0o600)
				f.write("blocked", "", 0o600)
				if err := f.run(); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(f.dir, "blocked")); err != nil {
					t.Fatal(err)
				}
			}
			f.seed("bead.updated", true)
			if !f.due() {
				t.Fatal("returning route was suppressed before intermediate delivery")
			}
			if err := f.run(); err != nil {
				t.Fatal(err)
			}
			if got := f.nudges(); got != "worker\nworker\n" {
				t.Fatalf("returning-route notifications = %q", got)
			}
		})
	}
}

func TestRouteNotificationRepeatedStaleRouteDoesNotRepeatLiveDelivery(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	stale := json.RawMessage(`{"id":"test-work","status":"open","metadata":{"gc.routed_to":"stale"}}`)
	f.write("beads", `[{"id":"test-work","status":"open","metadata":{"gc.routed_to":"current"}}]`, 0o600)
	for i := 0; i < 2; i++ {
		f.bus.Record(events.Event{Type: "bead.updated", Subject: "test-work", Payload: stale})
		f.syncEvents()
		if err := f.run(); err != nil {
			t.Fatal(err)
		}
	}
	if got := f.nudges(); got != "worker\ncurrent\n" {
		t.Fatalf("stale replay repeated a live-route delivery: %q", got)
	}
}

func TestRouteNotificationUnrelatedEventsDoNotSelfTrigger(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.bus.Record(events.Event{Type: "bead.created", Subject: "order-run", Payload: json.RawMessage(`{"id":"order-run","status":"open"}`)})
	f.bus.Record(events.Event{Type: "order.completed", Subject: "nudge-on-route"})
	f.syncEvents()
	if f.due() {
		t.Fatal("unrelated/tracking events caused a self-trigger loop")
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\n" {
		t.Fatalf("unrelated events nudged: %q", got)
	}
}

func TestRouteNotificationLogReset(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	f.bus.Record(events.Event{Type: "order.completed"})
	f.syncEvents()
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.bus = events.NewFake()
	f.seed("bead.created", false)
	if !f.due() {
		t.Fatal("log reset did not trigger recovery")
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	state, err := os.ReadFile(filepath.Join(f.dir, "state/nudge-on-route-delivery.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Cursor int `json:"cursor"`
	}
	if err := json.Unmarshal(state, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Cursor != 1 {
		t.Fatalf("cursor after reset = %d", saved.Cursor)
	}
	if got := f.nudges(); got != "worker\n" {
		t.Fatalf("log reset duplicated notification: %q", got)
	}
}

func TestRouteNotificationBoundedRecovery(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.bus.Record(events.Event{Type: "order.completed"})
	f.bus.Events[1].Seq = 5001
	f.syncEvents()
	f.write("beads", `[{"id":"old-work","status":"open","metadata":{"gc.routed_to":"other"}}]`, 0o600)
	if !f.due() {
		t.Fatal("large gap must trigger bounded recovery")
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\nother\n" {
		t.Fatalf("recovered routes = %q", got)
	}
	calls, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(calls), "--since") || strings.Contains(string(calls), "--watch") {
		t.Fatalf("bootstrap/large gap attempted history replay:\n%s", calls)
	}
}

func TestRouteNotificationAllowsSkippedEventRows(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	f.seed("bead.updated", false)
	f.bus.Events[1].Seq = 4 // the API can omit undecodable seqs 2 and 3
	f.syncEvents()
	f.write("beads", `[{"id":"test-work","status":"open","metadata":{"gc.routed_to":"worker"}},{"id":"test-hidden","status":"open","metadata":{"gc.routed_to":"other"}}]`, 0o600)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\nother\n" {
		t.Fatalf("gap recovery missed the omitted route or duplicated a success: %q", got)
	}
}

func TestRouteNotificationWaitsForReadyWork(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	f.write("blocked", "", 0o600)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "" {
		t.Fatalf("blocked work was nudged: %q", got)
	}
	if err := os.Remove(filepath.Join(f.dir, "blocked")); err != nil {
		t.Fatal(err)
	}
	// A readiness update resets blocked deferral without waiting for its timer.
	f.seed("bead.updated", true)
	if !f.due() {
		t.Fatal("unblocked work with unchanged route was lost")
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\n" {
		t.Fatalf("ready work = %q", got)
	}
}

func TestRouteNotificationPreservesEachBacklogNotification(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	f.write("beads", `[{"id":"test-work","status":"open","metadata":{"gc.routed_to":"worker"}},{"id":"test-second","status":"open","metadata":{"gc.routed_to":"worker"}}]`, 0o600)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\nworker\n" {
		t.Fatalf("a sibling route was marked notified without its notification: %q", got)
	}
}

func TestRouteNotificationNamedTemplateMember(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	f.write("sessions", `{"sessions":[{"id":"session-1","name":"worker-seat"}]}`, 0o600)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker-seat\n" {
		t.Fatalf("named template member = %q", got)
	}
}

func TestRouteNotificationMultiplePoolMembersDeferred(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	f.write("sessions", `{"sessions":[{"name":"worker-1"},{"name":"worker-2"}]}`, 0o600)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "" {
		t.Fatalf("multi-member pool received broadcast nudges: %q", got)
	}
	f.expireRetry()
	if f.due() {
		t.Fatal("work handed to the pool backstop remained pending for notification")
	}
}

func TestRouteNotificationPoolOwnershipIgnoresActiveCount(t *testing.T) {
	for _, sessions := range []string{`{"sessions":[]}`, `{"sessions":[{"name":"worker-1"}]}`} {
		t.Run(sessions, func(t *testing.T) {
			f := newRouteScriptFixture(t)
			f.seed("bead.created", true)
			f.write("agents", `{"agents":[{"qualified_name":"worker","pool":{"min":0,"max":3},"routes_to_pool":true}]}`, 0o600)
			f.write("sessions", sessions, 0o600)
			if err := f.run(); err != nil {
				t.Fatal(err)
			}
			if got := f.nudges(); got != "" {
				t.Fatalf("pool got direct notification: %q", got)
			}
			if f.due() {
				t.Fatal("pool handoff remained pending")
			}
		})
	}
}

func TestRouteNotificationLookupFailureRetries(t *testing.T) {
	for _, failure := range []string{"fail-agent-list", "fail-session-list", "malformed-session-list"} {
		t.Run(failure, func(t *testing.T) {
			f := newRouteScriptFixture(t)
			f.seed("bead.created", true)
			if failure == "malformed-session-list" {
				f.write("sessions", `{}`, 0o600)
			} else {
				f.write(failure, "", 0o600)
			}
			if err := f.run(); err == nil {
				t.Fatal("lookup failure reported success")
			}
			if got := f.nudges(); got != "" {
				t.Fatalf("lookup failure fell through to nudge: %q", got)
			}
			if failure == "malformed-session-list" {
				f.write("sessions", `{"sessions":[]}`, 0o600)
			} else if err := os.Remove(filepath.Join(f.dir, failure)); err != nil {
				t.Fatal(err)
			}
			f.expireRetry()
			if !f.due() {
				t.Fatal("lookup failure lost its retry")
			}
			if err := f.run(); err != nil {
				t.Fatal(err)
			}
			if got := f.nudges(); got != "worker\n" {
				t.Fatalf("lookup retry = %q", got)
			}
		})
	}
}

func TestRouteNotificationRequiresRoutingOwnershipField(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	f.write("agents", `{"agents":[{"qualified_name":"worker","pool":{"min":0,"max":1}}]}`, 0o600)
	if err := f.run(); err == nil {
		t.Fatal("older CLI output reported successful ownership transfer")
	}
	if got := f.nudges(); got != "" {
		t.Fatalf("older CLI output guessed a target: %q", got)
	}
	f.expireRetry()
	if !f.due() {
		t.Fatal("older CLI output lost pending work")
	}
	f.write("agents", `{"agents":[{"qualified_name":"worker","pool":{"min":0,"max":1},"routes_to_pool":false}]}`, 0o600)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\n" {
		t.Fatalf("upgrade retry = %q", got)
	}
}

func TestRouteNotificationImportsLegacySuccesses(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	f.write("state/nudge-on-route-state.json", `{"test-work|worker":"2026-09-27T00:00:00Z"}`, 0o600)
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "" {
		t.Fatalf("upgrade repeated an accepted notification: %q", got)
	}
}

func TestRouteNotificationSerializesManualRuns(t *testing.T) {
	f := newRouteScriptFixture(t)
	f.seed("bead.created", true)
	for _, name := range []string{"entered", "release"} {
		if err := unix.Mkfifo(filepath.Join(f.dir, name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entered, err := os.OpenFile(filepath.Join(f.dir, "entered"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer entered.Close()
	release, err := os.OpenFile(filepath.Join(f.dir, "release"), os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer release.Close()
	defer func() { _, _ = release.WriteString("release\n") }()
	arrived := make(chan error, 1)
	go func() { _, err := bufio.NewReader(entered).ReadString('\n'); arrived <- err }()
	finished := make(chan error, 1)
	go func() { finished <- f.run() }()
	select {
	case err := <-arrived:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-finished:
		t.Fatalf("first run exited before delivery barrier: %v", err)
	case <-t.Context().Done():
		t.Fatal("delivery barrier was never reached")
	}
	if err := f.run(); err == nil {
		t.Fatal("concurrent manual exec bypassed the delivery lock")
	}
	if _, err := release.WriteString("release\n"); err != nil {
		t.Fatal(err)
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := f.run(); err != nil {
		t.Fatal(err)
	}
	if got := f.nudges(); got != "worker\n" {
		t.Fatalf("concurrent notifications = %q", got)
	}
}
