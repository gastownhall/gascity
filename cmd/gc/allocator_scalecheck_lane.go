package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// The scale_check lane (I5, POOL-005): runs every custom scale_check pool's
// command on the patrol cadence, off the allocator pass, and publishes the
// counts. The allocator reads the latest result in memory; a command never
// runs inside an allocator pass.
//
// It runs the same work legacy's demand pass runs (evaluatePendingPools under
// the probe_concurrency semaphore) over the same pools: non-suspended,
// generic-ephemeral agents with a custom scale_check outside suspended rigs,
// and none while the city is suspended.
// One difference is deliberate (BEHAVIORS #38): a pool whose probe env cannot
// be built is marked partial rather than silently skipped, so its count reads
// as untrusted (retain, block create) instead of zero.
//
// Unwired in this slice: P3-7 starts it beside the allocator lane.

const (
	// scaleCheckMinGap is the shortest gap between a pass and a woken one.
	scaleCheckMinGap = 2 * time.Second
	// scaleCheckSafeTickTrigger names the lane in safeTick panic lines.
	scaleCheckSafeTickTrigger = "v2-scale-check"
)

// scaleCheckResult is one lane pass: each custom scale_check pool's count
// (additive new demand), the pools whose count cannot be trusted, and when the
// pass finished. Immutable once published.
type scaleCheckResult struct {
	Counts  map[string]int
	Partial map[string]bool
	At      time.Time
}

// partial reports whether template's count cannot be trusted at now: no pass
// has published, the result is older than maxAge (2 × patrol), the pass had
// no check for template, or its check or env build failed. It is meaningful
// only for templates with a custom scale_check: every other template reads
// partial.
func (r *scaleCheckResult) partial(template string, now time.Time, maxAge time.Duration) bool {
	if r == nil || now.Sub(r.At) > maxAge || r.Partial[template] {
		return true
	}
	_, ok := r.Counts[template]
	return !ok
}

// scaleCheckLane owns the lane's inputs and its latest result.
type scaleCheckLane struct {
	cityName, cityPath string
	// env returns the published environment; the lane reads its config each
	// pass and its patrol interval once, at start.
	env func() *reconcileEnv
	// suspension is the runtime suspension state (I9), read once per pass.
	suspension *suspensionReader
	runner     ScaleCheckRunner
	// queryEnv builds a pool's probe env (controllerQueryRuntimeEnv).
	queryEnv probeEnvFunc
	// onChange wakes the allocator after a pass whose result differs from
	// the one before.
	onChange func()
	// safeTick runs each pass, recovering and reporting a panic.
	safeTick func(fn func(), trigger string) (panicked bool)
	stderr   io.Writer

	wakeCh chan struct{}
	result atomic.Pointer[scaleCheckResult]
	passes atomic.Int64
}

func newScaleCheckLane(cityName, cityPath string, env func() *reconcileEnv, fs fsys.FS, onChange func(), safeTick func(fn func(), trigger string) bool, stderr io.Writer) *scaleCheckLane {
	return &scaleCheckLane{
		cityName:   cityName,
		cityPath:   cityPath,
		env:        env,
		suspension: newSuspensionReader(fs, cityPath),
		runner:     shellScaleCheck,
		queryEnv:   controllerQueryRuntimeEnv,
		onChange:   onChange,
		safeTick:   safeTick,
		stderr:     stderr,
		wakeCh:     make(chan struct{}, 1),
	}
}

// start runs the lane until ctx is done and returns a channel closed when it
// exits. The first pass runs at once, so custom pools are not partial for a
// patrol after boot; then a pass runs one patrol interval after the previous
// one ended, or on a wake, paced by startGatedPacedLane: a pass skipped while
// the city is suspended does not count toward the duty cycle. A pass that
// panics is recovered by safeTick, counts as run, and the lane keeps its
// cadence.
func (l *scaleCheckLane) start(ctx context.Context) <-chan struct{} {
	l.wake()
	return startGatedPacedLane(ctx, l.env().patrol(), scaleCheckMinGap, l.wakeCh, func(bool) bool {
		ran := false
		panicked := l.safeTick(func() { ran = l.pass() }, scaleCheckSafeTickTrigger)
		return ran || panicked
	})
}

// wake asks for a pass, for example after a reload changed the pools.
// Non-blocking.
func (l *scaleCheckLane) wake() {
	select {
	case l.wakeCh <- struct{}{}:
	default:
	}
}

// latest returns the last published result, or nil before the first pass.
func (l *scaleCheckLane) latest() *scaleCheckResult { return l.result.Load() }

// pass runs every custom scale_check once and publishes the result, and
// reports whether it ran. While the city is suspended it runs nothing and
// publishes nothing, as legacy's demand pass returns before any scale_check
// (buildDesiredStateWithSessionBeadsAt); the last result ages into partial.
// Like legacy, it reads an unreadable suspension file as the zero state.
func (l *scaleCheckLane) pass() (ran bool) {
	env := l.env()
	if env == nil || env.Cfg == nil {
		return false
	}
	cfg := env.Cfg
	st, _ := l.suspension.state()
	if effectiveCitySuspended(cfg, st) {
		return false
	}
	work, envFailed := customScaleCheckWork(l.cityName, l.cityPath, cfg, suspendedRigPathsWithState(cfg, st), l.queryEnv, l.stderr)
	counts, partials := evaluatePendingPoolsMapWith(cfg, work, l.runner, l.stderr, nil)
	for _, template := range envFailed {
		counts[template] = 0
		partials = markScaleCheckPartialTemplate(partials, template)
	}
	next := &scaleCheckResult{Counts: counts, Partial: partials, At: time.Now()}
	prev := l.result.Swap(next)
	l.passes.Add(1)
	if prev == nil || !maps.Equal(prev.Counts, next.Counts) || !maps.Equal(prev.Partial, next.Partial) {
		l.onChange()
	}
	return true
}

// customScaleCheckWork builds the pools whose custom scale_check legacy's
// demand pass runs with a store, and names the pools whose probe env could
// not be built. A pool backing a named session runs with no probe env, as in
// legacy.
func customScaleCheckWork(
	cityName, cityPath string,
	cfg *config.City,
	suspendedRigPaths map[string]bool,
	queryEnv probeEnvFunc,
	stderr io.Writer,
) (work []poolEvalWork, envFailed []string) {
	for i := range cfg.Agents {
		agent := &cfg.Agents[i]
		if agent.Suspended || !agent.SupportsGenericEphemeralSessions() || strings.TrimSpace(agent.ScaleCheck) == "" {
			continue
		}
		if rig := configuredRigName(cityPath, agent, cfg.Rigs); rig != "" && suspendedRigPaths[filepath.Clean(rigRootForName(rig, cfg.Rigs))] {
			continue
		}
		sp := scaleParamsForTopology(agent, config.QueryTopology{Beads: cfg.Beads})
		sp.Check = expandAgentCommandTemplate(cityPath, cityName, agent, cfg.Rigs, "scale_check", sp.Check, stderr)
		w := poolEvalWork{agentIdx: i, sp: sp, poolDir: agentCommandDir(cityPath, agent, cfg.Rigs), newDemand: true}
		if !agentBacksNamedSession(cfg, agent) {
			env, err := queryEnv(cityPath, cfg, agent)
			if err != nil {
				fmt.Fprintf(stderr, "scaleCheck: building env for %s: %v (marking partial)\n", agent.QualifiedName(), err) //nolint:errcheck
				envFailed = append(envFailed, agent.QualifiedName())
				continue
			}
			w.env = env
		}
		work = append(work, w)
	}
	return work, envFailed
}

// agentBacksNamedSession reports whether a named session uses agent as its
// template.
func agentBacksNamedSession(cfg *config.City, agent *config.Agent) bool {
	for j := range cfg.NamedSessions {
		if cfg.NamedSessions[j].TemplateQualifiedName() == agent.QualifiedName() {
			return true
		}
	}
	return false
}
