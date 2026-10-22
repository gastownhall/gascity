package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestExecutionBackstopReadsTheWorkStartedCell (ga-jfzcmp, ga-fsseyx,
// maintainer-city 2026-10-20). Kills a backstop whose only started evidence is
// its own stamp or a step_started event: an adopt-pr-review seat thinks for
// many minutes with no pane output and emits neither, so with no nudge
// configured it was drained at 0/3 the moment its grace ran out, though its
// hook had recorded the claim in work_started at the row's generation. That
// entry is started evidence: the seat is stamped, gets the held-claim nudge,
// and is held as executing, never drained. An entry that does not prove this
// incarnation started this claim, or a cell that cannot be read, is no
// evidence, and the seat drains as before.
func TestExecutionBackstopReadsTheWorkStartedCell(t *testing.T) {
	const (
		token    = "incarnation-2"
		storeRef = "rig:gascity"
	)
	at := time.Date(2026, 10, 20, 0, 5, 0, 0, time.UTC).Format(time.RFC3339)
	cell := func(store, bead string, gen int) string {
		return fmt.Sprintf(`{"v":1,"claims":[{"store":%q,"bead":%q,"gen":%d,"at":%q,"via":"claim"}]}`, store, bead, gen, at)
	}
	resident := func(store, bead string, gen int) string {
		return fmt.Sprintf(`{"v":1,"claims":[{"store":%q,"bead":%q,"gen":%d,"at":%q,"via":"claim","resident":true}]}`, store, bead, gen, at)
	}
	for _, tc := range []struct {
		name    string
		gen     string // the row's generation
		cell    func(bead string) string
		started bool
		// notGraph strips the claim's graph-step stamps, so it is no
		// lifecycle candidate and no step_started of it could ever exist.
		notGraph bool
		// census is the claim's AssignedWorkStoreRef; "" is the gascity rig's
		// bare name. bindingCensusRef is a graph step resident in the
		// relocated binding, which the snapshot keys city (claimStoreRef).
		census string
	}{
		{name: "an entry at the row's generation", gen: "2", cell: func(b string) string { return cell(storeRef, b, 2) }, started: true},
		{name: "an entry the hook could not resolve a store for", gen: "2", cell: func(b string) string { return cell("", b, 2) }, started: true},
		{name: "an entry behind another claim at the row's generation", gen: "2", started: true, cell: func(b string) string {
			return fmt.Sprintf(`{"v":1,"claims":[{"store":%q,"bead":"ga-other","gen":2,"at":%q,"via":"claim"},{"store":%q,"bead":%q,"gen":2,"at":%q,"via":"adopt"}]}`, storeRef, at, storeRef, b, at)
		}},
		{name: "an entry for a claim no step_started could name", gen: "2", cell: func(b string) string { return cell(storeRef, b, 2) }, started: true, notGraph: true},
		{name: "an entry from an older generation", gen: "2", cell: func(b string) string { return cell(storeRef, b, 1) }},
		// The claim and the row's generation each appear, but in different
		// entries: no one entry proves this incarnation started this claim.
		{name: "an older entry for the claim behind a current one for another", gen: "2", cell: func(b string) string {
			return fmt.Sprintf(`{"v":1,"claims":[{"store":%q,"bead":"ga-other","gen":2,"at":%q,"via":"claim"},{"store":%q,"bead":%q,"gen":1,"at":%q,"via":"claim"}]}`, storeRef, at, storeRef, b, at)
		}},
		// A resident entry's store is exact: another store is another claim.
		{name: "a resident entry in another store", gen: "2", cell: func(b string) string { return resident("rig:other", b, 2) }},
		// An unmarked entry is an older hook's, which spelled the claim by
		// the leg it ran in: at the row's generation it names the bead in any
		// store (ga-ylpbsy.41).
		{name: "an unmarked entry in another store at the row's generation", gen: "2", cell: func(b string) string { return cell("rig:other", b, 2) }, started: true},
		{name: "an unmarked entry in another store at an older generation", gen: "2", cell: func(b string) string { return cell("rig:other", b, 1) }},
		// The live 8d6r4 shape: a rig seat's graph step, resident in the
		// binding (claim keyed city), its entry spelled by the rig leg.
		{name: "a binding claim's leg spelling at the row's generation", gen: "2", census: bindingCensusRef, cell: func(b string) string { return cell("rig:gascity", b, 2) }, started: true},
		{name: "a binding claim's leg spelling at an older generation", gen: "2", census: bindingCensusRef, cell: func(b string) string { return cell("rig:gascity", b, 1) }},
		{name: "a binding claim's residence", gen: "2", census: bindingCensusRef, cell: func(b string) string { return resident("city", b, 2) }, started: true},
		{name: "a binding claim's residence at an older generation", gen: "2", census: bindingCensusRef, cell: func(b string) string { return resident("city", b, 1) }},
		{name: "a binding claim, a resident entry in the rig", gen: "2", census: bindingCensusRef, cell: func(b string) string { return resident("rig:gascity", b, 2) }},
		{name: "an entry for another bead", gen: "2", cell: func(string) string { return cell(storeRef, "ga-other", 2) }},
		{name: "an undecodable cell", gen: "2", cell: func(string) string { return `{"v":1,"claims":[` }},
		{name: "a cell of an unknown version", gen: "2", cell: func(b string) string { return strings.Replace(cell(storeRef, b, 2), `"v":1`, `"v":2`, 1) }},
		{name: "no row generation", gen: "", cell: func(b string) string { return cell(storeRef, b, 0) }},
		{name: "no cell", gen: "2", cell: func(string) string { return "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionBackstopFixture(t)
			f.cfg.Agents[0].Nudge = "" // the maintainer-city pool templates, verbatim
			f.workStoreRef = "gascity" // AssignedWorkStoreRefs' bare rig name
			if tc.census != "" {
				f.workStoreRef = tc.census
			}
			if tc.notGraph {
				if err := f.store.SetMetadataBatch(f.work.ID, map[string]string{
					beadmeta.RootBeadIDMetadataKey: "", beadmeta.StepIDMetadataKey: "",
					beadmeta.SessionIDMetadataKey: "", beadmeta.SessionNameMetadataKey: "",
				}); err != nil {
					t.Fatalf("stripping the claim's graph-step stamps: %v", err)
				}
			}
			f.setSessionMeta(t, map[string]string{
				"instance_token": token,
				"generation":     tc.gen,
				"last_woke_at":   f.now.Add(-20 * time.Minute).Format(time.RFC3339),
			})
			// work_started is CASOnly: seed it by a CAS, as the hook writes it.
			if raw := tc.cell(f.work.ID); raw != "" {
				if ok, err := f.store.(*beads.MemStore).CompareAndSetMetadataKey(f.session.ID, sessionpkg.WorkStartedKey, "", raw); err != nil || !ok {
					t.Fatalf("seeding %s: ok=%v err=%v", sessionpkg.WorkStartedKey, ok, err)
				}
			}

			f.idleFor(t, 10*time.Minute) // a silent max-effort turn
			f.tick(t)                    // first sighting: the grace clock starts
			f.now = f.now.Add(idleClaimNudgeGrace + time.Second)
			f.idleFor(t, 10*time.Minute)
			f.tick(t) // the grace has run

			if !tc.started {
				if len(f.drained) != 1 || f.sessionMeta(t, executionClaimStartedKey) != "" {
					t.Fatalf("no started evidence: drains %v, stamp %q; want one drain at 0/3 and no stamp, as before; stdout=%s",
						f.drained, f.sessionMeta(t, executionClaimStartedKey), f.stdout.String())
				}
				return
			}
			if got := f.sessionMeta(t, executionClaimStartedKey); got != token+"|"+f.work.ID {
				t.Fatalf("%s = %q, want the seat stamped started from its work_started entry", executionClaimStartedKey, got)
			}
			want := fmt.Sprintf(defaultHeldClaimNudge, f.work.ID)
			if got := f.nudgesDelivered(); len(got) != 1 || got[0] != want {
				t.Fatalf("nudges delivered = %q, want exactly the held-claim nudge [%q]; stdout=%s", got, want, f.stdout.String())
			}
			if len(f.drained) != 0 || f.stalledEvents() != 0 {
				t.Fatalf("drains %v, step_stalled %d; want neither for a seat whose hook recorded the claim started; stdout=%s",
					f.drained, f.stalledEvents(), f.stdout.String())
			}

			// Quiet through every remaining window: the nudges are spent and
			// the seat is held as executing, never drained.
			f.tickStale(t)
			if len(f.drained) != 0 || f.stalledEvents() != 0 {
				t.Fatalf("after the attempts were spent: drains %v, step_stalled %d; want neither", f.drained, f.stalledEvents())
			}
			if got := f.sessionMeta(t, executionClaimHoldKey); !strings.HasPrefix(got, executionHoldExecuting) {
				t.Fatalf("hold breadcrumb = %q, want %s", got, executionHoldExecuting)
			}
			// The hold is logged once as it is entered, not on every pass.
			line := fmt.Sprintf("execution-claim-nudge: %s holds started %s after %d/%d nudge attempts; holding as executing (no drain while work_ladder is off)\n",
				f.sessName, f.work.ID, idleClaimNudgeMaxAttempts, idleClaimNudgeMaxAttempts)
			if got := strings.Count(f.stdout.String(), line); got != 1 {
				t.Fatalf("executing-hold log lines = %d, want 1; stdout=%s", got, f.stdout.String())
			}
		})
	}
}
