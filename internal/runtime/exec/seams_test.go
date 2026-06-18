package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamExecScript returns a script body that declares proc.exec (+ activity /
// attachment / stream / tty caps) and implements a REAL exec op (it runs the
// piped command, so its exit code propagates) plus file-backed meta under
// metaDir. Used to exercise the load-bearing Place.Exec + capability mapping.
func seamExecScript(metaDir string) string {
	return fmt.Sprintf(`
op="$1"
case "$op" in
  protocol)    printf '%%s' '{"version":0,"capabilities":["proc.exec","report-activity","report-attachment","proc.stream","tty.attach"]}' ;;
  start)       cat > /dev/null ;;
  stop)        ;;
  is-running)  echo "true" ;;
  is-attached) echo "true" ;;
  attach)      ;;
  exec)        sh -c "$(cat)" ;;
  list-running) echo "sess-a" ;;
  get-last-activity) echo "2025-06-15T10:30:00Z" ;;
  set-meta)    mkdir -p %q; cat > %q/"$3" ;;
  get-meta)    cat %q/"$3" 2>/dev/null || true ;;
  remove-meta) rm -f %q/"$3" ;;
  *) exit 2 ;;
esac
`, metaDir, metaDir, metaDir, metaDir)
}

// TestSeamsExecSupported proves the first REAL seam capability: Place.Exec runs
// the command in the box over the live exec op, returns its output and exit code,
// and PlaceCapabilities/TransportCapabilities reflect the handshake.
func TestSeamsExecSupported(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, seamExecScript(filepath.Join(dir, "meta")))
	p := NewProvider(script)
	rt, tp := p.Seams()
	ctx := context.Background()

	place, err := rt.Provision(ctx, "sess", runtime.ProvisionRequest{Config: runtime.Config{}})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Real exec: command runs, stdout returned, exit 0.
	res, err := place.Exec(ctx, runtime.ExecRequest{Argv: []string{"echo", "hi"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if strings.TrimSpace(string(res.Output)) != "hi" || res.Code != 0 {
		t.Fatalf("Exec = %q, code %d; want hi, 0", res.Output, res.Code)
	}

	// A non-zero command exit is the command's own result, not an error, and the
	// command's stdout is still returned.
	res, err = place.Exec(ctx, runtime.ExecRequest{Argv: []string{"sh", "-c", "echo out; exit 3"}})
	if err != nil {
		t.Fatalf("Exec(exit 3) err: %v", err)
	}
	if res.Code != 3 || strings.TrimSpace(string(res.Output)) != "out" {
		t.Fatalf("Exec(exit 3) = %q, code %d; want out, 3", res.Output, res.Code)
	}

	// The subtle overload: exit 2 with proc.exec declared is the command's OWN
	// exit code (not the unknown-op sentinel / ErrExecUnsupported), stdout kept.
	res, err = place.Exec(ctx, runtime.ExecRequest{Argv: []string{"sh", "-c", "echo two; exit 2"}})
	if err != nil {
		t.Fatalf("Exec(exit 2, proc.exec declared) err: %v; want nil (command's own exit)", err)
	}
	if res.Code != 2 || strings.TrimSpace(string(res.Output)) != "two" {
		t.Fatalf("Exec(exit 2) = %q, code %d; want two, 2", res.Output, res.Code)
	}

	// Capabilities are split from the handshake: box props on the Runtime,
	// attach-reporting on the Transport.
	if caps := rt.Capabilities(); !caps.ReportActivity {
		t.Fatalf("PlaceCapabilities = %+v; want ReportActivity true", caps)
	}
	if !tp.Capabilities().ReportAttachment {
		t.Fatal("TransportCapabilities.ReportAttachment = false; want true")
	}
}

// TestSeamsExecUnsupportedFallsBack proves a pack without proc.exec yields
// ErrExecUnsupported from Place.Exec, and the Attachment's driving verbs fall
// back to the dedicated wire ops (no carrier/tmux needed).
func TestSeamsExecUnsupportedFallsBack(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript()) // no protocol op, no exec op
	p := NewProvider(script)
	rt, tp := p.Seams()
	ctx := context.Background()

	place, err := rt.Provision(ctx, "sess", runtime.ProvisionRequest{Config: runtime.Config{}})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	if _, err := place.Exec(ctx, runtime.ExecRequest{Argv: []string{"echo", "hi"}}); !errors.Is(err, runtime.ErrExecUnsupported) {
		t.Fatalf("Exec err = %v; want ErrExecUnsupported", err)
	}

	att, err := tp.Launch(ctx, place, runtime.LaunchSpec{Config: runtime.Config{ProcessNames: []string{"agent"}}})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	// Peek falls back to the dedicated peek op (allOpsScript prints two lines).
	got, err := att.Peek(ctx, 5)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if !strings.Contains(got, "line 1") {
		t.Fatalf("Peek = %q; want it to contain 'line 1'", got)
	}
	if err := att.Nudge(ctx, runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if err := att.SendKeys(ctx, "Enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if err := att.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if err := att.ClearScrollback(ctx); err != nil {
		t.Fatalf("ClearScrollback: %v", err)
	}
	if err := att.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Observe folds the three liveness reads: process-alive=true (script),
	// attach=false (no report-attachment cap, so the op is never called),
	// activity parsed from get-last-activity.
	obs, err := att.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ProcessAlive {
		t.Fatal("Observe ProcessAlive = false; want true")
	}
	if obs.Attached {
		t.Fatal("Observe Attached = true; want false (no report-attachment cap)")
	}
	if obs.LastActivity.IsZero() {
		t.Fatal("Observe LastActivity zero; want parsed time")
	}
}

// TestSeamsRuntime proves Runtime.List/Open delegate and the MetaStore seam
// round-trips through the pack's file-backed meta ops.
func TestSeamsRuntime(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, seamExecScript(filepath.Join(dir, "meta")))
	rt, _ := NewProvider(script).Seams()
	ctx := context.Background()

	if names, err := rt.List(ctx, ""); err != nil || len(names) != 1 || names[0] != "sess-a" {
		t.Fatalf("List = %v, %v; want [sess-a]", names, err)
	}
	if _, ok, err := rt.Open(ctx, "x"); err != nil || !ok {
		t.Fatalf("Open(live) = %v, %v; want true, nil", ok, err)
	}

	ms, ok := rt.(runtime.MetaStore)
	if !ok {
		t.Fatal("exec Runtime should implement runtime.MetaStore")
	}
	if err := ms.SetMeta("sess", "k", "v"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if got, err := ms.GetMeta("sess", "k"); err != nil || got != "v" {
		t.Fatalf("GetMeta = %q, %v; want v, nil", got, err)
	}
	if err := ms.RemoveMeta("sess", "k"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
	if got, _ := ms.GetMeta("sess", "k"); got != "" {
		t.Fatalf("GetMeta after remove = %q; want empty", got)
	}
}

// TestSeamsOpenAbsent proves Open / Transport.Open return (nil,false,nil) when
// the box is not running.
func TestSeamsOpenAbsent(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
op="$1"
case "$op" in
  is-running) echo "false" ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)
	rt, tp := p.Seams()
	ctx := context.Background()

	if pl, ok, err := rt.Open(ctx, "ghost"); pl != nil || ok || err != nil {
		t.Fatalf("Open(absent) = %v, %v, %v; want nil, false, nil", pl, ok, err)
	}
	place := &execPlace{p: p, name: "ghost"}
	if att, ok, err := tp.Open(ctx, place, "ghost"); att != nil || ok || err != nil {
		t.Fatalf("Transport.Open(dead) = %v, %v, %v; want nil, false, nil", att, ok, err)
	}
}

// TestSeamsExecTransportError proves a transport failure (spawn error) from
// Exec is surfaced as an error with an empty result — not a bogus clean
// ExecResult that would be mistaken for a command that ran.
func TestSeamsExecTransportError(t *testing.T) {
	dir := t.TempDir()
	place := &execPlace{p: NewProvider(filepath.Join(dir, "does-not-exist")), name: "sess"}
	res, err := place.Exec(context.Background(), runtime.ExecRequest{Argv: []string{"echo", "hi"}})
	if err == nil {
		t.Fatal("Exec on a missing script should return a transport error")
	}
	if res.Output != nil || res.Code != 0 {
		t.Fatalf("Exec transport error result = %q, code %d; want empty result", res.Output, res.Code)
	}
}

// TestSeamsStageAndTeardown proves Place.Stage delegates to copy-to (happy path
// stages every entry; a real copy failure aborts the batch with an error) and
// Place.Teardown delegates to stop.
func TestSeamsStageAndTeardown(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "ops.log")
	script := writeScript(t, dir, `
op="$1"
case "$op" in
  start)   cat > /dev/null ;;
  copy-to) if [ "$4" = "boom" ]; then echo "copy failed" >&2; exit 1; fi; echo "copy $3 $4" >> "`+logFile+`" ;;
  stop)    echo "stop $2" >> "`+logFile+`" ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)
	rt, _ := p.Seams()
	ctx := context.Background()

	place, err := rt.Provision(ctx, "sess", runtime.ProvisionRequest{Config: runtime.Config{}})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	if err := place.Stage(ctx, []runtime.CopyEntry{{Src: "/a", RelDst: "x"}, {Src: "/b", RelDst: "y"}}); err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := place.Stage(ctx, []runtime.CopyEntry{{Src: "/a", RelDst: "boom"}}); err == nil {
		t.Fatal("Stage should propagate a real copy-to failure")
	}
	if err := place.Teardown(ctx); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	data, _ := os.ReadFile(logFile)
	got := string(data)
	for _, want := range []string{"copy /a x", "copy /b y", "stop sess"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ops log = %q; want it to contain %q", got, want)
		}
	}
}

// TestSeamsExecTransportShape pins the transport identity and Attach delegation.
func TestSeamsExecTransportShape(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	_, tp := NewProvider(script).Seams()
	if tp.Name() != "tmux" {
		t.Fatalf("Name = %q; want tmux", tp.Name())
	}
	if err := tp.Attach(context.Background(), nil, "sess"); err != nil {
		t.Fatalf("Attach: %v", err)
	}
}
