package main

import "context"

// The drain-begin effect for idle and no-wake-reason (CONTRACT v5 D2, F4,
// C8.9): attach (L3) and a pending interaction (L4) are read fresh through
// the row's routed backend, and only when neither holds does the begin's row
// write run. An attached or pending row, or one whose probe cannot answer,
// is refused with the holding leg as its cause and backs off (P4). Every
// other begin, cancel and void is a plain row write (rowWriteEffect).
func drainBeginFreshEffect(p *effectPass, it intent) func(context.Context) settlement {
	return func(ctx context.Context) settlement {
		row, ok := p.World.Census.Rows[it.Key]
		if !ok {
			return settlement{Outcome: settledRefused, Cause: causeRedecided}
		}
		req := fenceRequest{Row: row.Info, Legs: legAttach | legPending}
		if p.Runtime == nil {
			return settlement{Outcome: settledRefused, Cause: fenceRouteUnknown}
		}
		if v := fenceDestructive(ctx, p.Runtime, req, p.World.Now); !v.Proceed {
			return settlement{Outcome: settledRefused, Cause: v.Reason}
		}
		return rowWriteEffect(p, it)(ctx)
	}
}
