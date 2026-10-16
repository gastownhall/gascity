package tmux

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]) for 17
// driving/lifecycle methods (Peek rides the raw provider, below), while EMBEDDING the raw *Provider so that tmux's
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
// non-seam-routed method (known or not) stays on the real provider; only the 17
// Provider methods below are explicitly routed through the seams. RunLive is
// intentionally NOT overridden, so the embedded provider's real RunLive runs.
type seamBackedProvider struct {
	*Provider                  // raw: carries RunLive + every optional interface
	seams     runtime.Provider // the seam adapter, for the 17 routed methods
}

var (
	_ runtime.Provider                 = (*seamBackedProvider)(nil)
	_ runtime.UnattendedSessionStopper = (*seamBackedProvider)(nil)
	// Relaunch (B2) rides the embedded raw *Provider — it is NOT one of the 17
	// seam-routed methods, so the warm-box relaunch stays on the real provider.
	_ runtime.RelaunchProvider = (*seamBackedProvider)(nil)
	// The listing attestation and the batched inventory also ride the
	// embedded raw provider; ListRunning routes through the seams to the same
	// raw listing.
	_ runtime.ListingAttestation = (*seamBackedProvider)(nil)
	_ runtime.InventoryProvider  = (*seamBackedProvider)(nil)
	// The error-bearing attachment probe is promoted from the embedded raw
	// provider; IsAttached (bool) routes through the seams to the same probe.
	_ runtime.AttachmentObserverWithError = (*seamBackedProvider)(nil)
	// The error-bearing liveness observation is promoted from the embedded raw
	// provider; IsRunning and ProcessAlive (bool) route through the seams.
	_ runtime.LivenessObserverWithError = (*seamBackedProvider)(nil)
	// The confirmed-dead server check, the fresh read and the exact-object
	// kills are promoted from the embedded raw provider.
	_ runtime.ServerDeathConfirmer  = (*seamBackedProvider)(nil)
	_ runtime.FreshLivenessObserver = (*seamBackedProvider)(nil)
	_ runtime.SessionObjectKiller   = (*seamBackedProvider)(nil)
)

// NewSeamBackedWithConfig constructs a tmux provider served through the seams.
func NewSeamBackedWithConfig(cfg Config) runtime.Provider {
	raw := NewProviderWithConfig(cfg)
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: raw, seams: runtime.NewProviderFromSeams(rt, tp)}
}

// --- the 17 Provider methods routed through the seams (RunLive intentionally
// NOT overridden, so the embedded raw provider's real RunLive is used, and Peek
// overridden onto the raw provider) ---

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

// Peek rides the embedded raw provider, not the seams: the seam's Peek reads
// through an Attachment, which the tmux transport opens only on a running
// session, so a remain-on-exit corpse would read as an empty screen with no
// error. The dead pane's screen is what the controller classifies (a
// provider rate-limit or terminal error, SESS-088/530; mc-g3la1), and
// capture-pane reads it, as legacy's raw Peek always did.
func (s *seamBackedProvider) Peek(name string, lines int) (string, error) {
	return s.Provider.Peek(name, lines)
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
