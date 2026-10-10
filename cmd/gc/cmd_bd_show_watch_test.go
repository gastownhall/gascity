package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseBdShowWatchArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		ok       bool
		id       string
		current  bool
		display  []string
		snapshot []string
	}{
		{
			name:     "trailing --watch",
			args:     []string{"show", "mp-ff9", "--watch"},
			ok:       true,
			id:       "mp-ff9",
			display:  []string{"show", "mp-ff9"},
			snapshot: []string{"show", "--json", "--id=mp-ff9"},
		},
		{
			name:     "view alias",
			args:     []string{"view", "mp-ff9", "-w"},
			ok:       true,
			id:       "mp-ff9",
			display:  []string{"show", "mp-ff9"},
			snapshot: []string{"show", "--json", "--id=mp-ff9"},
		},
		{
			// bd's watchIssue ignores show's display flags and --json.
			name:     "display flags are dropped like bd's watch drops them",
			args:     []string{"show", "-w", "--long", "--json", "--short", "--refs", "--children", "mp-ff9"},
			ok:       true,
			id:       "mp-ff9",
			display:  []string{"show", "mp-ff9"},
			snapshot: []string{"show", "--json", "--id=mp-ff9"},
		},
		{
			name:     "--id flag and --watch=true",
			args:     []string{"show", "--id", "--odd", "--watch=true"},
			ok:       true,
			id:       "--odd",
			display:  []string{"show", "--id=--odd"},
			snapshot: []string{"show", "--json", "--id=--odd"},
		},
		{
			name:     "global flags before the verb are kept",
			args:     []string{"--db", "/x", "show", "mp-1", "--watch"},
			ok:       true,
			id:       "mp-1",
			display:  []string{"--db", "/x", "show", "mp-1"},
			snapshot: []string{"--db", "/x", "show", "--json", "--id=mp-1"},
		},
		{
			name:     "global flags after the id reach the change poll too",
			args:     []string{"show", "mp-1", "--watch", "--db", "/p", "--actor=me", "--no-color"},
			ok:       true,
			id:       "mp-1",
			display:  []string{"--db", "/p", "--actor=me", "--no-color", "show", "mp-1"},
			snapshot: []string{"--db", "/p", "--actor=me", "--no-color", "show", "--json", "--id=mp-1"},
		},
		{
			name:    "--current",
			args:    []string{"show", "--current", "--watch", "--db", "/p"},
			ok:      true,
			current: true,
		},
		{name: "no watch", args: []string{"show", "mp-1"}},
		{name: "--watch=false", args: []string{"show", "mp-1", "--watch=false"}},
		{name: "two ids", args: []string{"show", "mp-1", "mp-2", "--watch"}},
		{name: "no id", args: []string{"show", "--watch"}},
		{name: "--current with an id", args: []string{"show", "mp-1", "--current", "--watch"}},
		{name: "--as-of", args: []string{"show", "mp-1", "--as-of", "main", "--watch"}},
		{name: "--help", args: []string{"show", "mp-1", "--watch", "--help"}},
		{name: "unknown flag", args: []string{"show", "mp-1", "--watch", "--bogus"}},
		{name: "list --watch is bd's", args: []string{"list", "--watch"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, ok := parseBdShowWatchArgs(tt.args)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v (req %+v)", ok, tt.ok, req)
			}
			if !ok {
				return
			}
			if req.ID != tt.id || req.Current != tt.current {
				t.Errorf("ID, Current = %q, %v; want %q, %v", req.ID, req.Current, tt.id, tt.current)
			}
			if tt.current {
				if got, want := req.currentArgs(true), []string{"--db", "/p", "show", "--current", "--json"}; !reflect.DeepEqual(got, want) {
					t.Errorf("currentArgs = %q, want %q", got, want)
				}
				return
			}
			if got := req.displayArgs(req.ID); !reflect.DeepEqual(got, tt.display) {
				t.Errorf("displayArgs = %q, want %q", got, tt.display)
			}
			if got := req.snapshotArgs(req.ID); !reflect.DeepEqual(got, tt.snapshot) {
				t.Errorf("snapshotArgs = %q, want %q", got, tt.snapshot)
			}
		})
	}
}

// syncBuffer is a bytes.Buffer safe to read while the watch loop writes it.
// Every write pings changed, so a waiter needs no fixed sleeps.
type syncBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{}
}

func newSyncBuffer() *syncBuffer { return &syncBuffer{changed: make(chan struct{}, 1)} }

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, err := b.buf.Write(p)
	b.mu.Unlock()
	select {
	case b.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForText(t *testing.T, buf *syncBuffer, want string, n int) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for strings.Count(buf.String(), want) < n {
		select {
		case <-buf.changed:
		case <-deadline.C:
			t.Fatalf("timed out waiting for %d x %q in:\n%s", n, want, buf.String())
		}
	}
}

// The loop renders once, redraws only when bd's id:status:updated_at snapshot
// changes, and returns 0 with bd's "Stopped watching." line on cancel. Every
// snapshot read hands off on polled, so the test paces the loop exactly.
func TestRunBdShowWatchRedrawsOnChangeAndStopsOnCancel(t *testing.T) {
	var mu sync.Mutex
	status, updated := "open", "t1"
	renders := 0
	polled := make(chan struct{})
	var run bdWatchRunFunc = func(ctx context.Context, args []string, stdout, _ io.Writer) int {
		if args[1] == "--json" {
			mu.Lock()
			_, _ = io.WriteString(stdout, `[{"id":"mp-1","status":"`+status+`","updated_at":"`+updated+`","title":"x"}]`)
			mu.Unlock()
			select {
			case polled <- struct{}{}:
			case <-ctx.Done():
			}
			return 0
		}
		mu.Lock()
		renders++
		_, _ = io.WriteString(stdout, "mp-1 ["+strings.ToUpper(status)+"]\n")
		mu.Unlock()
		return 0
	}
	rendered := func() int {
		mu.Lock()
		defer mu.Unlock()
		return renders
	}
	req, ok := parseBdShowWatchArgs([]string{"show", "mp-1", "--watch"})
	if !ok {
		t.Fatal("parse failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout, stderr := newSyncBuffer(), newSyncBuffer()
	done := make(chan int, 1)
	go func() { done <- runBdShowWatch(ctx, req, run, time.Millisecond, stdout, stderr) }()

	// Initial snapshot, then three unchanged polls: still one render.
	for i := 0; i < 4; i++ {
		<-polled
	}
	if got := rendered(); got != 1 {
		t.Fatalf("renders = %d before any change, want 1", got)
	}
	mu.Lock()
	status, updated = "closed", "t2"
	mu.Unlock()
	<-polled // sees the change and redraws
	<-polled // the redraw and its hint line are done
	<-polled // unchanged again: no further redraw
	if got := rendered(); got != 2 {
		t.Fatalf("renders = %d after one change, want 2", got)
	}
	if !strings.Contains(stdout.String(), "mp-1 [CLOSED]") {
		t.Fatalf("redraw missing new status:\n%s", stdout.String())
	}
	if got := strings.Count(stderr.String(), "Watching for changes... (Press Ctrl+C to exit)"); got != 2 {
		t.Fatalf("watch hint printed %d times, want 2:\n%s", got, stderr.String())
	}

	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("watch did not stop on cancel")
	}
	if !strings.HasSuffix(stderr.String(), "\nStopped watching.\n") {
		t.Fatalf("stderr does not end with bd's stop line:\n%q", stderr.String())
	}
}

// A failing first render (bead not found, store down) is bd's answer: its exit
// code comes back and no watch starts.
func TestRunBdShowWatchReturnsInitialRenderFailure(t *testing.T) {
	run := func(_ context.Context, args []string, _, stderr io.Writer) int {
		if args[1] != "--json" {
			_, _ = io.WriteString(stderr, "Error: no issue found\n")
		}
		return 3
	}
	req, _ := parseBdShowWatchArgs([]string{"show", "mp-1", "--watch"})
	var stdout, stderr bytes.Buffer
	if code := runBdShowWatch(context.Background(), req, run, time.Millisecond, &stdout, &stderr); code != 3 {
		t.Fatalf("exit = %d, want 3", code)
	}
	if strings.Contains(stderr.String(), "Watching") {
		t.Fatalf("watch started after a failed render: %q", stderr.String())
	}
}

// showWatchFakeBd behaves like bd v1.3.0 on a proxied scope: it refuses
// `show --watch`, and serves plain and --json show reads from a status file.
const showWatchFakeBd = `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_BD_LOG"
status=$(cat "$FAKE_BD_STATUS")
for a in "$@"; do
  case "$a" in
    --watch|-w) echo "Error: watch mode not supported in proxied-server mode" >&2; exit 1 ;;
  esac
done
case "$*" in
  "show --json --id=mp-1") printf '[{"id":"mp-1","status":"%s","updated_at":"%s"}]\n' "$status" "$status" ;;
  "show mp-1") printf 'mp-1 · hello [%s]\n' "$status" ;;
  *) echo "unexpected: $*" >&2; exit 2 ;;
esac
`

func showWatchFakeBdSetup(t *testing.T, cityPath string) (logPath, statusPath string) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(showWatchFakeBd), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatal(err)
	}
	logPath = filepath.Join(binDir, "log")
	statusPath = filepath.Join(binDir, "status")
	if err := os.WriteFile(statusPath, []byte("open"), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_BD_LOG", logPath)
	t.Setenv("FAKE_BD_STATUS", statusPath)
	if cityPath != "" {
		t.Setenv("GC_CITY_PATH", cityPath)
	}
	origCityFlag, origRigFlag := cityFlag, rigFlag
	t.Cleanup(func() { cityFlag, rigFlag = origCityFlag, origRigFlag })
	cityFlag, rigFlag = "", ""
	return logPath, statusPath
}

// On a proxied scope `gc bd show <id> --watch` no longer reaches bd with
// --watch (which bd refuses); gc polls plain reads, redraws on change and
// exits 0 on Ctrl+C.
func TestGcBdShowWatchIsServedByGCOnProxiedScope(t *testing.T) {
	cityPath, _ := proxiedEnvTestCity(t)
	logPath, statusPath := showWatchFakeBdSetup(t, cityPath)

	origInterval, origCtx := bdShowWatchInterval, bdShowWatchContext
	t.Cleanup(func() { bdShowWatchInterval, bdShowWatchContext = origInterval, origCtx })
	bdShowWatchInterval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bdShowWatchContext = func() (context.Context, context.CancelFunc) { return ctx, func() {} }

	stdout, stderr := newSyncBuffer(), newSyncBuffer()
	done := make(chan int, 1)
	go func() { done <- doBd([]string{"show", "mp-1", "--watch"}, stdout, stderr) }()

	waitForText(t, stdout, "mp-1 · hello [open]", 1)
	waitForText(t, stderr, "Watching for changes... (Press Ctrl+C to exit)", 1)
	if err := os.WriteFile(statusPath, []byte("closed"), 0o644); err != nil { //nolint:gosec // fixture
		t.Fatal(err)
	}
	waitForText(t, stdout, "mp-1 · hello [closed]", 1)
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("doBd = %d, want 0; stderr=%q", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gc bd show --watch did not stop on cancel")
	}
	if strings.Contains(stderr.String(), "watch mode not supported") {
		t.Fatalf("bd's proxied refusal reached the user:\n%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Stopped watching.") {
		t.Fatalf("stderr missing stop line:\n%s", stderr.String())
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "--watch") {
		t.Fatalf("bd was handed --watch on a proxied scope:\n%s", log)
	}
}

// Every scope bd serves watch on itself keeps the unchanged passthrough.
func TestGcBdShowWatchPassesThroughOnNonProxiedScope(t *testing.T) {
	silentFallbackTestSetup(t, "#!/bin/sh\nexit 0\n")
	logPath, _ := showWatchFakeBdSetup(t, "")
	orig := bdScopeRefusesShowWatch
	t.Cleanup(func() { bdScopeRefusesShowWatch = orig })
	asked := false
	bdScopeRefusesShowWatch = func(cityPath string, target execStoreTarget, env []string) bool {
		asked = true
		return orig(cityPath, target, env)
	}

	var stdout, stderr bytes.Buffer
	code := doBd([]string{"show", "mp-1", "--watch"}, &stdout, &stderr)
	if !asked {
		t.Fatal("proxied check was not consulted")
	}
	// The fake bd refuses --watch with exit 1: reaching it proves passthrough.
	if code != 1 {
		t.Fatalf("doBd = %d, want bd's own exit 1; stderr=%q", code, stderr.String())
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(log)); got != "show mp-1 --watch" {
		t.Fatalf("bd argv = %q, want the original show mp-1 --watch", got)
	}
}

// bd resolves --current once and watches that bead; gc does the same, and
// carries the globals into the lookup.
func TestRunBdShowWatchResolvesCurrentOnce(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	polled := make(chan struct{})
	var run bdWatchRunFunc = func(ctx context.Context, args []string, stdout, _ io.Writer) int {
		mu.Lock()
		calls = append(calls, append([]string{}, args...))
		mu.Unlock()
		joined := strings.Join(args, " ")
		switch joined {
		case "--db /p show --current --json":
			_, _ = io.WriteString(stdout, `[{"id":"mp-7","status":"open","updated_at":"t1"}]`)
		case "--db /p show --json --id=mp-7":
			_, _ = io.WriteString(stdout, `[{"id":"mp-7","status":"open","updated_at":"t1"}]`)
			select {
			case polled <- struct{}{}:
			case <-ctx.Done():
			}
		case "--db /p show mp-7":
			_, _ = io.WriteString(stdout, "mp-7 [OPEN]\n")
		default:
			t.Errorf("unexpected bd call %q", joined)
			return 2
		}
		return 0
	}
	req, ok := parseBdShowWatchArgs([]string{"show", "--current", "--watch", "--db", "/p"})
	if !ok {
		t.Fatal("parse failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdout, stderr := newSyncBuffer(), newSyncBuffer()
	done := make(chan int, 1)
	go func() { done <- runBdShowWatch(ctx, req, run, time.Millisecond, stdout, stderr) }()
	<-polled // initial snapshot
	<-polled // first tick: the render is done
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), "mp-7 [OPEN]") {
		t.Fatalf("current bead not rendered:\n%s", stdout.String())
	}
	mu.Lock()
	defer mu.Unlock()
	lookups := 0
	for _, c := range calls {
		if strings.Contains(strings.Join(c, " "), "--current") {
			lookups++
		}
	}
	if lookups != 1 {
		t.Fatalf("--current resolved %d times, want once: %q", lookups, calls)
	}
}

// When --current names nothing, bd's own error and exit code come back.
func TestRunBdShowWatchCurrentUnresolvedReturnsBdError(t *testing.T) {
	var run bdWatchRunFunc = func(_ context.Context, args []string, _, stderr io.Writer) int {
		if strings.Join(args, " ") == "show --current" {
			_, _ = io.WriteString(stderr, "Error: no current issue found\n")
		}
		return 1
	}
	req, _ := parseBdShowWatchArgs([]string{"show", "--current", "--watch"})
	var stdout, stderr bytes.Buffer
	if code := runBdShowWatch(context.Background(), req, run, time.Millisecond, &stdout, &stderr); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no current issue found") || strings.Contains(stderr.String(), "Watching") {
		t.Fatalf("stderr = %q, want bd's error and no watch", stderr.String())
	}
}

// forceGCServedWatch makes doBd take the gc watch path on a test city that is
// not proxied, so the passthrough's stderr checks can be exercised on it.
func forceGCServedWatch(t *testing.T) {
	t.Helper()
	orig := bdScopeRefusesShowWatch
	t.Cleanup(func() { bdScopeRefusesShowWatch = orig })
	bdScopeRefusesShowWatch = func(string, execStoreTarget, []string) bool { return true }
}

// The watch path traces its bd calls and fails loudly on bd's silent fallback
// to on-disk auto-import, exactly like the passthrough.
func TestGcBdShowWatchTracesAndSurfacesSilentFallback(t *testing.T) {
	silentFallbackTestSetup(t, silentFallbackFakeBdScript)
	forceGCServedWatch(t)
	tracePath := filepath.Join(t.TempDir(), "trace.jsonl")
	t.Setenv("GC_BD_TRACE_JSON", tracePath)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"show", "demo-abc", "--watch"}, &stdout, &stderr); code != bdSilentFallbackExitCode {
		t.Fatalf("doBd = %d, want %d; stderr=%q", code, bdSilentFallbackExitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "managed Dolt unreachable") {
		t.Fatalf("stderr missing loud-fail message: %q", stderr.String())
	}
	trace, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(trace), `"go:gc-bd-show-watch"`) {
		t.Fatalf("watch bd call not traced:\n%s", trace)
	}
}

// bd's "bd dolt start" advice gets the gc-managed remedy on the watch path
// too, once, and bd's exit code is kept.
func TestGcBdShowWatchSurfacesDoltStartConflictHintOnce(t *testing.T) {
	managedDoltTestSetup(t, doltStartConflictFakeBdScript)
	forceGCServedWatch(t)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"show", "demo-abc", "--watch"}, &stdout, &stderr); code != 1 {
		t.Fatalf("doBd = %d, want bd's 1; stderr=%q", code, stderr.String())
	}
	if got := strings.Count(stderr.String(), bdDoltStartConflictUserMessage); got != 1 {
		t.Fatalf("conflict hint printed %d times, want 1:\n%s", got, stderr.String())
	}
}
