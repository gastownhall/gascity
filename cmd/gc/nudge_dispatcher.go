package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// pingNudgeWakeSocketDialTimeout bounds how long a producer waits to dial
// the supervisor wake socket. Producers must not block on a stale or
// missing socket — legacy-mode cities and pre-start producers expect the
// dial to fail fast.
const pingNudgeWakeSocketDialTimeout = 200 * time.Millisecond

// pingNudgeWakeSocket sends a best-effort wake signal to the supervisor's
// nudge dispatcher. Callers invoke this after enqueueing a queued nudge so
// the supervisor delivers within sub-second latency instead of waiting for
// the next patrol tick. Failures (no listener, dial timeout, write error)
// are intentionally silent: the patrol-tick fallback in supervisor mode
// and the per-session poller in legacy mode each guarantee eventual
// delivery without the wake.
//
// It is a variable so the file queue's characterization tests can count the
// pings a queue verb sends without opening a listener; tests that replace it
// must stay serial.
var pingNudgeWakeSocket = func(cityPath string) {
	if cityPath == "" {
		return
	}
	path := nudgequeue.WakeSocketPath(cityPath)
	conn, err := net.DialTimeout("unix", path, pingNudgeWakeSocketDialTimeout)
	if err != nil {
		return
	}
	defer conn.Close() //nolint:errcheck // best-effort signaling
	_ = conn.SetWriteDeadline(time.Now().Add(pingNudgeWakeSocketDialTimeout))
	_, _ = conn.Write([]byte{1})
}

// startNudgeWakeListener opens the supervisor wake socket and spawns an
// accept loop that signals wakeCh on every connection. The returned
// listener is closed when ctx is canceled. Returns nil, nil when the
// socket cannot be opened (e.g. permission, path-too-long); callers fall
// back to patrol-interval dispatching.
func startNudgeWakeListener(ctx context.Context, cityPath string, wakeCh chan<- struct{}, stderr io.Writer, logPrefix string) (net.Listener, error) {
	path := nudgequeue.WakeSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("creating nudge wake dir: %w", err)
	}
	// A stale socket from a prior supervisor crash blocks Listen with
	// "address already in use". Removing it is safe because flock-based
	// queue access protects state; the socket carries no data of its own.
	_ = os.Remove(path)
	lis, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on nudge wake socket: %w", err)
	}
	// TOCTOU: there is a narrow window between Listen and Chmod where
	// the socket exists at the umask-default permissions and a co-local
	// user could connect. Worst case is a spurious dispatch tick — the
	// socket carries a single signal byte with no payload or auth — so
	// this is acceptable for now. A future hardening pass could set
	// umask before Listen, or use platform-specific abstract namespace
	// sockets where supported.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = lis.Close()
		return nil, fmt.Errorf("chmod nudge wake socket: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = lis.Close()
	}()
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					return
				}
				if stderr != nil {
					fmt.Fprintf(stderr, "%s: nudge wake accept: %v\n", logPrefix, err) //nolint:errcheck
				}
				continue
			}
			// Drain whatever the producer sent (a single signal byte) and
			// close. The wake itself is the signal — payload is reserved
			// for future protocol extensions.
			_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			var buf [16]byte
			_, _ = conn.Read(buf[:])
			_ = conn.Close()
			select {
			case wakeCh <- struct{}{}:
			default:
				// Already-pending wake covers this enqueue; coalesced.
			}
		}
	}()
	return lis, nil
}

// dispatchAllQueuedNudges runs one supervisor-side dispatcher pass: scan
// the queue for pending agents, resolve each to a nudgeTarget via
// sessionBeads, and try delivery. Returns the number of targets that
// successfully delivered at least one item.
//
// In "legacy" mode the per-session `gc nudge poll` processes own delivery,
// except for ACP sessions: session/prompt needs the process that owns the
// in-memory ACP connection, so no poller is started for them
// (ensureNudgePollerForTarget), and this pass delivers their queued items
// (mc-z0og9). Legacy mode skips every other target, and returns at once
// when no open session is on ACP.
//
// debugOut receives one GC_DEBUG-gated line per silently-skipped target (see
// logNudgeDispatchSkip); pass nil to suppress (skip counts still accumulate
// into the persisted queue state's DispatchSkips regardless of debugOut, so
// `gc nudge status` stays informative even with GC_DEBUG unset).
func dispatchAllQueuedNudges(cityPath string, cfg *config.City, store, sessStore beads.Store, sp runtime.Provider, sessionBeads *sessionBeadSnapshot, debugOut io.Writer) (int, error) {
	if cfg == nil || sessionBeads == nil || cityPath == "" {
		return 0, nil
	}
	acpOnly := !nudgeDispatcherIsSupervisor(cfg)
	if acpOnly && !anyOpenACPSession(cityPath, cfg, sessionBeads) {
		return 0, nil
	}
	return dispatchQueuedNudges(openNudgeQueue(cityPath, beads.NudgesStore{Store: store}), cityPath, cfg, store, sessStore, sp, sessionBeads, debugOut)
}

// dispatchQueuedNudges is the dispatcher pass of dispatchAllQueuedNudges over
// queue. A DueKeys read that comes back partial still dispatches the agents it
// read, and the pass reports its error.
func dispatchQueuedNudges(queue nudgeQueueHandle, cityPath string, cfg *config.City, store, sessStore beads.Store, sp runtime.Provider, sessionBeads *sessionBeadSnapshot, debugOut io.Writer) (int, error) {
	now := time.Now()
	// Run the queue's TTL/max-attempts maintenance sweep unconditionally,
	// independent of whether any item below matches an open session. The
	// per-session loop's only path to recover/prune is a successful claim in
	// claimDueQueuedNudgesForTarget, which a structurally orphaned item
	// (target agent has no open session, and never will again) can never
	// reach — leaving it in Pending past its ExpiresAt forever. See
	// ra-oudpha finding-3.
	if _, err := queue.Queue.Maintain(context.Background(), now); err != nil {
		return 0, fmt.Errorf("nudge queue maintenance sweep: %w", err)
	}
	// The agents with a due pending item, or with an in-flight item whose
	// lease has expired: that one is recoverable on the next claim attempt,
	// so including its agent retries without waiting for the patrol tick.
	// A record DueKeys cannot decode costs only its own agent this pass: the
	// agents it did read are dispatched, and the partial result is reported.
	var firstErr error
	dueKeys, err := queue.Queue.DueKeys(now)
	if err != nil {
		if !beads.IsPartialResult(err) {
			return 0, fmt.Errorf("loading nudge queue: %w", err)
		}
		firstErr = fmt.Errorf("loading nudge queue: %w", err)
	}
	if len(dueKeys) == 0 {
		return 0, firstErr
	}
	pendingAgents := make(map[string]bool, len(dueKeys))
	for _, key := range dueKeys {
		pendingAgents[key] = true
	}

	// The dispatcher receives the nudges-class store (store) PLUS the session-class
	// store (sessStore) the caller resolved from the WORK store — the controller
	// threads cr.sessionsBeadStore().Store, whose fallback is the work store, NOT
	// the nudges store. The session observe below and the queue-delivery path's
	// session ops route through sessStore; the queue record/dead-letter stays on
	// store. Identity today; corrects the pre-existing controller-side class mix
	// (deriving sessStore from the nudges base would mis-resolve session beads once
	// nudges relocates independently of sessions).
	delivered := 0
	// skipCounts accumulates this tick's silent-skip reasons in memory; it is
	// merged into the persisted queue state's running totals once, after the
	// loop, rather than on every skip — recordNudgeDispatchSkips takes the
	// queue flock, and taking it once per matched info instead of once per
	// tick would multiply lock contention against the claim path below for
	// no benefit (the counters only need tick-granularity, not per-item).
	skipCounts := make(map[string]int64)
	for _, info := range sessionBeads.OpenInfos() {
		target := resolveNudgeTargetFromSessionInfo(cityPath, cfg, info)
		if acpOnly && target.sessionTransport() != "acp" {
			continue // its poller delivers it
		}
		if target.sessionName == "" {
			skipCounts["no-target"]++
			logNudgeDispatchSkip(debugOut, "no-target", info.AgentName, info.ID, "")
			continue
		}
		// ACP sessions also flow through this dispatcher. The inject-on-hook
		// drain path still catches deliveries when the agent receives external
		// prompts, but a warm-idle ACP session never fires its hook on its
		// own — queued patrol wisps would otherwise pile up forever. The
		// atomic queue claim in claimDueQueuedNudgesForTarget guarantees a
		// nudge is delivered exactly once across the dispatcher + drain paths.
		matched := false
		for _, key := range target.queueKeys() {
			if pendingAgents[key] {
				matched = true
				break
			}
		}
		if !matched {
			// Routine: this open session simply has no pending queue item
			// targeting it. Counted (not just logged) so an operator can
			// tell "nothing queued for anyone" apart from the anomalous
			// skip reasons below at a glance.
			skipCounts["not-matched"]++
			logNudgeDispatchSkip(debugOut, "not-matched", target.agentKey(), target.sessionName, "")
			continue
		}
		if sessionpkg.IsKillPendingInfo(info, now) {
			// A `gc session kill` is tearing this runtime down. Delivering now
			// would type into a process that is about to die and ack the item
			// as delivered; leave it queued for the next incarnation.
			skipCounts["kill-pending"]++
			logNudgeDispatchSkip(debugOut, "kill-pending", target.agentKey(), target.sessionName, "")
			continue
		}
		obs, err := workerObserveNudgeTarget(target, sessStore, sp)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			skipCounts["observe-error"]++
			logNudgeDispatchSkip(debugOut, "observe-error", target.agentKey(), target.sessionName, err.Error())
			continue
		}
		if !obs.Running {
			skipCounts["not-running"]++
			logNudgeDispatchSkip(debugOut, "not-running", target.agentKey(), target.sessionName, "")
			continue
		}
		var ok bool
		if target.sessionTransport() == "acp" {
			var pending bool
			if ok, pending, err = deliverACPQueuedNudgesBounded(cityPath, target, func() (bool, error) {
				return tryDeliverQueuedNudgesByPoller(target, store, sessStore, sp, defaultNudgePollQuiescence, obs)
			}); pending {
				skipCounts["acp-delivery-pending"]++
				logNudgeDispatchSkip(debugOut, "acp-delivery-pending", target.agentKey(), target.sessionName, "")
				continue
			}
		} else {
			ok, err = tryDeliverQueuedNudgesByPoller(target, store, sessStore, sp, defaultNudgePollQuiescence, obs)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if ok {
			delivered++
			continue
		}
		// Matched a live, running session, yet nothing was claimed/delivered
		// this tick — e.g. the poller quiescence gate (pollerSessionIdleEnough)
		// hasn't cleared, or claimDueQueuedNudgesForTarget found nothing
		// claimable (already claimed by a concurrent drain path). Either way
		// this is the class of skip ra-oudpha finding-3 could not otherwise
		// distinguish from "not matched" or "not running" without a trace.
		reason := "not-delivered"
		if err != nil {
			reason = "not-delivered-error"
		}
		skipCounts[reason]++
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		logNudgeDispatchSkip(debugOut, reason, target.agentKey(), target.sessionName, detail)
	}
	if err := queue.Skips.RecordDispatchSkips(skipCounts); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("recording nudge dispatch skip counters: %w", err)
	}
	return delivered, firstErr
}

// acpDeliveryWait bounds how long a dispatch pass waits for one ACP
// target's queued delivery. An ACP nudge waits up to its busy timeout (60s
// by default) for a busy agent, and the pass runs on the controller's loop
// (#7816 review), so a delivery still running past the bound is left to
// finish on its own, its items claimed, and the pass moves on.
var acpDeliveryWait = 3 * time.Second

// acpDeliveries are the ACP targets with a delivery running, by city and
// session name: one at a time per target, so a busy agent never piles up
// waiting deliveries across passes.
var acpDeliveries sync.Map

// deliverACPQueuedNudgesBounded runs deliver for target off the pass, and
// waits for it at most acpDeliveryWait. pending reports a delivery still
// running, this one or an earlier pass's; ok and err are deliver's when it
// returned in time.
func deliverACPQueuedNudgesBounded(cityPath string, target nudgeTarget, deliver func() (bool, error)) (ok, pending bool, err error) {
	key := cityPath + "\x00" + target.sessionName
	if _, running := acpDeliveries.LoadOrStore(key, struct{}{}); running {
		return false, true, nil
	}
	type result struct {
		ok  bool
		err error
	}
	done := make(chan result, 1)
	go func() {
		// Off the pass, safeTick no longer recovers a panic here, and one
		// would end the controller and, under a supervisor, every city. A
		// recovered delivery's claimed items go back to pending when their
		// lease expires (#7816 review). The target is freed before the
		// result is sent, so a pass that reads it sees no delivery running.
		var r result
		defer func() {
			if p := recover(); p != nil {
				r = result{err: fmt.Errorf("acp delivery to %s panicked: %v", target.sessionName, p)}
			}
			acpDeliveries.Delete(key)
			done <- r
		}()
		r.ok, r.err = deliver()
	}()
	select {
	case r := <-done:
		return r.ok, false, r.err
	case <-time.After(acpDeliveryWait):
		return false, true, nil
	}
}

// anyOpenACPSession reports an open session on the ACP transport, whose
// queued nudges only the controller can deliver.
func anyOpenACPSession(cityPath string, cfg *config.City, sessionBeads *sessionBeadSnapshot) bool {
	for _, info := range sessionBeads.OpenInfos() {
		if resolveNudgeTargetFromSessionInfo(cityPath, cfg, info).sessionTransport() == "acp" {
			return true
		}
	}
	return false
}

// logNudgeDispatchSkip emits a single GC_DEBUG-gated line documenting one
// silently-skipped target from the dispatchAllQueuedNudges loop. w may be
// nil, in which case this is a no-op (callers that don't have a debug sink,
// e.g. most existing tests, pass nil).
func logNudgeDispatchSkip(w io.Writer, reason, agent, session, detail string) {
	if w == nil {
		return
	}
	extra := []string{"agent", agent, "session", session}
	if detail != "" {
		extra = append(extra, "detail", detail)
	}
	logRoute(w, "nudge-dispatch-tick", "skip", reason, extra...)
}
