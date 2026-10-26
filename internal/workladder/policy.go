package workladder

import "time"

// OD7's parameters. Changing one is a policy change: the decision table
// golden (testdata/decision_table.golden) shows its effect.
const (
	DefaultGrace         = 90 * time.Second
	DefaultBase          = 3 * time.Minute
	DefaultCap           = 60 * time.Minute
	DefaultPausedCap     = 10 * time.Minute
	DefaultProgressReset = 10 * time.Minute
	DefaultRecycleRepost = 30 * time.Minute
	DefaultMaxRecycles   = 1

	DefaultTriggerThreshold      = 3 // trigger/assigned: then recycle (keep nudging at Cap)
	DefaultContinuationThreshold = 6 // continuation/assigned: then alert (keep nudging at Cap)
	DefaultClaimedThreshold      = 3 // claim/claimed: then release
	DefaultStartedThreshold      = 6 // claim/started: then recycle, or escalate
)

// DefaultPolicy is OD7's: grace 90s, base 3m doubling to a 60m cap and then
// hourly forever, a 10m cap while the provider is degraded, R = 1.
func DefaultPolicy() Policy {
	return Policy{
		Grace:         DefaultGrace,
		Base:          DefaultBase,
		Cap:           DefaultCap,
		PausedCap:     DefaultPausedCap,
		ProgressReset: DefaultProgressReset,
		RecycleRepost: DefaultRecycleRepost,
		Threshold: map[Kind]map[Rung]int{
			KindTrigger:      {RungAssigned: DefaultTriggerThreshold},
			KindContinuation: {RungAssigned: DefaultContinuationThreshold},
			KindClaim:        {RungClaimed: DefaultClaimedThreshold, RungStarted: DefaultStartedThreshold},
		},
		MaxRecycles: DefaultMaxRecycles,
	}
}

// withDefaults fills every unset or non-positive duration and threshold from
// DefaultPolicy, so no Policy can pace at zero (WL1, WL3).
func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	fill := func(v *time.Duration, def time.Duration) {
		if *v <= 0 {
			*v = def
		}
	}
	fill(&p.Grace, d.Grace)
	fill(&p.Base, d.Base)
	fill(&p.Cap, d.Cap)
	fill(&p.PausedCap, d.PausedCap)
	fill(&p.ProgressReset, d.ProgressReset)
	fill(&p.RecycleRepost, d.RecycleRepost)
	if p.Base > p.Cap {
		p.Base = p.Cap
	}
	if p.PausedCap > p.Cap {
		p.PausedCap = p.Cap
	}
	if p.Threshold == nil {
		p.Threshold = d.Threshold
	}
	if p.MaxRecycles < 0 {
		p.MaxRecycles = 0
	}
	return p
}

// threshold is the counted nudges an entry gets on its rung before the rung's
// escalation step. A missing or non-positive entry uses the default.
func (p Policy) threshold(k Kind, r Rung) int {
	if t := p.Threshold[k][r]; t > 0 {
		return t
	}
	if t := DefaultPolicy().Threshold[k][r]; t > 0 {
		return t
	}
	return DefaultClaimedThreshold
}

// backoff is the wait after the attempts-th counted nudge: Base doubling,
// never above Cap. Once the rung's threshold step has acted (attempts past
// the threshold, or a release that did not finish), and on the escalated
// rung, it is Cap: "keep nudging at Cap".
func (p Policy) backoff(e Entry) time.Duration {
	if e.Rung == RungEscalated || e.Unreleased || e.Attempts > p.threshold(e.Kind, e.Rung) {
		return p.Cap
	}
	d := p.Base
	for i := 1; i < e.Attempts && d < p.Cap; i++ {
		d *= 2
	}
	return min(d, p.Cap)
}
