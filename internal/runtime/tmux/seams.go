package tmux

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file makes the local tmux provider satisfy the de-conflated typed seams
// (runtime.Runtime / Place / Transport / Attachment / MetaStore) ADDITIVELY: the
// legacy [Provider] and its call sites are untouched; these wrappers expose the
// same logic through the new contract so the cut-over (cutover.go) can route
// through them.
//
// tmux is the local reference provider and drives tmux NATIVELY (not via the
// carrier-over-exec used by the remote providers): its Attachment verbs delegate
// to the provider's own tmux-driving Peek/Nudge/SendKeys/Interrupt/
// ClearScrollback. It exposes no in-box exec op, so Place.Exec is unsupported.
// The Transport is "tmux"; it can observe attachment locally (ReportAttachment).
//
// As with the other providers, Start welds provision+launch, so Transport.Launch
// and Attachment.Close are no-ops and teardown lives in Place.Teardown→Stop.
// tmux's large optional-interface surface (InteractionProvider, IdleWait,
// ProcessTableScanner, ServerLifecycle, …) and its REAL RunLive are kept on the
// raw provider by cutover.go (embed-raw), not modeled here.

// Seams returns the tmux provider decomposed into its WHERE (Runtime) and HOW
// (Transport) halves; the same *Provider backs both.
func (p *Provider) Seams() (runtime.Runtime, runtime.Transport) {
	return &tmuxRuntime{p: p}, &tmuxTransport{p: p}
}

// --- WHERE: Runtime + MetaStore ---

type tmuxRuntime struct{ p *Provider }

var (
	_ runtime.Runtime   = (*tmuxRuntime)(nil)
	_ runtime.MetaStore = (*tmuxRuntime)(nil)
)

// Provision starts the local tmux session for name (←Start).
func (r *tmuxRuntime) Provision(ctx context.Context, name string, req runtime.ProvisionRequest) (runtime.Place, error) {
	if err := r.p.Start(ctx, name, req.Config); err != nil {
		return nil, err
	}
	return &tmuxPlace{p: r.p, name: name}, nil
}

// Open re-resolves a running session by name without creating it (←IsRunning).
func (r *tmuxRuntime) Open(_ context.Context, name string) (runtime.Place, bool, error) {
	if !r.p.IsRunning(name) {
		return nil, false, nil
	}
	return &tmuxPlace{p: r.p, name: name}, true, nil
}

// Teardown kills the tmux session for name UNCONDITIONALLY (←Stop where-half).
// Unlike Open it does not gate on liveness, so a dead/corpse (pane_dead) session
// is still killed instead of surviving while the reaper frees the slot
// (SEAM-3 / the #2437 ghost-accumulation leak).
func (r *tmuxRuntime) Teardown(_ context.Context, name string) error {
	return r.p.Stop(name)
}

// List returns running session names with the prefix (←ListRunning).
func (r *tmuxRuntime) List(_ context.Context, prefix string) ([]string, error) {
	return r.p.ListRunning(prefix)
}

// Capabilities maps the provider capabilities to the box/Place half (tmux can
// report activity).
func (r *tmuxRuntime) Capabilities() runtime.PlaceCapabilities {
	return runtime.PlaceCapabilities{ReportActivity: r.p.Capabilities().CanReportActivity}
}

// SetMeta/GetMeta/RemoveMeta delegate to the tmux session-environment meta.
func (r *tmuxRuntime) SetMeta(name, key, value string) error {
	return r.p.SetMeta(name, key, value)
}

func (r *tmuxRuntime) GetMeta(name, key string) (string, error) {
	return r.p.GetMeta(name, key)
}

func (r *tmuxRuntime) RemoveMeta(name, key string) error {
	return r.p.RemoveMeta(name, key)
}

// --- WHERE: Place ---

type tmuxPlace struct {
	p    *Provider
	name string
}

var _ runtime.Place = (*tmuxPlace)(nil)

// Exec is unsupported: the local tmux provider drives tmux natively and exposes
// no in-box exec connection.
func (pl *tmuxPlace) Exec(context.Context, runtime.ExecRequest) (runtime.ExecResult, error) {
	return runtime.ExecResult{}, runtime.ErrExecUnsupported
}

// Stage copies entries into the session workdir via CopyTo (←CopyTo).
func (pl *tmuxPlace) Stage(_ context.Context, files []runtime.CopyEntry) error {
	for _, f := range files {
		if err := pl.p.CopyTo(pl.name, f.Src, f.RelDst); err != nil {
			return err
		}
	}
	return nil
}

func (pl *tmuxPlace) IsRunning(_ context.Context) (bool, error) {
	return pl.p.IsRunning(pl.name), nil
}

// Teardown is Stop's where-half: destroy the session + its process tree (←Stop).
func (pl *tmuxPlace) Teardown(_ context.Context) error {
	return pl.p.Stop(pl.name)
}

// --- HOW: native tmux Transport ---

type tmuxTransport struct{ p *Provider }

var _ runtime.Transport = (*tmuxTransport)(nil)

// Launch relaunches the agent inside the (already-provisioned) Place and returns
// the live Attachment. In the in-repo pragmatic un-weld (B1) box creation
// (Provision←Start) is welded and already launches the agent on the normal Start
// path, so this is the SEPARATE relaunch-into-a-warm-box capability the reconciler
// uses to apply a launch-only config change without a full reprovision — it is NOT
// a step of a normal Start (see seamProvider.Start). A pure provision/launch split
// (launch the agent into a never-launched box) lands at the RPP wire (B3).
func (t *tmuxTransport) Launch(ctx context.Context, place runtime.Place, spec runtime.LaunchSpec) (runtime.Attachment, error) {
	name := placeName(place)
	if err := t.p.Relaunch(ctx, name, spec.Config); err != nil {
		return nil, err
	}
	return &tmuxAttachment{p: t.p, name: name}, nil
}

// Open returns the Attachment for an already-running session (reconnect).
func (t *tmuxTransport) Open(ctx context.Context, place runtime.Place, name string) (runtime.Attachment, bool, error) {
	alive, err := place.IsRunning(ctx)
	if err != nil || !alive {
		return nil, false, err
	}
	return &tmuxAttachment{p: t.p, name: name}, true, nil
}

// Attach connects the local terminal to the tmux session (←Attach).
func (t *tmuxTransport) Attach(_ context.Context, _ runtime.Place, name string) error {
	return t.p.Attach(name)
}

func (t *tmuxTransport) Name() string { return "tmux" }

func (t *tmuxTransport) Capabilities() runtime.TransportCapabilities {
	return runtime.TransportCapabilities{ReportAttachment: t.p.Capabilities().CanReportAttachment}
}

// placeName extracts the session name from a Place. Only *tmuxPlace is ever
// passed here (tmuxRuntime produces no other Place type); the assertion is
// defensive.
func placeName(place runtime.Place) string {
	if tp, ok := place.(*tmuxPlace); ok {
		return tp.name
	}
	return ""
}

// --- HOW: Attachment (native tmux driving) ---

type tmuxAttachment struct {
	p    *Provider
	name string
}

var _ runtime.Attachment = (*tmuxAttachment)(nil)

// The five driving verbs delegate to the provider's native tmux driving.
func (a *tmuxAttachment) Peek(_ context.Context, lines int) (string, error) {
	return a.p.Peek(a.name, lines)
}

func (a *tmuxAttachment) Nudge(_ context.Context, content []runtime.ContentBlock) error {
	return a.p.Nudge(a.name, content)
}

func (a *tmuxAttachment) SendKeys(_ context.Context, keys ...string) error {
	return a.p.SendKeys(a.name, keys...)
}

func (a *tmuxAttachment) Interrupt(_ context.Context) error {
	return a.p.Interrupt(a.name)
}

func (a *tmuxAttachment) ClearScrollback(_ context.Context) error {
	return a.p.ClearScrollback(a.name)
}

// Observe folds the three liveness reads — tmux observes all three locally
// (ProcessAlive via the process tree, attachment + activity via tmux).
func (a *tmuxAttachment) Observe(_ context.Context, processNames []string) (runtime.LiveObservation, error) {
	lastActivity, _ := a.p.GetLastActivity(a.name)
	return runtime.LiveObservation{
		ProcessAlive: a.p.ProcessAlive(a.name, processNames),
		Attached:     a.p.IsAttached(a.name),
		LastActivity: lastActivity,
	}, nil
}

// Close is a no-op: the session is torn down in Place.Teardown→Stop, not here.
func (a *tmuxAttachment) Close(_ context.Context) error { return nil }
