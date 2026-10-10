package runtime

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"
)

// Profile names a production backend by the optional interfaces its provider
// implements. A profiled fake ([NewFakeProfile]) implements exactly those and
// reports the backend's [ProviderCapabilities], so a test meets the
// capability checks production meets. The profile test pins each against the
// provider the runtime registry builds.
type Profile int

// The profiled backends.
const (
	ProfileTmux Profile = iota
	ProfileACP
	ProfileSubprocess
	ProfileHerdr
	ProfileK8s
	ProfileExec
	ProfileSSH
	ProfileT3Bridge
)

// String is the profile's runtime selection name.
func (p Profile) String() string {
	return [...]string{"tmux", "acp", "subprocess", "herdr", "k8s", "exec", "ssh", "t3bridge"}[p]
}

// ProfileBackend is every optional interface a profiled backend implements
// that answers from session state. A profiled fake forwards each such
// interface its profile has to one, and hides the rest; it answers the ones
// that report a backend's fixed traits (sleep capability, fresh reads, an
// identity sidecar, transports) itself, as the backend does. [FullFake] is
// one; a test scripting a read embeds it and overrides that method.
type ProfileBackend interface {
	Provider
	AttachmentObserverWithError
	DeadRuntimeSessionChecker
	DialogProvider
	EnvironmentBatchProvider
	FreshLivenessObserver
	IdleSnapshotProvider
	IdleWaitProvider
	ImmediateNudgeProvider
	InteractionProvider
	InterruptBoundaryWaitProvider
	InterruptedTurnResetProvider
	InventoryProvider
	ListingAttestation
	LivenessObserver
	LivenessObserverWithError
	ProcessTableScanner
	RelaunchProvider
	ServerDeathConfirmer
	ServerLifecycleProvider
	SessionEventProvider
	SessionObjectKiller
	SessionRosterProvider
	UnattendedSessionStopper
}

// NewFakeProfile returns b as backend p: a provider implementing exactly p's
// optional interfaces, each forwarded to b but p's fixed traits, and
// reporting p's capabilities.
func NewFakeProfile(p Profile, b ProfileBackend) Provider {
	core, timed := profileProvider{b, profileCapabilities[p]}, sleepTrait{SessionSleepCapabilityTimedOnly}
	switch p {
	case ProfileTmux:
		return tmuxProfile{core, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, b, sleepTrait{SessionSleepCapabilityFull}}
	case ProfileACP:
		return acpProfile{core, b, b, b, b, freshTrait{}, sidecarTrait{}, timed, transportTrait{}}
	case ProfileSubprocess:
		return subprocessProfile{core, b, b, b, freshTrait{}, sidecarTrait{}, timed}
	case ProfileHerdr:
		return herdrProfile{core, b, b, b, b, b}
	case ProfileK8s:
		return k8sProfile{core, b, b, timed}
	case ProfileExec:
		return execProfile{core, b, b, b, b, timed}
	case ProfileSSH:
		return sshProfile{core, b, timed}
	case ProfileT3Bridge:
		return t3bridgeProfile{core, b, timed}
	}
	panic(fmt.Sprintf("runtime: unknown profile %d", p))
}

// The fixed traits a backend reports.
type (
	sleepTrait     struct{ c SessionSleepCapability }
	freshTrait     struct{} // acp and subprocess probe their control socket on every read
	sidecarTrait   struct{} // acp and subprocess keep identity in a local sidecar
	transportTrait struct{} // acp's
)

func (t sleepTrait) SleepCapability(string) SessionSleepCapability { return t.c }

func (freshTrait) LivenessReadsFresh() bool { return true }

func (sidecarTrait) LocalIdentitySidecar() bool { return true }

func (transportTrait) SupportsTransport(transport string) bool { return transport == "acp" }

// profileCapabilities are each backend's static capabilities; subprocess
// has none. exec's come from its script's handshake, so its profile has
// none, as an exec provider has before one.
var profileCapabilities = map[Profile]ProviderCapabilities{
	ProfileTmux:     {CanReportAttachment: true, CanReportActivity: true},
	ProfileACP:      {CanReportActivity: true},
	ProfileHerdr:    {CanReportActivity: true, CanStream: true, CanAttachTTY: true, NeedsClaimBackstop: true},
	ProfileK8s:      {CanReportActivity: true},
	ProfileSSH:      {CanReportActivity: true},
	ProfileT3Bridge: {CanReportActivity: true},
}

// profileProvider is a backend's Provider surface with its capabilities.
type profileProvider struct {
	ProfileBackend
	caps ProviderCapabilities
}

func (p profileProvider) Capabilities() ProviderCapabilities { return p.caps }

// The profiles: each embeds its backend's optional interfaces, no others.
type (
	tmuxProfile struct {
		Provider
		AttachmentObserverWithError
		DeadRuntimeSessionChecker
		DialogProvider
		EnvironmentBatchProvider
		FreshLivenessObserver
		IdleSnapshotProvider
		IdleWaitProvider
		ImmediateNudgeProvider
		InteractionProvider
		InterruptBoundaryWaitProvider
		InterruptedTurnResetProvider
		InventoryProvider
		ListingAttestation
		LivenessObserver
		LivenessObserverWithError
		ProcessTableScanner
		RelaunchProvider
		ServerDeathConfirmer
		ServerLifecycleProvider
		SessionObjectKiller
		SessionRosterProvider
		UnattendedSessionStopper
		sleepTrait
	}
	acpProfile struct {
		Provider
		InteractionProvider
		ListingAttestation
		LivenessObserverWithError
		ProcessTableScanner
		freshTrait
		sidecarTrait
		sleepTrait
		transportTrait
	}
	subprocessProfile struct {
		Provider
		ListingAttestation
		LivenessObserverWithError
		ProcessTableScanner
		freshTrait
		sidecarTrait
		sleepTrait
	}
	herdrProfile struct {
		Provider
		IdleWaitProvider
		ImmediateNudgeProvider
		LivenessObserver
		ServerLifecycleProvider
		SessionEventProvider
	}
	k8sProfile struct {
		Provider
		ListingAttestation
		RelaunchProvider
		sleepTrait
	}
	execProfile struct {
		Provider
		AttachmentObserverWithError
		DialogProvider
		IdleWaitProvider
		RelaunchProvider
		sleepTrait
	}
	sshProfile struct {
		Provider
		RelaunchProvider
		sleepTrait
	}
	t3bridgeProfile struct {
		Provider
		LivenessObserverWithError
		sleepTrait
	}
)

// FullFake is a [Fake] implementing every [ProfileBackend] interface, the
// ones Fake lacks answered from its session state.
type FullFake struct{ *Fake }

var _ ProfileBackend = FullFake{}

// liveness reads name's state: running while started, alive unless a
// zombie. A broken fake's read fails.
func (f FullFake) liveness(method, name string) (Liveness, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: method, Name: name})
	if f.broken {
		return Liveness{}, ErrRuntimeUnavailable
	}
	_, running := f.sessions[name]
	return Liveness{Running: running, Alive: running && !f.Zombies[name]}, nil
}

// ObserveLiveness implements [LivenessObserver].
func (f FullFake) ObserveLiveness(name string, _ []string) Liveness {
	l, _ := f.liveness("ObserveLiveness", name)
	return l
}

// ObserveLivenessWithError implements [LivenessObserverWithError].
func (f FullFake) ObserveLivenessWithError(name string, _ []string) (Liveness, error) {
	return f.liveness("ObserveLivenessWithError", name)
}

// ObserveLivenessSince implements [FreshLivenessObserver]: every read is fresh.
func (f FullFake) ObserveLivenessSince(name string, pn []string, _ time.Time) (Liveness, error) {
	return f.ObserveLivenessWithError(name, pn)
}

// ServerConfirmedDead implements [ServerDeathConfirmer]: never confirmed.
func (FullFake) ServerConfirmedDead() bool { return false }

// IsDeadRuntimeSession implements [DeadRuntimeSessionChecker]: a zombie's
// runtime is dead.
func (f FullFake) IsDeadRuntimeSession(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, running := f.sessions[name]
	return running && f.Zombies[name], nil
}

// GetAllEnvironment implements [EnvironmentBatchProvider] with the session's
// metadata, where tmux keeps it. It fails as a GetMeta of any key would
// (GetMetaErrors).
func (f FullFake) GetAllEnvironment(name string) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: "GetAllEnvironment", Name: name})
	if _, running := f.sessions[name]; !running || f.broken {
		return nil, fmt.Errorf("environment of %q: %w", name, ErrSessionNotFound)
	}
	for _, k := range slices.Sorted(maps.Keys(f.GetMetaErrors[name])) {
		if err := f.GetMetaErrors[name][k]; err != nil {
			return nil, err // one read: any key's configured error fails it
		}
	}
	return maps.Clone(f.meta[name]), nil
}

// SnapshotIdle implements [IdleSnapshotProvider]: a running session is idle.
func (f FullFake) SnapshotIdle(name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, running := f.sessions[name]
	return running, nil
}

// RuntimeInventory implements [InventoryProvider]: every running session,
// its attachment known.
func (f FullFake) RuntimeInventory(context.Context) (map[string]InventoryEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv := make(map[string]InventoryEntry, len(f.sessions))
	for name := range f.sessions {
		inv[name] = InventoryEntry{AttachedKnown: true, Attached: f.Attached[name]}
	}
	return inv, nil
}

// SessionRoster implements [SessionRosterProvider].
func (f FullFake) SessionRoster() (map[string]SessionRosterEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	roster := make(map[string]SessionRosterEntry, len(f.sessions))
	for name := range f.sessions {
		roster[name] = SessionRosterEntry{Attached: f.Attached[name], LastActivity: f.Activity[name]}
	}
	return roster, nil
}

// ConfigureServer implements [ServerLifecycleProvider].
func (f FullFake) ConfigureServer() error {
	f.record("ConfigureServer", "")
	return nil
}

// TeardownServer implements [ServerLifecycleProvider].
func (f FullFake) TeardownServer() error {
	f.record("TeardownServer", "")
	return nil
}

// StopUnattendedSession implements [UnattendedSessionStopper] as Stop.
func (f FullFake) StopUnattendedSession(name, _ string) error {
	f.record("StopUnattendedSession", name)
	return f.Stop(name)
}

// KillCorpseObject implements [SessionObjectKiller]: a fake keeps no
// corpses, so the object is gone.
func (f FullFake) KillCorpseObject(name, _, _ string) (SessionObjectKillResult, error) {
	f.record("KillCorpseObject", name)
	return SessionObjectGone, nil
}

// KillZombieObject implements [SessionObjectKiller]: it stops a zombie.
func (f FullFake) KillZombieObject(name, _, _, _ string) (SessionObjectKillResult, error) {
	f.record("KillZombieObject", name)
	if dead, _ := f.IsDeadRuntimeSession(name); !dead {
		return SessionObjectChanged, nil
	}
	return SessionObjectKilled, f.Stop(name)
}

// SubscribeSessionEvents implements [SessionEventProvider]: a stream that
// sends nothing and closes with ctx.
func (f FullFake) SubscribeSessionEvents(ctx context.Context) (<-chan SessionEvent, error) {
	ch := make(chan SessionEvent)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch, nil
}

func (f FullFake) record(method, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, Call{Method: method, Name: name})
}
