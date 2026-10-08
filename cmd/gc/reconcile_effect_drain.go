package main

import (
	"context"
	"strings"

	"github.com/gastownhall/gascity/internal/telemetry"
)

// The drain effects (CONTRACT v5 D1, D2): a cancel or a void is the row
// write of its patch, re-decided inside the CAS (rowWriteEffect), and
// records legacy's drain transition once it lands. The begin's effect is
// C6a2's.

// drainClearEffect is a cancel's or a void's effect.
func drainClearEffect(p *effectPass, it intent) func(context.Context) settlement {
	return func(ctx context.Context) settlement {
		name := p.World.Census.Rows[it.Key].Info.SessionNameMetadata
		_, reason, _ := strings.Cut(it.Reason, ":")
		return drainTransition(ctx, rowWriteEffect(p, it)(ctx), name, reason, "cancel")
	}
}

// drainTransition records legacy's drain telemetry for a landed write.
func drainTransition(ctx context.Context, s settlement, name, reason, transition string) settlement {
	if s.Outcome == settledLanded {
		telemetry.RecordDrainTransition(context.WithoutCancel(ctx), name, reason, transition)
	}
	return s
}
