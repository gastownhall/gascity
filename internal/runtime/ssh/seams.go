package ssh

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file makes the ssh provider satisfy the de-conflated typed seams
// (runtime.Runtime / Place / Transport / Attachment / MetaStore) ADDITIVELY: the
// legacy [Provider] and its call sites are untouched; these wrappers expose the
// same logic through the new contract so the eventual cut-over (the Resolver
// tail) can route through them. Each method cites the §11 migration map.
//
// ssh mirrors the exec/k8s decomposition; the ssh-specific behaviors live INSIDE
// the provider's methods, which the seam simply delegates to:
//   - the box pre-exists (Provision does NOT create it — Start launches a tmux
//     session on the remote host and validates the name as a safe tmux target,
//     so Provision may return runtime.ErrInvalidSessionName / ErrSessionExists);
//   - the carrier's in-box tmux target is the SESSION NAME (one host, many named
//     sessions), not "main" — but that is internal to the provider's driving
//     methods, so the Attachment delegation is identical;
//   - CopyTo is not yet supported (a best-effort no-op), so Stage is a no-op;
//   - meta lives in the tmux session environment (box ground-truth).
//
// As with exec/k8s, Start welds provision+launch, so Transport.Launch and
// Attachment.Close are no-ops and teardown lives in Place.Teardown→Stop.

// Seams returns the ssh provider decomposed into its WHERE (Runtime) and HOW
// (Transport) halves; the same *Provider backs both.
func (p *Provider) Seams() (runtime.Runtime, runtime.Transport) {
	return &sshRuntime{p: p}, &sshTransport{p: p}
}

// --- WHERE: Runtime + MetaStore ---

type sshRuntime struct{ p *Provider }

var (
	_ runtime.Runtime   = (*sshRuntime)(nil)
	_ runtime.MetaStore = (*sshRuntime)(nil)
)

// Provision launches the agent in a new remote tmux session named name (←Start);
// the box pre-exists, so this only starts+drives the session. May return
// runtime.ErrInvalidSessionName (unsafe tmux name) or ErrSessionExists.
func (r *sshRuntime) Provision(ctx context.Context, name string, req runtime.ProvisionRequest) (runtime.Place, error) {
	if err := r.p.Start(ctx, name, req.Config); err != nil {
		return nil, err
	}
	return &sshPlace{p: r.p, name: name}, nil
}

// Open re-resolves a running session by name without creating it (←IsRunning).
func (r *sshRuntime) Open(_ context.Context, name string) (runtime.Place, bool, error) {
	if !r.p.IsRunning(name) {
		return nil, false, nil
	}
	return &sshPlace{p: r.p, name: name}, true, nil
}

// Teardown destroys the box for name UNCONDITIONALLY (←Stop where-half). Unlike
// Open it does not gate on liveness, so a non-running box is still torn down
// instead of leaked (SEAM-1/2/3).
func (r *sshRuntime) Teardown(_ context.Context, name string) error {
	return r.p.Stop(name)
}

// List returns running session names with the prefix (←ListRunning).
func (r *sshRuntime) List(_ context.Context, prefix string) ([]string, error) {
	return r.p.ListRunning(prefix)
}

// Capabilities maps the provider capabilities to the box/Place half (ssh reports
// activity via tmux #{session_activity}).
func (r *sshRuntime) Capabilities() runtime.PlaceCapabilities {
	return runtime.PlaceCapabilities{ReportActivity: r.p.Capabilities().CanReportActivity}
}

// SetMeta/GetMeta/RemoveMeta delegate to the tmux-session-environment meta, which
// is box ground-truth (←Provider.{SetMeta,GetMeta,RemoveMeta}).
func (r *sshRuntime) SetMeta(name, key, value string) error {
	return r.p.SetMeta(name, key, value)
}

func (r *sshRuntime) GetMeta(name, key string) (string, error) {
	return r.p.GetMeta(name, key)
}

func (r *sshRuntime) RemoveMeta(name, key string) error {
	return r.p.RemoveMeta(name, key)
}

// --- WHERE: Place ---

type sshPlace struct {
	p    *Provider
	name string
}

var _ runtime.Place = (*sshPlace)(nil)

// Exec runs argv on the box over the ssh connection (←ExecProvider.Exec). A
// non-zero exit is the command's own result (Code set, nil error); a transport
// failure yields an error. req.Stdin is ignored: the v0 exec op reserves the
// connection's stdin for the command itself.
func (pl *sshPlace) Exec(ctx context.Context, req runtime.ExecRequest) (runtime.ExecResult, error) {
	out, code, err := pl.p.Exec(ctx, pl.name, req.Argv)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	return runtime.ExecResult{Output: out, Code: code}, nil
}

// Stage copies entries via CopyTo (←CopyTo). The v0 ssh provider's CopyTo is a
// best-effort no-op (it returns nil), so Stage is effectively a no-op today; a
// future CopyTo failure would abort the batch at that entry.
func (pl *sshPlace) Stage(_ context.Context, files []runtime.CopyEntry) error {
	for _, f := range files {
		if err := pl.p.CopyTo(pl.name, f.Src, f.RelDst); err != nil {
			return err
		}
	}
	return nil
}

func (pl *sshPlace) IsRunning(_ context.Context) (bool, error) {
	return pl.p.IsRunning(pl.name), nil
}

// Teardown is Stop's where-half: kill the remote tmux session (←Stop).
func (pl *sshPlace) Teardown(_ context.Context) error {
	return pl.p.Stop(pl.name)
}

// --- HOW: tmux Transport (carrier over the ssh connection) ---

type sshTransport struct{ p *Provider }

var _ runtime.Transport = (*sshTransport)(nil)

// Launch relaunches the agent inside the (already-provisioned) remote tmux
// session and returns the live Attachment. In the pragmatic un-weld, Provision
// (←Start) is welded and already launches the agent on the normal Start path, so
// this is the SEPARATE relaunch-into-a-warm-box capability the reconciler uses to
// apply a launch-only config change without re-provisioning — it is NOT a step of
// a normal Start (see seamProvider.Start). The box (remote host) pre-exists, so a
// missing session is a runtime.ErrSessionNotFound (B3a, mirroring tmux B1).
func (t *sshTransport) Launch(ctx context.Context, place runtime.Place, spec runtime.LaunchSpec) (runtime.Attachment, error) {
	name := placeName(place)
	if err := t.p.Relaunch(ctx, name, spec.Config); err != nil {
		return nil, err
	}
	return &sshAttachment{p: t.p, name: name}, nil
}

// Open returns the Attachment for an already-running session (reconnect). Process
// names are unknown on reconnect, so Observe falls back to box liveness.
func (t *sshTransport) Open(ctx context.Context, place runtime.Place, name string) (runtime.Attachment, bool, error) {
	alive, err := place.IsRunning(ctx)
	if err != nil || !alive {
		return nil, false, err
	}
	return &sshAttachment{p: t.p, name: name}, true, nil
}

// Attach connects the local terminal to the remote tmux session over ssh -t
// (←Attach).
func (t *sshTransport) Attach(_ context.Context, _ runtime.Place, name string) error {
	return t.p.Attach(name)
}

func (t *sshTransport) Name() string { return "tmux" }

func (t *sshTransport) Capabilities() runtime.TransportCapabilities {
	return runtime.TransportCapabilities{ReportAttachment: t.p.Capabilities().CanReportAttachment}
}

// placeName extracts the box/session name from a Place. Only *sshPlace is ever
// passed here (sshRuntime produces no other Place type); the assertion is
// defensive — an empty name reaching the carrier is undefined.
func placeName(place runtime.Place) string {
	if sp, ok := place.(*sshPlace); ok {
		return sp.name
	}
	return ""
}

// --- HOW: Attachment (the carrier verbs, reused from the provider) ---

type sshAttachment struct {
	p    *Provider
	name string
}

var _ runtime.Attachment = (*sshAttachment)(nil)

// The five driving verbs reuse the provider's public methods, which drive the
// remote tmux session (target = the session name) via the carrier over the ssh
// connection.
func (a *sshAttachment) Peek(_ context.Context, lines int) (string, error) {
	return a.p.Peek(a.name, lines)
}

// Nudge delivers content to the remote tmux session.
func (a *sshAttachment) Nudge(_ context.Context, content []runtime.ContentBlock) error {
	return a.p.Nudge(a.name, content)
}

func (a *sshAttachment) SendKeys(_ context.Context, keys ...string) error {
	return a.p.SendKeys(a.name, keys...)
}

func (a *sshAttachment) Interrupt(_ context.Context) error {
	return a.p.Interrupt(a.name)
}

func (a *sshAttachment) ClearScrollback(_ context.Context) error {
	return a.p.ClearScrollback(a.name)
}

// Observe folds the three liveness reads. ProcessAlive uses the per-call process
// names (empty → box-liveness proxy per the ProcessAlive contract); LastActivity
// is best-effort (zero when unsupported or on error).
func (a *sshAttachment) Observe(_ context.Context, processNames []string) (runtime.LiveObservation, error) {
	lastActivity, _ := a.p.GetLastActivity(a.name)
	return runtime.LiveObservation{
		ProcessAlive: a.p.ProcessAlive(a.name, processNames),
		Attached:     a.p.IsAttached(a.name),
		LastActivity: lastActivity,
	}, nil
}

// Close is a no-op: the remote tmux session is killed in Place.Teardown→Stop,
// not here.
func (a *sshAttachment) Close(_ context.Context) error { return nil }
