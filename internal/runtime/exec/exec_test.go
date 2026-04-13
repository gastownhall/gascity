package exec //nolint:revive // internal package, always imported with alias

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

// writeScript creates an executable shell script in dir and returns its path.
func writeScript(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "provider")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// allOpsScript returns a script body that handles all operations with
// simple, predictable responses.
func allOpsScript() string {
	return `
op="$1"
name="$2"

case "$op" in
  start)       cat > /dev/null ;; # consume stdin
  stop)        ;;
  interrupt)   ;;
  is-running)  echo "true" ;;
  attach)      ;; # just exit 0
  process-alive) cat > /dev/null; echo "true" ;;
  nudge)       cat > /dev/null ;; # consume stdin
  set-meta)    cat > /dev/null ;; # consume stdin
  get-meta)    echo "meta-value" ;;
  remove-meta) ;;
  peek)        echo "line 1"; echo "line 2" ;;
  list-running) echo "sess-a"; echo "sess-b" ;;
  get-last-activity) echo "2025-06-15T10:30:00Z" ;;
  *) exit 2 ;; # unknown operation
esac
`
}

func TestStart(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	err := p.Start(context.Background(), "test-sess", runtime.Config{
		WorkDir: "/tmp",
		Command: "echo hello",
		Env:     map[string]string{"FOO": "bar"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
}

func TestStartWrapsDuplicateSessionError(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	script := writeScript(t, dir, mockProviderScript(stateDir))
	p := NewProvider(script)

	if err := p.Start(context.Background(), "test-sess", runtime.Config{}); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	err := p.Start(context.Background(), "test-sess", runtime.Config{})
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("Start error = %v, want ErrSessionExists", err)
	}
}

func TestStart_configReachesStdin(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "stdin.json")

	// Script that captures stdin to a file.
	script := writeScript(t, dir, `
op="$1"
case "$op" in
  start) cat > "`+outFile+`" ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	err := p.Start(context.Background(), "test-sess", runtime.Config{
		WorkDir: "/tmp/work",
		Command: "claude",
		Env:     map[string]string{"KEY": "val"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, `"work_dir":"/tmp/work"`) {
		t.Errorf("stdin missing work_dir, got: %s", s)
	}
	if !strings.Contains(s, `"command":"claude"`) {
		t.Errorf("stdin missing command, got: %s", s)
	}
}

func TestRunSetsExecStateDir(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")
	stateDir := filepath.Join(dir, "state")

	script := writeScript(t, dir, `
case "$1" in
  stop) printf '%s' "$GC_EXEC_STATE_DIR" > "`+outFile+`" ;;
  *) exit 2 ;;
esac
`)
	p := NewProviderWithOptions(script, WithStateDir(stateDir))

	if err := p.Stop("test-sess"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read env output: %v", err)
	}
	if string(data) != stateDir {
		t.Fatalf("GC_EXEC_STATE_DIR = %q, want %q", string(data), stateDir)
	}
}

func TestRemoteWorkerProviderVerifiesProfileAndSetsEnv(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "env.txt")
	stateDir := filepath.Join(dir, "state")

	script := writeScript(t, dir, `
case "$1" in
  profile) echo "remote-worker/v1" ;;
  stop) printf '%s|%s' "$GC_EXEC_PROFILE" "$GC_EXEC_STATE_DIR" > "`+outFile+`" ;;
  *) exit 2 ;;
esac
`)
	p := NewRemoteWorkerProvider(script, stateDir)

	if err := p.Stop("test-sess"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read env output: %v", err)
	}
	want := "remote-worker/v1|" + stateDir
	if string(data) != want {
		t.Fatalf("remote-worker env = %q, want %q", string(data), want)
	}
}

func TestRemoteWorkerProviderRejectsBadProfile(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  profile) echo "legacy" ;;
  *) exit 2 ;;
esac
`)
	p := NewRemoteWorkerProvider(script, filepath.Join(dir, "state"))

	err := p.Stop("test-sess")
	if err == nil {
		t.Fatal("Stop error = nil, want profile error")
	}
	if !strings.Contains(err.Error(), `got "legacy", want "remote-worker/v1"`) {
		t.Fatalf("Stop error = %v, want profile mismatch", err)
	}
}

func TestRemoteWorkerProviderRetriesFailedProfileNegotiation(t *testing.T) {
	dir := t.TempDir()
	countFile := filepath.Join(dir, "profile-count")
	script := writeScript(t, dir, `
case "$1" in
  profile)
    if [ ! -f "`+countFile+`" ]; then
      echo "1" > "`+countFile+`"
      echo "temporary profile failure" >&2
      exit 1
    fi
    echo "remote-worker/v1"
    ;;
  stop) ;;
  *) exit 2 ;;
esac
`)
	p := NewRemoteWorkerProvider(script, filepath.Join(dir, "state"))

	if err := p.Stop("test-sess"); err == nil {
		t.Fatal("first Stop error = nil, want transient profile error")
	}
	if err := p.Stop("test-sess"); err != nil {
		t.Fatalf("second Stop error = %v, want retry success", err)
	}
}

func TestRemoteWorkerProviderTreatsUnknownOperationAsError(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  profile) echo "remote-worker/v1" ;;
  stop) exit 2 ;;
  *) exit 2 ;;
esac
`)
	p := NewRemoteWorkerProvider(script, filepath.Join(dir, "state"))

	err := p.Stop("test-sess")
	if err == nil {
		t.Fatal("Stop error = nil, want unknown operation error")
	}
	if !strings.Contains(err.Error(), "exit status 2") {
		t.Fatalf("Stop error = %v, want exit status 2", err)
	}
}

func TestStop(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	if err := p.Stop("test-sess"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestInterrupt(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	if err := p.Interrupt("test-sess"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
}

func TestIsRunning_true(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	if !p.IsRunning("test-sess") {
		t.Error("IsRunning returned false, want true")
	}
}

func TestIsRunning_false(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  is-running) echo "false" ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	if p.IsRunning("test-sess") {
		t.Error("IsRunning returned true, want false")
	}
}

func TestIsRunning_error(t *testing.T) {
	dir := t.TempDir()
	// Script that fails for is-running → treated as false.
	script := writeScript(t, dir, `
case "$1" in
  is-running) echo "oops" >&2; exit 1 ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	if p.IsRunning("test-sess") {
		t.Error("IsRunning returned true on error, want false")
	}
}

func TestProcessAlive_true(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	if !p.ProcessAlive("test-sess", []string{"claude", "node"}) {
		t.Error("ProcessAlive returned false, want true")
	}
}

func TestProcessAlive_false(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  process-alive) cat > /dev/null; echo "false" ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	if p.ProcessAlive("test-sess", []string{"claude"}) {
		t.Error("ProcessAlive returned true, want false")
	}
}

func TestProcessAlive_emptyNames(t *testing.T) {
	dir := t.TempDir()
	// Per interface contract: empty processNames → true.
	script := writeScript(t, dir, `exit 1`)
	p := NewProvider(script)

	if !p.ProcessAlive("test-sess", nil) {
		t.Error("ProcessAlive with nil names returned false, want true")
	}
}

func TestNudge(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "nudge.txt")

	script := writeScript(t, dir, `
case "$1" in
  nudge) cat > "`+outFile+`" ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	if err := p.Nudge("test-sess", runtime.TextContent("wake up!")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read nudge output: %v", err)
	}
	if string(data) != "wake up!" {
		t.Errorf("nudge message = %q, want %q", string(data), "wake up!")
	}
}

func TestSetMeta(t *testing.T) {
	dir := t.TempDir()
	outFile := filepath.Join(dir, "meta.txt")

	script := writeScript(t, dir, `
case "$1" in
  set-meta) cat > "`+outFile+`" ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	if err := p.SetMeta("test-sess", "config-hash", "abc123"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read meta output: %v", err)
	}
	if string(data) != "abc123" {
		t.Errorf("meta value = %q, want %q", string(data), "abc123")
	}
}

func TestGetMeta(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	val, err := p.GetMeta("test-sess", "config-hash")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "meta-value" {
		t.Errorf("GetMeta = %q, want %q", val, "meta-value")
	}
}

func TestGetMeta_empty(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  get-meta) ;; # empty stdout = not set
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	val, err := p.GetMeta("test-sess", "nonexistent")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "" {
		t.Errorf("GetMeta = %q, want empty", val)
	}
}

func TestRemoveMeta(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	if err := p.RemoveMeta("test-sess", "config-hash"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
}

func TestPeek(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	output, err := p.Peek("test-sess", 50)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if !strings.Contains(output, "line 1") || !strings.Contains(output, "line 2") {
		t.Errorf("Peek output = %q, want lines 1 and 2", output)
	}
}

func TestListRunning(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	names, err := p.ListRunning("sess-")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 2 || names[0] != "sess-a" || names[1] != "sess-b" {
		t.Errorf("ListRunning = %v, want [sess-a sess-b]", names)
	}
}

func TestListRunning_empty(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  list-running) ;; # empty stdout
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	names, err := p.ListRunning("prefix-")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("ListRunning = %v, want empty", names)
	}
}

func TestGetLastActivity(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, allOpsScript())
	p := NewProvider(script)

	ts, err := p.GetLastActivity("test-sess")
	if err != nil {
		t.Fatalf("GetLastActivity: %v", err)
	}
	want := time.Date(2025, 6, 15, 10, 30, 0, 0, time.UTC)
	if !ts.Equal(want) {
		t.Errorf("GetLastActivity = %v, want %v", ts, want)
	}
}

func TestGetLastActivity_empty(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  get-last-activity) ;; # empty = unsupported
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	ts, err := p.GetLastActivity("test-sess")
	if err != nil {
		t.Fatalf("GetLastActivity: %v", err)
	}
	if !ts.IsZero() {
		t.Errorf("GetLastActivity = %v, want zero", ts)
	}
}

func TestGetLastActivity_malformed(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
case "$1" in
  get-last-activity) echo "not-a-date" ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	ts, err := p.GetLastActivity("test-sess")
	if err != nil {
		t.Fatalf("GetLastActivity should not error on malformed time: %v", err)
	}
	if !ts.IsZero() {
		t.Errorf("GetLastActivity = %v, want zero on malformed input", ts)
	}
}

// --- Error handling ---

func TestErrorPropagation(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
echo "something went wrong" >&2
exit 1
`)
	p := NewProvider(script)

	err := p.Stop("test-sess")
	if err == nil {
		t.Fatal("expected error from exit 1, got nil")
	}
	if !strings.Contains(err.Error(), "something went wrong") {
		t.Errorf("error = %q, want stderr content", err.Error())
	}
}

func TestUnknownOperation_exit2(t *testing.T) {
	dir := t.TempDir()
	// Script that returns exit 2 for everything.
	script := writeScript(t, dir, `exit 2`)
	p := NewProvider(script)

	// Exit 2 means "unknown operation" → treated as success.
	if err := p.Stop("test-sess"); err != nil {
		t.Fatalf("exit 2 should be treated as success, got: %v", err)
	}
}

func TestTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("slow test")
	}

	dir := t.TempDir()
	script := writeScript(t, dir, `sleep 60`)
	p := NewProvider(script)
	p.timeout = 500 * time.Millisecond

	start := time.Now()
	err := p.Stop("test-sess")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout took %v, expected ~500ms", elapsed)
	}
}

func TestProvider_StartUsesLongerTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("slow test")
	}

	dir := t.TempDir()
	// Script that sleeps 2s for start (simulating readiness polling),
	// and sleeps 60s for everything else.
	script := writeScript(t, dir, `
case "$1" in
  start)
    cat > /dev/null
    sleep 2
    ;;
  *) sleep 60 ;;
esac
`)
	p := NewProvider(script)
	// Default timeout too short for the 2s sleep.
	p.timeout = 500 * time.Millisecond
	// But startTimeout is long enough.
	p.startTimeout = 5 * time.Second

	err := p.Start(context.Background(), "test-sess", runtime.Config{Command: "echo hi"})
	if err != nil {
		t.Fatalf("Start should succeed with startTimeout, got: %v", err)
	}

	// Verify that non-start operations still use the short timeout.
	start := time.Now()
	err = p.Stop("test-sess")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Stop should timeout with short timeout")
	}
	if elapsed > 5*time.Second {
		t.Errorf("Stop timeout took %v, expected ~500ms", elapsed)
	}
}

// --- Conformance ---

// mockProviderScript returns a shell script body that implements the full
// exec session protocol backed by files in stateDir. Stateful: tracks
// running sessions and per-session metadata.
func mockProviderScript(stateDir string) string {
	return `
STATE="` + stateDir + `"
op="$1"
name="$2"
key="$3"

case "$op" in
  start)
    cat > /dev/null
    if [ -f "$STATE/$name.running" ]; then
      echo "session $name already exists" >&2
      exit 1
    fi
    touch "$STATE/$name.running"
    ;;
  stop)
    rm -f "$STATE/$name.running"
    rm -f "$STATE/$name.meta."*
    ;;
  interrupt)
    ;;
  is-running)
    if [ -f "$STATE/$name.running" ]; then
      echo "true"
    else
      echo "false"
    fi
    ;;
  attach)
    ;;
  process-alive)
    cat > /dev/null
    if [ -f "$STATE/$name.running" ]; then
      echo "true"
    else
      echo "false"
    fi
    ;;
  nudge)
    cat > /dev/null
    ;;
  set-meta)
    cat > "$STATE/$name.meta.$key"
    ;;
  get-meta)
    if [ -f "$STATE/$name.meta.$key" ]; then
      cat "$STATE/$name.meta.$key"
    fi
    ;;
  remove-meta)
    rm -f "$STATE/$name.meta.$key"
    ;;
  peek)
    ;;
  list-running)
    prefix="$name"
    for f in "$STATE"/*.running; do
      [ -f "$f" ] || continue
      sn=$(basename "$f" .running)
      case "$sn" in
        "$prefix"*) echo "$sn" ;;
      esac
    done
    ;;
  get-last-activity)
    ;;
  clear-scrollback)
    ;;
  *) exit 2 ;;
esac
`
}

func TestExecConformance(t *testing.T) {
	stateDir := t.TempDir()
	dir := t.TempDir()
	script := writeScript(t, dir, mockProviderScript(stateDir))
	p := NewProvider(script)
	p.timeout = 5 * time.Second
	p.startTimeout = 5 * time.Second

	var counter int64

	runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
		id := atomic.AddInt64(&counter, 1)
		name := fmt.Sprintf("exec-conform-%d", id)
		return p, runtime.Config{WorkDir: t.TempDir()}, name
	})
}

// --- Compile-time interface check ---

var _ runtime.Provider = (*Provider)(nil)
