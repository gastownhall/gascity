//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// controlDispatchSigtermChildEnv carries the JSON-encoded
// controlDispatchSigtermChildArgs of a re-executed cmd/gc test binary that
// runs one workflow-finalize control and nothing else.
const controlDispatchSigtermChildEnv = "GC_TEST_CONTROL_DISPATCH_SIGTERM_CHILD"

type controlDispatchSigtermChildArgs struct {
	CityPath    string `json:"city_path"`
	FinalizerID string `json:"finalizer_id"`
}

// maybeRunControlDispatchSigtermChild turns a re-executed cmd/gc test binary
// into a control dispatcher that processes one control bead, before TestMain
// installs its leak guard: that guard catches SIGTERM itself, which would
// hide what the dispatcher does with one.
func maybeRunControlDispatchSigtermChild() {
	raw := os.Getenv(controlDispatchSigtermChildEnv)
	if raw == "" {
		return
	}
	// Under bazel test this binary inherits TEST_TIMEOUT, so rules_go's
	// generated main has already registered a SIGTERM handler. Drop it: the
	// child starts with the disposition a gc process starts with.
	signal.Reset(syscall.SIGTERM)
	var args controlDispatchSigtermChildArgs
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		fmt.Fprintf(os.Stderr, "control dispatch sigterm child: decoding args: %v\n", err) //nolint:errcheck
		os.Exit(97)
	}
	store, err := openStoreAtForCity(args.CityPath, args.CityPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "control dispatch sigterm child: opening store: %v\n", err) //nolint:errcheck
		os.Exit(97)
	}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	err = runControlDispatcherWithStoreAndConfig(args.CityPath, args.CityPath, store, args.FinalizerID, cfg, io.Discard, os.Stderr)
	fmt.Fprintf(os.Stdout, "control dispatcher returned: %v\n", err) //nolint:errcheck
	os.Exit(0)
}

// TestControlDispatcherStopDuringWorkflowFinalizeEndsTheProcess pins what a
// stop does to a control dispatcher that is in the middle of a
// workflow-finalize control: the SIGTERM ends the process, exactly as it does
// for every other control kind and for an idle serve loop, and the finalizer
// stays open for the next dispatcher.
//
// The child parks inside the finalize on the source bead's workflow lock, which
// this test holds. A closed workflow root says it got there: the finalize
// closes the root before it takes that lock.
func TestControlDispatcherStopDuringWorkflowFinalizeEndsTheProcess(t *testing.T) {
	processgrouptest.RequireRealProcessSignals(t)
	clearGCEnv(t)

	cityPath := t.TempDir()
	// The provider comes from city.toml, not GC_BEADS: the child's testenv
	// init scrubs GC_BEADS and the other store-routing variables.
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n\n[daemon]\nformula_v2 = true\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	store, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	source, err := store.Create(beads.Bead{Title: "Source", Type: "task"})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	root, err := store.Create(beads.Bead{
		Title: "Workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         "workflow",
			"gc.formula_contract":            "graph.v2",
			beadmeta.SourceBeadIDMetadataKey: source.ID,
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	step, err := store.Create(beads.Bead{
		Title: "step",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	if err := store.Close(step.ID); err != nil {
		t.Fatalf("close step: %v", err)
	}
	finalizer, err := store.Create(beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindWorkflowFinalize,
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("create finalizer: %v", err)
	}
	if err := store.DepAdd(finalizer.ID, step.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(finalizer->step): %v", err)
	}
	if err := store.DepAdd(root.ID, finalizer.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd(root->finalizer): %v", err)
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	lockScope := sourceWorkflowLockScopeForStoreRef(cityPath, cfg, cityPath, "")
	held := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- sourceworkflow.WithLock(context.Background(), cityPath, lockScope, source.ID, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	t.Cleanup(func() {
		close(release)
		if err := <-lockDone; err != nil {
			t.Errorf("source workflow lock: %v", err)
		}
	})
	select {
	case <-held:
	case err := <-lockDone:
		t.Fatalf("taking the source workflow lock: %v", err)
	}

	rawArgs, err := json.Marshal(controlDispatchSigtermChildArgs{CityPath: cityPath, FinalizerID: finalizer.ID})
	if err != nil {
		t.Fatalf("encoding child args: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	// GC_HOME is the test's own home; without the passthrough the child's
	// testenv init would scrub it and the child would resolve the real one.
	cmd.Env = append(os.Environ(),
		"GC_TESTENV_PASSTHROUGH=GC_HOME",
		controlDispatchSigtermChildEnv+"="+string(rawArgs),
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting control dispatcher child: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	exited := false
	t.Cleanup(func() {
		if !exited {
			_ = cmd.Process.Kill()
			<-waited
		}
	})

	beadsPath := filepath.Join(cityPath, ".gc", "beads.json")
	rootStatus := func() string {
		view, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
		if err != nil {
			t.Fatalf("reopening the store: %v", err)
		}
		got, err := view.Get(root.ID)
		if err != nil {
			t.Fatalf("reading the workflow root: %v", err)
		}
		return got.Status
	}
	// dumpAndFail ends a child that is stuck where it should not be. SIGQUIT
	// makes the Go runtime print every goroutine's stack into the output.
	dumpAndFail := func(format string, args ...any) {
		t.Helper()
		_ = cmd.Process.Signal(syscall.SIGQUIT)
		<-waited
		exited = true
		t.Fatalf(format+"\n%s", append(args, output.String())...)
	}
	poll := time.NewTicker(25 * time.Millisecond)
	defer poll.Stop()
	ready := time.NewTimer(60 * time.Second)
	defer ready.Stop()
	for rootStatus() != "closed" {
		select {
		case err := <-waited:
			exited = true
			t.Fatalf("control dispatcher child exited (%v) before the finalize reached the source bead lock:\n%s", err, output.String())
		case <-ready.C:
			dumpAndFail("workflow root still open after 60s: the finalize never reached the source bead lock")
		case <-poll.C:
		}
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signaling the control dispatcher child: %v", err)
	}
	stopped := time.NewTimer(30 * time.Second)
	defer stopped.Stop()
	var waitErr error
	select {
	case waitErr = <-waited:
		exited = true
	case <-stopped.C:
		dumpAndFail("control dispatcher child still running 30s after SIGTERM: the stop was swallowed")
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		t.Fatalf("control dispatcher child exited cleanly (%v) after SIGTERM, want it ended by the signal: the finalize caught the stop and kept running:\n%s", waitErr, output.String())
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGTERM {
		t.Fatalf("control dispatcher child ended with %v, want it ended by SIGTERM:\n%s", waitErr, output.String())
	}

	view, err := beads.OpenFileStore(fsys.OSFS{}, beadsPath)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	after, err := view.Get(finalizer.ID)
	if err != nil {
		t.Fatalf("reading the finalizer: %v", err)
	}
	if after.Status != "open" {
		t.Errorf("finalizer status = %q after the stop, want open: it is the retry handle for the source bead close", after.Status)
	}
	if slices.Contains(after.Labels, "gc:control-quarantined") || after.Metadata[beadmeta.ControlQuarantinedMetadataKey] != "" {
		t.Errorf("finalizer was quarantined by the stop: labels=%v quarantine_reason=%q", after.Labels, after.Metadata[beadmeta.ControlQuarantineReasonMetadataKey])
	}
	sourceAfter, err := view.Get(source.ID)
	if err != nil {
		t.Fatalf("reading the source bead: %v", err)
	}
	if sourceAfter.Status != "open" {
		t.Errorf("source bead status = %q, want open: the stop came before the finalize took its lock", sourceAfter.Status)
	}
}
