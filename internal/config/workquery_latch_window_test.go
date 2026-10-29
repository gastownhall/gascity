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

func unstampedExpandedRoot(id string) latchWindowRow {
	return latchWindowRow{ID: id, Status: "open", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.formula_contract": "graph.v2", "gc.routed_to": latchWindowTarget,
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

// routedReadReads returns the logged routed ready reads for latchWindowTarget.
func routedReadReads(t *testing.T, logPath string) []string {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd invocation log: %v", err)
	}
	var reads []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "ready ") && strings.Contains(line, "gc.routed_to="+latchWindowTarget) {
			reads = append(reads, line)
		}
	}
	return reads
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

// TestEffectiveWorkQueryLatchWindowRereadsOnlyAFullAllLatchWindow pins the
// re-read's trigger: a short window is the whole routed set, and a full
// window with one row surviving the latch filter already has an answer, so
// both keep the single bounded read and the base candidates. The unbounded
// read serves an extra ready step so an unwanted re-read would show in the
// candidates as well as in the read log.
func TestEffectiveWorkQueryLatchWindowRereadsOnlyAFullAllLatchWindow(t *testing.T) {
	lateStep := scopeMemberStep("late-step")
	for _, tc := range []struct {
		name    string
		bounded []latchWindowRow
		want    []string
	}{
		{
			name:    "short window with executable work",
			bounded: append(fullLatchWindow()[:5], scopeMemberStep("member-step")),
			want:    []string{"member-step"},
		},
		{
			name:    "short window of only latches",
			bounded: fullLatchWindow()[:routedReadyWindowRows-1],
			want:    []string{},
		},
		{
			name:    "full window with executable work",
			bounded: append(fullLatchWindow()[:routedReadyWindowRows-1], scopeMemberStep("member-step")),
			want:    []string{"member-step"},
		},
		{
			name:    "full window with an unstamped graph.v2 workflow root",
			bounded: append(fullLatchWindow()[:routedReadyWindowRows-1], unstampedExpandedRoot("unstamped-root")),
			want:    []string{"unstamped-root"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unbounded := append(append([]latchWindowRow{}, tc.bounded...), lateStep)
			script, env, logPath := latchWindowFakeBd(t, tc.bounded, unbounded, false)

			ids := workQueryOutputIDOrder(t, runEffectiveWorkQuery(t, Agent{Name: "worker", Dir: "hello-world"}, env, script))
			if strings.Join(ids, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("EffectiveWorkQuery() candidates = %v, want %v", ids, tc.want)
			}
			reads := routedReadReads(t, logPath)
			if len(reads) != 1 || !strings.Contains(reads[0], "--limit="+fmt.Sprint(routedReadyWindowRows)) {
				t.Fatalf("routed ready reads = %q, want exactly one bounded read", reads)
			}
		})
	}
}

// TestEffectiveWorkQueryLatchWindowRereadFailureFailsTheQuery pins the
// re-read's failure discipline: a failed unbounded read behind a full
// all-latch window is a read failure, not an idle queue, so the query exits
// non-zero with the reader-failure message instead of printing [].
func TestEffectiveWorkQueryLatchWindowRereadFailureFailsTheQuery(t *testing.T) {
	script, env, logPath := latchWindowFakeBd(t, fullLatchWindow(), nil, true)
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	commandEnv := []string{"PATH=" + tmp + ":" + os.Getenv("PATH")}
	for k, v := range env {
		commandEnv = append(commandEnv, k+"="+v)
	}

	stdout, stderr, exit := runShellCommandCapture(t, (&Agent{Name: "worker", Dir: "hello-world"}).EffectiveWorkQuery(), commandEnv)
	reads := routedReadReads(t, logPath)
	if len(reads) != 2 || !strings.Contains(reads[1], "--limit 0") {
		t.Fatalf("routed ready reads = %q, want the bounded read then the failing unbounded re-read", reads)
	}
	if exit == 0 {
		t.Fatalf("work query exited 0 over a failing re-read and printed %q; that is the answer an idle queue gives", stdout)
	}
	if strings.Contains(stdout, "[]") {
		t.Errorf("work query printed %q over a failing re-read", stdout)
	}
	for _, want := range []string{"gc work query: bd ready failed (exit 1)", "bd: store unreadable"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("work query stderr = %q, want %q", stderr, want)
		}
	}
}
