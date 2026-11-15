package exec

import (
	"context"
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
// and this one's published after it, as a welded start does.
func (p *Provider) runLaunchOp(ctx context.Context, name string, cfg runtime.Config) error {
	data, err := p.launchConfig(cfg)
	if err != nil {
		return err
	}
	p.clearInputFence(ctx, name)
	if _, err := p.runWithContext(ctx, p.startTimeout, data, launchOp, name); err != nil {
		return fmt.Errorf("exec provider: launching agent in %q: %w", name, err)
	}
	return p.publishInputFence(ctx, name, cfg)
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
// queue cannot read, has the turn held until ReadyDelayMs after the launch
// (StartupNudge.NotBefore). A welded pack's start op already did all of it,
// so there it does nothing.
func (p *Provider) finishSeparableLaunch(ctx context.Context, name string, cfg runtime.Config, hostSetup bool) {
	if !p.supportsSeparableLaunch() {
		return
	}
	launched := time.Now()
	if hostSetup {
		p.runHostSessionSetup(ctx, name, cfg)
	}
	if cfg.Nudge == "" {
		return
	}
	turn := runtime.StartupNudge{Text: cfg.Nudge}
	if delay := time.Duration(cfg.ReadyDelayMs) * time.Millisecond; delay > 0 && !p.idleBoundaryDeclared() {
		turn.NotBefore = launched.Add(delay)
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
