package session

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

const finalizedNudge = "FINALIZED NUDGE"

// recordingFinalizer records every launch it sees and returns the config with
// a nudge and the delivered marker added, so a test sees both what the seam
// handed in and that what comes out is what reaches the provider.
type recordingFinalizer struct {
	seen []LaunchContext
	err  error
}

func (r *recordingFinalizer) finalize(lc LaunchContext) (runtime.Config, error) {
	r.seen = append(r.seen, lc)
	if r.err != nil {
		return runtime.Config{}, r.err
	}
	cfg := lc.Config
	env := make(map[string]string, len(cfg.Env)+1)
	for k, v := range cfg.Env {
		env[k] = v
	}
	env["GC_STARTUP_PROMPT_DELIVERED"] = "1"
	cfg.Env = env
	cfg.Nudge = finalizedNudge
	return cfg, nil
}

func finalizerCreateOptions() CreateOptions {
	return CreateOptions{
		ExplicitName: "sky", Template: "helper", Title: "chat", Command: "claude", WorkDir: "/tmp",
		Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"},
	}
}

func finalizerSuspendedRow(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Title: "worker", Type: BeadType, Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "test-city--worker", "state": string(StateSuspended),
			"template": "worker", "work_dir": "/tmp", "provider": "claude",
		},
	})
	if err != nil {
		t.Fatalf("creating session bead: %v", err)
	}
	return b
}

// TestLaunchFinalizerCreateSeam: createStarted hands the assembled config —
// identity env included — to the finalizer once, and what the finalizer
// returns is what the provider starts.
func TestLaunchFinalizerCreateSeam(t *testing.T) {
	rec := &recordingFinalizer{}
	sp := runtime.NewFake()
	mgr := newTestManager(t, beads.NewMemStore(), sp, WithLaunchFinalizer(rec.finalize))

	info, err := mgr.CreateSession(context.Background(), finalizerCreateOptions())
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(rec.seen) != 1 {
		t.Fatalf("finalizer invoked %d times by createStarted, want 1", len(rec.seen))
	}
	lc := rec.seen[0]
	if lc.Kind != LaunchCreate {
		t.Errorf("Kind = %q, want %q", lc.Kind, LaunchCreate)
	}
	if lc.Info.ID != info.ID {
		t.Errorf("Info.ID = %q, want the created session %q", lc.Info.ID, info.ID)
	}
	if lc.Config.Env["GC_SESSION_ID"] != info.ID {
		t.Errorf("finalizer saw GC_SESSION_ID=%q, want %q: it must run after the identity env is merged", lc.Config.Env["GC_SESSION_ID"], info.ID)
	}
	if lc.Config.Command != "claude" || lc.Config.WorkDir != "/tmp" {
		t.Errorf("finalizer saw Command=%q WorkDir=%q, want the launch's own", lc.Config.Command, lc.Config.WorkDir)
	}
	got := sp.LastStartConfig("sky")
	if got == nil {
		t.Fatalf("no Start recorded: %#v", sp.Calls)
	}
	if got.Nudge != finalizedNudge || got.Env["GC_STARTUP_PROMPT_DELIVERED"] != "1" {
		t.Errorf("Start config = Nudge %q marker %q, want the finalizer's output", got.Nudge, got.Env["GC_STARTUP_PROMPT_DELIVERED"])
	}
}

// TestLaunchFinalizerResumeSeam: ensureRunning's launch branch hands the
// assembled config to the finalizer once, with the resume kind.
func TestLaunchFinalizerResumeSeam(t *testing.T) {
	rec := &recordingFinalizer{}
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp, WithLaunchFinalizer(rec.finalize))
	b := finalizerSuspendedRow(t, store)

	if err := mgr.Attach(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude --resume abc", runtime.Config{}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if len(rec.seen) != 1 {
		t.Fatalf("finalizer invoked %d times by ensureRunning, want 1", len(rec.seen))
	}
	lc := rec.seen[0]
	if lc.Kind != LaunchResume {
		t.Errorf("Kind = %q, want %q", lc.Kind, LaunchResume)
	}
	if lc.Info.ID != b.ID {
		t.Errorf("Info.ID = %q, want %q", lc.Info.ID, b.ID)
	}
	if lc.Config.Command != "claude --resume abc" {
		t.Errorf("finalizer saw Command=%q, want the resume command", lc.Config.Command)
	}
	if lc.Config.Env["GC_SESSION_ID"] != b.ID {
		t.Errorf("finalizer saw GC_SESSION_ID=%q, want %q: it must run after the minted identity env is merged", lc.Config.Env["GC_SESSION_ID"], b.ID)
	}
	got := sp.LastStartConfig("test-city--worker")
	if got == nil {
		t.Fatalf("no Start recorded: %#v", sp.Calls)
	}
	if got.Nudge != finalizedNudge || got.Env["GC_STARTUP_PROMPT_DELIVERED"] != "1" {
		t.Errorf("Start config = Nudge %q marker %q, want the finalizer's output", got.Nudge, got.Env["GC_STARTUP_PROMPT_DELIVERED"])
	}
}

// TestLaunchFinalizerNotInvokedWithoutLaunch (T3, T4): only a launch renders.
// A runtime already running, and the reconciler's runtime-only start, never
// reach the finalizer.
func TestLaunchFinalizerNotInvokedWithoutLaunch(t *testing.T) {
	t.Run("ensureRunning over a running runtime", func(t *testing.T) {
		rec := &recordingFinalizer{}
		store := beads.NewMemStore()
		sp := runtime.NewFake()
		mgr := newTestManager(t, store, sp, WithLaunchFinalizer(rec.finalize))
		b, err := store.Create(beads.Bead{
			Title: "worker", Type: BeadType, Labels: []string{LabelSession},
			Metadata: map[string]string{"session_name": "test-city--worker", "state": string(StateActive), "template": "worker", "work_dir": "/tmp", "provider": "claude"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := sp.Start(context.Background(), "test-city--worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		if err := mgr.Attach(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude --resume abc", runtime.Config{}); err != nil {
			t.Fatalf("Attach: %v", err)
		}
		if len(rec.seen) != 0 {
			t.Fatalf("finalizer invoked %d times over a running runtime, want 0", len(rec.seen))
		}
	})
	t.Run("runtime-only start", func(t *testing.T) {
		rec := &recordingFinalizer{}
		store := beads.NewMemStore()
		sp := runtime.NewFake()
		mgr := newTestManager(t, store, sp, WithLaunchFinalizer(rec.finalize))
		b, err := store.Create(beads.Bead{Title: "worker", Type: BeadType, Labels: []string{LabelSession}, Metadata: mintRowMeta()})
		if err != nil {
			t.Fatal(err)
		}
		if err := startRuntimeOnly(t, mgr, b.ID, "claude", runtime.Config{}); err != nil {
			t.Fatalf("StartRuntimeOnly: %v", err)
		}
		if len(rec.seen) != 0 {
			t.Fatalf("finalizer invoked %d times by StartRuntimeOnly, want 0: the reconciler renders its own prompt", len(rec.seen))
		}
		if sp.LastStartConfig("s-mint") == nil {
			t.Fatalf("StartRuntimeOnly started nothing: %#v", sp.Calls)
		}
	})
}

// TestLaunchFinalizerFailureAtomicity (T6): a finalizer error starts no
// runtime. A failed create leaves no live row behind.
func TestLaunchFinalizerFailureAtomicity(t *testing.T) {
	boom := errors.New("prompt cannot be rendered")
	t.Run("create", func(t *testing.T) {
		rec := &recordingFinalizer{err: boom}
		store := beads.NewMemStore()
		sp := runtime.NewFake()
		mgr := newTestManager(t, store, sp, WithLaunchFinalizer(rec.finalize))

		_, err := mgr.CreateSession(context.Background(), finalizerCreateOptions())
		if !errors.Is(err, boom) {
			t.Fatalf("CreateSession error = %v, want it to wrap the finalizer's", err)
		}
		if len(rec.seen) != 1 {
			t.Fatalf("finalizer invoked %d times, want 1", len(rec.seen))
		}
		for _, c := range sp.Calls {
			if c.Method == "Start" {
				t.Fatalf("a finalizer error must start no runtime, saw %#v", c)
			}
		}
		if err := ensureSessionNameAvailable(mgr.store, "sky"); err != nil {
			t.Fatalf("name sky after a failed create: %v, want it released", err)
		}
	})
	t.Run("resume", func(t *testing.T) {
		rec := &recordingFinalizer{err: boom}
		store := beads.NewMemStore()
		sp := runtime.NewFake()
		mgr := newTestManager(t, store, sp, WithLaunchFinalizer(rec.finalize))
		b := finalizerSuspendedRow(t, store)

		err := mgr.Attach(context.Background(), testActor(mgr, ActorOperator), b.ID, "claude --resume abc", runtime.Config{})
		if !errors.Is(err, boom) {
			t.Fatalf("Attach error = %v, want it to wrap the finalizer's", err)
		}
		for _, c := range sp.Calls {
			if c.Method == "Start" {
				t.Fatalf("a finalizer error must start no runtime, saw %#v", c)
			}
		}
	})
}

// TestLaunchFinalizerNilIsIdentity (T10): without a finalizer a launch is the
// config the Manager assembled, byte for byte.
func TestLaunchFinalizerNilIsIdentity(t *testing.T) {
	withSp, withoutSp := runtime.NewFake(), runtime.NewFake()
	identity := func(lc LaunchContext) (runtime.Config, error) { return lc.Config, nil }
	withMgr := newTestManager(t, beads.NewMemStore(), withSp, WithLaunchFinalizer(identity))
	withoutMgr := newTestManager(t, beads.NewMemStore(), withoutSp)

	if _, err := withMgr.CreateSession(context.Background(), finalizerCreateOptions()); err != nil {
		t.Fatal(err)
	}
	if _, err := withoutMgr.CreateSession(context.Background(), finalizerCreateOptions()); err != nil {
		t.Fatal(err)
	}
	a, b := withSp.LastStartConfig("sky"), withoutSp.LastStartConfig("sky")
	if a == nil || b == nil {
		t.Fatalf("no Start recorded: %#v / %#v", withSp.Calls, withoutSp.Calls)
	}
	// Identity env carries ids that differ between the two cities; compare the
	// launch shape, which an identity finalizer must not change.
	if a.Command != b.Command || a.WorkDir != b.WorkDir || a.Nudge != b.Nudge || a.PromptSuffix != b.PromptSuffix || a.PromptFlag != b.PromptFlag {
		t.Errorf("identity finalizer changed the launch:\n with:    %+v\n without: %+v", *a, *b)
	}
	if len(a.Env) != len(b.Env) {
		t.Errorf("env size %d vs %d", len(a.Env), len(b.Env))
	}
}

// TestLaunchFinalizerSeamsAreOnlyTheTwoLaunchBranches (T7): a runtime start
// that bypasses the finalizer re-opens ga-4k4zfk — a launch whose prompt rides
// no payload and no marker. Every sp.Start( in non-test session code must live
// in a function this guard knows, and the two hint-based launch seams must
// finalize while the other two must not.
//
// If this fails because you added a new start path: decide whether it builds
// its config from hints (it must call m.launchFinalizer, see the
// ga-vytkfv ruling) or reuses an already-finalized config (add it to
// reusesFinalized below, with the reason).
func TestLaunchFinalizerSeamsAreOnlyTheTwoLaunchBranches(t *testing.T) {
	finalizing := map[string]bool{"createStarted": true, "ensureRunningOn": true}
	reusesFinalized := map[string]bool{"ensureRunningRuntimeOnly": true, "retryFreshStartAfterStaleKey": true}

	_, file, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(file)
	if root := bazeltest.OverrideRoot(); root != "" {
		dir = filepath.Join(root, "internal", "session")
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no session sources under %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	startedIn := map[string]bool{}
	refsFinalizer := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Start" {
						if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "sp" {
							startedIn[fn.Name.Name] = true
						}
					}
				case *ast.SelectorExpr:
					if x.Sel.Name == "launchFinalizer" {
						refsFinalizer[fn.Name.Name] = true
					}
				}
				return true
			})
		}
	}
	for name := range startedIn {
		if !finalizing[name] && !reusesFinalized[name] {
			t.Errorf("%s starts a runtime with sp.Start( but is not a known launch seam: finalize its config through m.launchFinalizer or declare why it reuses one (ga-vytkfv ruling; ga-4k4zfk)", name)
		}
	}
	for name := range finalizing {
		if !startedIn[name] {
			t.Errorf("%s no longer starts a runtime: update this guard", name)
		}
		if !refsFinalizer[name] {
			t.Errorf("%s starts a runtime without finalizing its config: call m.launchFinalizer before sp.Start (ga-vytkfv ruling)", name)
		}
	}
	for name := range reusesFinalized {
		if refsFinalizer[name] {
			t.Errorf("%s must not call the finalizer: it reuses an already-finalized config (ga-vytkfv ruling)", name)
		}
	}
}

// TestLaunchFinalizerInterruptRelaunchOfRunningSession (T1 row): an
// interrupting submit that hard-restarts a running session relaunches it
// through ensureRunning, so the relaunch carries the finalized config too: it
// is the launch that left a prompted session hook-only (ga-4k4zfk).
func TestLaunchFinalizerInterruptRelaunchOfRunningSession(t *testing.T) {
	rec := &recordingFinalizer{}
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := newTestManager(t, store, sp, WithLaunchFinalizer(rec.finalize))
	b, err := store.Create(beads.Bead{
		Title: "worker", Type: BeadType, Labels: []string{LabelSession},
		Metadata: map[string]string{
			"session_name": "test-city--worker", "state": string(StateActive),
			"template": "worker", "work_dir": t.TempDir(), "provider": "pi", "provider_kind": "pi",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "test-city--worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	starts := sp.CountCalls("Start", "test-city--worker")

	if _, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), b.ID, "now", "pi --resume k", runtime.Config{}, SubmitIntentInterruptNow); err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if got := sp.CountCalls("Start", "test-city--worker") - starts; got != 1 {
		t.Fatalf("interrupt relaunch made %d Start calls, want 1", got)
	}
	if len(rec.seen) != 1 || rec.seen[0].Kind != LaunchResume {
		t.Fatalf("finalizer saw %d launches (%+v), want one resume", len(rec.seen), rec.seen)
	}
	if got := sp.LastStartConfig("test-city--worker"); got == nil || got.Nudge != finalizedNudge {
		t.Fatalf("relaunch config = %+v, want the finalizer's output", got)
	}
}
