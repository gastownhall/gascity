package runtime

import (
	"context"
	"fmt"
	"time"
)

// seamProvider implements the legacy [Provider] interface on top of the
// de-conflated seams (Runtime + Transport, and MetaStore when the runtime carries
// meta). It is the INVERSE of the per-provider seams.go wrappers: where those
// expose a *Provider AS the seams, this exposes the seams AS a Provider — so call
// sites that still depend on [Provider] can be served entirely by the seams. It
// is the vehicle for the cut-over: flip a provider's construction to route
// through the seams without touching the ~105 call sites.
//
// It is stateless-by-name: each call re-resolves the box via Runtime.Open and the
// live attachment via Transport.Open, matching how the stateless providers
// (exec/ssh/k8s) already behave.
//
// KNOWN GAPS — the seam contract does not yet cover these:
//   - RunLive: no seam exists, so this is a no-op — correct only for providers
//     whose RunLive is already a no-op (subprocess/exec/k8s/ssh/t3bridge).
//   - The optional extensions (ProcessTableScanner, SleepCapability,
//     InteractionProvider, ExecProvider, …) live outside the seams; a provider
//     needing them composes them alongside this adapter (see each provider's
//     cutover.go).
//
// ProcessAlive's per-call processNames ARE threaded through Attachment.Observe.
// A provider whose Observe ignores them (subprocess, which can only report
// IsRunning) does not reproduce the legacy "empty names => true" result, but
// that path is unreachable in production (both ProcessAlive callers gate on
// non-empty names).
type seamProvider struct {
	rt   Runtime
	tp   Transport
	meta MetaStore // rt asserted to MetaStore; nil when the runtime carries no meta
}

var _ Provider = (*seamProvider)(nil)

// NewProviderFromSeams builds a legacy [Provider] backed by the given seams. If
// rt also implements [MetaStore], SetMeta/GetMeta/RemoveMeta route to it.
func NewProviderFromSeams(rt Runtime, tp Transport) Provider {
	meta, _ := rt.(MetaStore)
	return &seamProvider{rt: rt, tp: tp, meta: meta}
}

// Start provisions the box for name, then launches the agent IFF the transport
// reports a separable launch.
//
// For the welded carriers (tmux/ssh: the agent IS the new-session command; k8s:
// the pod entrypoint; welded exec packs: the `start` op launches), Provision
// alone launches the agent and SeparableLaunch is false, so Start does NOT call
// Launch — Transport.Launch is the SEPARATE relaunch-into-a-warm-box capability
// the reconciler uses for a launch-only change, and calling it here would launch
// the agent twice (see the un-weld design, B1/B3a).
//
// For an exec pack that declares proc.provision (B3b), Provision creates the box
// WITHOUT the agent (SeparableLaunch true), so Start must Provision THEN Launch.
func (s *seamProvider) Start(ctx context.Context, name string, cfg Config) error {
	place, err := s.rt.Provision(ctx, name, ProvisionRequest{Config: cfg})
	if err != nil {
		return err
	}
	if s.tp.Capabilities().SeparableLaunch {
		_, err = s.tp.Launch(ctx, place, LaunchSpec{Config: cfg})
	}
	return err
}

// attach re-resolves the live attachment for name, or (nil,false) when the box is
// not running.
func (s *seamProvider) attach(name string) (Attachment, bool) {
	ctx := context.Background()
	place, ok, err := s.rt.Open(ctx, name)
	if err != nil || !ok {
		return nil, false
	}
	att, ok, err := s.tp.Open(ctx, place, name)
	if err != nil || !ok {
		return nil, false
	}
	return att, true
}

func (s *seamProvider) Stop(name string) error {
	ctx := context.Background()
	place, ok, err := s.rt.Open(ctx, name)
	if err != nil || !ok {
		return nil // idempotent: already gone
	}
	if att, ok, _ := s.tp.Open(ctx, place, name); ok {
		_ = att.Close(ctx) // how-half
	}
	return place.Teardown(ctx) // where-half
}

func (s *seamProvider) Interrupt(name string) error {
	if att, ok := s.attach(name); ok {
		return att.Interrupt(context.Background())
	}
	return nil
}

func (s *seamProvider) IsRunning(name string) bool {
	_, ok, _ := s.rt.Open(context.Background(), name)
	return ok
}

func (s *seamProvider) IsAttached(name string) bool {
	att, ok := s.attach(name)
	if !ok {
		return false
	}
	obs, err := att.Observe(context.Background(), nil)
	return err == nil && obs.Attached
}

func (s *seamProvider) Attach(name string) error {
	ctx := context.Background()
	place, ok, err := s.rt.Open(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrSessionNotFound, name)
	}
	return s.tp.Attach(ctx, place, name)
}

// ProcessAlive threads the per-call process names through Attachment.Observe.
func (s *seamProvider) ProcessAlive(name string, processNames []string) bool {
	att, ok := s.attach(name)
	if !ok {
		return false
	}
	obs, err := att.Observe(context.Background(), processNames)
	return err == nil && obs.ProcessAlive
}

func (s *seamProvider) Nudge(name string, content []ContentBlock) error {
	if att, ok := s.attach(name); ok {
		return att.Nudge(context.Background(), content)
	}
	return nil
}

func (s *seamProvider) SetMeta(name, key, value string) error {
	if s.meta == nil {
		return fmt.Errorf("runtime does not implement MetaStore")
	}
	return s.meta.SetMeta(name, key, value)
}

func (s *seamProvider) GetMeta(name, key string) (string, error) {
	if s.meta == nil {
		return "", nil
	}
	return s.meta.GetMeta(name, key)
}

func (s *seamProvider) RemoveMeta(name, key string) error {
	if s.meta == nil {
		return nil
	}
	return s.meta.RemoveMeta(name, key)
}

func (s *seamProvider) Peek(name string, lines int) (string, error) {
	if att, ok := s.attach(name); ok {
		return att.Peek(context.Background(), lines)
	}
	return "", nil
}

func (s *seamProvider) ListRunning(prefix string) ([]string, error) {
	return s.rt.List(context.Background(), prefix)
}

func (s *seamProvider) GetLastActivity(name string) (time.Time, error) {
	att, ok := s.attach(name)
	if !ok {
		return time.Time{}, nil
	}
	obs, err := att.Observe(context.Background(), nil)
	if err != nil {
		return time.Time{}, err
	}
	return obs.LastActivity, nil
}

func (s *seamProvider) ClearScrollback(name string) error {
	if att, ok := s.attach(name); ok {
		return att.ClearScrollback(context.Background())
	}
	return nil
}

func (s *seamProvider) CopyTo(name, src, relDst string) error {
	ctx := context.Background()
	place, ok, err := s.rt.Open(ctx, name)
	if err != nil || !ok {
		return nil // best-effort
	}
	return place.Stage(ctx, []CopyEntry{{Src: src, RelDst: relDst}})
}

func (s *seamProvider) SendKeys(name string, keys ...string) error {
	if att, ok := s.attach(name); ok {
		return att.SendKeys(context.Background(), keys...)
	}
	return nil
}

// RunLive is a no-op: there is no seam for live session_live re-apply (see GAPS).
func (s *seamProvider) RunLive(string, Config) error { return nil }

func (s *seamProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{
		CanReportActivity:   s.rt.Capabilities().ReportActivity,
		CanReportAttachment: s.tp.Capabilities().ReportAttachment,
	}
}
