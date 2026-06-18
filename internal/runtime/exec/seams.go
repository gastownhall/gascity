package exec

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file makes the exec provider satisfy the de-conflated typed seams
// (runtime.Runtime / Place / Transport / Attachment / MetaStore) ADDITIVELY: the
// legacy [Provider] and its call sites are untouched; these wrappers expose the
// same logic through the new contract so the eventual cut-over (the Resolver
// tail) can route through them. Each method cites the §11 migration map.
//
// Exec is the first REAL decomposition (subprocess was degenerate): Place.Exec
// is the live RPP `exec` op, and the Attachment's driving verbs are real — they
// reuse the provider's existing public methods, which already drive an in-box
// tmux session via the carrier over the exec op and fall back to the dedicated
// wire ops when the pack does not declare proc.exec. So the WHERE is the pack
// (Runtime), the connection is Place.Exec, and the HOW is tmux (Transport).
//
// Like subprocess, the pack's `start` op welds provision+launch (it launches the
// agent in tmux session "main"), so Transport.Launch is a no-op and
// Attachment.Close is a no-op — teardown lives in Place.Teardown→Stop. Extracting
// a single GENERIC tmux Transport that drives the carrier directly over Place.Exec
// (capability-selected against the wire-op fallback) is deferred to the cut-over;
// here we delegate to the provider's existing carrier-then-fallback methods to
// stay behavior-preserving.

// Seams returns the exec provider decomposed into its WHERE (Runtime) and HOW
// (Transport) halves; the same *Provider backs both.
func (p *Provider) Seams() (runtime.Runtime, runtime.Transport) {
	return &execRuntime{p: p}, &execTransport{p: p}
}

// --- WHERE: Runtime + MetaStore ---

type execRuntime struct{ p *Provider }

var (
	_ runtime.Runtime   = (*execRuntime)(nil)
	_ runtime.MetaStore = (*execRuntime)(nil)
)

// Provision runs the pack's `start` op for name (←Start); for an exec-pack this
// also launches the agent in tmux "main", so Transport.Launch over the returned
// Place is a no-op.
func (r *execRuntime) Provision(ctx context.Context, name string, req runtime.ProvisionRequest) (runtime.Place, error) {
	if err := r.p.Start(ctx, name, req.Config); err != nil {
		return nil, err
	}
	return &execPlace{p: r.p, name: name}, nil
}

// Open re-resolves a running session by name without creating it. Exec-packs are
// stateless-by-name, so this is a liveness check (←IsRunning).
func (r *execRuntime) Open(_ context.Context, name string) (runtime.Place, bool, error) {
	if !r.p.IsRunning(name) {
		return nil, false, nil
	}
	return &execPlace{p: r.p, name: name}, true, nil
}

// List returns running session names with the prefix (←ListRunning).
func (r *execRuntime) List(_ context.Context, prefix string) ([]string, error) {
	return r.p.ListRunning(prefix)
}

// Capabilities maps the handshake-derived ProviderCapabilities to the box/Place
// half (exec reports activity when the pack declares it).
func (r *execRuntime) Capabilities() runtime.PlaceCapabilities {
	return runtime.PlaceCapabilities{ReportActivity: r.p.Capabilities().CanReportActivity}
}

// SetMeta/GetMeta/RemoveMeta delegate to the pack's set-meta/get-meta/remove-meta
// ops, which are box ground-truth (←Provider.{SetMeta,GetMeta,RemoveMeta}).
func (r *execRuntime) SetMeta(name, key, value string) error {
	return r.p.SetMeta(name, key, value)
}

func (r *execRuntime) GetMeta(name, key string) (string, error) {
	return r.p.GetMeta(name, key)
}

func (r *execRuntime) RemoveMeta(name, key string) error {
	return r.p.RemoveMeta(name, key)
}

// --- WHERE: Place ---

type execPlace struct {
	p    *Provider
	name string
}

var _ runtime.Place = (*execPlace)(nil)

// Exec runs argv inside the box via the live RPP `exec` op (←ExecProvider.Exec).
// A non-zero exit is the command's own result (Code set, nil error); a pack that
// does not implement exec yields runtime.ErrExecUnsupported. req.Stdin is ignored:
// the v0 exec op reserves the connection's stdin for the command itself.
func (pl *execPlace) Exec(ctx context.Context, req runtime.ExecRequest) (runtime.ExecResult, error) {
	out, code, err := pl.p.Exec(ctx, pl.name, req.Argv)
	if err != nil {
		return runtime.ExecResult{}, err
	}
	return runtime.ExecResult{Output: out, Code: code}, nil
}

// Stage copies entries into the session workdir via the pack's copy-to op
// (←CopyTo). An unknown copy-to op (exit 2) is a no-op, but a real copy failure
// returns an error and aborts the batch at that entry.
func (pl *execPlace) Stage(_ context.Context, files []runtime.CopyEntry) error {
	for _, f := range files {
		if err := pl.p.CopyTo(pl.name, f.Src, f.RelDst); err != nil {
			return err
		}
	}
	return nil
}

func (pl *execPlace) IsRunning(_ context.Context) (bool, error) {
	return pl.p.IsRunning(pl.name), nil
}

// Teardown is Stop's where-half: destroy the box (←Stop).
func (pl *execPlace) Teardown(_ context.Context) error {
	return pl.p.Stop(pl.name)
}

// --- HOW: tmux Transport (carrier-over-exec, with wire-op fallback) ---

type execTransport struct{ p *Provider }

var _ runtime.Transport = (*execTransport)(nil)

// Launch is a no-op: the pack's `start` op already launched the agent in the box;
// this returns the live Attachment over the Place (←Start how-half).
func (t *execTransport) Launch(_ context.Context, place runtime.Place, _ runtime.LaunchSpec) (runtime.Attachment, error) {
	return &execAttachment{p: t.p, name: placeName(place)}, nil
}

// Open returns the Attachment for an already-running box (reconnect). Process
// names are unknown on reconnect, so Observe falls back to box liveness.
func (t *execTransport) Open(ctx context.Context, place runtime.Place, name string) (runtime.Attachment, bool, error) {
	alive, err := place.IsRunning(ctx)
	if err != nil || !alive {
		return nil, false, err
	}
	return &execAttachment{p: t.p, name: name}, true, nil
}

// Attach connects the terminal to the session via the pack's attach op (←Attach).
func (t *execTransport) Attach(_ context.Context, _ runtime.Place, name string) error {
	return t.p.Attach(name)
}

func (t *execTransport) Name() string { return "tmux" }

func (t *execTransport) Capabilities() runtime.TransportCapabilities {
	return runtime.TransportCapabilities{ReportAttachment: t.p.Capabilities().CanReportAttachment}
}

// placeName extracts the box/session name from a Place. Only *execPlace is ever
// passed here (execRuntime produces no other Place type); the assertion is
// defensive.
func placeName(place runtime.Place) string {
	if ep, ok := place.(*execPlace); ok {
		return ep.name
	}
	return ""
}

// --- HOW: Attachment (the carrier verbs, reused from the provider) ---

type execAttachment struct {
	p    *Provider
	name string
}

var _ runtime.Attachment = (*execAttachment)(nil)

// The five driving verbs reuse the provider's existing public methods, which
// drive the in-box tmux session via the carrier over the exec op and fall back
// to the dedicated wire ops when the pack does not declare proc.exec.
func (a *execAttachment) Peek(_ context.Context, lines int) (string, error) {
	return a.p.Peek(a.name, lines)
}

// Nudge delivers content as input to the in-box tmux session.
func (a *execAttachment) Nudge(_ context.Context, content []runtime.ContentBlock) error {
	return a.p.Nudge(a.name, content)
}

func (a *execAttachment) SendKeys(_ context.Context, keys ...string) error {
	return a.p.SendKeys(a.name, keys...)
}

func (a *execAttachment) Interrupt(_ context.Context) error {
	return a.p.Interrupt(a.name)
}

func (a *execAttachment) ClearScrollback(_ context.Context) error {
	return a.p.ClearScrollback(a.name)
}

// Observe folds the three liveness reads. ProcessAlive uses the per-call process
// names (empty → box-liveness proxy per the ProcessAlive contract); LastActivity
// is best-effort (zero when unsupported or on error).
func (a *execAttachment) Observe(_ context.Context, processNames []string) (runtime.LiveObservation, error) {
	lastActivity, _ := a.p.GetLastActivity(a.name)
	return runtime.LiveObservation{
		ProcessAlive: a.p.ProcessAlive(a.name, processNames),
		Attached:     a.p.IsAttached(a.name),
		LastActivity: lastActivity,
	}, nil
}

// Close is a no-op: the agent and the box are torn down together in
// Place.Teardown→Stop, not here.
func (a *execAttachment) Close(_ context.Context) error { return nil }
