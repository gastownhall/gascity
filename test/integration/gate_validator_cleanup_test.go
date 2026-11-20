//go:build integration && !windows

package integration

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	gascitypacks "github.com/gastownhall/gascity-packs"
	"github.com/gastownhall/gascity/internal/convergence"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
)

// Exercise the real gate runner and shipped helper together: killing only the
// helper leaves its validator, which deliberately owns a different session,
// writing into a worktree after the gate has returned.
func TestGraphWorkflowGateCancellationCleansShippedValidator(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)
	t.Setenv("GC_HOME", t.TempDir())
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("the process integration lane requires python3: ", err)
	}
	helper, err := fs.ReadFile(gascitypacks.Gascity(), "assets/scripts/source_delivery.py")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"deadline", "parent_cancel", "cleanup_hold"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			write := func(name, body string, permission os.FileMode) string {
				path := filepath.Join(dir, name)
				if err := os.WriteFile(path, []byte(body), permission); err != nil {
					t.Fatal(err)
				}
				return path
			}
			write("source_delivery.py", string(helper), 0o644)
			childPID := filepath.Join(dir, "child.pid")
			parentPID := filepath.Join(dir, "validator.pid")
			sessionPID := filepath.Join(dir, "session.pid")
			for _, path := range []string{childPID, parentPID, sessionPID} {
				t.Cleanup(func() { processgrouptest.KillFromPIDFile(t, path) })
			}
			childCode := fmt.Sprintf("import os,pathlib,signal,time; os.setpgid(0,0); signal.signal(signal.SIGTERM,signal.SIG_IGN); pathlib.Path(%q).write_text(str(os.getpid())); time.sleep(30)", childPID)
			validator := write("validator.py", fmt.Sprintf(`import os,pathlib,subprocess,sys,time
pathlib.Path(%q).write_text(str(os.getpid()))
subprocess.Popen([sys.executable, '-u', '-c', %q], stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
print('owned validator diagnostic', flush=True)
time.sleep(30)
`, parentPID, childCode), 0o644)
			gate := write("check", fmt.Sprintf(`#!%s
import pathlib,shlex,subprocess,sys
sys.path.insert(0, %q)
import source_delivery as delivery
original = subprocess.Popen
def record_session(*args, **kwargs):
    child = original(*args, **kwargs)
    if kwargs.get('start_new_session'):
        pathlib.Path(%q).write_text(str(child.pid))
    return child
subprocess.Popen = record_session
if %q == 'cleanup_hold':
    def refuse_cleanup(child):
        raise RuntimeError('controlled cleanup observation failure')
    delivery._stop_validator = refuse_cleanup
try:
    delivery.run_validator(shlex.join([sys.executable, '-u', %q]), pathlib.Path(%q))
except delivery.ValidatorEnvironmentError as exc:
    print('source delivery environment: ' + str(exc), file=sys.stderr, flush=True)
    raise SystemExit(78)
`, python, dir, sessionPID, mode, validator, dir), 0o755)

			sentinel := exec.Command(python, "-c", "import time; time.sleep(30)")
			if err := sentinel.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = sentinel.Process.Kill()
				_ = sentinel.Wait()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			timeout := 3 * time.Second
			if mode == "parent_cancel" {
				timeout = 30 * time.Second
			}
			done := make(chan convergence.GateResult, 1)
			go func() {
				done <- convergence.RunCondition(ctx, gate, convergence.ConditionEnv{CityPath: dir}, timeout, 0)
			}()
			if mode == "parent_cancel" {
				processgrouptest.WaitForFileSize(t, childPID)
				cancel()
			}
			var result convergence.GateResult
			select {
			case result = <-done:
			case <-time.After(35 * time.Second):
				t.Fatal("gate did not finish within its cancellation cleanup bound")
			}
			if !strings.Contains(result.Stdout, "owned validator diagnostic") {
				t.Errorf("cancellation discarded validator diagnostics: %+v", result)
			}
			if mode == "cleanup_hold" {
				if !convergence.IsGateConfigError(result) {
					t.Errorf("cleanup failure must hold instead of retrying as a timeout: %+v", result)
				}
				if !strings.Contains(result.Stderr, "controlled cleanup observation failure") {
					t.Errorf("cleanup hold lost its remedy: %+v", result)
				}
			} else {
				want := convergence.GateTimeout
				if mode == "parent_cancel" {
					want = convergence.GateError
				}
				if result.Outcome != want || result.RetryCount != 0 {
					t.Errorf("cancellation result = %+v, want %s without retry", result, want)
				}
				for _, path := range []string{childPID, parentPID, sessionPID} {
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal("validator fixture did not reach readiness: ", err)
					}
					pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
					if err != nil || pid <= 1 {
						t.Fatalf("invalid owned PID %q: %v", data, err)
					}
					state, err := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
					if err != nil {
						var exit *exec.ExitError
						if !errors.As(err, &exit) || exit.ExitCode() != 1 {
							t.Fatalf("cannot inspect owned validator process %d: %v", pid, err)
						}
					}
					if err == nil && !strings.HasPrefix(strings.TrimSpace(string(state)), "Z") {
						t.Errorf("owned validator process %d survived gate return: %s", pid, state)
					}
				}
			}
			if err := sentinel.Process.Signal(syscall.Signal(0)); err != nil {
				t.Errorf("unrelated process was affected: %v", err)
			}
		})
	}
}
