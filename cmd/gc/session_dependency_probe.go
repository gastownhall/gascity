package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// dependencyProbeBound is the longest one start-dependency probe waits for the
// runtime to answer whether a dependency session is alive. It is a variable so
// a test can shrink it; production code never assigns it.
var dependencyProbeBound = postStartObservationBound

// dependencyProbeAbandonBudget is how many abandoned waits in a row a pass
// spends before it stops calling the provider.
const dependencyProbeAbandonBudget = 3

// abandonBudget counts the waits a pass gave up on, so a provider-wide wedge
// costs a pass a few bounds instead of one per probe. Only a wait that ran out
// its bound counts. An answer that came in time resets the count. A refusal
// that came at once, because an earlier observation of the session had not
// returned, neither counts nor resets it.
type abandonBudget struct {
	limit     int
	abandoned int
}

// exhausted reports whether the pass has given up on enough consecutive waits
// to stop calling the provider.
func (b *abandonBudget) exhausted() bool { return b.abandoned >= b.limit }

// abandon records a wait that ran out its bound.
func (b *abandonBudget) abandon() { b.abandoned++ }

// answered records an answer that arrived in time.
func (b *abandonBudget) answered() { b.abandoned = 0 }

// dependencyProbePass carries what one planner pass shares across its
// dependency checks: the single open-session list, the answer for each
// dependency session and the abandon budget. A pass is one candidateWaveOrder
// call, one wave's first filter or one recheck batch, never a whole tick: the
// recheck exists to see a dependency that died or came up since the first
// filter, so an answer must not outlive the pass that probed for it. A pass
// belongs to one goroutine.
type dependencyProbePass struct {
	ctx      context.Context
	cityPath string
	stderr   io.Writer

	// listed is true once the open sessions were read, whatever came of it.
	listed bool
	// listErr is why the read failed; a failed read leaves byName empty.
	listErr error
	// byName groups the open sessions by the session name their bead records.
	byName map[string][]sessionpkg.Info

	// alive memoizes, per session name, whether the dependency answered alive.
	// A probe that did not answer is memoized as not alive.
	alive  map[string]bool
	budget abandonBudget
}

// newDependencyProbePass starts a pass for the city at cityPath whose probes
// end when ctx does (a nil ctx never ends a probe early) and whose unanswered
// probes are logged to stderr.
func newDependencyProbePass(ctx context.Context, cityPath string, stderr io.Writer) *dependencyProbePass {
	if ctx == nil {
		ctx = context.Background()
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return &dependencyProbePass{
		ctx:      ctx,
		cityPath: cityPath,
		stderr:   stderr,
		alive:    map[string]bool{},
		budget:   abandonBudget{limit: dependencyProbeAbandonBudget},
	}
}

// listOpenSessions reads the open sessions. The pass takes it as a function so
// that it never holds the session store itself: the caller that already has the
// store binds it.
type listOpenSessions func() ([]sessionpkg.Info, error)

// openSessions reads the open sessions once for the pass, through list, and
// groups them by session name. A read that failed is remembered with its
// error, so the pass does not retry it and every check in it sees the same
// answer. A nil list is a city with no session store: no open sessions.
func (p *dependencyProbePass) openSessions(list listOpenSessions) (map[string][]sessionpkg.Info, error) {
	if p.listed {
		return p.byName, p.listErr
	}
	p.listed = true
	if list == nil {
		return nil, nil
	}
	infos, err := list()
	if err != nil {
		p.listErr = err
		return nil, err
	}
	p.byName = make(map[string][]sessionpkg.Info, len(infos))
	for _, info := range infos {
		p.byName[info.SessionNameMetadata] = append(p.byName[info.SessionNameMetadata], info)
	}
	return p.byName, nil
}

// startInFlight reports whether a create of sessionName is still in flight
// within startupTimeout. It reads the pass's open-session list, so it costs a
// store read only for the first check of the pass. A list error answers true
// (still starting): the dependent waits until the store recovers. A city with no
// session store (a nil list) has nothing in flight.
func (p *dependencyProbePass) startInFlight(list listOpenSessions, sessionName string, startupTimeout time.Duration, clk clock.Clock) bool {
	sessionName = strings.TrimSpace(sessionName)
	if list == nil || sessionName == "" {
		return false
	}
	byName, err := p.openSessions(list)
	if err != nil {
		return true
	}
	for _, info := range byName[sessionName] {
		if pendingCreateStartInFlightInfo(info, clk, startupTimeout) {
			return true
		}
	}
	return false
}

// probeTarget names what the liveness probe of sessionName resolves: the id of
// the one open bead that records the name, so the lookup is a read by id, or
// the name itself when the list holds no such bead or more than one, which the
// by-name lookup already handles.
func (p *dependencyProbePass) probeTarget(list listOpenSessions, sessionName string) string {
	byName, err := p.openSessions(list)
	if err != nil {
		return sessionName
	}
	if matches := byName[sessionName]; len(matches) == 1 && strings.TrimSpace(matches[0].ID) != "" {
		return matches[0].ID
	}
	return sessionName
}

// observeAlive asks the runtime whether the session target (a bead id or a
// session name) is alive.
type observeAlive func(target string) (bool, error)

// sessionAlive reports whether the dependency session sessionName answered
// alive in this pass. It probes the runtime at most once per pass: the answer is
// memoized by name, including an answer of "did not answer". The probe resolves
// the session by the id of its one open bead, falls back to the name otherwise
// (probeTarget), and runs under postStartObservationKey(cityPath, sessionName),
// the key every other bounded observer of the session uses. A probe that does
// not answer within dependencyProbeBound, or is refused because an earlier
// observation of the session is still outstanding, is NOT alive for this pass
// and is logged as unverifiable: starting a dependent against an unverified
// dependency is the wrong action, and waiting a tick costs nothing else
// (SESSION-RUNTIME-009). After dependencyProbeAbandonBudget abandoned waits in a
// row the rest of the pass answers the same way without calling the provider.
// Any other error is not alive and is not logged.
func (p *dependencyProbePass) sessionAlive(list listOpenSessions, sessionName string, observe observeAlive) bool {
	if alive, ok := p.alive[sessionName]; ok {
		return alive
	}
	if p.budget.exhausted() {
		p.alive[sessionName] = false
		p.logUnverifiable(sessionName, fmt.Sprintf("%d consecutive probes were abandoned; not probing", p.budget.abandoned))
		return false
	}
	target := p.probeTarget(list, sessionName)
	ctx, cancel := context.WithTimeout(p.ctx, dependencyProbeBound)
	defer cancel()
	alive, err := observeSessionBounded(ctx, p.cityPath, sessionName, func() (bool, error) {
		return observe(target)
	})
	switch {
	case errors.Is(err, worker.ErrObservationOutstanding):
		p.logUnverifiable(sessionName, "an earlier observation of it has not returned")
		alive = false
	case errors.Is(err, runtime.ErrRuntimeUnavailable) && ctx.Err() != nil:
		p.budget.abandon()
		p.logUnverifiable(sessionName, fmt.Sprintf("no answer within %s", dependencyProbeBound))
		alive = false
	default:
		p.budget.answered()
		alive = err == nil && alive
	}
	p.alive[sessionName] = alive
	return alive
}

// logUnverifiable says a dependency could not be probed, in words that differ
// from the ordinary "not alive" wait.
func (p *dependencyProbePass) logUnverifiable(sessionName, why string) {
	fmt.Fprintf(p.stderr, "session reconciler: dependency %s unverifiable (%s); dependents wait a tick\n", sessionName, why) //nolint:errcheck
}
