package tmux

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]) for the 18
// driving/lifecycle methods, while EMBEDDING the raw *Provider so that tmux's
// large optional-interface surface AND its real RunLive (session_live re-apply,
// which the seam adapter cannot model) are carried unchanged. The early cut-over
// for the local tmux provider.
//
// Embed-raw (rather than the other providers' enumerate-passthrough) is used
// here because tmux's optional surface is too large to enumerate safely —
// InteractionProvider, ImmediateNudge, IdleWait, InterruptedTurnReset,
// InterruptBoundaryWait, DialogProvider, ProcessTableScanner, ServerLifecycle,
// DeadRuntimeSessionChecker, LivenessObserver, SleepCapability, … — and a missed
// interface would silently degrade a capability. Embedding raw guarantees every
// non-seam-routed method (known or not) stays on the real provider; only the 18
// Provider methods below are explicitly routed through the seams. RunLive is
// intentionally NOT overridden, so the embedded provider's real RunLive runs.
type seamBackedProvider struct {
	*Provider                  // raw: carries RunLive + every optional interface
	seams     runtime.Provider // the seam adapter, for the 18 routed methods
}

var _ runtime.Provider = (*seamBackedProvider)(nil)

// NewSeamBackedWithConfig constructs a tmux provider served through the seams.
func NewSeamBackedWithConfig(cfg Config) runtime.Provider {
	raw := NewProviderWithConfig(cfg)
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: raw, seams: runtime.NewProviderFromSeams(rt, tp)}
}

// --- the 18 Provider methods routed through the seams (RunLive intentionally
// NOT overridden, so the embedded raw provider's real RunLive is used) ---

func (s *seamBackedProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	return s.seams.Start(ctx, name, cfg)
}
func (s *seamBackedProvider) Stop(name string) error      { return s.seams.Stop(name) }
func (s *seamBackedProvider) Interrupt(name string) error { return s.seams.Interrupt(name) }
func (s *seamBackedProvider) IsRunning(name string) bool  { return s.seams.IsRunning(name) }
func (s *seamBackedProvider) IsAttached(name string) bool { return s.seams.IsAttached(name) }
func (s *seamBackedProvider) Attach(name string) error    { return s.seams.Attach(name) }
func (s *seamBackedProvider) ProcessAlive(name string, processNames []string) bool {
	return s.seams.ProcessAlive(name, processNames)
}

func (s *seamBackedProvider) Nudge(name string, content []runtime.ContentBlock) error {
	return s.seams.Nudge(name, content)
}

func (s *seamBackedProvider) SetMeta(name, key, value string) error {
	return s.seams.SetMeta(name, key, value)
}

func (s *seamBackedProvider) GetMeta(name, key string) (string, error) {
	return s.seams.GetMeta(name, key)
}

func (s *seamBackedProvider) RemoveMeta(name, key string) error {
	return s.seams.RemoveMeta(name, key)
}

func (s *seamBackedProvider) Peek(name string, lines int) (string, error) {
	return s.seams.Peek(name, lines)
}

func (s *seamBackedProvider) ListRunning(prefix string) ([]string, error) {
	return s.seams.ListRunning(prefix)
}

func (s *seamBackedProvider) GetLastActivity(name string) (time.Time, error) {
	return s.seams.GetLastActivity(name)
}

func (s *seamBackedProvider) ClearScrollback(name string) error {
	return s.seams.ClearScrollback(name)
}

func (s *seamBackedProvider) CopyTo(name, src, relDst string) error {
	return s.seams.CopyTo(name, src, relDst)
}

func (s *seamBackedProvider) SendKeys(name string, keys ...string) error {
	return s.seams.SendKeys(name, keys...)
}

func (s *seamBackedProvider) Capabilities() runtime.ProviderCapabilities {
	return s.seams.Capabilities()
}
