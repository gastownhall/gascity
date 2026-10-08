package main

import (
	"context"
	"strings"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/telemetry"
)

// The drain effects (CONTRACT v5 D1, D2, F1, F4). A begin proves its
// reason's legs immediately before its CAS, and refuses with the holding
// leg as its cause, backing the row off (P4):
//   - idle and no-wake-reason (the -fresh kind): attach (L3) and a pending
//     interaction (L4) read fresh through the routed backend (C8.9), and
//     for an interactive idle session the agent proved idle (SESS-621);
//   - orphaned and suspended: L5, the row's open or in-progress assigned
//     work read live through the read-only stores (SESS-074); work or a
//     failed read refuses.
//
// Every drain write records legacy's drain transition once it lands.

// Drain-begin refusal causes, besides the fence's.
const (
	causeHasWork = "has-work"
	causeNotIdle = "not-idle"
)

// drainBeginEffect is a begin's effect, either kind: under the runtime name
// lock, one session mutation section proves the legs and runs the CAS.
func drainBeginEffect(p *effectPass, it intent) func(context.Context) settlement {
	return func(ctx context.Context) settlement {
		row, ok := p.World.Census.Rows[it.Key]
		if !ok {
			return settlement{Outcome: settledRefused, Cause: causeRedecided}
		}
		name, unlock, ok := lockRuntimeName(p.World, row.Info)
		switch {
		case !ok && name == "":
			return settlement{Outcome: settledRefused, Cause: causeRouteUnknown}
		case !ok:
			return settlement{Outcome: settledRefused, Cause: causeNameBusy}
		}
		defer unlock()
		reason := strings.TrimPrefix(it.Reason, decideDrainBegin)
		var s settlement
		_ = session.WithSessionMutationLock(it.Key.ID, func() error {
			if cause := drainBeginLegs(ctx, p, row.Info, reason, it.Kind == intentDrainBeginFresh); cause != "" {
				s = settlement{Outcome: settledRefused, Cause: cause}
				return nil
			}
			s = rowWrite{pass: p, it: it, decide: decideRow}.runLocked(ctx)
			return nil
		})
		return drainTransition(ctx, s, name, reason, "begin")
	}
}

// drainClearEffect is a cancel's or a void's effect.
func drainClearEffect(p *effectPass, it intent) func(context.Context) settlement {
	return func(ctx context.Context) settlement {
		name := p.World.Census.Rows[it.Key].Info.SessionNameMetadata
		_, reason, _ := strings.Cut(it.Reason, ":")
		return drainTransition(ctx, rowWriteEffect(p, it)(ctx), name, reason, "cancel")
	}
}

// drainBeginLegs is the begin's legs for reason, or the cause that holds it.
func drainBeginLegs(ctx context.Context, p *effectPass, info session.Info, reason string, fresh bool) string {
	if fresh {
		if p.Runtime == nil {
			return causeRouteUnknown
		}
		if v := fenceDestructive(ctx, p.Runtime, fenceRequest{Row: info, Legs: legAttach | legPending}, p.World.Now); !v.Proceed {
			return v.Reason
		}
		if _, probe := idleProof(p.World, info, reason); probe && !provedIdle(ctx, p, info.SessionNameMetadata) {
			return causeNotIdle
		}
	}
	if reason != drainOrphaned && reason != drainSuspended {
		return ""
	}
	if p.Reads.City == nil || p.World.Env == nil {
		return causeHasWork
	}
	has, err := sessionHasOpenAssignedWorkForReachableStore(p.World.CityPath, p.World.Env.Cfg, p.Reads.City, p.Reads.Rigs, info)
	if err != nil || has {
		return causeHasWork
	}
	return ""
}

// provedIdle is legacy's idle probe (launchIdleProbes, then
// shouldBeginIdleDrainInfo): WaitForIdle succeeds, and the runtime reports
// no activity after the pass began, stricter than legacy's probe completion.
func provedIdle(ctx context.Context, p *effectPass, name string) bool {
	wp, ok := p.Runtime.(runtime.IdleWaitProvider)
	if !ok || wp.WaitForIdle(ctx, name, idleSleepProbeTimeout) != nil {
		return false
	}
	last, err := p.Runtime.GetLastActivity(name)
	return err == nil && (last.IsZero() || !last.After(p.World.Now))
}

// drainTransition records legacy's drain telemetry for a landed write.
func drainTransition(ctx context.Context, s settlement, name, reason, transition string) settlement {
	if s.Outcome == settledLanded {
		telemetry.RecordDrainTransition(context.WithoutCancel(ctx), name, reason, transition)
	}
	return s
}
