package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

// TestRenderConditionalWritesBlock pins the gc status text rendering of the
// §12.5 block: silent when off with nothing to say, one line per store with
// probe/latch detail only on incapable rows, notices flagged with "!".
func TestRenderConditionalWritesBlock(t *testing.T) {
	var sb strings.Builder
	renderConditionalWritesBlock(&sb, nil)
	renderConditionalWritesBlock(&sb, &api.StatusConditionalWrites{Mode: "off", Effective: "off"})
	if sb.Len() != 0 {
		t.Fatalf("nil/off block rendered %q, want silence", sb.String())
	}

	sb.Reset()
	renderConditionalWritesBlock(&sb, &api.StatusConditionalWrites{
		Mode: "require", Origin: "config", Effective: "fail_closed",
		Stores: []api.StatusConditionalWriteStoreVerdict{
			{StoreID: "city", Kind: "bd", Probe: "capable", Latch: "unlatched", Capable: true},
			{
				StoreID: "rig/gastown", Kind: "bd", Probe: "capable", Latch: "incapable", Capable: false,
				Reason: "conditional writes latched unsupported at runtime (bd rejected --if-revision)",
			},
		},
		Notices: []api.StatusRolloutNotice{{
			Kind: "pending_restart", FlagKey: "beads.conditional_writes",
			Message: "pending restart: conditional_writes auto (city.toml) != require (latched at start)",
		}},
	})
	out := sb.String()
	for _, want := range []string{
		"Conditional writes: require (origin=config, effective=fail_closed)",
		"city",
		"capable",
		"rig/gastown",
		"INCAPABLE (probe=capable latch=incapable)",
		"bd rejected --if-revision",
		"! pending restart:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered block missing %q:\n%s", want, out)
		}
	}

	// off with a live notice still surfaces the notice (an operator edited
	// the gate on a running city; the drift must not be invisible).
	sb.Reset()
	renderConditionalWritesBlock(&sb, &api.StatusConditionalWrites{
		Mode: "off", Origin: "builtin", Effective: "off",
		Notices: []api.StatusRolloutNotice{{Kind: "pending_restart", Message: "pending restart: conditional_writes require (city.toml) != off (latched at start)"}},
	})
	if !strings.Contains(sb.String(), "! pending restart") {
		t.Errorf("off-with-drift rendered %q, want the notice line", sb.String())
	}
}
