package exec

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// seamBackedProvider serves the legacy [runtime.Provider] through the
// de-conflated seams (via [runtime.NewProviderFromSeams]), passing the optional
// interfaces production callers type-assert — DialogProvider (dialog dismissal),
// SleepCapability, IdleWaitProvider (the idle boundary) and its probe budget,
// FencedInputProvider (the carrier guard),
// the error-bearing attachment probe, and the image-checker (CheckImage) — through to the
// underlying *Provider. The early cut-over for the exec provider.
//
// The observe.v1 interfaces (the error-bearing liveness read, fresh reads,
// identity, exact object kills, v2 readiness, the batched inventory and its
// gate, the attested listing) are forwarded too, as are the driving verbs
// (behind a presence check, so input to a vanished session is never reported
// delivered) and the single-op reads, and Start
// routes a FreshOnly start to the raw provider's: an optional interface the
// cut-over does not forward does not exist in production.
//
// The process-table scanner, which only a v2-ready pack sharing the host
// can use (process_scan.go), and the local-transcript trait (hoststage.go)
// are passed through from the files that define them.
//
// ExecProvider is deliberately NOT passed through: exec's Exec op is the
// connection the carrier drives over INTERNALLY (no production caller
// type-asserts it), so seam-backed driving reaches it through the provider's own
// carrier-backed Nudge/Peek/SendKeys/Interrupt/ClearScrollback.
type seamBackedProvider struct {
	runtime.Provider
	raw *Provider
}

var (
	_ runtime.Provider                = (*seamBackedProvider)(nil)
	_ runtime.DialogProvider          = (*seamBackedProvider)(nil)
	_ runtime.SleepCapabilityProvider = (*seamBackedProvider)(nil)
	_ runtime.RelaunchProvider        = (*seamBackedProvider)(nil)

	_ runtime.FencedInputProvider         = (*seamBackedProvider)(nil)
	_ runtime.IdleWaitProvider            = (*seamBackedProvider)(nil)
	_ runtime.AttachmentObserverWithError = (*seamBackedProvider)(nil)
	_ runtime.IdleProbeBudgetProvider     = (*seamBackedProvider)(nil)

	_ runtime.LivenessObserverWithError = (*seamBackedProvider)(nil)
	_ runtime.FreshByConstruction       = (*seamBackedProvider)(nil)
	_ runtime.IdentityObserver          = (*seamBackedProvider)(nil)
	_ runtime.SessionObjectKiller       = (*seamBackedProvider)(nil)
	_ runtime.V2ReadinessReporter       = (*seamBackedProvider)(nil)
	_ runtime.InventoryProvider         = (*seamBackedProvider)(nil)
	_ runtime.InventoryGate             = (*seamBackedProvider)(nil)
	_ runtime.ListingAttestation        = (*seamBackedProvider)(nil)
	_ runtime.InventoryLister           = (*seamBackedProvider)(nil)

	_ runtime.ServerLifecycleProvider = (*seamBackedProvider)(nil)
)

// NewSeamBacked wraps an exec provider for the given script so it is served
// through the seams.
func NewSeamBacked(script string, opts ...Option) runtime.Provider {
	return seamBacked(NewProvider(script, opts...))
}

// NewSeamBackedForCity is NewSeamBacked for a provider serving one city: every
// op carries GC_EXEC_CITY_NAME and GC_EXEC_CITY_PATH (NewProviderForCity).
func NewSeamBackedForCity(script, cityName, cityPath string, opts ...Option) runtime.Provider {
	return seamBacked(NewProviderForCity(script, cityName, cityPath, opts...))
}

func seamBacked(raw *Provider) *seamBackedProvider {
	rt, tp := raw.Seams()
	return &seamBackedProvider{Provider: runtime.NewProviderFromSeams(rt, tp), raw: raw}
}

// Start serves a FreshOnly start from the raw provider's startFresh, which
// never respawns into a box the seams' Provision then Launch would find;
// every other start goes through the seams.
func (s *seamBackedProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if cfg.FreshOnly {
		return s.raw.startFresh(ctx, name, cfg)
	}
	return s.Provider.Start(ctx, name, cfg)
}

// ObserveLivenessWithError passes a v2-ready pack's fresh `observe` through;
// otherwise it is the read runtime.ObserveLivenessWithError made through the
// seams before this provider had the capability.
func (s *seamBackedProvider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	if !s.raw.v2Ready() {
		return runtime.ObserveLiveness(s.Provider, name, processNames), nil
	}
	return s.raw.ObserveLivenessWithError(name, processNames)
}

// LivenessReadsFresh passes through to the underlying provider. Safe on a nil
// provider.
func (s *seamBackedProvider) LivenessReadsFresh() bool {
	return s != nil && s.raw.LivenessReadsFresh()
}

// IdentityObservable passes through to the underlying provider. Safe on a nil
// provider.
func (s *seamBackedProvider) IdentityObservable() bool {
	return s != nil && s.raw.IdentityObservable()
}

// ObserveIdentity passes through to the underlying provider's `observe`.
func (s *seamBackedProvider) ObserveIdentity(name string) (map[string]string, error) {
	return s.raw.ObserveIdentity(name)
}

// KillCorpseObject passes through to the underlying provider's `kill-object`.
func (s *seamBackedProvider) KillCorpseObject(name, objectID, created string) (runtime.SessionObjectKillResult, error) {
	return s.raw.KillCorpseObject(name, objectID, created)
}

// KillZombieObject passes through to the underlying provider's `kill-object`.
func (s *seamBackedProvider) KillZombieObject(name, objectID, created, panePID string) (runtime.SessionObjectKillResult, error) {
	return s.raw.KillZombieObject(name, objectID, created, panePID)
}

// RuntimeInventory passes through to the underlying provider's `inventory`.
func (s *seamBackedProvider) RuntimeInventory(ctx context.Context) (map[string]runtime.InventoryEntry, error) {
	return s.raw.RuntimeInventory(ctx)
}

// ListInventory passes through to the underlying provider's `inventory`; the
// seams' ListRunning is the same read.
func (s *seamBackedProvider) ListInventory(prefix string) ([]string, map[string]runtime.InventoryEntry, error) {
	return s.raw.ListInventory(prefix)
}

// InventoryAvailable passes through to the underlying provider. Safe on a
// nil provider.
func (s *seamBackedProvider) InventoryAvailable() bool {
	return s != nil && s.raw.InventoryAvailable()
}

// ListRunningComplete passes through to the underlying provider; the seams'
// ListRunning is the underlying provider's. Safe on a nil provider.
func (s *seamBackedProvider) ListRunningComplete() bool {
	return s != nil && s.raw.ListRunningComplete()
}

// V2Readiness passes through to the underlying provider's handshake.
func (s *seamBackedProvider) V2Readiness() ([]string, error) {
	return s.raw.V2Readiness()
}

// ConfigureServer passes through to the underlying provider's no-op
// (non-seam).
func (s *seamBackedProvider) ConfigureServer() error {
	return s.raw.ConfigureServer()
}

// TeardownServer passes through to the underlying provider, which runs
// `server-stop` for a pack declaring server-lifecycle (non-seam).
func (s *seamBackedProvider) TeardownServer() error {
	return s.raw.TeardownServer()
}

// DismissKnownDialogs implements [runtime.DialogProvider] (non-seam passthrough).
func (s *seamBackedProvider) DismissKnownDialogs(ctx context.Context, name string, timeout time.Duration) error {
	return s.raw.DismissKnownDialogs(ctx, name, timeout)
}

// SleepCapability passes through to the underlying provider (non-seam).
func (s *seamBackedProvider) SleepCapability(name string) runtime.SessionSleepCapability {
	return s.raw.SleepCapability(name)
}

// WaitForIdle passes through to the underlying provider's pane-scan idle
// boundary (non-seam).
func (s *seamBackedProvider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	return s.raw.WaitForIdle(ctx, name, timeout)
}

// IdleProbeBudget passes through to the underlying provider (non-seam), so
// the orchestrator sizes idle proofs for this pack's round trips.
func (s *seamBackedProvider) IdleProbeBudget() time.Duration {
	return s.raw.IdleProbeBudget()
}

// IsAttachedWithError passes through to the underlying provider's single
// is-attached op (non-seam). Without it, runtime.IsAttachedWithError would
// fall back to the seam's IsAttached, which re-resolves the box and drops
// the probe error.
func (s *seamBackedProvider) IsAttachedWithError(name string) (bool, error) {
	return s.raw.IsAttachedWithError(name)
}

// Peek reads a v2-ready script's screen of any box the name holds, live or
// dead (peekBox): the seams' Peek reads only a box `is-running` opens, and
// answers anything else, and every failure, as an empty screen, so v2 would
// recycle a dead box without reading why it died. Other scripts keep the
// seams' Peek.
func (s *seamBackedProvider) Peek(name string, lines int) (string, error) {
	if s.raw.v2Ready() {
		return s.raw.peekBox(name, lines)
	}
	return s.Provider.Peek(name, lines)
}

// Nudge passes through to the underlying provider once requirePresent has
// not found the session gone. The seams' Nudge answers nil for a session
// `is-running` does not open, which a caller reads as delivered.
func (s *seamBackedProvider) Nudge(name string, content []runtime.ContentBlock) error {
	if err := s.raw.requirePresent(name); err != nil {
		return err
	}
	return s.raw.Nudge(name, content)
}

// SendKeys passes through to the underlying provider once requirePresent
// has not found the session gone.
func (s *seamBackedProvider) SendKeys(name string, keys ...string) error {
	if err := s.raw.requirePresent(name); err != nil {
		return err
	}
	return s.raw.SendKeys(name, keys...)
}

// Interrupt passes through to the underlying provider once requirePresent
// has not found the session gone.
func (s *seamBackedProvider) Interrupt(name string) error {
	if err := s.raw.requirePresent(name); err != nil {
		return err
	}
	return s.raw.Interrupt(name)
}

// ClearScrollback passes through to the underlying provider once
// requirePresent has not found the session gone.
func (s *seamBackedProvider) ClearScrollback(name string) error {
	if err := s.raw.requirePresent(name); err != nil {
		return err
	}
	return s.raw.ClearScrollback(name)
}

// IsAttached passes through to the underlying provider's single probe; the
// seams' IsAttached opens the box twice and reads every liveness field.
func (s *seamBackedProvider) IsAttached(name string) bool {
	return s.raw.IsAttached(name)
}

// GetLastActivity passes through to the underlying provider's single
// get-last-activity op; the seams' read opens the box twice first.
func (s *seamBackedProvider) GetLastActivity(name string) (time.Time, error) {
	return s.raw.GetLastActivity(name)
}

// ProcessAlive reads a v2-ready pack's `observe` once: a live agent is
// alive, a dead agent or a missing box is not, and a failed read is alive,
// never a spurious dead. Other packs keep the seams' read, because a pack
// without process-alive answers exit 2, which reads alive even for a
// missing box.
func (s *seamBackedProvider) ProcessAlive(name string, processNames []string) bool {
	if !s.raw.v2Ready() {
		return s.Provider.ProcessAlive(name, processNames)
	}
	o, err := s.raw.observe(context.Background(), name, processNames)
	if err != nil {
		return true
	}
	l := o.liveness(processNames)
	return l.Running && l.Alive
}

// CheckImage passes through (non-seam; cmd/gc start asserts an image-checker).
func (s *seamBackedProvider) CheckImage(image string) error {
	return s.raw.CheckImage(image)
}

// Relaunch passes through to the underlying provider's warm-box relaunch (B2,
// RelaunchProvider): for a separable pack it respawns the agent over the exec op;
// for a welded pack it degrades to a reprovision.
func (s *seamBackedProvider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	return s.raw.Relaunch(ctx, name, cfg)
}

// Capabilities preserves the raw provider's complete handshake-derived
// capability set through the production seam-backed composition.
func (s *seamBackedProvider) Capabilities() runtime.ProviderCapabilities {
	return s.raw.Capabilities()
}

// InputFencing passes through to the underlying provider (non-seam): the
// pack's handshake decides it.
func (s *seamBackedProvider) InputFencing(name string) runtime.InputFencing {
	return s.raw.InputFencing(name)
}

// NudgeFenced passes through to the underlying provider's carrier guard.
func (s *seamBackedProvider) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	return s.raw.NudgeFenced(ctx, name, content, n)
}

// SendKeysFenced passes through to the underlying provider's carrier guard.
func (s *seamBackedProvider) SendKeysFenced(ctx context.Context, name string, fence runtime.InputFence, keys ...string) error {
	return s.raw.SendKeysFenced(ctx, name, fence, keys...)
}

// InterruptFenced passes through to the underlying provider's carrier guard.
func (s *seamBackedProvider) InterruptFenced(ctx context.Context, name string, fence runtime.InputFence) error {
	return s.raw.InterruptFenced(ctx, name, fence)
}
