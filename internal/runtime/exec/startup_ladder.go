package exec

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// This file is the un-welded start's tail (proc.provision): the non-tmux
// `launch` op (proc.launch), and the handback of the first turn. `provision`
// runs before any agent exists and `launch` types nothing, so on this path
// the start config's nudge, which carries a pack runtime's startup prompt, is
// gc's to deliver. Start never types it: after the launch and the dialog pass
// it hands the turn back to the Start's caller
// ([runtime.StartupNudgeHandbackProvider]), which queues it, and the queue
// delivers it once the agent takes input, through the path every later nudge
// takes.

// launchOp is the op that starts the agent in a provisioned box.
const launchOp = "launch"

// launchDeclared reports whether the pack declared the `launch` op.
func (p *Provider) launchDeclared() bool {
	return p.handshakeCapability(runtime.ProtocolCapabilityLaunch)
}

// launchConfig is the start config the `launch` op receives: the config
// `provision` received, without the nudge gc queues itself (and, for a pack
// sharing the host filesystem, without the host work gc does itself).
func (p *Provider) launchConfig(cfg runtime.Config) ([]byte, error) {
	cfg.Nudge = ""
	marshal := marshalStartConfig
	if p.sharesHostFS() {
		marshal = marshalHostStagedStartConfig
	}
	data, err := marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("exec provider: marshaling launch config: %w", err)
	}
	return data, nil
}

// runLaunchOp starts the agent in name's box through the `launch` op. A pack
// that can fence has the previous incarnation's fence cleared before the op
// and this one's published after it, as a welded start does. When the op or
// the publish fails, the cleared fence goes back on the pane
// (restoreInputFence), as after a welded start a live box refused: a launch
// refused before it replaced the agent leaves that agent running, with the
// fence it held. The restore applies only to the pane and root process that
// held the fence, so an agent the op did replace never gets its
// predecessor's.
func (p *Provider) runLaunchOp(ctx context.Context, name string, cfg runtime.Config) error {
	data, err := p.launchConfig(cfg)
	if err != nil {
		return err
	}
	saved := p.clearInputFence(ctx, name)
	if _, err := p.runWithContext(ctx, p.startTimeout, data, launchOp, name); err != nil {
		p.restoreInputFence(context.WithoutCancel(ctx), name, saved)
		return fmt.Errorf("exec provider: launching agent in %q: %w", name, err)
	}
	if err := p.publishInputFence(ctx, name, cfg); err != nil {
		p.restoreInputFence(context.WithoutCancel(ctx), name, saved)
		return err
	}
	return nil
}

// launchIdentityKeys are the identity keys a separable launch publishes
// with set-meta (publishLaunchIdentity), in the order they are written: the
// counters before the token, so a reader that sees the new token sees the
// counters of its launch.
var launchIdentityKeys = []string{runtime.RuntimeEpochEnv, runtime.ContinuationEpochEnv, runtime.InstanceTokenEnv}

// publishLaunchIdentity records, with set-meta, the identity of the agent a
// separable launch of name just started: the instance token and both epochs
// cfg.Env carries. The launch hands its first turn back bound to that token
// (runtime.ReadLaunchID), and the queue judges and fences the turn by the
// same get-meta reads (runtime.ReadInputFence), so they must answer this
// launch from the moment Start returns, whatever the pack's own launch op
// stored. A key cfg.Env does not carry is left as the box holds it (a warm
// relaunch without a token keeps the box's). The writes run past the
// start's ctx, so a start that spent its budget on the launch still
// publishes, and share one op timeout, so a store that hangs holds the start
// for one op timeout at most.
//
// A failed write is read back with get-meta, and stands when the box
// already holds the value (on a fresh start, the one provision seeded for
// this launch). Otherwise publishLaunchIdentity returns the error, and the
// launch fails: a launch reported up under an identity get-meta cannot
// answer would have its first turn held, and the session's sends behind
// it, until the turn expired, and one under its predecessor's would have
// the turn misjudged. A runtime that keeps no metadata
// (runtime.ErrMetaUnsupported) fails the launch too, as a pack that
// un-welds start must keep the launch identity (RPP-PROVISION-005): its
// turn could be bound to no launch, so it would be queued as a plain
// submit, which the session's sends overtake. Nothing is removed, so the
// box keeps the token it held for the failed start's cleanup.
func (p *Provider) publishLaunchIdentity(ctx context.Context, name string, cfg runtime.Config) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
	defer cancel()
	for _, key := range launchIdentityKeys {
		value, ok := cfg.Env[key]
		if !ok || value == "" {
			continue
		}
		if err := p.writeLaunchIdentityKey(ctx, name, key, value); err != nil {
			return err
		}
	}
	return nil
}

// writeLaunchIdentityKey writes key's value for name with set-meta, under
// publishLaunchIdentity's deadline. A failed write stands when get-meta
// reads the key back holding value. An unsupported one
// (runtime.ErrMetaUnsupported) is never read back: the box keeps no
// metadata, so the error wraps it and names the requirement. The error
// names the key, never the value.
func (p *Provider) writeLaunchIdentityKey(ctx context.Context, name, key, value string) error {
	_, err := p.runWithContext(ctx, p.timeout, []byte(value), "set-meta", name, key)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, runtime.ErrMetaUnsupported):
		return fmt.Errorf("exec provider: session %q: publishing the launch's %s: a pack that un-welds start (proc.provision) must keep the launch identity in its metadata store (RPP-PROVISION-005): %w", name, key, err)
	}
	held, readErr := p.runWithContext(ctx, p.timeout, nil, "get-meta", name, key)
	switch {
	case readErr != nil:
		return fmt.Errorf("exec provider: session %q: publishing the launch's %s: %w; reading it back: %w", name, key, err, readErr)
	case held != value:
		return fmt.Errorf("exec provider: session %q: publishing the launch's %s: %w; get-meta answers another value", name, key, err)
	}
	return nil
}

// withoutNudge is cfg without its first turn, which a separable launch hands
// back for its caller to queue (finishSeparableLaunch), so `provision` is
// never sent it.
func withoutNudge(cfg runtime.Config) runtime.Config {
	cfg.Nudge = ""
	return cfg
}

// finishSeparableLaunch runs what follows a separable launch of name: the
// host session_setup when hostSetup, then the handback of the config's first
// turn, which the Start's caller queues and the queue delivers once the agent
// takes input. Start never types the turn, so no deadline of its own can cut
// it off or lose it. A pack without an idle boundary, whose readiness the
// queue cannot read, has the turn held until ReadyDelayMs past the end of
// the launch's dialog pass (StartupNudge.NotBefore). A welded pack's start
// op already did all of it, so there it does nothing.
func (p *Provider) finishSeparableLaunch(ctx context.Context, name string, cfg runtime.Config, hostSetup bool) {
	if !p.supportsSeparableLaunch() {
		return
	}
	// The launch and its dialog pass are done; the floor counts from here.
	dialogsDone := time.Now()
	if hostSetup {
		p.runHostSessionSetup(ctx, name, cfg)
	}
	if cfg.Nudge == "" {
		return
	}
	turn := runtime.StartupNudge{Text: cfg.Nudge}
	if delay := time.Duration(cfg.ReadyDelayMs) * time.Millisecond; delay > 0 && !p.idleBoundaryDeclared() {
		turn.NotBefore = dialogsDone.Add(delay)
	}
	p.handbacks.set(name, turn)
}

// startupHandbacks holds, per session, the first turn a separable launch
// handed back, until the Start's caller takes it. Each launch clears its
// session's entry first, so an untaken one never outlives the next start.
type startupHandbacks struct {
	mu     sync.Mutex
	nudges map[string]runtime.StartupNudge
}

// set records nudge for session; an empty nudge clears it.
func (h *startupHandbacks) set(session string, nudge runtime.StartupNudge) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if nudge.Text == "" {
		delete(h.nudges, session)
		return
	}
	if h.nudges == nil {
		h.nudges = make(map[string]runtime.StartupNudge)
	}
	h.nudges[session] = nudge
}

// take returns and forgets session's handed-back turn.
func (h *startupHandbacks) take(session string) (runtime.StartupNudge, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	nudge, ok := h.nudges[session]
	delete(h.nudges, session)
	return nudge, ok
}

// TakeStartupNudgeHandback implements [runtime.StartupNudgeHandbackProvider]:
// the first turn the last separable launch of name handed back, once.
func (p *Provider) TakeStartupNudgeHandback(name string) (runtime.StartupNudge, bool) {
	return p.handbacks.take(name)
}

var _ runtime.StartupNudgeHandbackProvider = (*Provider)(nil)
