package herdr

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// herdr's AgentInfo carries two different identifiers (src/api/schema/agents.rs,
// unchanged from 0.7.0 through 0.9.1): "name" is the registered agent name
// (terminal.agent_name — what `agent start <name>` set, and what `agent get`
// resolves), while "agent" is the DETECTED agent kind (claude, opencode, pi,
// ...). The fixtures below are shaped like live herdr 0.9.1 output. Reading the
// kind as the name made every gc lookup of a session literally named after a
// kind resolve to whichever pane of that kind herdr listed first.

// herdr091AgentList is `herdr agent list` with two opencode panes that gc
// started under distinct names, one opencode pane herdr detected but that
// carries no registered name (a user-run TUI: "name" omitted), and one named
// pane of another kind that belongs to no bound gc session.
const herdr091AgentList = `{"id":"cli:agent:list","result":{"agents":[` +
	`{"agent":"opencode","agent_status":"idle","cwd":"/city","focused":true,"interactive_ready":true,"name":"mayor","pane_id":"wX:p1","revision":2,"state_change_seq":456,"tab_id":"wX:t1","terminal_id":"term_1","workspace_id":"wX"},` +
	`{"agent":"opencode","agent_status":"working","cwd":"/city","focused":false,"interactive_ready":true,"name":"architect","pane_id":"wY:p1","revision":7,"state_change_seq":12,"tab_id":"wY:t1","terminal_id":"term_2","workspace_id":"wY"},` +
	`{"agent":"opencode","agent_status":"idle","cwd":"/elsewhere","focused":false,"interactive_ready":true,"pane_id":"wZ:p1","revision":1,"state_change_seq":3,"tab_id":"wZ:t1","terminal_id":"term_3","workspace_id":"wZ"},` +
	`{"agent":"claude","agent_status":"idle","cwd":"/city","focused":false,"interactive_ready":true,"name":"foreign-helper","pane_id":"wF:p1","revision":0,"state_change_seq":1,"tab_id":"wF:t1","terminal_id":"term_4","workspace_id":"wF"}` +
	`]}}`

// herdr091AgentListNamedOpencode is herdr091AgentList plus a pane whose agent
// is registered under the name "opencode" and is also of kind opencode. It is
// listed after mayor's pane of the same kind, so a lookup that matched on kind
// would return mayor's pane instead.
const herdr091AgentListNamedOpencode = `{"id":"cli:agent:list","result":{"agents":[` +
	`{"agent":"opencode","agent_status":"idle","cwd":"/city","focused":true,"interactive_ready":true,"name":"mayor","pane_id":"wX:p1","revision":2,"state_change_seq":456,"tab_id":"wX:t1","terminal_id":"term_1","workspace_id":"wX"},` +
	`{"agent":"opencode","agent_status":"working","cwd":"/city","focused":false,"interactive_ready":true,"name":"architect","pane_id":"wY:p1","revision":7,"state_change_seq":12,"tab_id":"wY:t1","terminal_id":"term_2","workspace_id":"wY"},` +
	`{"agent":"opencode","agent_status":"idle","cwd":"/city","focused":false,"interactive_ready":true,"name":"opencode","pane_id":"wO:p1","revision":3,"state_change_seq":9,"tab_id":"wO:t1","terminal_id":"term_5","workspace_id":"wO"},` +
	`{"agent":"opencode","agent_status":"idle","cwd":"/elsewhere","focused":false,"interactive_ready":true,"pane_id":"wZ:p1","revision":1,"state_change_seq":3,"tab_id":"wZ:t1","terminal_id":"term_3","workspace_id":"wZ"}` +
	`]}}`

func TestAgentInfoDecodesRegisteredNameNotDetectedKind(t *testing.T) {
	var got agentInfo
	raw := `{"agent":"opencode","agent_status":"idle","name":"mayor","pane_id":"wX:p1","tab_id":"wX:t1","workspace_id":"wX","terminal_id":"term_1","revision":2}`
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "mayor" {
		t.Fatalf("Name = %q; want the registered name %q, not the detected kind", got.Name, "mayor")
	}
	if got.Kind != "opencode" {
		t.Fatalf("Kind = %q; want the detected kind %q", got.Kind, "opencode")
	}

	// A detected agent with no registered name must decode as unnamed, whether
	// herdr omits the field (its skip_serializing_if) or sends null. Falling
	// back to the kind here is exactly the collision this guards against.
	for _, raw := range []string{
		`{"agent":"opencode","agent_status":"idle","pane_id":"wZ:p1"}`,
		`{"agent":"opencode","name":null,"agent_status":"idle","pane_id":"wZ:p1"}`,
	} {
		var unnamed agentInfo
		if err := json.Unmarshal([]byte(raw), &unnamed); err != nil {
			t.Fatal(err)
		}
		if unnamed.Name != "" {
			t.Fatalf("unnamed agent %s decoded Name = %q; want empty", raw, unnamed.Name)
		}
		if unnamed.Kind != "opencode" {
			t.Fatalf("unnamed agent %s decoded Kind = %q; want %q", raw, unnamed.Kind, "opencode")
		}
	}
}

// newHerdr091Provider builds a Provider against a fake herdr modeled on 0.9.1:
// `agent get` resolves a pane id or a registered name and otherwise fails the
// way the real CLI does (error envelope on stderr, exit 1); `agent list`
// returns herdr091AgentList. Setting the state file "get_by_pane_only"
// makes `agent get` reject names and resolve only pane ids, which is what
// sends getAgent to its list fallback. Setting "named_opencode" adds pane
// wO:p1, whose agent is genuinely registered under the name "opencode" and is
// of kind opencode, listed AFTER mayor's opencode pane. Every pane probes busy.
func newHerdr091Provider(t *testing.T) (*Provider, string) {
	t.Helper()
	state := t.TempDir()
	metaDir := t.TempDir()
	script := filepath.Join(t.TempDir(), "herdr")
	fake := `#!/bin/sh
STATE='` + state + `'
shift 2
printf '%s\n' "$*" >> "$STATE/calls.log"
notfound() {
  printf '%s' '{"error":{"code":"agent_not_found","message":"agent target '"$1"' not found"},"id":"cli:agent:get"}' >&2
  exit 1
}
emit() {
  printf '%s' '{"id":"cli:agent:get","result":{"agent":{"agent":"'"$1"'","agent_status":"idle","name":"'"$2"'","pane_id":"'"$3"'","tab_id":"t","workspace_id":"w","terminal_id":"term","revision":0}}}'
}
case "$1_$2" in
agent_get)
  case "$3" in
  wX:p1) emit opencode mayor wX:p1 ;;
  wY:p1) emit opencode architect wY:p1 ;;
  wF:p1) emit claude foreign-helper wF:p1 ;;
  wO:p1)
    if [ ! -e "$STATE/named_opencode" ]; then notfound "$3"; fi
    emit opencode opencode wO:p1 ;;
  mayor|architect|foreign-helper|opencode)
    if [ "$3" = opencode ] && [ ! -e "$STATE/named_opencode" ]; then notfound "$3"; fi
    if [ -e "$STATE/get_by_pane_only" ]; then notfound "$3"; fi
    case "$3" in
    mayor) emit opencode mayor wX:p1 ;;
    architect) emit opencode architect wY:p1 ;;
    foreign-helper) emit claude foreign-helper wF:p1 ;;
    opencode) emit opencode opencode wO:p1 ;;
    esac ;;
  *) notfound "$3" ;;
  esac ;;
agent_list)
  if [ -e "$STATE/named_opencode" ]; then
    printf '%s' '` + herdr091AgentListNamedOpencode + `'
  else
    printf '%s' '` + herdr091AgentList + `'
  fi ;;
pane_process-info)
  printf '%s' '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4243,"name":"opencode"}]}}}' ;;
*)
  exit 0 ;;
esac
`
	if err := os.WriteFile(script, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	p := New("gctest-identity", metaDir, t.TempDir(), time.Second, time.Second)
	p.c.bin = script
	return p, state
}

// bindSession seeds the sidecar binding Start persists for a gc session.
func bindSession(t *testing.T, p *Provider, name, pane string) {
	t.Helper()
	for key, val := range map[string]string{
		metaBoundPane: pane,
		metaBoundMode: bindModeAgent,
		metaBoundName: name,
		metaBoundAt:   "1",
	} {
		if err := p.SetMeta(name, key, val); err != nil {
			t.Fatal(err)
		}
	}
}

// A target that is only an agent KIND is no registered agent: herdr's own
// `agent get` rejects it, and the list fallback must not rescue it by matching
// the kind of whichever pane herdr lists first.
func TestGetAgentFallbackDoesNotResolveAKindAsAName(t *testing.T) {
	for _, byPaneOnly := range []bool{false, true} {
		p, state := newHerdr091Provider(t)
		if byPaneOnly {
			setState(t, state, "get_by_pane_only")
		}
		for _, target := range []string{"opencode", "claude"} {
			info, ok, err := p.c.getAgent(context.Background(), target)
			if err != nil {
				t.Fatalf("getAgent(%q): %v", target, err)
			}
			if ok {
				t.Fatalf("getAgent(%q) resolved pane %s (name %q); want absent: %q is an agent kind, not a registered name", target, info.PaneID, info.Name, target)
			}
		}
		if _, ok, err := p.c.getAgent(context.Background(), ""); err != nil || ok {
			t.Fatalf("getAgent(\"\") = ok %v, err %v; want absent (an unnamed pane must never match an empty target)", ok, err)
		}
	}
}

// A name that `agent get` cannot resolve directly must still be found by the
// list fallback, on exactly its own pane even when other panes share its kind.
func TestGetAgentFallbackResolvesRegisteredNameExactly(t *testing.T) {
	p, state := newHerdr091Provider(t)
	setState(t, state, "get_by_pane_only")
	for name, pane := range map[string]string{"mayor": "wX:p1", "architect": "wY:p1", "foreign-helper": "wF:p1"} {
		info, ok, err := p.c.getAgent(context.Background(), name)
		if err != nil || !ok {
			t.Fatalf("getAgent(%q) = ok %v, err %v; want the registered agent", name, ok, err)
		}
		if info.PaneID != pane || info.Name != name {
			t.Fatalf("getAgent(%q) = pane %s name %q; want pane %s", name, info.PaneID, info.Name, pane)
		}
	}
	if !strings.Contains(fakeCalls(t, state), "agent list") {
		t.Fatal("the pane-only agent get never exercised the list fallback")
	}
}

// ListRunning feeds the startup adoption barrier. It must report exactly the
// bound gc sessions plus genuinely foreign NAMED agents: never a phantom
// session named after an agent kind, and never an unnamed detected pane.
func TestListRunningReportsRegisteredNamesNotKinds(t *testing.T) {
	p, _ := newHerdr091Provider(t)
	bindSession(t, p, "mayor", "wX:p1")
	bindSession(t, p, "architect", "wY:p1")

	got, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	sort.Strings(got)
	want := []string{"architect", "foreign-helper", "mayor"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ListRunning(\"\") = %q; want exactly %q (no kind-named phantom, no unnamed pane)", got, want)
	}

	got, err = p.ListRunning("op")
	if err != nil || len(got) != 0 {
		t.Fatalf("ListRunning(\"op\") = %q, %v; want empty: no session is named after the opencode kind", got, err)
	}
}

// Liveness and Stop for a session that shares nothing but a kind name with a
// live pane must see nothing: Stop closing the first opencode pane it finds
// would kill another session's agent.
func TestKindNamedSessionDoesNotResolveAnotherSessionsPane(t *testing.T) {
	for _, byPaneOnly := range []bool{false, true} {
		p, state := newHerdr091Provider(t)
		if byPaneOnly {
			setState(t, state, "get_by_pane_only")
		}
		bindSession(t, p, "mayor", "wX:p1")

		if p.IsRunning("opencode") {
			t.Fatal("IsRunning(\"opencode\") = true; the only opencode panes belong to other sessions")
		}
		if lv := p.ObserveLiveness("opencode", nil); lv.Running || lv.Alive {
			t.Fatalf("ObserveLiveness(\"opencode\") = %+v; want zero", lv)
		}
		if err := p.Stop("opencode"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if calls := fakeCalls(t, state); strings.Contains(calls, "pane close") {
			t.Fatalf("Stop(\"opencode\") closed a pane owned by another session:\n%s", calls)
		}

		// The real session is untouched and still resolves to its own pane.
		if err := p.Stop("mayor"); err != nil {
			t.Fatalf("Stop(mayor): %v", err)
		}
		calls := fakeCalls(t, state)
		if !strings.Contains(calls, "pane close wX:p1") || strings.Contains(calls, "pane close wY:p1") {
			t.Fatalf("Stop(mayor) did not close exactly its own pane:\n%s", calls)
		}
	}
}

// The positive side of the same rule: a session genuinely registered under the
// name "opencode" is an ordinary name and must resolve to exactly its own pane,
// even though mayor's pane of the same kind is listed first. The lookup keys on
// the registered name, not on any heuristic about kind-shaped names.
func TestRegisteredAgentNamedLikeAKindResolvesExactly(t *testing.T) {
	for _, byPaneOnly := range []bool{false, true} {
		p, state := newHerdr091Provider(t)
		setState(t, state, "named_opencode")
		if byPaneOnly {
			setState(t, state, "get_by_pane_only")
		}
		bindSession(t, p, "mayor", "wX:p1")
		bindSession(t, p, "opencode", "wO:p1")

		info, ok, err := p.c.getAgent(context.Background(), "opencode")
		if err != nil || !ok {
			t.Fatalf("byPaneOnly=%v: getAgent(opencode) = ok %v, err %v; want the registered agent", byPaneOnly, ok, err)
		}
		if info.PaneID != "wO:p1" || info.Name != "opencode" || info.Kind != "opencode" {
			t.Fatalf("byPaneOnly=%v: getAgent(opencode) = %+v; want pane wO:p1 named opencode", byPaneOnly, info)
		}
		if calls := fakeCalls(t, state); byPaneOnly != strings.Contains(calls, "agent list") {
			t.Fatalf("byPaneOnly=%v: list fallback used = %v; want %v:\n%s", byPaneOnly, !byPaneOnly, byPaneOnly, calls)
		}

		got, err := p.ListRunning("")
		if err != nil {
			t.Fatalf("ListRunning: %v", err)
		}
		sort.Strings(got)
		if want := []string{"architect", "mayor", "opencode"}; strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("byPaneOnly=%v: ListRunning(\"\") = %q; want exactly %q", byPaneOnly, got, want)
		}

		if !p.IsRunning("opencode") {
			t.Fatalf("byPaneOnly=%v: IsRunning(opencode) = false for the registered agent", byPaneOnly)
		}
		if err := p.Stop("opencode"); err != nil {
			t.Fatalf("Stop(opencode): %v", err)
		}
		calls := fakeCalls(t, state)
		if !strings.Contains(calls, "pane close wO:p1") {
			t.Fatalf("byPaneOnly=%v: Stop(opencode) did not close its own pane:\n%s", byPaneOnly, calls)
		}
		for _, other := range []string{"wX:p1", "wY:p1", "wZ:p1"} {
			if strings.Contains(calls, "pane close "+other) {
				t.Fatalf("byPaneOnly=%v: Stop(opencode) closed %s, which it does not own:\n%s", byPaneOnly, other, calls)
			}
		}
	}
}
