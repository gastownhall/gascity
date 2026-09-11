package k8s

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file makes the k8s provider satisfy the de-conflated typed seams
// (runtime.Runtime / Place / Transport / Attachment / MetaStore) ADDITIVELY: the
// legacy [Provider] and its call sites are untouched; these wrappers expose the
// same logic through the new contract so the eventual cut-over (the Resolver
// tail) can route through them. Each method cites the §11 migration map.
//
// k8s mirrors the exec decomposition: the WHERE is the pod (Runtime provisions it
// via Start, which also launches the agent in tmux "main"), the connection is
// Place.Exec (execInPod), and the HOW is tmux (Transport) — the driving verbs
// drive the in-box tmux session via the carrier over execInPod. Unlike exec
// there is no wire-op fallback (k8s always implements the exec connection), and
// meta lives in the tmux session environment (box ground-truth).
//
// Like exec, the pod's start welds provision+launch, so Transport.Launch and
// Attachment.Close are no-ops and teardown lives in Place.Teardown→Stop.
// Extracting one generic tmux Transport that drives the carrier directly over
// Place.Exec is deferred to the cut-over; here we delegate to the provider's
// existing carrier-backed methods to stay behavior-preserving.

// Seams returns the k8s provider decomposed into its WHERE (Runtime) and HOW
// (Transport) halves; the same *Provider backs both.
func (p *Provider) Seams() (runtime.Runtime, runtime.Transport) {
	return &k8sRuntime{p: p}, &k8sTransport{p: p}
}

// --- WHERE: Runtime + MetaStore ---

type k8sRuntime struct{ p *Provider }

var (
	_ runtime.Runtime   = (*k8sRuntime)(nil)
	_ runtime.MetaStore = (*k8sRuntime)(nil)
)

// Provision creates the pod for name and launches the agent (←Start); for a
// k8s session this welds provision+launch, so Transport.Launch over the returned
// Place is a no-op.
func (r *k8sRuntime) Provision(ctx context.Context, name string, req runtime.ProvisionRequest) (runtime.Place, error) {
	if err := r.p.Start(ctx, name, req.Config); err != nil {
		return nil, err
	}
	return &k8sPlace{p: r.p, name: name}, nil
}

// Open re-resolves a running session by name without creating it (←IsRunning).
func (r *k8sRuntime) Open(_ context.Context, name string) (runtime.Place, bool, error) {
	if !r.p.IsRunning(name) {
		return nil, false, nil
	}
	return &k8sPlace{p: r.p, name: name}, true, nil
}

// Teardown destroys the pod for name UNCONDITIONALLY (←Stop where-half). Unlike
// Open it does not gate on liveness, so a Pending/Failed/CrashLoopBackOff or
// Running-but-tmux-dead pod is still deleted (by label) instead of leaking the
// pod + its PVC (SEAM-1).
func (r *k8sRuntime) Teardown(_ context.Context, name string) error {
	return r.p.Stop(name)
}

// List returns running session names with the prefix (←ListRunning).
func (r *k8sRuntime) List(_ context.Context, prefix string) ([]string, error) {
	return r.p.ListRunning(prefix)
}

// Capabilities maps the provider capabilities to the box/Place half (k8s reports
// activity).
func (r *k8sRuntime) Capabilities() runtime.PlaceCapabilities {
	return runtime.PlaceCapabilities{ReportActivity: r.p.Capabilities().CanReportActivity}
}

// SetMeta/GetMeta/RemoveMeta delegate to the tmux-session-environment meta, which
// is box ground-truth (←Provider.{SetMeta,GetMeta,RemoveMeta}).
func (r *k8sRuntime) SetMeta(name, key, value string) error {
	return r.p.SetMeta(name, key, value)
}

func (r *k8sRuntime) GetMeta(name, key string) (string, error) {
	return r.p.GetMeta(name, key)
}

func (r *k8sRuntime) RemoveMeta(name, key string) error {
	return r.p.RemoveMeta(name, key)
}

// --- WHERE: Place ---

type k8sPlace struct {
	p    *Provider
	name string
}

var _ runtime.Place = (*k8sPlace)(nil)

// Exec runs argv inside the pod's agent container via execInPod
// (←ExecProvider.Exec). A non-zero exit is the command's own result (Code set,
// nil error); only a transport failure (no running pod, stream error) yields an
// error. req.Stdin is ignored: the v0 exec op reserves the connection's stdin
// for the command itself.
func (pl *k8sPlace) Exec(ctx context.Context, req runtime.ExecRequest) (runtime.ExecResult, error) {
	out, code, err := pl.p.Exec(ctx, pl.name, req.Argv)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	return runtime.ExecResult{Output: out, Code: code}, nil
}

// Stage copies entries into the pod workspace via CopyTo/tar (←CopyTo). CopyTo is
// best-effort when the pod is absent; a real copy failure returns an error and
// aborts the batch at that entry.
func (pl *k8sPlace) Stage(_ context.Context, files []runtime.CopyEntry) error {
	for _, f := range files {
		if err := pl.p.CopyTo(pl.name, f.Src, f.RelDst); err != nil {
			return err
		}
	}
	return nil
}

func (pl *k8sPlace) IsRunning(_ context.Context) (bool, error) {
	return pl.p.IsRunning(pl.name), nil
}

// Teardown is Stop's where-half: delete the pod (←Stop).
func (pl *k8sPlace) Teardown(_ context.Context) error {
	return pl.p.Stop(pl.name)
}

// --- HOW: tmux Transport (carrier over execInPod) ---

type k8sTransport struct{ p *Provider }

var _ runtime.Transport = (*k8sTransport)(nil)

// Launch relaunches the agent inside the (already-provisioned) pod and returns
// the live Attachment. In the pragmatic un-weld, Provision (←Start) is welded and
// already launches the agent on the normal Start path, so this is the SEPARATE
// relaunch-into-a-warm-pod capability the reconciler uses to apply a launch-only
// config change without recreating the pod — it is NOT a step of a normal Start
// (see seamProvider.Start). A pod with no live tmux "main" session is a
// runtime.ErrSessionNotFound (B3a, mirroring tmux B1 / ssh).
func (t *k8sTransport) Launch(ctx context.Context, place runtime.Place, spec runtime.LaunchSpec) (runtime.Attachment, error) {
	name := placeName(place)
	if err := t.p.Relaunch(ctx, name, spec.Config); err != nil {
		return nil, err
	}
	return &k8sAttachment{p: t.p, name: name}, nil
}

// Open returns the Attachment for an already-running pod (reconnect). Process
// names are unknown on reconnect, so Observe falls back to box liveness.
func (t *k8sTransport) Open(ctx context.Context, place runtime.Place, name string) (runtime.Attachment, bool, error) {
	alive, err := place.IsRunning(ctx)
	if err != nil || !alive {
		return nil, false, err
	}
	return &k8sAttachment{p: t.p, name: name}, true, nil
}

// Attach connects the terminal to the session (←Attach).
func (t *k8sTransport) Attach(_ context.Context, _ runtime.Place, name string) error {
	return t.p.Attach(name)
}

func (t *k8sTransport) Name() string { return "tmux" }

func (t *k8sTransport) Capabilities() runtime.TransportCapabilities {
	return runtime.TransportCapabilities{ReportAttachment: t.p.Capabilities().CanReportAttachment}
}

// placeName extracts the box/session name from a Place. Only *k8sPlace is ever
// passed here (k8sRuntime produces no other Place type); the assertion is
// defensive — an empty name reaching the carrier is undefined.
func placeName(place runtime.Place) string {
	if kp, ok := place.(*k8sPlace); ok {
		return kp.name
	}
	return ""
}

// --- HOW: Attachment (the carrier verbs, reused from the provider) ---

type k8sAttachment struct {
	p    *Provider
	name string
}

var _ runtime.Attachment = (*k8sAttachment)(nil)

// The five driving verbs reuse the provider's public methods, which drive the
// in-box tmux session via the carrier over execInPod.
func (a *k8sAttachment) Peek(_ context.Context, lines int) (string, error) {
	return a.p.Peek(a.name, lines)
}

// Nudge delivers content as input to the in-box tmux session.
func (a *k8sAttachment) Nudge(_ context.Context, content []runtime.ContentBlock) error {
	return a.p.Nudge(a.name, content)
}

func (a *k8sAttachment) SendKeys(_ context.Context, keys ...string) error {
	return a.p.SendKeys(a.name, keys...)
}

func (a *k8sAttachment) Interrupt(_ context.Context) error {
	return a.p.Interrupt(a.name)
}

func (a *k8sAttachment) ClearScrollback(_ context.Context) error {
	return a.p.ClearScrollback(a.name)
}

// Observe folds the three liveness reads. ProcessAlive uses the per-call process
// names (empty → box-liveness proxy per the ProcessAlive contract); LastActivity
// is best-effort (zero when unsupported or on error).
func (a *k8sAttachment) Observe(_ context.Context, processNames []string) (runtime.LiveObservation, error) {
	lastActivity, _ := a.p.GetLastActivity(a.name)
	return runtime.LiveObservation{
		ProcessAlive: a.p.ProcessAlive(a.name, processNames),
		Attached:     a.p.IsAttached(a.name),
		LastActivity: lastActivity,
	}, nil
}

// Close is a no-op: the pod and its agent are torn down together in
// Place.Teardown→Stop, not here.
func (a *k8sAttachment) Close(_ context.Context) error { return nil }
