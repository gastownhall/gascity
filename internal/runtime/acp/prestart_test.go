package acp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gastownhall/gascity/internal/execgrace"
	"github.com/gastownhall/gascity/internal/runtime"
)

func newPreStartTestProvider(t *testing.T, cfg Config) *Provider {
	t.Helper()
	return NewProviderWithDir(filepath.Join(shortTempDir(t), "acp-prestart"), cfg)
}

func newPathCreationWatcher(t *testing.T, dir string) *fsnotify.Watcher {
	t.Helper()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("create filesystem watcher: %v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	if err := watcher.Add(dir); err != nil {
		t.Fatalf("watch %s: %v", dir, err)
	}
	return watcher
}

func waitForPathCreation(t *testing.T, watcher *fsnotify.Watcher, path string) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				t.Fatal("filesystem watcher closed before path creation")
			}
			if event.Name == path && event.Has(fsnotify.Create) {
				return
			}
		case err, ok := <-watcher.Errors:
			if !ok {
				t.Fatal("filesystem watcher error stream closed before path creation")
			}
			t.Fatalf("watching for %s: %v", path, err)
		case <-timer.C:
			t.Fatalf("timed out waiting for creation of %s", path)
		}
	}
}

func TestRunPreStartNoCommandsIsNoOp(t *testing.T) {
	p := newPreStartTestProvider(t, Config{})
	if err := p.runPreStart(context.Background(), runtime.Config{}); err != nil {
		t.Fatalf("runPreStart with no commands = %v, want nil", err)
	}
}

func TestRunPreStartRunsCommandsInOrder(t *testing.T) {
	dir := t.TempDir()
	orderFile := filepath.Join(dir, "order")
	p := newPreStartTestProvider(t, Config{SetupTimeout: time.Second})
	cfg := runtime.Config{
		PreStart: []string{
			fmt.Sprintf("printf one >> %q", orderFile),
			fmt.Sprintf("printf two >> %q", orderFile),
		},
	}

	if err := p.runPreStart(context.Background(), cfg); err != nil {
		t.Fatalf("runPreStart: %v", err)
	}
	got, err := os.ReadFile(orderFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "onetwo" {
		t.Fatalf("order = %q, want %q", got, "onetwo")
	}
}

func TestRunPreStartPassesConfigEnv(t *testing.T) {
	dir := t.TempDir()
	p := newPreStartTestProvider(t, Config{SetupTimeout: time.Second})
	cfg := runtime.Config{
		Env: map[string]string{
			"GC_DIR":        dir,
			"GC_TEST_TOKEN": "sentinel",
		},
		PreStart: []string{`printf %s "$GC_TEST_TOKEN" > token`},
	}

	if err := p.runPreStart(context.Background(), cfg); err != nil {
		t.Fatalf("runPreStart: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "sentinel" {
		t.Fatalf("token = %q, want sentinel", got)
	}
}

func TestRunPreStartUsesExistingGCDirAsCwd(t *testing.T) {
	dir := t.TempDir()
	p := newPreStartTestProvider(t, Config{
		SetupTimeout: time.Second,
		CityRoot:     t.TempDir(),
	})
	cfg := runtime.Config{
		Env:      map[string]string{"GC_DIR": dir},
		PreStart: []string{"pwd > cwd"},
	}

	if err := p.runPreStart(context.Background(), cfg); err != nil {
		t.Fatalf("runPreStart: %v", err)
	}
	assertPathFileEquals(t, filepath.Join(dir, "cwd"), dir)
}

func TestRunPreStartMissingGCDirFallsBackToCityRoot(t *testing.T) {
	cityRoot := t.TempDir()
	p := newPreStartTestProvider(t, Config{
		SetupTimeout: time.Second,
		CityRoot:     cityRoot,
	})
	cfg := runtime.Config{
		Env:      map[string]string{"GC_DIR": filepath.Join(cityRoot, "missing", "workdir")},
		PreStart: []string{"pwd > cwd"},
	}

	if err := p.runPreStart(context.Background(), cfg); err != nil {
		t.Fatalf("runPreStart: %v", err)
	}
	assertPathFileEquals(t, filepath.Join(cityRoot, "cwd"), cityRoot)
}

func TestRunPreStartWithoutCityFallsBackToWorkDirParent(t *testing.T) {
	parent := t.TempDir()
	workDir := filepath.Join(parent, "missing-workdir")
	p := newPreStartTestProvider(t, Config{SetupTimeout: time.Second})
	cfg := runtime.Config{
		Env:      map[string]string{"GC_DIR": workDir},
		WorkDir:  workDir,
		PreStart: []string{"pwd > cwd"},
	}

	if err := p.runPreStart(context.Background(), cfg); err != nil {
		t.Fatalf("runPreStart: %v", err)
	}
	assertPathFileEquals(t, filepath.Join(parent, "cwd"), parent)
}

func TestRunPreStartWithoutGCDirUsesExistingWorkDir(t *testing.T) {
	workDir := t.TempDir()
	p := newPreStartTestProvider(t, Config{SetupTimeout: time.Second})
	cfg := runtime.Config{
		WorkDir:  workDir,
		PreStart: []string{"pwd > cwd"},
	}

	if err := p.runPreStart(context.Background(), cfg); err != nil {
		t.Fatalf("runPreStart: %v", err)
	}
	assertPathFileEquals(t, filepath.Join(workDir, "cwd"), workDir)
}

func TestRunPreStartFailureIsFatalWithBoundedOutputTail(t *testing.T) {
	dir := t.TempDir()
	shouldNotRun := filepath.Join(dir, "should-not-run")
	p := newPreStartTestProvider(t, Config{SetupTimeout: 5 * time.Second})
	cfg := runtime.Config{
		PreStart: []string{
			"true",
			`printf HEADMARK; i=0; while [ "$i" -lt 5000 ]; do printf x; i=$((i + 1)); done; printf TAILMARK >&2; exit 7`,
			fmt.Sprintf("touch %q", shouldNotRun),
		},
	}

	err := p.runPreStart(context.Background(), cfg)
	if err == nil {
		t.Fatal("runPreStart = nil, want fatal command error")
	}
	if !strings.Contains(err.Error(), "pre_start[1]") {
		t.Errorf("error %q does not identify pre_start[1]", err)
	}
	if !strings.Contains(err.Error(), "TAILMARK") {
		t.Errorf("error %q does not include the output tail", err)
	}
	if strings.Contains(err.Error(), "HEADMARK") {
		t.Errorf("error retained output older than the bounded tail: %q", err)
	}
	if len(err.Error()) > preStartOutputLimit+256 {
		t.Errorf("error length = %d, want bounded near %d bytes", len(err.Error()), preStartOutputLimit)
	}
	if _, statErr := os.Stat(shouldNotRun); !os.IsNotExist(statErr) {
		t.Fatalf("command after failure ran; stat error = %v", statErr)
	}
}

func TestRunPreStartRespectsSetupTimeout(t *testing.T) {
	p := newPreStartTestProvider(t, Config{SetupTimeout: 100 * time.Millisecond})
	start := time.Now()
	err := p.runPreStart(context.Background(), runtime.Config{
		PreStart: []string{"sleep 30"},
	})

	if err == nil {
		t.Fatal("runPreStart = nil, want timeout error")
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Fatalf("timeout error = %q, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("runPreStart took %v, want bounded by setup timeout", elapsed)
	}
}

func TestRunPreStartSetupMaxTimeoutAllowsOutputProgress(t *testing.T) {
	p := newPreStartTestProvider(t, Config{
		SetupTimeout:    100 * time.Millisecond,
		SetupMaxTimeout: 2 * time.Second,
	})
	start := time.Now()
	err := p.runPreStart(context.Background(), runtime.Config{
		PreStart: []string{`i=0; while [ "$i" -lt 8 ]; do printf .; sleep 0.04; i=$((i + 1)); done`},
	})
	if err != nil {
		t.Fatalf("runPreStart = %v, want progress to survive idle timeout", err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("command completed in %v; test did not outlive fixed setup_timeout", elapsed)
	}
}

func TestRunPreStartSetupMaxTimeoutUsesIdleBudgetAndRunsRollback(t *testing.T) {
	dir := t.TempDir()
	rollback := filepath.Join(dir, "rolled-back")
	p := newPreStartTestProvider(t, Config{
		SetupTimeout:    100 * time.Millisecond,
		SetupMaxTimeout: 5 * time.Second,
	})
	err := p.runPreStart(context.Background(), runtime.Config{
		PreStart: []string{fmt.Sprintf(
			`trap 'printf done > %q; exit 130' INT TERM; printf started; sleep 30`,
			rollback,
		)},
	})
	if err == nil {
		t.Fatal("runPreStart = nil, want output-idle timeout")
	}
	if !errors.Is(err, execgrace.ErrIdle) {
		t.Fatalf("runPreStart error = %v, want ErrIdle", err)
	}
	if got, readErr := os.ReadFile(rollback); readErr != nil || string(got) != "done" {
		t.Fatalf("rollback marker = %q, err = %v", got, readErr)
	}
}

func TestRunPreStartSetupMaxTimeoutCapsContinuousOutput(t *testing.T) {
	p := newPreStartTestProvider(t, Config{
		SetupTimeout:    100 * time.Millisecond,
		SetupMaxTimeout: 300 * time.Millisecond,
	})
	err := p.runPreStart(context.Background(), runtime.Config{
		PreStart: []string{`while :; do printf .; sleep 0.04; done`},
	})
	if err == nil {
		t.Fatal("runPreStart = nil, want maximum-runtime error")
	}
	if !errors.Is(err, execgrace.ErrCeiling) {
		t.Fatalf("runPreStart error = %v, want ErrCeiling", err)
	}
}

func TestStartRejectsMissingCommandBeforePreStart(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	p := newPreStartTestProvider(t, Config{SetupTimeout: time.Second})
	err := p.Start(context.Background(), testName(), runtime.Config{
		PreStart: []string{fmt.Sprintf("touch %q", marker)},
	})
	if err == nil {
		t.Fatal("Start = nil, want missing-command error")
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("pre_start ran for invalid ACP command; stat error = %v", statErr)
	}
}

func TestStopDuringPreStartKeepsNameReservedUntilRollbackCompletes(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	watcher := newPathCreationWatcher(t, dir)
	p := newPreStartTestProvider(t, Config{
		HandshakeTimeout: 5 * time.Second,
		SetupTimeout:     30 * time.Second,
	})
	name := testName()
	startErr := make(chan error, 1)
	go func() {
		startErr <- p.Start(context.Background(), name, runtime.Config{
			Command: fakeACPShellCommand(),
			WorkDir: dir,
			PreStart: []string{fmt.Sprintf(
				`trap 'sleep 1; exit 130' INT TERM; touch %q; sleep 30`,
				started,
			)},
		})
	}()

	waitForPathCreation(t, watcher, started)
	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// The rollback trap is deliberately still running for one second. A new
	// Start must see the sentinel reservation instead of overlapping it.
	err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: dir,
	})
	if !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("replacement Start during rollback = %v, want ErrSessionExists", err)
	}

	select {
	case err := <-startErr:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Start = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled Start did not finish after rollback grace")
	}
	if p.IsRunning(name) {
		t.Fatal("startup sentinel remained running after canceled Start unwound")
	}
}

func TestStartStagesWorkDirBeforePreStart(t *testing.T) {
	cityRoot := t.TempDir()
	workDir := filepath.Join(cityRoot, "worktrees", "staged-agent")
	source := filepath.Join(t.TempDir(), "staged-source")
	if err := os.WriteFile(source, []byte("staged"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := newPreStartTestProvider(t, Config{
		HandshakeTimeout: 5 * time.Second,
		SetupTimeout:     2 * time.Second,
		CityRoot:         cityRoot,
	})
	name := testName()
	err := p.Start(context.Background(), name, runtime.Config{
		Command: `test -f prestart-after-stage && ` + fakeACPShellCommand(),
		WorkDir: workDir,
		Env:     map[string]string{"GC_DIR": workDir},
		CopyFiles: []runtime.CopyEntry{{
			Src:    source,
			RelDst: "staged-source",
		}},
		PreStart: []string{`test "$(cat staged-source)" = staged && touch prestart-after-stage`},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
}

// Start must run PreStart before both chdir and process launch. This exercises
// the real ACP handshake while the hook creates the initially absent WorkDir.
func TestStartPreStartCreatesWorkDirBeforeLaunch(t *testing.T) {
	cityRoot := t.TempDir()
	workDir := filepath.Join(cityRoot, "worktrees", "agent")
	p := newPreStartTestProvider(t, Config{
		HandshakeTimeout: 5 * time.Second,
		SetupTimeout:     2 * time.Second,
		CityRoot:         cityRoot,
	})
	name := testName()
	err := p.Start(context.Background(), name, runtime.Config{
		Command:  `test "$(cat prestart-ready)" = ready && ` + fakeACPShellCommand(),
		WorkDir:  workDir,
		Env:      map[string]string{"GC_DIR": workDir},
		PreStart: []string{`mkdir -p "$GC_DIR"; printf ready > "$GC_DIR/prestart-ready"`},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	if !p.IsRunning(name) {
		t.Fatal("ACP process is not running after successful Start")
	}
	if got, readErr := os.ReadFile(filepath.Join(workDir, "prestart-ready")); readErr != nil || string(got) != "ready" {
		t.Fatalf("pre_start marker = %q, err = %v", got, readErr)
	}
}

func assertPathFileEquals(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gotPath, err := filepath.EvalSymlinks(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("EvalSymlinks(cwd): %v", err)
	}
	wantPath, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatalf("EvalSymlinks(want): %v", err)
	}
	if gotPath != wantPath {
		t.Fatalf("cwd = %q, want %q", gotPath, wantPath)
	}
}
