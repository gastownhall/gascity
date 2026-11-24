package k8s

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Production defaults the ctx-free verbs must run under (#6055): one request
// bound per request-style verb, a longer one for CopyTo's tar stream.
const (
	wantRequestBound = 30 * time.Second
	wantCopyBound    = 120 * time.Second
)

// deadlineCall is one Kubernetes call a verb made and the deadline its ctx
// carried, if any.
type deadlineCall struct {
	method      string
	deadline    time.Time
	hasDeadline bool
}

// deadlineRecorder wraps the in-memory fake and records the ctx deadline of
// every Kubernetes call, so a test can tell a bounded verb from one running on
// a bare context.Background().
type deadlineRecorder struct {
	*fakeK8sOps
	mu    sync.Mutex
	calls []deadlineCall
}

func (r *deadlineRecorder) note(ctx context.Context, method string) {
	d, ok := ctx.Deadline()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, deadlineCall{method: method, deadline: d, hasDeadline: ok})
}

func (r *deadlineRecorder) recorded() []deadlineCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]deadlineCall(nil), r.calls...)
}

func (r *deadlineRecorder) createPod(ctx context.Context, pod *corev1.Pod) (*corev1.Pod, error) {
	r.note(ctx, "createPod")
	return r.fakeK8sOps.createPod(ctx, pod)
}

func (r *deadlineRecorder) getPod(ctx context.Context, name string) (*corev1.Pod, error) {
	r.note(ctx, "getPod")
	return r.fakeK8sOps.getPod(ctx, name)
}

func (r *deadlineRecorder) deletePod(ctx context.Context, name string, grace int64) error {
	r.note(ctx, "deletePod")
	return r.fakeK8sOps.deletePod(ctx, name, grace)
}

func (r *deadlineRecorder) listPods(ctx context.Context, selector, fieldSelector string) ([]corev1.Pod, error) {
	r.note(ctx, "listPods")
	return r.fakeK8sOps.listPods(ctx, selector, fieldSelector)
}

func (r *deadlineRecorder) execInPod(ctx context.Context, pod, container string, cmd []string, stdin io.Reader) (string, error) {
	r.note(ctx, "execInPod")
	return r.fakeK8sOps.execInPod(ctx, pod, container, cmd, stdin)
}

// TestCtxFreeVerbsRunUnderDeadline: every Kubernetes call a ctx-free verb
// makes carries a deadline no later than the verb's production bound, so an
// API server or exec stream that never answers cannot hold the verb forever.
func TestCtxFreeVerbsRunUnderDeadline(t *testing.T) {
	src := filepath.Join(t.TempDir(), "payload.txt")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	const name = "deadline-session"
	cases := []struct {
		verb  string
		bound time.Duration
		call  func(p *Provider)
	}{
		{"Stop", wantRequestBound, func(p *Provider) { _ = p.Stop(name) }},
		{"Interrupt", wantRequestBound, func(p *Provider) { _ = p.Interrupt(name) }},
		{"IsRunning", wantRequestBound, func(p *Provider) { _ = p.IsRunning(name) }},
		{"IsAttached", wantRequestBound, func(p *Provider) { _ = p.IsAttached(name) }},
		{"ProcessAlive", wantRequestBound, func(p *Provider) { _ = p.ProcessAlive(name, []string{"claude"}) }},
		{"Nudge", wantRequestBound, func(p *Provider) {
			_ = p.Nudge(name, []runtime.ContentBlock{{Type: "text", Text: "hi"}})
		}},
		{"SendKeys", wantRequestBound, func(p *Provider) { _ = p.SendKeys(name, "Enter") }},
		{"SetMeta", wantRequestBound, func(p *Provider) { _ = p.SetMeta(name, "k", "v") }},
		{"GetMeta", wantRequestBound, func(p *Provider) { _, _ = p.GetMeta(name, "k") }},
		{"RemoveMeta", wantRequestBound, func(p *Provider) { _ = p.RemoveMeta(name, "k") }},
		{"Peek", wantRequestBound, func(p *Provider) { _, _ = p.Peek(name, 10) }},
		{"ListRunning", wantRequestBound, func(p *Provider) { _, _ = p.ListRunning("") }},
		{"GetLastActivity", wantRequestBound, func(p *Provider) { _, _ = p.GetLastActivity(name) }},
		{"ClearScrollback", wantRequestBound, func(p *Provider) { _ = p.ClearScrollback(name) }},
		{"CopyTo", wantCopyBound, func(p *Provider) { _ = p.CopyTo(name, src, "x/payload.txt") }},
	}
	for _, tc := range cases {
		t.Run(tc.verb, func(t *testing.T) {
			rec := &deadlineRecorder{fakeK8sOps: newFakeK8sOps()}
			addRunningPod(rec.fakeK8sOps, SanitizeName(name), SanitizeLabel(name))
			p := newProviderWithOps(rec)

			latest := time.Now().Add(tc.bound)
			tc.call(p)

			calls := rec.recorded()
			if len(calls) == 0 {
				t.Fatalf("%s made no Kubernetes call: the test is vacuous", tc.verb)
			}
			for _, c := range calls {
				if !c.hasDeadline {
					t.Fatalf("%s: %s ran with no deadline", tc.verb, c.method)
				}
				if c.deadline.After(latest.Add(time.Second)) {
					t.Fatalf("%s: %s deadline %v is past the %v bound (latest %v)", tc.verb, c.method, c.deadline, tc.bound, latest)
				}
			}
		})
	}
}
