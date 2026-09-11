package rig

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/git"
)

// errComposeSentinel short-circuits Provision at step 5 (ComposePacks), which
// runs just after the clone step. A test can assert what happened up to that
// point without needing a fully valid city on disk.
var errComposeSentinel = errors.New("compose short-circuit")

// depsWithComposeSentinel is stubDeps whose ComposePacks fails, so Provision
// stops right after the clone step.
func depsWithComposeSentinel(t *testing.T) Deps {
	t.Helper()
	d := stubDeps(t.TempDir())
	d.ComposePacks = func(string, []config.BoundImport) ([]config.BoundImport, func() error, error) {
		return nil, nil, errComposeSentinel
	}
	return d
}

func TestProvision_CloneGitURLInvokedWithHardenedArgs(t *testing.T) {
	deps := depsWithComposeSentinel(t)
	rigPath := filepath.Join(t.TempDir(), "rig")

	var gotURL, gotDst string
	var gotOpts git.CloneOptions
	var called bool
	deps.CloneGitURL = func(_ context.Context, gitURL, dstDir string, opts git.CloneOptions) error {
		called = true
		gotURL, gotDst, gotOpts = gitURL, dstDir, opts
		return nil
	}

	_, _, err := Provision(deps, ProvisionRequest{
		Name:              "x",
		Path:              rigPath,
		GitURL:            "https://github.com/o/r",
		RecurseSubmodules: false,
	})
	// Flow continued past the clone into ComposePacks, proving the clone ran and
	// did not short-circuit the pipeline.
	if !errors.Is(err, errComposeSentinel) {
		t.Fatalf("Provision err = %v, want the ComposePacks sentinel (flow past clone)", err)
	}
	if !called {
		t.Fatal("Deps.CloneGitURL was not invoked for a req.GitURL")
	}
	if gotURL != "https://github.com/o/r" {
		t.Errorf("clone gitURL = %q, want the request URL", gotURL)
	}
	if gotDst != rigPath {
		t.Errorf("clone dst = %q, want rigPath %q", gotDst, rigPath)
	}
	if gotOpts.RecurseSubmodules {
		t.Errorf("clone opts.RecurseSubmodules = true, want false (default off)")
	}
}

func TestProvision_CloneGitURLRecurseSubmodulesThreaded(t *testing.T) {
	deps := depsWithComposeSentinel(t)
	var gotOpts git.CloneOptions
	deps.CloneGitURL = func(_ context.Context, _, _ string, opts git.CloneOptions) error {
		gotOpts = opts
		return nil
	}
	_, _, _ = Provision(deps, ProvisionRequest{
		Name:              "x",
		Path:              filepath.Join(t.TempDir(), "rig"),
		GitURL:            "https://github.com/o/r",
		RecurseSubmodules: true,
	})
	if !gotOpts.RecurseSubmodules {
		t.Errorf("clone opts.RecurseSubmodules = false, want true (threaded from request)")
	}
}

func TestProvision_CloneGitURLFailureIsFatalAndShortCircuits(t *testing.T) {
	deps := stubDeps(t.TempDir())
	cloneErr := errors.New("boom: clone rejected")

	var composeCalled, writeRoutesCalled bool
	deps.ComposePacks = func(string, []config.BoundImport) ([]config.BoundImport, func() error, error) {
		composeCalled = true
		return nil, nil, nil
	}
	deps.WriteRoutes = func(string, *config.City) error {
		writeRoutesCalled = true
		return nil
	}
	deps.CloneGitURL = func(_ context.Context, _, _ string, _ git.CloneOptions) error {
		return cloneErr
	}

	_, _, err := Provision(deps, ProvisionRequest{
		Name:   "x",
		Path:   filepath.Join(t.TempDir(), "rig"),
		GitURL: "https://github.com/o/r",
	})
	if !errors.Is(err, cloneErr) {
		t.Fatalf("Provision err = %v, want the clone error", err)
	}
	if composeCalled {
		t.Error("ComposePacks ran after a failed clone; clone failure must short-circuit")
	}
	if writeRoutesCalled {
		t.Error("WriteRoutes ran after a failed clone; no config must be written")
	}
}

func TestProvision_NilCloneGitURLSkipsCloneEvenWithGitURL(t *testing.T) {
	// The CLI passes a nil CloneGitURL. Even if a GitURL somehow arrives, the nil
	// guard must skip the clone (a nil call would panic) and let the flow proceed
	// to ComposePacks exactly as a local add would.
	deps := depsWithComposeSentinel(t)
	deps.CloneGitURL = nil

	_, _, err := Provision(deps, ProvisionRequest{
		Name:   "x",
		Path:   filepath.Join(t.TempDir(), "rig"),
		GitURL: "https://github.com/o/r",
	})
	if !errors.Is(err, errComposeSentinel) {
		t.Fatalf("Provision err = %v, want the ComposePacks sentinel (nil clone skipped cleanly)", err)
	}
}
