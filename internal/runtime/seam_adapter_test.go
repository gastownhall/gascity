package runtime

import (
	"context"
	"testing"
)

// Minimal in-test seam fakes used to pin seamProvider.Stop's teardown contract
// (SEAM-1/2/3): teardown must be UNCONDITIONAL — it runs even when Open reports
// the box is not running. A liveness gate on the teardown path leaks the box (a
// non-Running pod + its PVC, a t3 event-watcher goroutine, a tmux corpse).

type fakeSeamRuntime struct {
	openOK    bool     // does Open report the box as running?
	teardowns []string // names passed to Teardown, in order
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
	return nil
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
	openOK bool // does Open report a live attachment?
	closed int  // count of Attachment.Close calls
}

func (t *fakeSeamTransport) Launch(context.Context, Place, LaunchSpec) (Attachment, error) {
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
func (t *fakeSeamTransport) Capabilities() TransportCapabilities         { return TransportCapabilities{} }

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
