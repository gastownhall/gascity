package subprocess

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file makes the subprocess provider satisfy the de-conflated typed seams
// (runtime.Runtime / Place / Transport / Attachment / MetaStore) ADDITIVELY: the
// legacy [Provider] and its call sites are untouched; these wrappers expose the
// same logic through the new contract so the eventual cut-over (the Resolver
// tail) can route through them. Each method cites the §11 migration map.
//
// Subprocess is the DEGENERATE case for the split: the child process IS the
// agent (Provision and launch are welded) and there is no terminal to drive, so
// the Transport/Attachment are the "detached" no-op forms — exactly the no-ops
// the legacy Provider already returns (Nudge/Peek/SendKeys/ClearScrollback;
// Attach unsupported). This isolates the compat + MetaStore mechanics from
// carrier complexity before the load-bearing exec/ssh providers.

// Seams returns the subprocess provider decomposed into its WHERE (Runtime) and
// HOW (Transport) halves. The same underlying *Provider backs both, so state
// (tracked procs, sockets, sidecar meta) is shared with the legacy view.
func (p *Provider) Seams() (runtime.Runtime, runtime.Transport) {
	return &subprocessRuntime{p: p}, detachedTransport{}
}

// --- WHERE: Runtime + MetaStore ---

type subprocessRuntime struct{ p *Provider }

var (
	_ runtime.Runtime   = (*subprocessRuntime)(nil)
	_ runtime.MetaStore = (*subprocessRuntime)(nil)
)

// Provision spawns the child process for name (←Start). For subprocess this also
// "launches" the agent — the child IS the agent — so the Transport.Launch over
// the returned Place is a no-op.
func (r *subprocessRuntime) Provision(ctx context.Context, name string, req runtime.ProvisionRequest) (runtime.Place, error) {
	if err := r.p.Start(ctx, name, req.Config); err != nil {
		return nil, err
	}
	return &subprocessPlace{p: r.p, name: name}, nil
}

// Open re-resolves a running session by name without creating it. Subprocess is
// stateless-by-socket, so this is a liveness check (←IsRunning).
func (r *subprocessRuntime) Open(_ context.Context, name string) (runtime.Place, bool, error) {
	if !r.p.IsRunning(name) {
		return nil, false, nil
	}
	return &subprocessPlace{p: r.p, name: name}, true, nil
}

// Teardown destroys the box for name UNCONDITIONALLY (←Stop where-half). Unlike
// Open it does not gate on liveness, so a non-running box is still torn down
// instead of leaked (SEAM-1/2/3).
func (r *subprocessRuntime) Teardown(_ context.Context, name string) error {
	return r.p.Stop(name)
}

// List returns running session names with the prefix (←ListRunning).
func (r *subprocessRuntime) List(_ context.Context, prefix string) ([]string, error) {
	return r.p.ListRunning(prefix)
}

// Capabilities: subprocess has no stream/PTY and no activity tracking.
func (r *subprocessRuntime) Capabilities() runtime.PlaceCapabilities {
	return runtime.PlaceCapabilities{}
}

// SetMeta/GetMeta/RemoveMeta delegate to the provider's sidecar-file meta, which
// is box ground-truth (←Provider.{SetMeta,GetMeta,RemoveMeta}).
func (r *subprocessRuntime) SetMeta(name, key, value string) error {
	return r.p.SetMeta(name, key, value)
}

func (r *subprocessRuntime) GetMeta(name, key string) (string, error) {
	return r.p.GetMeta(name, key)
}

func (r *subprocessRuntime) RemoveMeta(name, key string) error {
	return r.p.RemoveMeta(name, key)
}

// --- WHERE: Place ---

type subprocessPlace struct {
	p    *Provider
	name string
}

var _ runtime.Place = (*subprocessPlace)(nil)

// Exec is unsupported: subprocess is not an ExecProvider (no in-box command op).
func (pl *subprocessPlace) Exec(context.Context, runtime.ExecRequest) (runtime.ExecResult, error) {
	return runtime.ExecResult{}, runtime.ErrExecUnsupported
}

// Stage copies entries into the session workdir via the legacy CopyTo (←CopyTo).
// CopyTo no-ops an unknown session / missing src; a real copy failure aborts the
// batch at that entry.
func (pl *subprocessPlace) Stage(_ context.Context, files []runtime.CopyEntry) error {
	for _, f := range files {
		if err := pl.p.CopyTo(pl.name, f.Src, f.RelDst); err != nil {
			return err
		}
	}
	return nil
}

func (pl *subprocessPlace) IsRunning(context.Context) (bool, error) {
	return pl.p.IsRunning(pl.name), nil
}

// Teardown is Stop's where-half: terminate the box (which, for subprocess, is the
// agent process too).
func (pl *subprocessPlace) Teardown(context.Context) error {
	return pl.p.Stop(pl.name)
}

// --- HOW: detached Transport + Attachment (null driving) ---

type detachedTransport struct{}

var _ runtime.Transport = detachedTransport{}

// Launch is a no-op: the agent is already running as the provisioned box; this
// just returns the live Attachment over the Place (←Start how-half).
func (detachedTransport) Launch(_ context.Context, place runtime.Place, _ runtime.LaunchSpec) (runtime.Attachment, error) {
	return attachmentFor(place), nil
}

// Open returns the Attachment for an already-running box.
func (detachedTransport) Open(ctx context.Context, place runtime.Place, _ string) (runtime.Attachment, bool, error) {
	alive, err := place.IsRunning(ctx)
	if err != nil || !alive {
		return nil, false, err
	}
	return attachmentFor(place), true, nil
}

// Attach is unsupported: subprocess has no terminal concept (←Attach).
func (detachedTransport) Attach(context.Context, runtime.Place, string) error {
	return fmt.Errorf("subprocess transport does not support attach")
}

func (detachedTransport) Name() string { return "detached" }

func (detachedTransport) Capabilities() runtime.TransportCapabilities {
	return runtime.TransportCapabilities{}
}

// attachmentFor builds the live Attachment for a Place. The detached transport
// only ever drives a *subprocessPlace (subprocess produces no other Place type),
// so the assertion is defensive: a foreign Place yields a nil-backed attachment
// whose Interrupt/Observe degrade to safe no-ops rather than panicking.
func attachmentFor(place runtime.Place) runtime.Attachment {
	sp, _ := place.(*subprocessPlace) // only *subprocessPlace is ever passed here
	return &detachedAttachment{place: sp}
}

type detachedAttachment struct{ place *subprocessPlace }

var _ runtime.Attachment = (*detachedAttachment)(nil)

// The five driving verbs are best-effort no-ops — subprocess has no terminal
// (matching the legacy Provider's no-op Peek/Nudge/SendKeys/ClearScrollback).
func (a *detachedAttachment) Peek(context.Context, int) (string, error) { return "", nil }

func (a *detachedAttachment) Nudge(context.Context, []runtime.ContentBlock) error {
	return nil
}

func (a *detachedAttachment) SendKeys(context.Context, ...string) error { return nil }

// Interrupt signals SIGINT to the process group (←Interrupt).
func (a *detachedAttachment) Interrupt(context.Context) error {
	if a.place == nil {
		return nil
	}
	return a.place.p.Interrupt(a.place.name)
}

func (a *detachedAttachment) ClearScrollback(context.Context) error { return nil }

// Observe reports process liveness via IsRunning; subprocess cannot detect
// attach or activity, nor inspect the process tree, so processNames is ignored.
func (a *detachedAttachment) Observe(ctx context.Context, _ []string) (runtime.LiveObservation, error) {
	if a.place == nil {
		return runtime.LiveObservation{}, nil
	}
	alive, err := a.place.IsRunning(ctx)
	if err != nil {
		return runtime.LiveObservation{}, err
	}
	return runtime.LiveObservation{ProcessAlive: alive}, nil
}

// Close is a no-op: for subprocess the agent and the box are one process, so
// shutdown happens in Place.Teardown, not here.
func (a *detachedAttachment) Close(context.Context) error { return nil }
