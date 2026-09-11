package t3bridge

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file makes the t3bridge provider satisfy the de-conflated typed seams
// (runtime.Runtime / Place / Transport / Attachment / MetaStore) ADDITIVELY: the
// legacy [Provider] and its call sites are untouched; these wrappers expose the
// same logic through the new contract so the eventual cut-over (the Resolver
// tail) can route through them. Each method cites the §11 migration map.
//
// t3bridge diverges from the carrier providers: it is a WebSocket, turn-based
// bridge to the T3 API — there is NO in-box exec op and NO tmux. So:
//   - Place.Exec returns runtime.ErrExecUnsupported (the connection primitive
//     does not exist here; t3bridge speaks turns, not commands);
//   - the Transport is BESPOKE ("t3"), driving the bridge's own WebSocket rather
//     than a carrier over Place.Exec — Nudge starts a user turn, Interrupt
//     interrupts the turn, Peek summarizes recent thread messages, while
//     SendKeys/ClearScrollback are no-ops (T3 is headless / turn-based);
//   - Attach returns a "view it in the T3 UI" pointer, never a local terminal;
//   - meta is file-backed (with drain transitions reflected onto the T3 thread).
//
// As with the other providers, Start welds provision+launch (it creates the
// thread/session and makes it turn-able), so Transport.Launch and
// Attachment.Close are no-ops and teardown lives in Place.Teardown→Stop.

// Seams returns the t3bridge provider decomposed into its WHERE (Runtime) and
// HOW (Transport) halves; the same *Provider backs both.
func (p *Provider) Seams() (runtime.Runtime, runtime.Transport) {
	return &t3Runtime{p: p}, &t3Transport{p: p}
}

// --- WHERE: Runtime + MetaStore ---

type t3Runtime struct{ p *Provider }

var (
	_ runtime.Runtime   = (*t3Runtime)(nil)
	_ runtime.MetaStore = (*t3Runtime)(nil)
)

// Provision creates the T3 thread/session for name (←Start). The thread is
// turn-able once created, so the Transport.Launch over the returned Place is a
// no-op.
func (r *t3Runtime) Provision(ctx context.Context, name string, req runtime.ProvisionRequest) (runtime.Place, error) {
	if err := r.p.Start(ctx, name, req.Config); err != nil {
		return nil, err
	}
	return &t3Place{p: r.p, name: name}, nil
}

// Open re-resolves a running session by name without creating it (←IsRunning).
func (r *t3Runtime) Open(_ context.Context, name string) (runtime.Place, bool, error) {
	if !r.p.IsRunning(name) {
		return nil, false, nil
	}
	return &t3Place{p: r.p, name: name}, true, nil
}

// Teardown tears down the thread/session for name UNCONDITIONALLY (←Stop
// where-half). Unlike Open it does not gate on liveness, so a stopped/idle/error
// thread is still cleaned up — stopping the event watcher goroutine, clearing
// bridge meta, and dispatching session-stop — instead of leaking it (SEAM-2).
func (r *t3Runtime) Teardown(_ context.Context, name string) error {
	return r.p.Stop(name)
}

// List returns running session names with the prefix (←ListRunning).
func (r *t3Runtime) List(_ context.Context, prefix string) ([]string, error) {
	return r.p.ListRunning(prefix)
}

// Capabilities maps the provider capabilities to the box/Place half (t3bridge
// reports activity).
func (r *t3Runtime) Capabilities() runtime.PlaceCapabilities {
	return runtime.PlaceCapabilities{ReportActivity: r.p.Capabilities().CanReportActivity}
}

// SetMeta/GetMeta/RemoveMeta delegate to the provider's file-backed meta (which
// also reflects drain transitions onto the T3 thread).
func (r *t3Runtime) SetMeta(name, key, value string) error {
	return r.p.SetMeta(name, key, value)
}

func (r *t3Runtime) GetMeta(name, key string) (string, error) {
	return r.p.GetMeta(name, key)
}

func (r *t3Runtime) RemoveMeta(name, key string) error {
	return r.p.RemoveMeta(name, key)
}

// --- WHERE: Place ---

type t3Place struct {
	p    *Provider
	name string
}

var _ runtime.Place = (*t3Place)(nil)

// Exec is unsupported: t3bridge speaks turns over a WebSocket, not in-box
// commands, so there is no exec connection primitive.
func (pl *t3Place) Exec(context.Context, runtime.ExecRequest) (runtime.ExecResult, error) {
	return runtime.ExecResult{}, runtime.ErrExecUnsupported
}

// Stage copies entries via CopyTo (←CopyTo). t3bridge's CopyTo returns nil on
// all its internal errors, so Stage is effectively a no-op today; a future
// non-nil CopyTo would abort the batch at that entry.
func (pl *t3Place) Stage(_ context.Context, files []runtime.CopyEntry) error {
	for _, f := range files {
		if err := pl.p.CopyTo(pl.name, f.Src, f.RelDst); err != nil {
			return err
		}
	}
	return nil
}

func (pl *t3Place) IsRunning(_ context.Context) (bool, error) {
	return pl.p.IsRunning(pl.name), nil
}

// Teardown is Stop's where-half: stop the T3 session (←Stop).
func (pl *t3Place) Teardown(_ context.Context) error {
	return pl.p.Stop(pl.name)
}

// --- HOW: bespoke "t3" turn Transport ---

type t3Transport struct{ p *Provider }

var _ runtime.Transport = (*t3Transport)(nil)

// Launch is a no-op: Start already created the turn-able thread; this returns the
// live Attachment over the Place (←Start how-half).
func (t *t3Transport) Launch(_ context.Context, place runtime.Place, _ runtime.LaunchSpec) (runtime.Attachment, error) {
	return &t3Attachment{p: t.p, name: placeName(place)}, nil
}

// Open returns the Attachment for an already-running session (reconnect).
func (t *t3Transport) Open(ctx context.Context, place runtime.Place, name string) (runtime.Attachment, bool, error) {
	alive, err := place.IsRunning(ctx)
	if err != nil || !alive {
		return nil, false, err
	}
	return &t3Attachment{p: t.p, name: name}, true, nil
}

// Attach delegates to the provider, which returns a pointer to the T3 UI rather
// than attaching a local terminal (←Attach).
func (t *t3Transport) Attach(_ context.Context, _ runtime.Place, name string) error {
	return t.p.Attach(name)
}

func (t *t3Transport) Name() string { return "t3" }

func (t *t3Transport) Capabilities() runtime.TransportCapabilities {
	return runtime.TransportCapabilities{ReportAttachment: t.p.Capabilities().CanReportAttachment}
}

// placeName extracts the session name from a Place. Only *t3Place is ever passed
// here (t3Runtime produces no other Place type); the assertion is defensive.
func placeName(place runtime.Place) string {
	if tp, ok := place.(*t3Place); ok {
		return tp.name
	}
	return ""
}

// --- HOW: Attachment (turn driving over the bridge) ---

type t3Attachment struct {
	p    *Provider
	name string
}

var _ runtime.Attachment = (*t3Attachment)(nil)

// Peek summarizes recent thread messages (←Peek).
func (a *t3Attachment) Peek(_ context.Context, lines int) (string, error) {
	return a.p.Peek(a.name, lines)
}

// Nudge delivers content as a new user turn (←Nudge).
func (a *t3Attachment) Nudge(_ context.Context, content []runtime.ContentBlock) error {
	return a.p.Nudge(a.name, content)
}

// SendKeys is a no-op: T3 sessions do not accept raw key input (←SendKeys).
func (a *t3Attachment) SendKeys(_ context.Context, keys ...string) error {
	return a.p.SendKeys(a.name, keys...)
}

// Interrupt interrupts the active turn (←Interrupt).
func (a *t3Attachment) Interrupt(_ context.Context) error {
	return a.p.Interrupt(a.name)
}

// ClearScrollback is a no-op: T3 keeps no local scrollback (←ClearScrollback).
func (a *t3Attachment) ClearScrollback(_ context.Context) error {
	return a.p.ClearScrollback(a.name)
}

// Observe folds the liveness reads. t3bridge's ProcessAlive checks the T3 thread
// status and ignores process names; IsAttached is always false (headless);
// LastActivity is the thread's updated-at (best-effort zero).
func (a *t3Attachment) Observe(_ context.Context, processNames []string) (runtime.LiveObservation, error) {
	lastActivity, _ := a.p.GetLastActivity(a.name)
	return runtime.LiveObservation{
		ProcessAlive: a.p.ProcessAlive(a.name, processNames),
		Attached:     a.p.IsAttached(a.name),
		LastActivity: lastActivity,
	}, nil
}

// Close is a no-op: the T3 session is torn down in Place.Teardown→Stop, not here.
func (a *t3Attachment) Close(_ context.Context) error { return nil }
