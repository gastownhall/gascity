package runtime

import (
	"context"
	"errors"
	"testing"
)

// Minimal in-test seam fakes used to pin seamProvider.Stop's teardown contract
// (SEAM-1/2/3): teardown must be UNCONDITIONAL — it runs even when Open reports
// the box is not running. A liveness gate on the teardown path leaks the box (a
// non-Running pod + its PVC, a t3 event-watcher goroutine, a tmux corpse).

type fakeSeamRuntime struct {
	openOK      bool     // does Open report the box as running?
	teardowns   []string // names passed to Teardown, in order
	teardownErr error    // when non-nil, Teardown fails with this
}

func (r *fakeSeamRuntime) Provision(context.Context, string, ProvisionRequest) (Place, error) {
	return &fakeSeamPlace{}, nil
}

func (r *fakeSeamRuntime) Open(_ context.Context, _ string) (Place, bool, error) {
	if !r.openOK {
		return nil, false, nil
	}
	return &fakeSeamPlace{}, true, nil
}

func (r *fakeSeamRuntime) Teardown(_ context.Context, name string) error {
	r.teardowns = append(r.teardowns, name)
	return r.teardownErr
}

func (r *fakeSeamRuntime) List(context.Context, string) ([]string, error) { return nil, nil }
func (r *fakeSeamRuntime) Capabilities() PlaceCapabilities                { return PlaceCapabilities{} }

type fakeSeamPlace struct{}

func (*fakeSeamPlace) Exec(context.Context, ExecRequest) (ExecResult, error) {
	return ExecResult{}, nil
}
func (*fakeSeamPlace) Stage(context.Context, []CopyEntry) error { return nil }
func (*fakeSeamPlace) IsRunning(context.Context) (bool, error)  { return true, nil }
func (*fakeSeamPlace) Teardown(context.Context) error           { return nil }

type fakeSeamTransport struct {
	openOK          bool  // does Open report a live attachment?
	closed          int   // count of Attachment.Close calls
	separableLaunch bool  // does Capabilities report a separable launch?
	launchErr       error // when non-nil, Launch fails with this (B3b launch-failure path)
	launches        int   // count of Launch calls
}

func (t *fakeSeamTransport) Launch(context.Context, Place, LaunchSpec) (Attachment, error) {
	t.launches++
	if t.launchErr != nil {
		return nil, t.launchErr
	}
	return &fakeSeamAttachment{t: t}, nil
}

func (t *fakeSeamTransport) Open(_ context.Context, _ Place, _ string) (Attachment, bool, error) {
	if !t.openOK {
		return nil, false, nil
	}
	return &fakeSeamAttachment{t: t}, true, nil
}

func (t *fakeSeamTransport) Attach(context.Context, Place, string) error { return nil }
func (t *fakeSeamTransport) Name() string                                { return "fake" }
func (t *fakeSeamTransport) Capabilities() TransportCapabilities {
	return TransportCapabilities{SeparableLaunch: t.separableLaunch}
}

type fakeSeamAttachment struct{ t *fakeSeamTransport }

func (*fakeSeamAttachment) Peek(context.Context, int) (string, error)   { return "", nil }
func (*fakeSeamAttachment) Nudge(context.Context, []ContentBlock) error { return nil }
func (*fakeSeamAttachment) SendKeys(context.Context, ...string) error   { return nil }
func (*fakeSeamAttachment) Interrupt(context.Context) error             { return nil }
func (*fakeSeamAttachment) ClearScrollback(context.Context) error       { return nil }
func (*fakeSeamAttachment) Observe(context.Context, []string) (LiveObservation, error) {
	return LiveObservation{}, nil
}
func (a *fakeSeamAttachment) Close(context.Context) error { a.t.closed++; return nil }

// TestSeamProviderStopTearsDownNonRunningBox is the SEAM-1/2/3 regression guard:
// a box that exists but is NOT running (Open reports not-ok) must still be torn
// down. Before the fix, Stop returned nil here and the raw teardown never ran,
// leaking the box.
func TestSeamProviderStopTearsDownNonRunningBox(t *testing.T) {
	rt := &fakeSeamRuntime{openOK: false} // box exists but is not running
	tp := &fakeSeamTransport{}
	p := NewProviderFromSeams(rt, tp)

	if err := p.Stop("dead-box"); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if len(rt.teardowns) != 1 || rt.teardowns[0] != "dead-box" {
		t.Fatalf("teardown must run unconditionally for a not-running box; got %v", rt.teardowns)
	}
}

// TestSeamProviderStopClosesAttachmentThenTearsDown pins the running-box path:
// the live attachment is closed (how-half) AND the box is torn down (where-half).
func TestSeamProviderStopClosesAttachmentThenTearsDown(t *testing.T) {
	rt := &fakeSeamRuntime{openOK: true}
	tp := &fakeSeamTransport{openOK: true}
	p := NewProviderFromSeams(rt, tp)

	if err := p.Stop("live-box"); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}
	if tp.closed != 1 {
		t.Fatalf("attachment Close (how-half) must run for a running box; closed=%d", tp.closed)
	}
	if len(rt.teardowns) != 1 || rt.teardowns[0] != "live-box" {
		t.Fatalf("teardown (where-half) must run for a running box; got %v", rt.teardowns)
	}
}

// TestSeamProviderStartTearsDownBoxWhenLaunchFails pins the separable-launch
// failure path (B3b: an exec pack whose proc.provision creates the box WITHOUT
// the agent, so Start must Provision THEN Launch). If Launch fails after a
// successful Provision, the freshly-provisioned box must be torn down
// best-effort instead of leaking — the asymmetric opposite of the unconditional
// Stop teardown (SEAM-1/2/3 — no leaked boxes).
func TestSeamProviderStartTearsDownBoxWhenLaunchFails(t *testing.T) {
	rt := &fakeSeamRuntime{}
	tp := &fakeSeamTransport{separableLaunch: true, launchErr: errors.New("launch boom")}
	p := NewProviderFromSeams(rt, tp)

	err := p.Start(context.Background(), "leaky-box", Config{})
	if err == nil {
		t.Fatal("Start must surface the launch error")
	}
	if tp.launches != 1 {
		t.Fatalf("Launch should be attempted once on the separable path; launches=%d", tp.launches)
	}
	if len(rt.teardowns) != 1 || rt.teardowns[0] != "leaky-box" {
		t.Fatalf("a failed separable launch must tear down the provisioned box; got %v", rt.teardowns)
	}
}

// TestSeamProviderStartSurfacesTeardownErrorWhenLaunchAndTeardownFail pins the
// double-failure path: when separable Launch fails AND the best-effort Teardown
// of the freshly-provisioned box ALSO fails, Start must surface BOTH errors. A
// discarded teardown failure means the provisioned box may still be running
// untracked, so hiding it behind the launch error alone silently leaks the box
// (SEAM-1/2/3 — no leaked boxes).
func TestSeamProviderStartSurfacesTeardownErrorWhenLaunchAndTeardownFail(t *testing.T) {
	launchErr := errors.New("launch boom")
	teardownErr := errors.New("teardown boom")
	rt := &fakeSeamRuntime{teardownErr: teardownErr}
	tp := &fakeSeamTransport{separableLaunch: true, launchErr: launchErr}
	p := NewProviderFromSeams(rt, tp)

	err := p.Start(context.Background(), "leaky-box", Config{})
	if err == nil {
		t.Fatal("Start must surface an error when launch and teardown both fail")
	}
	if !errors.Is(err, launchErr) {
		t.Fatalf("returned error must preserve the launch failure; got %v", err)
	}
	if !errors.Is(err, teardownErr) {
		t.Fatalf("returned error must preserve the teardown failure; got %v", err)
	}
	if len(rt.teardowns) != 1 || rt.teardowns[0] != "leaky-box" {
		t.Fatalf("a failed separable launch must still attempt teardown; got %v", rt.teardowns)
	}
}

// TestSeamProviderStartSucceedsWithoutTeardown pins the success path so the
// leak fix does not over-correct: a separable launch that succeeds must leave
// the box up (no teardown).
func TestSeamProviderStartSucceedsWithoutTeardown(t *testing.T) {
	rt := &fakeSeamRuntime{}
	tp := &fakeSeamTransport{separableLaunch: true}
	p := NewProviderFromSeams(rt, tp)

	if err := p.Start(context.Background(), "good-box", Config{}); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}
	if tp.launches != 1 {
		t.Fatalf("separable launch should run once; launches=%d", tp.launches)
	}
	if len(rt.teardowns) != 0 {
		t.Fatalf("a successful start must not tear down the box; got %v", rt.teardowns)
	}
}
