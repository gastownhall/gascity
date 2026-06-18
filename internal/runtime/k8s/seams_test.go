package k8s

import (
	"context"
	"fmt"
	"testing"

	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
)

// hasSessionAlive marks the in-box tmux session as present so IsRunning is true.
func hasSessionAlive(fake *fakeK8sOps, pod string) {
	fake.setExecResult(pod, []string{"tmux", "has-session", "-t", tmuxSession}, "", nil)
}

// TestSeamsK8sExecAndOpen proves Place.Exec delegates to execInPod (preserving
// the (output, code, err) contract) and Runtime.Open reflects pod+tmux liveness.
func TestSeamsK8sExecAndOpen(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	hasSessionAlive(fake, "s")
	rt, _ := p.Seams()
	ctx := context.Background()

	place, ok, err := rt.Open(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("Open(live) = %v, %v; want true, nil", ok, err)
	}

	// Real exec over execInPod: stdout + code 0.
	fake.setExecResult("s", []string{"echo", "hi"}, "hi\n", nil)
	res, err := place.Exec(ctx, runtime.ExecRequest{Argv: []string{"echo", "hi"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(res.Output) != "hi\n" || res.Code != 0 {
		t.Fatalf("Exec = %q, code %d; want hi, 0", res.Output, res.Code)
	}

	// A non-zero in-pod exit is the command's own result (code, nil err), with
	// the code extracted from the util/exec.ExitError.
	fake.setExecResult("s", []string{"false"}, "", execerr.CodeExitError{Err: fmt.Errorf("exit 7"), Code: 7})
	res, err = place.Exec(ctx, runtime.ExecRequest{Argv: []string{"false"}})
	if err != nil {
		t.Fatalf("Exec(false) err: %v; want nil (command's own exit)", err)
	}
	if res.Code != 7 {
		t.Fatalf("Exec(false) code = %d; want 7", res.Code)
	}

	// Open on an absent session is (nil,false,nil).
	if pl, ok, err := rt.Open(ctx, "ghost"); pl != nil || ok || err != nil {
		t.Fatalf("Open(absent) = %v, %v, %v; want nil, false, nil", pl, ok, err)
	}
}

// TestSeamsK8sCapabilitiesAndTransport proves the capability split (k8s reports
// activity only) and the transport identity.
func TestSeamsK8sCapabilitiesAndTransport(t *testing.T) {
	rt, tp := newProviderWithOps(newFakeK8sOps()).Seams()

	if caps := rt.Capabilities(); !caps.ReportActivity {
		t.Fatalf("PlaceCapabilities = %+v; want ReportActivity true", caps)
	}
	if tp.Capabilities().ReportAttachment {
		t.Fatal("TransportCapabilities.ReportAttachment should be false for k8s")
	}
	if tp.Name() != "tmux" {
		t.Fatalf("Name = %q; want tmux", tp.Name())
	}
}

// TestSeamsK8sMetaStore proves the MetaStore seam round-trips through the tmux
// session environment (set-environment / show-environment over execInPod).
func TestSeamsK8sMetaStore(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	rt, _ := p.Seams()

	ms, ok := rt.(runtime.MetaStore)
	if !ok {
		t.Fatal("k8s Runtime should implement runtime.MetaStore")
	}
	fake.setExecResult("s", []string{"tmux", "set-environment", "-t", tmuxSession, "k", "v"}, "", nil)
	if err := ms.SetMeta("s", "k", "v"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	fake.setExecResult("s", []string{"tmux", "show-environment", "-t", tmuxSession, "k"}, "k=v", nil)
	if got, err := ms.GetMeta("s", "k"); err != nil || got != "v" {
		t.Fatalf("GetMeta = %q, %v; want v, nil", got, err)
	}
	// An explicitly-unset key (tmux prints "-KEY") parses to empty.
	fake.setExecResult("s", []string{"tmux", "show-environment", "-t", tmuxSession, "missing"}, "-missing", nil)
	if got, err := ms.GetMeta("s", "missing"); err != nil || got != "" {
		t.Fatalf("GetMeta(unset) = %q, %v; want empty, nil", got, err)
	}
	fake.setExecResult("s", []string{"tmux", "set-environment", "-t", tmuxSession, "-u", "k"}, "", nil)
	if err := ms.RemoveMeta("s", "k"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
}

// TestSeamsK8sObserve proves Attachment.Observe folds the liveness reads:
// ProcessAlive via pgrep in the pod, Attached false (k8s can't observe it).
func TestSeamsK8sObserve(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	hasSessionAlive(fake, "s")
	rt, tp := p.Seams()
	ctx := context.Background()

	place, ok, err := rt.Open(ctx, "s")
	if err != nil || !ok {
		t.Fatalf("Open: %v, %v", ok, err)
	}
	att, err := tp.Launch(ctx, place, runtime.LaunchSpec{Config: runtime.Config{ProcessNames: []string{"claude"}}})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	fake.setExecResult("s", []string{"pgrep", "-f", "claude"}, "1234\n", nil)
	attachedCmd := []string{"tmux", "display-message", "-t", tmuxSession, "-p", "#{session_attached}"}
	fake.setExecResult("s", attachedCmd, "0\n", nil)

	obs, err := att.Observe(ctx)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ProcessAlive {
		t.Fatal("Observe ProcessAlive = false; want true (pgrep found the process)")
	}
	if obs.Attached {
		t.Fatal("Observe Attached = true; want false (#{session_attached}=0)")
	}

	// And the positive case proves the parse, not just the empty default.
	fake.setExecResult("s", attachedCmd, "1\n", nil)
	if obs, err := att.Observe(ctx); err != nil || !obs.Attached {
		t.Fatalf("Observe after attach = %+v, %v; want Attached true", obs, err)
	}
}

// TestSeamsK8sTransportOpen proves Transport.Open (reconnect) returns a live
// Attachment for a running pod and (nil,false,nil) for a dead one.
func TestSeamsK8sTransportOpen(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	hasSessionAlive(fake, "s")
	rt, tp := p.Seams()
	ctx := context.Background()

	place, _, _ := rt.Open(ctx, "s")
	if att, ok, err := tp.Open(ctx, place, "s"); att == nil || !ok || err != nil {
		t.Fatalf("Transport.Open(live) = %v, %v, %v; want attachment, true, nil", att, ok, err)
	}

	dead := &k8sPlace{p: newProviderWithOps(newFakeK8sOps()), name: "ghost"}
	if att, ok, err := tp.Open(ctx, dead, "ghost"); att != nil || ok || err != nil {
		t.Fatalf("Transport.Open(dead) = %v, %v, %v; want nil, false, nil", att, ok, err)
	}
}

// TestSeamsK8sStageAndTeardown proves Place.Stage delegates to CopyTo
// (best-effort no-op when the pod is absent) and Place.Teardown deletes the pod.
func TestSeamsK8sStageAndTeardown(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addRunningPod(fake, "s", "s")
	ctx := context.Background()

	// Stage on an absent session no-ops (CopyTo returns nil when no pod).
	ghost := &k8sPlace{p: p, name: "ghost"}
	if err := ghost.Stage(ctx, []runtime.CopyEntry{{Src: "/nope", RelDst: "x"}}); err != nil {
		t.Fatalf("Stage(no pod) = %v; want nil (best-effort)", err)
	}

	// Teardown deletes the pod.
	place := &k8sPlace{p: p, name: "s"}
	if err := place.Teardown(ctx); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if _, exists := fake.pods["s"]; exists {
		t.Fatal("pod still present after Teardown")
	}
	// Prove the mechanism, not just the effect: Teardown went through deletePod.
	deleted := false
	for _, c := range fake.calls {
		if c.method == "deletePod" && c.pod == "s" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("Teardown should call deletePod for the session pod")
	}
}
