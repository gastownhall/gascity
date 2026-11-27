package k8s

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	execerr "k8s.io/client-go/util/exec"

	"github.com/gastownhall/gascity/internal/runtime"
)

// hasSessionAlive marks the in-box tmux session as present so IsRunning is true.
func hasSessionAlive(fake *fakeK8sOps, pod string) { //nolint:unparam // pod varies in future tests
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

	obs, err := att.Observe(ctx, []string{"claude"})
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
	if obs, err := att.Observe(ctx, nil); err != nil || !obs.Attached {
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

// TestSeamsK8sStopDeletesNonRunningPod is the SEAM-1 carrier guard: Stop through
// the full seam path (adapter + k8s Runtime.Teardown) must delete a pod that
// EXISTS by label but is not Running (Pending/Failed/CrashLoopBackOff). The
// pre-fix gated Stop no-opped here because Open reports not-running, leaking the
// pod + its PVC.
func TestSeamsK8sStopDeletesNonRunningPod(t *testing.T) {
	fake := newFakeK8sOps()
	p := newProviderWithOps(fake)
	addFailedPod(fake, "s", "s") // exists by label, but not Running
	if p.IsRunning("s") {
		t.Fatal("precondition: IsRunning must be false for a Failed pod")
	}

	// Drive Stop through the seams exactly as production does (NewSeamBacked).
	seam := runtime.NewProviderFromSeams(p.Seams())
	if err := seam.Stop("s"); err != nil {
		t.Fatalf("seam Stop: %v", err)
	}

	if _, exists := fake.pods["s"]; exists {
		t.Fatal("non-running pod still present after seam Stop (SEAM-1 leak)")
	}
	deleted := false
	for _, c := range fake.calls {
		if c.method == "deletePod" && c.pod == "s" {
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("seam Stop should call deletePod for the non-running pod")
	}
}

// TestSeamsK8sStopSurfacesTransportFailure is the M3 carrier guard for the
// un-weld lifecycle (Runtime.Teardown → Stop): a transport failure during
// teardown must surface as an error so the seam adapter keeps tracking the
// session, instead of reporting SUCCESS while the pod + its PVC keep running
// untracked (the same rationale ssh.Stop already uses). A genuine Kubernetes
// NotFound stays idempotent (nil) — the session is really gone.
func TestSeamsK8sStopSurfacesTransportFailure(t *testing.T) {
	podGR := schema.GroupResource{Group: "", Resource: "pods"}

	t.Run("listPods transport failure surfaces (unknown state, not gone)", func(t *testing.T) {
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		fake.listErr = errors.New("etcdserver: request timed out")

		seam := runtime.NewProviderFromSeams(p.Seams())
		err := seam.Stop("s")
		if err == nil {
			t.Fatal("seam Stop must surface a pod-list transport failure (unknown state), not swallow it")
		}
		if !errors.Is(err, fake.listErr) {
			t.Fatalf("seam Stop error = %v; want it to wrap the list transport error", err)
		}
	})

	t.Run("deletePod transport failure surfaces and is joined", func(t *testing.T) {
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		addFailedPod(fake, "s", "s") // exists by label, so Stop reaches deletePod
		fake.deleteErr = errors.New("connection refused")

		seam := runtime.NewProviderFromSeams(p.Seams())
		err := seam.Stop("s")
		if err == nil {
			t.Fatal("seam Stop must surface a pod-delete transport failure, not discard it")
		}
		if !errors.Is(err, fake.deleteErr) {
			t.Fatalf("seam Stop error = %v; want it to join the delete transport error", err)
		}
		// The pod is still present because the delete failed — proving the
		// surfaced error matches reality (the pod was NOT torn down).
		if _, exists := fake.pods["s"]; !exists {
			t.Fatal("precondition: a failed deletePod must leave the pod present")
		}
	})

	t.Run("NotFound on delete stays idempotent (nil)", func(t *testing.T) {
		fake := newFakeK8sOps()
		p := newProviderWithOps(fake)
		addFailedPod(fake, "s", "s") // listed by label, then delete races a gone pod
		fake.deleteErr = apierrors.NewNotFound(podGR, "s")

		seam := runtime.NewProviderFromSeams(p.Seams())
		if err := seam.Stop("s"); err != nil {
			t.Fatalf("seam Stop on a genuine NotFound = %v; want nil (idempotent: session gone)", err)
		}
	})
}

// Kills: a listing attestation lost behind the seam-backed provider. Per
// CAPABILITIES A.0 a seam-backed provider has only the optional interfaces its
// cut-over forwards, and production constructs k8s through NewSeamBacked.
func TestCutoverForwardsListingAttestation(t *testing.T) {
	raw := newProviderWithOps(newFakeK8sOps())
	if !runtime.ListRunningAttested(raw) {
		t.Fatal("raw k8s provider does not attest its listing")
	}
	var sp runtime.Provider = &seamBackedProvider{Provider: runtime.NewProviderFromSeams(raw.Seams()), raw: raw}
	if !runtime.ListRunningAttested(sp) {
		t.Fatal("seam-backed k8s provider does not forward its listing attestation")
	}

	// The attestation promises complete-or-err, so a failed pod list must
	// surface as an error on both the raw and the seam-backed listing.
	fake := newFakeK8sOps()
	fake.listErr = errors.New("apiserver unavailable")
	raw = newProviderWithOps(fake)
	sp = &seamBackedProvider{Provider: runtime.NewProviderFromSeams(raw.Seams()), raw: raw}
	for label, lister := range map[string]runtime.Provider{"raw": raw, "seam-backed": sp} {
		names, err := lister.ListRunning("")
		if !errors.Is(err, fake.listErr) {
			t.Errorf("%s ListRunning on a failed pod list = (%q, %v), want the list error", label, names, err)
		}
	}
}

// sessionListProbe is what one session-list enrichment reads for a session.
type sessionListProbe struct {
	running      bool
	attached     bool
	lastActivity time.Time
	runningAgain bool
}

// runSessionListProbes runs the session-list probe sequence (IsRunning,
// IsAttached, GetLastActivity, IsRunning) for every name.
func runSessionListProbes(t *testing.T, sp runtime.Provider, names []string) []sessionListProbe {
	t.Helper()
	out := make([]sessionListProbe, 0, len(names))
	for _, name := range names {
		var probe sessionListProbe
		probe.running = sp.IsRunning(name)
		probe.attached = sp.IsAttached(name)
		activity, err := sp.GetLastActivity(name)
		if err != nil {
			t.Fatalf("GetLastActivity(%q): %v", name, err)
		}
		probe.lastActivity = activity
		probe.runningAgain = sp.IsRunning(name)
		out = append(out, probe)
	}
	return out
}

// TestSessionListProbesShareOnePodListing pins issue #4390: the probes one
// session-list request makes for each session (IsRunning, IsAttached,
// GetLastActivity, IsRunning) went through the seam adapter's attach fan-out,
// about 10 pod LISTs and 10 execs per session, all behind client-go's rate
// limiter. Through the seam-backed provider and its pod snapshot, every probe
// within one TTL shares a single app=gc-agent LIST and pays only its own exec,
// and every answer equals the snapshot-disabled seam path's.
func TestSessionListProbesShareOnePodListing(t *testing.T) {
	const genuine = int64(1709300000)
	live := func(name string) probeSession {
		return probeSession{name: name, pod: true, tmux: true, attached: "0", activity: genuine}
	}
	for _, tc := range []struct {
		name      string
		sessions  []probeSession
		poke      string // when set, this session is nudged before the probes
		wantExecs int
	}{
		{
			name:      "three live sessions",
			sessions:  []probeSession{live("a"), live("b"), live("c")},
			wantExecs: 12,
		},
		{
			name:      "tmux dead in a Running pod",
			sessions:  []probeSession{live("a"), {name: "b", pod: true}, live("c")},
			wantExecs: 12,
		},
		{
			name:      "no pod at all",
			sessions:  []probeSession{{name: "a"}, {name: "b"}, {name: "c"}},
			wantExecs: 0,
		},
		{
			name: "attached versus detached",
			sessions: []probeSession{
				{name: "a", pod: true, tmux: true, attached: "1", activity: genuine},
				live("b"),
				{name: "c", pod: true, tmux: true, attached: "1", activity: genuine + 5},
			},
			wantExecs: 12,
		},
		{
			name:      "poke-discounted activity",
			sessions:  []probeSession{live("a"), live("b"), live("c")},
			poke:      "b",
			wantExecs: 12,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names := make([]string, 0, len(tc.sessions))
			for _, s := range tc.sessions {
				names = append(names, s.name)
			}
			start := time.Unix(genuine, 0).Add(time.Hour)

			ops := newProbeOps(tc.sessions)
			raw, clock := newSnapshotProvider(ops)
			clock.set(start)
			sp := &seamBackedProvider{Provider: runtime.NewProviderFromSeams(raw.Seams()), raw: raw}

			refOps := newProbeOps(tc.sessions)
			refRaw := newProviderWithOps(refOps)
			refRaw.now = clock.now
			ref := runtime.NewProviderFromSeams(refRaw.Seams())

			if tc.poke != "" {
				if err := raw.Nudge(tc.poke, runtime.TextContent("wake up")); err != nil {
					t.Fatalf("Nudge: %v", err)
				}
				if err := refRaw.Nudge(tc.poke, runtime.TextContent("wake up")); err != nil {
					t.Fatalf("reference Nudge: %v", err)
				}
				// Past the grace an unanswered poke's echo is discounted.
				clock.set(start.Add(runtime.PokeGrace + time.Second))
			}
			ops.reset()

			got := runSessionListProbes(t, sp, names)

			if lists := ops.listCalls(); len(lists) != 1 || lists[0] != (listCall{selector: "app=gc-agent"}) {
				t.Errorf("pod LISTs = %+v, want exactly one {selector: app=gc-agent}", lists)
			}
			if execs := ops.execCount(); execs != tc.wantExecs {
				t.Errorf("execs = %d, want %d", execs, tc.wantExecs)
			}
			want := runSessionListProbes(t, ref, names)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("snapshot probes = %+v,\nsnapshot-disabled seam path = %+v", got, want)
			}
			if tc.poke != "" {
				for i, name := range names {
					if name == tc.poke && !got[i].lastActivity.Equal(time.Unix(genuine, 0)) {
						t.Errorf("poked session's LastActivity = %v, want the genuine %v", got[i].lastActivity, time.Unix(genuine, 0))
					}
				}
			}
		})
	}
}
