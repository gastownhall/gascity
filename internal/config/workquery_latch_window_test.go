package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// latchWindowRow is one routed ready row a fake bd serves to the latch-window
// tests.
type latchWindowRow struct {
	ID       string            `json:"id"`
	Status   string            `json:"status"`
	Assignee string            `json:"assignee"`
	Metadata map[string]string `json:"metadata"`
}

const latchWindowTarget = "hello-world/worker"

func stampedExpandedRoot(id string) latchWindowRow {
	return latchWindowRow{ID: id, Status: "open", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.workflow_expanded": "true", "gc.routed_to": latchWindowTarget,
	}}
}

func topologyLatch(id, kind string) latchWindowRow {
	return latchWindowRow{ID: id, Status: "open", Metadata: map[string]string{
		"gc.kind": kind, "gc.routed_to": latchWindowTarget,
	}}
}

func scopeMemberStep(id string) latchWindowRow {
	return latchWindowRow{ID: id, Status: "open", Metadata: map[string]string{
		"gc.kind": "task", "gc.scope_role": "member", "gc.routed_to": latchWindowTarget,
	}}
}

// fullLatchWindow is routedReadyWindowRows routed latches in reader order:
// stamped expanded workflow roots with a scope and a spec latch among them.
func fullLatchWindow() []latchWindowRow {
	rows := make([]latchWindowRow, 0, routedReadyWindowRows)
	for i := 0; i < routedReadyWindowRows; i++ {
		switch i {
		case 3:
			rows = append(rows, topologyLatch(fmt.Sprintf("latch-%02d", i), "scope"))
		case 7:
			rows = append(rows, topologyLatch(fmt.Sprintf("latch-%02d", i), "spec"))
		default:
			rows = append(rows, stampedExpandedRoot(fmt.Sprintf("latch-%02d", i)))
		}
	}
	return rows
}

// latchWindowFakeBd serves bounded as the routed ready read under the tier's
// --limit=N and unbounded under --limit 0, logs every invocation to
// $BD_INVOCATIONS, and fails the unbounded read when unboundedFails is set.
// Every other read is empty.
func latchWindowFakeBd(t *testing.T, bounded, unbounded []latchWindowRow, unboundedFails bool) (script string, env map[string]string, logPath string) {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, rows []latchWindowRow) string {
		data, err := json.Marshal(rows)
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	logPath = filepath.Join(dir, "bd-invocations.log")
	env = map[string]string{
		"GC_SESSION_ORIGIN": "ephemeral",
		"BD_INVOCATIONS":    logPath,
		"BOUNDED_JSON":      write("bounded.json", bounded),
		"UNBOUNDED_JSON":    write("unbounded.json", unbounded),
	}
	unboundedRead := `cat "$UNBOUNDED_JSON"`
	if unboundedFails {
		unboundedRead = `echo "bd: store unreadable" >&2; exit 1`
	}
	script = `#!/bin/sh
printf '%s\n' "$*" >> "$BD_INVOCATIONS"
case "$1" in
  ready)
    case "$*" in
      *"--metadata-field gc.routed_to=` + latchWindowTarget + `"*"--limit=` + fmt.Sprint(routedReadyWindowRows) + `"*)
        cat "$BOUNDED_JSON"
        ;;
      *"--metadata-field gc.routed_to=` + latchWindowTarget + `"*"--limit 0"*)
        ` + unboundedRead + `
        ;;
      *)
        printf '[]'
        ;;
    esac
    ;;
  *)
    printf '[]'
    ;;
esac
`
	return script, env, logPath
}

// TestEffectiveWorkQueryLatchWindowServesReadyMember pins issue #7436's
// residual on a fully stamped city: the routed tier reads a bounded window
// before dropping latches, so a full window of expanded workflow roots and
// scope/spec latches sorting ahead of a ready scope member left the worker
// with [] while the reconciler's unbounded count kept spawning for that
// member. The work query must re-read past such a window and serve it.
func TestEffectiveWorkQueryLatchWindowServesReadyMember(t *testing.T) {
	window := fullLatchWindow()
	all := append(append([]latchWindowRow{}, window...), stampedExpandedRoot("latch-late"), scopeMemberStep("member-step"))
	script, env, _ := latchWindowFakeBd(t, window, all, false)

	ids := workQueryOutputIDOrder(t, runEffectiveWorkQuery(t, Agent{Name: "worker", Dir: "hello-world"}, env, script))
	if len(ids) == 0 || ids[0] != "member-step" {
		t.Fatalf("EffectiveWorkQuery() candidates = %v, want member-step first behind a full window of routed latches", ids)
	}
	for _, id := range ids {
		if strings.HasPrefix(id, "latch-") {
			t.Fatalf("EffectiveWorkQuery() candidates = %v, served latch %q", ids, id)
		}
	}
}
