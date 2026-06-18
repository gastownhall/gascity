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

const (
	startupWatchNoHangTestTimeout = 10 * time.Second
	startupWatchBlockingSleep     = "30"
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

// separableScript declares proc.exec + proc.provision and logs each op (and the
// exec command from stdin) to logFile. The `exec` op simulates the in-box tmux:
// has-session exits 1 (no session yet) so launch picks new-session.
func separableScript(logFile string) string {
	return `
op="$1"; name="$2"
case "$op" in
  protocol)  echo '{"version":0,"capabilities":["proc.exec","proc.provision"]}' ;;
  provision) cat >/dev/null; echo "provision $name" >> "` + logFile + `" ;;
  start)     cat >/dev/null; echo "start $name"     >> "` + logFile + `" ;;
  exec)      cmd="$(cat)"; echo "exec: $cmd" >> "` + logFile + `"
             case "$cmd" in *has-session*) exit 1 ;; *) exit 0 ;; esac ;;
  is-running) echo true ;;
  stop)      ;;
  *) exit 2 ;;
esac
`
}

// weldedScript declares proc.exec only (NOT proc.provision): the welded `start`
// op provisions and launches, so the controller must not provision/launch.
func weldedScript(logFile string) string {
	return `
op="$1"; name="$2"
case "$op" in
  protocol)  echo '{"version":0,"capabilities":["proc.exec"]}' ;;
  provision) cat >/dev/null; echo "provision $name" >> "` + logFile + `" ;;
  start)     cat >/dev/null; echo "start $name"     >> "` + logFile + `" ;;
  exec)      cmd="$(cat)"; echo "exec: $cmd" >> "` + logFile + `"; exit 0 ;;
  is-running) echo true ;;
  stop)      ;;
  *) exit 2 ;;
esac
`
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "" // not yet written
	}
	return string(b)
}

// A pack that declares proc.provision un-welds: a full Start through the seam
// adapter provisions the box (the `provision` op, NOT welded `start`) and then
// launches the agent via `tmux new-session` over the `exec` op.
func TestSeparableLaunch_ProvisionsThenLaunchesAgent(t *testing.T) {
	dir := t.TempDir()
	logf := filepath.Join(dir, "ops.log")
	p := NewProvider(writeScript(t, dir, separableScript(logf)))

	_, tp := p.Seams()
	if !tp.Capabilities().SeparableLaunch {
		t.Fatal("SeparableLaunch = false; want true for a proc.provision pack")
	}

	prov := runtime.NewProviderFromSeams(p.Seams())
	if err := prov.Start(context.Background(), "s", runtime.Config{Command: "agent --serve"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	log := readLog(t, logf)
	if !strings.Contains(log, "provision s") {
		t.Errorf("missing box-only provision op:\n%s", log)
	}
	if strings.Contains(log, "start s") {
		t.Errorf("welded start op must NOT be used for a separable pack:\n%s", log)
	}
	if !strings.Contains(log, "has-session") {
		t.Errorf("launch should probe has-session first:\n%s", log)
	}
	if !strings.Contains(log, "new-session") || !strings.Contains(log, "agent --serve") {
		t.Errorf("launch should new-session the agent command:\n%s", log)
	}
	if pi, ni := strings.Index(log, "provision s"), strings.Index(log, "new-session"); pi < 0 || ni < 0 || pi > ni {
		t.Errorf("Provision must precede Launch:\n%s", log)
	}
}

// A welded pack (no proc.provision) keeps the old behavior: the `start` op
// provisions+launches, and the controller issues no provision/launch.
func TestSeparableLaunch_WeldedPackUsesStartOnly(t *testing.T) {
	dir := t.TempDir()
	logf := filepath.Join(dir, "ops.log")
	p := NewProvider(writeScript(t, dir, weldedScript(logf)))

	_, tp := p.Seams()
	if tp.Capabilities().SeparableLaunch {
		t.Fatal("SeparableLaunch = true; want false for a welded pack")
	}

	prov := runtime.NewProviderFromSeams(p.Seams())
	if err := prov.Start(context.Background(), "s", runtime.Config{Command: "agent"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	log := readLog(t, logf)
	if !strings.Contains(log, "start s") {
		t.Errorf("welded pack must use the start op:\n%s", log)
	}
	if strings.Contains(log, "provision s") {
		t.Errorf("welded pack must not use the provision op:\n%s", log)
	}
	if strings.Contains(log, "new-session") {
		t.Errorf("welded pack: controller must not launch the agent:\n%s", log)
	}
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

func TestStart_ReturnsDialogDismissalError(t *testing.T) {
	dir := t.TempDir()
	stopFile := filepath.Join(dir, "stop.log")
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start) cat > /dev/null ;;
  stop) echo "$*" >> "`+stopFile+`" ;;
  peek) echo "Bypass Permissions mode" ;;
  send-keys) echo "failed to dismiss dialog" >&2; exit 1 ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	err := p.Start(context.Background(), "test-sess", runtime.Config{
		EmitsPermissionWarning: true,
	})
	if err == nil {
		t.Fatal("Start succeeded, want dialog dismissal error")
	}
	if !strings.Contains(err.Error(), "failed to dismiss dialog") {
		t.Fatalf("Start error = %v, want dialog dismissal context", err)
	}
	data, readErr := os.ReadFile(stopFile)
	if readErr != nil {
		t.Fatalf("read stop log: %v", readErr)
	}
	if !strings.Contains(string(data), "stop test-sess") {
		t.Fatalf("stop log = %q, want cleanup stop call", string(data))
	}
}

func TestStartPrefersWatchStartupOverPeekPolling(t *testing.T) {
	dir := t.TempDir()
	peekFile := filepath.Join(dir, "peek.log")
	sendKeysFile := filepath.Join(dir, "send-keys.log")
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start)
    cat > /dev/null
    ;;
  watch-startup)
    printf '%s\n' '{"content":"Do you trust the contents of this directory?"}'
    printf '%s\n' '{"content":"user@host $"}'
    ;;
  peek)
    echo "peek" >> "`+peekFile+`"
    echo "user@host $"
    ;;
  send-keys)
    echo "$*" >> "`+sendKeysFile+`"
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	err := p.Start(context.Background(), "test-sess", runtime.Config{
		EmitsPermissionWarning: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if data, err := os.ReadFile(peekFile); err == nil && strings.TrimSpace(string(data)) != "" {
		t.Fatalf("peek should not be called when watch-startup is supported, got %q", string(data))
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read peek log: %v", err)
	}
	data, err := os.ReadFile(sendKeysFile)
	if err != nil {
		t.Fatalf("read send-keys log: %v", err)
	}
	if !strings.Contains(string(data), "send-keys test-sess Enter") {
		t.Fatalf("send-keys log = %q, want Enter dismissal", string(data))
	}
}

func TestStartAcceptStartupDialogsOnlyDismissesDialogs(t *testing.T) {
	dir := t.TempDir()
	sendKeysFile := filepath.Join(dir, "send-keys.log")
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start)
    cat > /dev/null
    ;;
  watch-startup)
    printf '%s\n' '{"content":"Do you trust the contents of this directory?"}'
    printf '%s\n' '{"content":"user@host $"}'
    ;;
  peek)
    echo "user@host $"
    ;;
  send-keys)
    echo "$*" >> "`+sendKeysFile+`"
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)
	accept := true

	err := p.Start(context.Background(), "test-sess", runtime.Config{
		AcceptStartupDialogs: &accept,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(sendKeysFile)
	if err != nil {
		t.Fatalf("read send-keys log: %v", err)
	}
	if !strings.Contains(string(data), "send-keys test-sess Enter") {
		t.Fatalf("send-keys log = %q, want Enter dismissal", string(data))
	}
}

func TestStartHandlesDelayedBypassDialogAfterInitialWatchPrompt(t *testing.T) {
	dir := t.TempDir()
	peekFile := filepath.Join(dir, "peek.log")
	sendKeysFile := filepath.Join(dir, "send-keys.log")
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start)
    cat > /dev/null
    ;;
  watch-startup)
    printf '%s\n' '{"content":"user@host $"}'
    sleep 0.02
    printf '%s\n' '{"content":"Bypass Permissions mode"}'
    ;;
  peek)
    echo "peek" >> "`+peekFile+`"
    echo "user@host $"
    ;;
  send-keys)
    echo "$*" >> "`+sendKeysFile+`"
    ;;
  *) exit 2 ;;
esac
	`)
	p := NewProvider(script)

	err := p.Start(context.Background(), "test-sess", runtime.Config{
		EmitsPermissionWarning: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := os.ReadFile(peekFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read peek log: %v", err)
	}

	data, err := os.ReadFile(sendKeysFile)
	if err != nil {
		t.Fatalf("read send-keys log: %v", err)
	}
	if !strings.Contains(string(data), "send-keys test-sess Down") ||
		!strings.Contains(string(data), "send-keys test-sess Enter") {
		t.Fatalf("send-keys log = %q, want delayed bypass dismissal", string(data))
	}
}

func TestStartFallsBackToPeekWhenStartupWatchClosesBeforeReadinessAfterDialog(t *testing.T) {
	dir := t.TempDir()
	peekFile := filepath.Join(dir, "peek.log")
	sendKeysFile := filepath.Join(dir, "send-keys.log")
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start)
    cat > /dev/null
    ;;
  watch-startup)
    printf '%s\n' '{"content":"Do you trust the contents of this directory?"}'
    ;;
  peek)
    echo "$*" >> "`+peekFile+`"
    if [ "$(wc -l < "`+sendKeysFile+`" 2>/dev/null || echo 0)" -ge 2 ]; then
      echo "user@host $"
    else
      echo "Bypass Permissions mode"
    fi
    ;;
  send-keys)
    echo "$*" >> "`+sendKeysFile+`"
    ;;
  *) exit 2 ;;
esac
	`)
	p := NewProvider(script)

	err := p.Start(context.Background(), "test-sess", runtime.Config{
		EmitsPermissionWarning: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	peekData, err := os.ReadFile(peekFile)
	if err != nil {
		t.Fatalf("read peek log: %v", err)
	}
	if strings.TrimSpace(string(peekData)) == "" {
		t.Fatalf("peek log = %q, want fallback peek calls after watch closes early", string(peekData))
	}

	sendData, err := os.ReadFile(sendKeysFile)
	if err != nil {
		t.Fatalf("read send-keys log: %v", err)
	}
	for _, want := range []string{
		"send-keys test-sess Enter",
		"send-keys test-sess Down",
	} {
		if !strings.Contains(string(sendData), want) {
			t.Fatalf("send-keys log = %q, want %q", string(sendData), want)
		}
	}
}

func TestStartFallsBackToPeekWhenWatchStartupUnsupported(t *testing.T) {
	dir := t.TempDir()
	sendKeysFile := filepath.Join(dir, "send-keys.log")
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start)
    cat > /dev/null
    ;;
  watch-startup)
    exit 2
    ;;
  peek)
    if [ -f "`+sendKeysFile+`" ]; then
      echo "user@host $"
    else
      echo "Bypass Permissions mode"
    fi
    ;;
  send-keys)
    echo "$*" >> "`+sendKeysFile+`"
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	err := p.Start(context.Background(), "test-sess", runtime.Config{
		EmitsPermissionWarning: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(sendKeysFile)
	if err != nil {
		t.Fatalf("read send-keys log: %v", err)
	}
	if !strings.Contains(string(data), "send-keys test-sess Down") ||
		!strings.Contains(string(data), "send-keys test-sess Enter") {
		t.Fatalf("send-keys log = %q, want separate Down and Enter dismissal calls", string(data))
	}
}

func TestStartFallsBackToPeekWhenWatchStartupDoesNotEmitInitialEvent(t *testing.T) {
	dir := t.TempDir()
	sendKeysFile := filepath.Join(dir, "send-keys.log")
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start)
    cat > /dev/null
    ;;
  watch-startup)
    sleep 5
    ;;
  peek)
    if [ -f "`+sendKeysFile+`" ]; then
      echo "user@host $"
    else
      echo "Do you trust the contents of this directory?"
    fi
    ;;
  send-keys)
    echo "$*" >> "`+sendKeysFile+`"
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	oldTimeout := startupWatchFirstEventTimeout
	startupWatchFirstEventTimeout = func() time.Duration { return 50 * time.Millisecond }
	t.Cleanup(func() {
		startupWatchFirstEventTimeout = oldTimeout
	})

	err := p.Start(context.Background(), "test-sess", runtime.Config{
		EmitsPermissionWarning: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(sendKeysFile)
	if err != nil {
		t.Fatalf("read send-keys log: %v", err)
	}
	if !strings.Contains(string(data), "send-keys test-sess Enter") {
		t.Fatalf("send-keys log = %q, want Enter dismissal after peek fallback", string(data))
	}
}

func TestStartFallsBackToPeekWhenWatchStartupLeavesStdoutOpenWithoutInitialEvent(t *testing.T) {
	dir := t.TempDir()
	sendKeysFile := filepath.Join(dir, "send-keys.log")
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start)
    cat > /dev/null
    ;;
  watch-startup)
    sh -c 'sleep `+startupWatchBlockingSleep+`' &
    exit 0
    ;;
  peek)
    if [ -f "`+sendKeysFile+`" ]; then
      echo "user@host $"
    else
      echo "Do you trust the contents of this directory?"
    fi
    ;;
  send-keys)
    echo "$*" >> "`+sendKeysFile+`"
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	oldTimeout := startupWatchFirstEventTimeout
	startupWatchFirstEventTimeout = func() time.Duration { return 50 * time.Millisecond }
	t.Cleanup(func() {
		startupWatchFirstEventTimeout = oldTimeout
	})

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- p.Start(ctx, "test-sess", runtime.Config{
			EmitsPermissionWarning: true,
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(startupWatchNoHangTestTimeout):
		cancel()
		t.Fatal("Start() hung while cleaning up a no-event watch-startup child")
	}

	data, err := os.ReadFile(sendKeysFile)
	if err != nil {
		t.Fatalf("read send-keys log: %v", err)
	}
	if !strings.Contains(string(data), "send-keys test-sess Enter") {
		t.Fatalf("send-keys log = %q, want Enter dismissal after peek fallback", string(data))
	}
}

func TestStartReturnsPromptlyWhenWatchStartupFirstEventIsMalformed(t *testing.T) {
	dir := t.TempDir()
	stopFile := filepath.Join(dir, "stop.log")
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start)
    cat > /dev/null
    ;;
  watch-startup)
    printf '%s\n' 'not-json'
    sleep `+startupWatchBlockingSleep+`
    ;;
  stop)
    echo "$*" >> "`+stopFile+`"
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- p.Start(ctx, "test-sess", runtime.Config{
			EmitsPermissionWarning: true,
		})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Start succeeded, want startup watcher decode error")
		}
		if !strings.Contains(err.Error(), "startup watcher decode") {
			t.Fatalf("Start error = %v, want startup watcher decode context", err)
		}
	case <-time.After(startupWatchNoHangTestTimeout):
		cancel()
		t.Fatal("Start() hung after malformed first watch-startup event")
	}

	data, err := os.ReadFile(stopFile)
	if err != nil {
		t.Fatalf("read stop log: %v", err)
	}
	if !strings.Contains(string(data), "stop test-sess") {
		t.Fatalf("stop log = %q, want cleanup stop call", string(data))
	}
}

func TestStartStartupWatchReturnsMalformedFirstEventError(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  watch-startup)
    printf '%s\n' 'not-json'
    sleep `+startupWatchBlockingSleep+`
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	snapshots, closeWatch, ok, err := p.startStartupWatch(context.Background(), "test-sess", startupWatchNoHangTestTimeout)
	if err == nil {
		t.Fatal("startStartupWatch succeeded, want malformed first event error")
	}
	if !strings.Contains(err.Error(), "startup watcher decode") {
		t.Fatalf("startStartupWatch error = %v, want startup watcher decode context", err)
	}
	if ok {
		t.Fatal("startStartupWatch ok = true, want false on malformed first event")
	}
	if snapshots != nil {
		t.Fatal("startStartupWatch returned snapshots, want nil on malformed first event")
	}
	if closeWatch != nil {
		t.Fatal("startStartupWatch returned closeWatch, want nil on malformed first event")
	}
}

func TestStartFallsBackToPeekWhenWatchStartupFailsAfterFirstEvent(t *testing.T) {
	dir := t.TempDir()
	sendKeysFile := filepath.Join(dir, "send-keys.log")
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start)
    cat > /dev/null
    ;;
  watch-startup)
    printf '%s\n' '{"content":"starting up"}'
    printf '%s\n' 'not-json'
    exit 1
    ;;
  peek)
    if [ -f "`+sendKeysFile+`" ]; then
      echo "user@host $"
    else
      echo "Bypass Permissions mode"
    fi
    ;;
  send-keys)
    echo "$*" >> "`+sendKeysFile+`"
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	err := p.Start(context.Background(), "test-sess", runtime.Config{
		EmitsPermissionWarning: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	data, err := os.ReadFile(sendKeysFile)
	if err != nil {
		t.Fatalf("read send-keys log: %v", err)
	}
	if !strings.Contains(string(data), "send-keys test-sess Down") ||
		!strings.Contains(string(data), "send-keys test-sess Enter") {
		t.Fatalf("send-keys log = %q, want peek fallback dismissal", string(data))
	}
}

func TestStartFallsBackToPeekWhenWatchStartupOnlyEmitsIrrelevantSnapshot(t *testing.T) {
	dir := t.TempDir()
	peekFile := filepath.Join(dir, "peek.log")
	sendKeysFile := filepath.Join(dir, "send-keys.log")
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start)
    cat > /dev/null
    ;;
  watch-startup)
    printf '%s\n' '{"content":"starting up"}'
    sleep `+startupWatchBlockingSleep+`
    ;;
  peek)
    echo "$*" >> "`+peekFile+`"
    if [ -f "`+sendKeysFile+`" ]; then
      echo "user@host $"
    else
      echo "Bypass Permissions mode"
    fi
    ;;
  send-keys)
    echo "$*" >> "`+sendKeysFile+`"
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- p.Start(ctx, "test-sess", runtime.Config{
			EmitsPermissionWarning: true,
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(startupWatchNoHangTestTimeout):
		cancel()
		t.Fatal("Start() hung while falling back from an irrelevant watch-startup snapshot")
	}

	peekData, err := os.ReadFile(peekFile)
	if err != nil {
		t.Fatalf("read peek log: %v", err)
	}
	if strings.TrimSpace(string(peekData)) == "" {
		t.Fatalf("peek log = %q, want fallback peek call", string(peekData))
	}

	sendData, err := os.ReadFile(sendKeysFile)
	if err != nil {
		t.Fatalf("read send-keys log: %v", err)
	}
	if !strings.Contains(string(sendData), "send-keys test-sess Down") ||
		!strings.Contains(string(sendData), "send-keys test-sess Enter") {
		t.Fatalf("send-keys log = %q, want fallback dismissal", string(sendData))
	}
}

func TestStartDoesNotHangWhenWatchStartupKeepsStreamingPromptSnapshots(t *testing.T) {
	dir := t.TempDir()
	script := writeScript(t, dir, `
op="$1"

case "$op" in
  start)
    cat > /dev/null
    ;;
  watch-startup)
    printf '%s\n' '{"content":"Do you trust the contents of this directory?"}'
    i=0
    while [ "$i" -lt 2000 ]; do
      printf '%s\n' '{"content":"user@host $"}'
      i=$((i+1))
    done
    sleep `+startupWatchBlockingSleep+`
    ;;
  send-keys)
    ;;
  peek)
    echo "peek should not be used"
    ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		done <- p.Start(ctx, "test-sess", runtime.Config{
			EmitsPermissionWarning: true,
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() error = %v, want nil", err)
		}
	case <-time.After(startupWatchNoHangTestTimeout):
		cancel()
		t.Fatal("Start() hung while cleaning up watch-startup stream")
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

	// This pack implements only the dedicated nudge op (no exec), so Nudge falls
	// back to it after the carrier reports the exec op unsupported.
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

func TestDrivingOverExecWhenSupported(t *testing.T) {
	// A pack that implements the exec op (tmux-in-box) is driven over the
	// carrier: the verbs ship tmux commands through exec, never the dedicated
	// nudge/peek/... ops (which here fail loudly if mistakenly used).
	dir := t.TempDir()
	execLog := filepath.Join(dir, "exec.log")
	script := writeScript(t, dir, `
case "$1" in
  exec) cmd=$(cat); echo "$cmd" >> "`+execLog+`"
        case "$cmd" in *capture-pane*) echo "PANE" ;; esac ;;
  nudge|peek|send-keys|interrupt|clear-scrollback) echo "legacy op used" >&2; exit 1 ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)

	if err := p.Nudge("s", runtime.TextContent("hi")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	out, err := p.Peek("s", 5)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if !strings.Contains(out, "PANE") {
		t.Errorf("Peek over exec = %q, want capture-pane output", out)
	}
	data, _ := os.ReadFile(execLog)
	logged := string(data)
	if !strings.Contains(logged, "send-keys") || !strings.Contains(logged, "hi") {
		t.Errorf("exec log = %q, want a send-keys carrying the message", logged)
	}
	if !strings.Contains(logged, "capture-pane") {
		t.Errorf("exec log = %q, want a capture-pane for Peek", logged)
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

func TestProcessAlive_unimplemented(t *testing.T) {
	dir := t.TempDir()
	// A pack that does not implement process-alive (exit 2) must read as ALIVE,
	// not dead — liveness for such packs is gated by IsRunning, and a false
	// "dead" would make ObserveLiveness reap a live session.
	script := writeScript(t, dir, `exit 2`)
	p := NewProvider(script)
	if !p.ProcessAlive("test-sess", []string{"claude", "node"}) {
		t.Error("ProcessAlive on a pack without process-alive should be true")
	}
}

func TestExec_ExitTwoIsCommandCodeWhenExecDeclared(t *testing.T) {
	dir := t.TempDir()
	// protocol declares proc.exec, so an exec-op exit of 2 is the in-box
	// command's own exit code (2), NOT ErrExecUnsupported.
	script := writeScript(t, dir, `
case "$1" in
  protocol) echo '{"version":0,"capabilities":["proc.exec"]}' ;;
  exec) echo "out"; exit 2 ;;
  *) exit 2 ;;
esac
`)
	p := NewProvider(script)
	_, code, err := p.Exec(context.Background(), "s", []string{"cmd"})
	if err != nil {
		t.Fatalf("Exec with exec declared + command exit 2 should not error, got %v", err)
	}
	if code != 2 {
		t.Errorf("code = %d, want 2 (the in-box command's exit code)", code)
	}
}

func TestExec_ExitTwoIsUnsupportedWhenExecNotDeclared(t *testing.T) {
	dir := t.TempDir()
	// No protocol/exec op: exit 2 means the op is unimplemented -> fall back.
	script := writeScript(t, dir, `exit 2`)
	p := NewProvider(script)
	if _, _, err := p.Exec(context.Background(), "s", []string{"cmd"}); !errors.Is(err, runtime.ErrExecUnsupported) {
		t.Errorf("err = %v, want ErrExecUnsupported when exec is not declared", err)
	}
}

// --- Compile-time interface check ---

var _ runtime.Provider = (*Provider)(nil)
