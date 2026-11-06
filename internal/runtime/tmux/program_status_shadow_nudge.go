package tmux

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// A nudge's one decision reading under the shadow switch. A nudge types once
// it decided to: the idle waits that let it go (its own, inside Nudge, and
// one decided outside the leaf whose delivery it carries) and its typing
// gate decide the same delivery, with nothing typed between them. So they
// share one fresh read, the one the nudge takes just before its last look
// ahead of the paste (nudgeDecide), which is also the baseline its submit is
// confirmed against: a wait deferred to it (deferToNudge) is judged there,
// before anything the delivery types can reach the record. A nudge that ends
// before that read judges what it deferred on a read taken as it ends
// (settleNudge), when it has typed nothing. The submit's confirmation is a
// decision of its own, after the paste, and keeps its own read.

// shadowNudgeLimit bounds how many sessions' deferred waits one Tmux keeps;
// past it they start over.
const shadowNudgeLimit = 4096

// shadowNudgeKey marks a context as a shadowing nudge's (withShadowNudge).
type shadowNudgeKey struct{}

// withShadowNudge marks ctx as session's nudge: an idle wait it decides
// defers its reading to the nudge's decision reading.
func withShadowNudge(ctx context.Context, session string) context.Context {
	return context.WithValue(ctx, shadowNudgeKey{}, session)
}

// inShadowNudge reports whether ctx is session's nudge (withShadowNudge).
func inShadowNudge(ctx context.Context, session string) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(shadowNudgeKey{}).(string)
	return marked != "" && marked == session
}

// deferToNudge defers judge to session's next nudge decision reading.
func (t *Tmux) deferToNudge(session string, judge func(shadowDecision)) {
	sh := &t.shadow
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, known := sh.atNudge[session]; sh.atNudge == nil || (!known && len(sh.atNudge) >= shadowNudgeLimit) {
		sh.atNudge = make(map[string][]func(shadowDecision))
	}
	sh.atNudge[session] = append(sh.atNudge[session], judge)
}

// takeNudgeJudges returns and forgets every judge deferred to session's
// next nudge decision reading.
func (t *Tmux) takeNudgeJudges(session string) []func(shadowDecision) {
	sh := &t.shadow
	sh.mu.Lock()
	defer sh.mu.Unlock()
	judges := sh.atNudge[session]
	delete(sh.atNudge, session)
	return judges
}

// nudgeDeferred reports whether a judge waits on session's next nudge
// decision reading.
func (t *Tmux) nudgeDeferred(session string) bool {
	sh := &t.shadow
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return len(sh.atNudge[session]) > 0
}

// nudgeDecide takes session's nudge decision reading (shadowDecide), just
// before the nudge types, and judges on it every wait deferred to it. A read
// that failed judges nothing: there is nothing to compare.
func (t *Tmux) nudgeDecide(session string) (shadowDecision, bool) {
	d, ok := t.shadowDecide(session)
	judges := t.takeNudgeJudges(session)
	if ok {
		for _, judge := range judges {
			judge(d)
		}
	}
	return d, ok
}

// settleNudge judges every wait still deferred to session's nudge decision
// reading on a reading taken now (nudgeDecide), and reads nothing when none
// is. A nudge calls it as it ends, and a caller once the delivery it
// deferred a wait for returned: either way no typing of that delivery came
// first, since every nudge takes its decision reading before it types.
func (t *Tmux) settleNudge(session string) {
	if t.nudgeDeferred(session) {
		t.nudgeDecide(session)
	}
}

// ProgramStatusShadowIdleWaitAtNudge implements
// [runtime.ProgramStatusShadowProvider]: an idle wait decided outside the
// leaf (the op lane's boundary) is judged as the leaf judges its own
// (shadowWaitForIdle), on session name's next nudge decision reading. It
// defers nothing, and reports false, unless the leaf is shadowing.
func (p *Provider) ProgramStatusShadowIdleWaitAtNudge(name string, started, decidedAt time.Time, scrape runtime.ProgramVerdict) bool {
	if !p.tm.shadowing() || !validSessionNameRe.MatchString(name) {
		return false
	}
	p.tm.deferToNudge(name, func(d shadowDecision) { p.tm.shadowWaitForIdle(name, started, decidedAt, scrape, d) })
	return true
}

// ProgramStatusShadowSettleNudge implements
// [runtime.ProgramStatusShadowProvider] (settleNudge).
func (p *Provider) ProgramStatusShadowSettleNudge(name string) {
	p.tm.settleNudge(name)
}

// ProgramStatusShadowGo implements [runtime.ProgramStatusShadowProvider]:
// fn runs on a shadow goroutine (shadowGo), one at a time per session name
// and point. A call that finds one running runs nothing and drops nothing:
// the one running reads the same moment.
func (p *Provider) ProgramStatusShadowGo(name string, point runtime.ProgramStatusPoint, fn func()) {
	release, ok := p.tm.claimShadowObserve(name, point)
	if !ok {
		return
	}
	if !p.tm.shadowGo(func() {
		defer release()
		fn()
	}) {
		release()
	}
}

// claimShadowObserve claims session's point for one background comparison
// and returns the call that releases it, or false when one is running.
func (t *Tmux) claimShadowObserve(session string, point runtime.ProgramStatusPoint) (func(), bool) {
	sh := &t.shadow
	key := shadowFollowKey{session: session, point: point}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, busy := sh.observing[key]; busy {
		return nil, false
	}
	if sh.observing == nil || len(sh.observing) >= shadowNudgeLimit {
		sh.observing = make(map[shadowFollowKey]struct{})
	}
	sh.observing[key] = struct{}{}
	return func() {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		delete(sh.observing, key)
	}, true
}
