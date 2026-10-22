//go:build integration

package conversationlane_test

// X12's incapable-store arm (the OJ-8 design §10): an embedded-Dolt
// workspace provisioned without the op bead type, so the journal's Propose
// refuses (type_unregistered). auto must answer as off and degrade loudly,
// once per gate; require must refuse before any effect (DES-O0API I8, D12).

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestX12IncapableStore(t *testing.T) {
	t.Parallel() // after the load, which measures latency alone
	if x12Evidence() && x12Backend(t) != armDolt {
		t.Skip("the incapable arm is an embedded-Dolt workspace: the dolt variant runs it")
	}
	baselines := x12Baselines(t)
	per := 1
	verbs := []string{"submit", "stop", "cli-nudge-wait"}
	if x12Evidence() {
		per, verbs = 5, []string{"submit", "stop", "cli-nudge-wait", "suspend"}
	}
	for _, mode := range x12Modes {
		t.Run(mode, func(t *testing.T) {
			if x12Evidence() {
				t.Parallel()
			} else {
				parallelCity(t)
			}
			runX12Incapable(t, mode, verbs, per, baselines)
		})
	}
}

func runX12Incapable(t *testing.T, mode string, verbs []string, per int, baselines map[string]x12Shape) {
	own := 0
	for _, name := range verbs {
		if verbByName(name).own {
			own += per
		}
	}
	arm := x12ArmRecord{Backend: string(armDolt), Mode: mode}
	fail := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		arm.Problems = append(arm.Problems, msg)
		t.Errorf("[I8] %s: %s", mode, msg)
	}
	defer func() {
		if t.Failed() && len(arm.Problems) == 0 {
			arm.Problems = append(arm.Problems, t.Name()+" failed outside every recorded check; its log says why")
		}
		x12Report.addArm(arm)
	}()
	c := newX12City(t, armDolt, mode, own, true)
	c.start("")
	degradesBefore := c.degradeEvents("propose", "type_unregistered")
	for _, name := range verbs {
		v := verbByName(name)
		for i := range per {
			var s x12Session
			if v.own {
				s = c.materialize(v.suspended)
			} else {
				s = c.worker()
			}
			off := c.offsets()
			key := fmt.Sprintf("x12-incapable-%s-%d", name, i)
			payload := fmt.Sprintf("x12 incapable %s %s %d alpha", mode, strings.ReplaceAll(name, "-", " "), i)
			a := c.send(v, s, key, payload, nil)
			arm.Requests++
			x12Report.eval("I8", 1)
			if mode == "require" {
				refused := (v.surface == surfaceAPI && a.status == 503 && strings.Contains(a.body, "op-journal-unavailable")) ||
					(v.surface == surfaceCLI && a.status == 1 && a.code == "op_journal_unavailable")
				if !refused {
					fail("%s answered %s, want the op_journal_unavailable refusal", name, a)
				}
				time.Sleep(settleWindow)
				if eff := effectCounts(c.invocations(), off.invocations, s.name); len(eff) != 0 || len(c.deliveries()) != off.deliveries {
					fail("%s refused in require still acted: %v, %d deliveries", name, eff, len(c.deliveries())-off.deliveries)
				}
				continue
			}
			// auto answers as off.
			if !a.ok() || a.opID != "" {
				fail("%s answered %s (op %q), want off's answer with no op", name, a, a.opID)
			}
			if v.async && !strings.HasPrefix(a.requestID, "req-") {
				fail("%s answered request_id %q, want off's req- id", name, a.requestID)
			}
			if v.surface == surfaceCLI && (strings.Count(a.stderr, "warning: op journal propose failed (type_unregistered)") != 1) {
				fail("%s's stderr carries no single propose type_unregistered warning: %s", name, a.stderr)
			}
			if v.async {
				if o := c.pollOutcome(a.requestID, a.cursor); o.state != "succeeded" {
					fail("%s outcome %+v, want succeeded", name, o)
				}
			}
			if v.message() {
				deadline := time.Now().Add(x12EffectWait)
				for len(deliveriesOf(c.deliveries()[off.deliveries:], payload)) == 0 && time.Now().Before(deadline) {
					time.Sleep(250 * time.Millisecond)
				}
				time.Sleep(settleWindow)
				got := deliveriesOf(c.deliveries()[off.deliveries:], payload)
				sh := baselines[v.shape]
				if len(got) != 1 || got[0].Text != sh.expected(payload) {
					fail("%s delivered %+v, want off's one %q", name, got, sh.expected(payload))
				}
			}
		}
	}
	if ops := c.allOps(); len(ops) != 0 {
		fail("%d op rows on a store that cannot record", len(ops))
	}
	if mode == "auto" {
		// One degrade per gate the API recorded through, within the latch.
		apiGates := 0
		for _, name := range verbs {
			if verbByName(name).surface == surfaceAPI {
				apiGates++
			}
		}
		if n := c.degradeEvents("propose", "type_unregistered") - degradesBefore; n != apiGates {
			fail("%d ops.recording_degraded{api, propose, type_unregistered} events for %d gates, want one each", n, apiGates)
		}
	}
}
