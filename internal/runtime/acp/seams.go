package acp

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file makes the acp provider satisfy the de-conflated typed seams
// (runtime.Runtime / Place / Transport / Attachment / MetaStore) ADDITIVELY: the
// legacy [Provider] and its call sites are untouched; these wrappers expose the
// same logic through the new contract so the cut-over (cutover.go) can route
// through them.
//
// acp resembles t3bridge: it speaks a JSON-RPC agent protocol over its own
// connection (spawned agent + control socket), with no in-box exec op and no
// tmux. So Place.Exec returns runtime.ErrExecUnsupported and the Transport is
// bespoke ("acp"): Nudge delivers an ACP prompt, Interrupt cancels, Peek reads
// the buffered output; SendKeys/ClearScrollback are no-ops (no terminal). Attach
// is unsupported. Meta is file-backed (sidecar files, like subprocess).
//
// InteractionProvider (Pending/Respond) and TransportCapabilityProvider
// (SupportsTransport) are optional extensions OUTSIDE the core seams; cutover.go
// passes them through to the raw provider. Like the other providers, Start welds
// provision+launch, so Transport.Launch and Attachment.Close are no-ops and
// teardown lives in Place.Teardown→Stop.

// Seams returns the acp provider decomposed into its WHERE (Runtime) and HOW
// (Transport) halves; the same *Provider backs both.
func (p *Provider) Seams() (runtime.Runtime, runtime.Transport) {
	return &acpRuntime{p: p}, &acpTransport{p: p}
}

// --- WHERE: Runtime + MetaStore ---

type acpRuntime struct{ p *Provider }

var (
	_ runtime.Runtime   = (*acpRuntime)(nil)
	_ runtime.MetaStore = (*acpRuntime)(nil)
)

// Provision spawns the ACP agent and completes the handshake for name (←Start);
// the agent is turn-able once up, so Transport.Launch over the Place is a no-op.
func (r *acpRuntime) Provision(ctx context.Context, name string, req runtime.ProvisionRequest) (runtime.Place, error) {
	if err := r.p.Start(ctx, name, req.Config); err != nil {
		return nil, err
	}
	return &acpPlace{p: r.p, name: name}, nil
}

// Open re-resolves a running session by name without creating it (←IsRunning).
func (r *acpRuntime) Open(_ context.Context, name string) (runtime.Place, bool, error) {
	if !r.p.IsRunning(name) {
		return nil, false, nil
	}
	return &acpPlace{p: r.p, name: name}, true, nil
}

// List returns running session names with the prefix (←ListRunning).
func (r *acpRuntime) List(_ context.Context, prefix string) ([]string, error) {
	return r.p.ListRunning(prefix)
}

// Capabilities: acp declares no box-side capabilities.
func (r *acpRuntime) Capabilities() runtime.PlaceCapabilities {
	return runtime.PlaceCapabilities{ReportActivity: r.p.Capabilities().CanReportActivity}
}

// SetMeta/GetMeta/RemoveMeta delegate to the provider's sidecar-file meta.
func (r *acpRuntime) SetMeta(name, key, value string) error {
	return r.p.SetMeta(name, key, value)
}

func (r *acpRuntime) GetMeta(name, key string) (string, error) {
	return r.p.GetMeta(name, key)
}

func (r *acpRuntime) RemoveMeta(name, key string) error {
	return r.p.RemoveMeta(name, key)
}

// --- WHERE: Place ---

type acpPlace struct {
	p    *Provider
	name string
}

var _ runtime.Place = (*acpPlace)(nil)

// Exec is unsupported: acp speaks a turn/prompt protocol, not in-box commands.
func (pl *acpPlace) Exec(context.Context, runtime.ExecRequest) (runtime.ExecResult, error) {
	return runtime.ExecResult{}, runtime.ErrExecUnsupported
}

// Stage copies entries into the session workdir via CopyTo (←CopyTo). Best-effort.
func (pl *acpPlace) Stage(_ context.Context, files []runtime.CopyEntry) error {
	for _, f := range files {
		if err := pl.p.CopyTo(pl.name, f.Src, f.RelDst); err != nil {
			return err
		}
	}
	return nil
}

func (pl *acpPlace) IsRunning(_ context.Context) (bool, error) {
	return pl.p.IsRunning(pl.name), nil
}

// Teardown is Stop's where-half: terminate the agent process (←Stop).
func (pl *acpPlace) Teardown(_ context.Context) error {
	return pl.p.Stop(pl.name)
}

// --- HOW: bespoke "acp" Transport ---

type acpTransport struct{ p *Provider }

var _ runtime.Transport = (*acpTransport)(nil)

// Launch is a no-op: Start already spawned the turn-able agent; this returns the
// live Attachment over the Place (←Start how-half).
func (t *acpTransport) Launch(_ context.Context, place runtime.Place, _ runtime.LaunchSpec) (runtime.Attachment, error) {
	return &acpAttachment{p: t.p, name: placeName(place)}, nil
}

// Open returns the Attachment for an already-running session (reconnect).
func (t *acpTransport) Open(ctx context.Context, place runtime.Place, name string) (runtime.Attachment, bool, error) {
	alive, err := place.IsRunning(ctx)
	if err != nil || !alive {
		return nil, false, err
	}
	return &acpAttachment{p: t.p, name: name}, true, nil
}

// Attach delegates to the provider, which does not support terminal attach (←Attach).
func (t *acpTransport) Attach(_ context.Context, _ runtime.Place, name string) error {
	return t.p.Attach(name)
}

func (t *acpTransport) Name() string { return "acp" }

func (t *acpTransport) Capabilities() runtime.TransportCapabilities {
	return runtime.TransportCapabilities{ReportAttachment: t.p.Capabilities().CanReportAttachment}
}

// placeName extracts the session name from a Place. Only *acpPlace is ever passed
// here (acpRuntime produces no other Place type); the assertion is defensive.
func placeName(place runtime.Place) string {
	if ap, ok := place.(*acpPlace); ok {
		return ap.name
	}
	return ""
}

// --- HOW: Attachment (ACP prompt driving) ---

type acpAttachment struct {
	p    *Provider
	name string
}

var _ runtime.Attachment = (*acpAttachment)(nil)

// Peek reads the session's buffered output (←Peek).
func (a *acpAttachment) Peek(_ context.Context, lines int) (string, error) {
	return a.p.Peek(a.name, lines)
}

// Nudge delivers content as an ACP prompt (←Nudge).
func (a *acpAttachment) Nudge(_ context.Context, content []runtime.ContentBlock) error {
	return a.p.Nudge(a.name, content)
}

// SendKeys is a no-op: ACP sessions take no raw key input (←SendKeys).
func (a *acpAttachment) SendKeys(_ context.Context, keys ...string) error {
	return a.p.SendKeys(a.name, keys...)
}

// Interrupt cancels the in-flight turn (←Interrupt).
func (a *acpAttachment) Interrupt(_ context.Context) error {
	return a.p.Interrupt(a.name)
}

// ClearScrollback (←ClearScrollback).
func (a *acpAttachment) ClearScrollback(_ context.Context) error {
	return a.p.ClearScrollback(a.name)
}

// Observe folds the liveness reads; acp can't observe attach (headless).
func (a *acpAttachment) Observe(_ context.Context, processNames []string) (runtime.LiveObservation, error) {
	lastActivity, _ := a.p.GetLastActivity(a.name)
	return runtime.LiveObservation{
		ProcessAlive: a.p.ProcessAlive(a.name, processNames),
		Attached:     a.p.IsAttached(a.name),
		LastActivity: lastActivity,
	}, nil
}

// Close is a no-op: the agent is torn down in Place.Teardown→Stop, not here.
func (a *acpAttachment) Close(_ context.Context) error { return nil }
