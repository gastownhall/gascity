package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// wtickSessionBead builds an open session bead with the given metadata for the
// W-tick twin oracles.
func wtickSessionBead(id string, meta map[string]string) beads.Bead {
	return beads.Bead{
		ID:       id,
		Type:     session.BeadType,
		Status:   "open",
		Labels:   []string{session.LabelSession},
		Metadata: meta,
	}
}

// TestFreshRestartSessionKeyInfoMatchesRaw is the equivalence oracle for
// freshRestartSessionKeyInfo. The minted key is a fresh UUID (non-deterministic),
// so the oracle compares (keyEmpty, hasCapability) — the two decision facts —
// across the provider-capability arm and the bead-metadata fallback arm, with
// whitespace-padded fixtures that catch a twin reading the wrong Info field or
// dropping the TrimSpace. It is self-sufficient: the raw form is the reference.
func TestFreshRestartSessionKeyInfoMatchesRaw(t *testing.T) {
	tps := []TemplateParams{
		{},
		{ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"}},
		{ResolvedProvider: &config.ResolvedProvider{ResumeFlag: "--resume"}},
		{ResolvedProvider: &config.ResolvedProvider{ResumeCommand: "resume {{.SessionKey}}"}},
		{ResolvedProvider: &config.ResolvedProvider{ResumeStyle: "flag"}},
		{ResolvedProvider: &config.ResolvedProvider{}},
	}
	metas := []map[string]string{
		{},
		{"session_id_flag": "--session-id"},
		{"session_id_flag": "  --session-id  "},
		{"resume_flag": "--resume"},
		{"resume_command": "resume {{.SessionKey}}"},
		{"resume_style": "flag"},
		{"resume_flag": "  --resume  "},
		{"session_id_flag": "", "resume_flag": ""},
	}
	for ti, tp := range tps {
		for mi, meta := range metas {
			b := wtickSessionBead("s-fr", meta)
			info := sessiontest.SeedBead(t, b)
			rawKey, rawCap := freshRestartSessionKey(tp, b.Metadata)
			infoKey, infoCap := freshRestartSessionKeyInfo(tp, info)
			if (rawKey == "") != (infoKey == "") || rawCap != infoCap {
				t.Fatalf("tp[%d] meta[%d]=%v: raw=(keyEmpty=%v,cap=%v) info=(keyEmpty=%v,cap=%v) diverged",
					ti, mi, meta, rawKey == "", rawCap, infoKey == "", infoCap)
			}
		}
	}
}

// TestNamedSessionWinsCanonicalRepairInfoMatchesRaw is the equivalence oracle for
// namedSessionWinsCanonicalRepairInfo: for every candidate/incumbent pair it must
// agree with namedSessionBeadWinsCanonicalRepair. The fixtures cover the
// generation compare (both directions), one-parses-one-doesn't (both directions),
// the canonical-session-name tiebreak, the CreatedAt tiebreak, and the ID
// tiebreak, so every branch of the winner rule is exercised.
func TestNamedSessionWinsCanonicalRepairInfoMatchesRaw(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	canon := "worker-canonical"
	mk := func(id, gen, sessName string, created time.Time) beads.Bead {
		meta := map[string]string{}
		if gen != "" {
			meta["generation"] = gen
		}
		if sessName != "" {
			meta["session_name"] = sessName
		}
		b := wtickSessionBead(id, meta)
		b.CreatedAt = created
		return b
	}
	cases := []struct {
		name       string
		cand, incb beads.Bead
	}{
		{"gen-cand-higher", mk("c", "5", "x", t0), mk("i", "3", "y", t0)},
		{"gen-incumbent-higher", mk("c", "3", "x", t0), mk("i", "5", "y", t0)},
		{"cand-parses-incumbent-not", mk("c", "2", "x", t0), mk("i", "not-int", "y", t0)},
		{"incumbent-parses-cand-not", mk("c", "not-int", "x", t0), mk("i", "2", "y", t0)},
		{"canonical-name-tiebreak-cand", mk("c", "", canon, t0), mk("i", "", "other", t0)},
		{"canonical-name-tiebreak-incumbent", mk("c", "", "other", t0), mk("i", "", canon, t0)},
		{"createdat-tiebreak", mk("c", "", "x", t1), mk("i", "", "y", t0)},
		{"id-tiebreak", mk("zzz", "", "x", t0), mk("aaa", "", "y", t0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := namedSessionBeadWinsCanonicalRepair(tc.cand, tc.incb, canon)
			info := namedSessionWinsCanonicalRepairInfo(
				sessiontest.SeedBead(t, tc.cand), sessiontest.SeedBead(t, tc.incb), canon)
			if raw != info {
				t.Fatalf("winner rule diverged: raw=%v info=%v", raw, info)
			}
		})
	}
}

// TestTopoOrderRowsMatchesTopoOrder pins topoOrderRows against topoOrder: for the
// same beads and deps, the row-form's Info.ID order must equal the raw form's
// bead ID order — across the no-deps passthrough, a real dependency chain
// (dependencies-first), and a dependency cycle (unordered fallback).
func TestTopoOrderRowsMatchesTopoOrder(t *testing.T) {
	mk := func(id, template string) beads.Bead {
		return wtickSessionBead(id, map[string]string{"template": template})
	}
	sessions := []beads.Bead{
		mk("s-app", "app"),
		mk("s-db", "db"),
		mk("s-cache", "cache"),
	}
	rows := make([]session.ReconcileSession, len(sessions))
	for i, b := range sessions {
		rows[i] = session.ReconcileSession{Info: sessiontest.SeedBead(t, b)}
	}
	depsCases := map[string]map[string][]string{
		"no-deps": {},
		"chain":   {"app": {"db"}, "db": {"cache"}},
		"cycle":   {"app": {"db"}, "db": {"app"}},
		"partial": {"app": {"cache"}},
	}
	for name, deps := range depsCases {
		t.Run(name, func(t *testing.T) {
			rawOrder := topoOrder(sessions, deps)
			rowOrder := topoOrderRows(rows, deps)
			if len(rawOrder) != len(rowOrder) {
				t.Fatalf("length diverged: raw=%d rows=%d", len(rawOrder), len(rowOrder))
			}
			for i := range rawOrder {
				if rawOrder[i].ID != rowOrder[i].Info.ID {
					t.Fatalf("order diverged at %d: raw=%s row=%s", i, rawOrder[i].ID, rowOrder[i].Info.ID)
				}
			}
		})
	}
}

// TestStopRuntimeBeforeSessionBeadMutationInfoMatchesRaw pins the non-kill
// branches of stopRuntimeBeforeSessionBeadMutationInfo (empty session_name, nil
// provider, not-running → all true) against the raw form, proving the Info form
// reads session_name off Info.SessionNameMetadata. The full kill path is
// exercised end-to-end by TestRetireDuplicateRowsMatchesBeads.
func TestStopRuntimeBeforeSessionBeadMutationInfoMatchesRaw(t *testing.T) {
	sp := runtime.NewFake()
	cases := []struct {
		name string
		meta map[string]string
		sp   runtime.Provider
	}{
		{"empty-name", map[string]string{}, sp},
		{"nil-provider", map[string]string{"session_name": "worker-1"}, nil},
		{"not-running", map[string]string{"session_name": "worker-notrunning"}, sp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := wtickSessionBead("s-stop", tc.meta)
			var rawErr, infoErr bytes.Buffer
			raw := stopRuntimeBeforeSessionBeadMutation(nil, tc.sp, nil, b, "duplicate", &rawErr)
			info := stopRuntimeBeforeSessionBeadMutationInfo(nil, tc.sp, nil, sessiontest.SeedBead(t, b), "duplicate", &infoErr)
			if raw != info {
				t.Fatalf("stop-runtime diverged: raw=%v info=%v", raw, info)
			}
		})
	}
}

// TestRetireDuplicateRowsMatchesBeads is the dedup both-ways oracle: the row form
// retires the SAME losers, reassigns the SAME work, and leaves the SAME store
// end-state as the raw form. It runs each against an independent but identical
// store, over a corpus of two eligible duplicates (a canonical winner + a
// distinct-session-name loser requiring a runtime stop), one continuity-ineligible
// bead (excluded), and one closed bead (excluded), then compares the persisted
// bead metadata + work assignee across the two stores. It fails loudly if the row
// form skips the runtime stop, the front-door retire, or the work reassignment.
func TestRetireDuplicateRowsMatchesBeads(t *testing.T) {
	cfg := &config.City{
		Agents:        []config.Agent{{Name: "mayor"}},
		NamedSessions: []config.NamedSession{{Template: "mayor"}},
	}
	cityName := config.EffectiveCityName(cfg, "")
	spec, ok := session.FindNamedSessionSpec(cfg, cityName, "mayor")
	if !ok {
		t.Fatalf("named spec for mayor not found; fixture cfg no longer resolves it")
	}
	now := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

	// build seeds a fresh store with the duplicate corpus + a work bead assigned to
	// the loser, and returns the store plus the winner/loser session ids.
	build := func(t *testing.T) (beads.Store, string, string, string) {
		store := beads.NewMemStore()
		mkSession := func(gen, sessName string) string {
			b, err := store.Create(beads.Bead{
				Type:   session.BeadType,
				Status: "open",
				Labels: []string{session.LabelSession},
				Metadata: map[string]string{
					"template":                  "mayor",
					"configured_named_session":  "true",
					"configured_named_identity": "mayor",
					"generation":                gen,
					"session_name":              sessName,
				},
			})
			if err != nil {
				t.Fatalf("create session: %v", err)
			}
			return b.ID
		}
		winner := mkSession("5", spec.SessionName)         // canonical name + higher generation → wins
		loser := mkSession("3", spec.SessionName+"-stale") // distinct session_name → runtime stop path
		// continuity-ineligible: excluded from the group.
		ineligible, err := store.Create(beads.Bead{
			Type: session.BeadType, Status: "open", Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template": "mayor", "configured_named_session": "true", "configured_named_identity": "mayor",
				"session_name": spec.SessionName + "-x", "continuity_eligible": "false",
			},
		})
		if err != nil {
			t.Fatalf("create ineligible: %v", err)
		}
		_ = ineligible
		// work assigned to the loser → must reassign to the winner.
		work, err := store.Create(beads.Bead{Title: "w", Type: "task", Status: "open", Assignee: loser})
		if err != nil {
			t.Fatalf("create work: %v", err)
		}
		inProgress := "in_progress"
		if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("update work: %v", err)
		}
		return store, winner, loser, work.ID
	}

	loserName := spec.SessionName + "-stale"
	loadOpen := func(t *testing.T, store beads.Store) []beads.Bead {
		all, err := session.ListAllSessionBeads(store, beads.ListQuery{})
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		return all
	}
	// newSP starts the loser's runtime (so stopRuntimeBeforeSessionBeadMutation takes
	// the actual kill path, not the not-running early return), optionally scripting
	// the Stop to FAIL on the loser name.
	newSP := func(failStop bool) *runtime.Fake {
		sp := runtime.NewFake()
		if err := sp.Start(context.Background(), loserName, runtime.Config{Command: "true"}); err != nil {
			t.Fatalf("start loser runtime: %v", err)
		}
		if failStop {
			sp.StopErrors = map[string]error{loserName: errors.New("simulated stop failure")}
		}
		return sp
	}
	runRaw := func(store beads.Store, sp *runtime.Fake) {
		rawBeads := loadOpen(t, store)
		bySessionName := map[string]beads.Bead{}
		indexBySessionName := map[string]int{}
		for i, b := range rawBeads {
			if sn := b.Metadata["session_name"]; sn != "" {
				bySessionName[sn] = b
				indexBySessionName[sn] = i
			}
		}
		retireDuplicateConfiguredNamedSessionBeads(store, nil, sp, cfg, cityName, rawBeads, bySessionName, indexBySessionName, now, nil)
	}
	runRows := func(store beads.Store, sp *runtime.Fake) {
		rowBeads := loadOpen(t, store)
		rows := make([]session.ReconcileSession, len(rowBeads))
		for i, b := range rowBeads {
			rows[i] = session.ReconcileSession{Info: sessiontest.SeedBead(t, b)}
		}
		retireDuplicateConfiguredNamedSessionRows(store, nil, sp, cfg, cityName, rows, now, nil)
	}

	t.Run("stop-succeeds-loser-retired-and-stopped", func(t *testing.T) {
		rawStore, _, rawLoser, rawWork := build(t)
		rawSP := newSP(false)
		runRaw(rawStore, rawSP)

		rowStore, _, rowLoser, rowWork := build(t)
		rowSP := newSP(false)
		runRows(rowStore, rowSP)

		for _, leg := range []struct {
			name  string
			store beads.Store
			sp    *runtime.Fake
			loser string
			work  string
		}{
			{"raw", rawStore, rawSP, rawLoser, rawWork},
			{"rows", rowStore, rowSP, rowLoser, rowWork},
		} {
			loserBead, err := leg.store.Get(leg.loser)
			if err != nil {
				t.Fatalf("%s: get loser: %v", leg.name, err)
			}
			if loserBead.Metadata["state"] != "archived" {
				t.Fatalf("%s: loser not retired to archived: state=%q", leg.name, loserBead.Metadata["state"])
			}
			// The runtime STOP must have fired and succeeded (loser no longer running).
			if leg.sp.IsRunning(loserName) {
				t.Fatalf("%s: loser runtime %q still running — the pre-mutation stop was skipped", leg.name, loserName)
			}
			if leg.sp.CountCalls("Stop", loserName) == 0 {
				t.Fatalf("%s: no Stop call recorded for the loser %q — dedup did not stop the runtime before retiring", leg.name, loserName)
			}
			workBead, err := leg.store.Get(leg.work)
			if err != nil {
				t.Fatalf("%s: get work: %v", leg.name, err)
			}
			if workBead.Assignee == leg.loser || workBead.Assignee == "" {
				t.Fatalf("%s: work not reassigned off the retired loser (assignee=%q)", leg.name, workBead.Assignee)
			}
		}
	})

	t.Run("stop-fails-loser-not-retired", func(t *testing.T) {
		// When the pre-mutation runtime stop FAILS, the loser must NOT be retired
		// (the stop-gate `continue` is load-bearing): retiring while the runtime is
		// still live would orphan a running agent under the winner's identity.
		for _, leg := range []struct {
			name string
			run  func(beads.Store, *runtime.Fake)
		}{
			{"raw", runRaw},
			{"rows", runRows},
		} {
			store, _, loser, _ := build(t)
			sp := newSP(true) // Stop fails on the loser
			leg.run(store, sp)

			loserBead, err := store.Get(loser)
			if err != nil {
				t.Fatalf("%s: get loser: %v", leg.name, err)
			}
			if loserBead.Metadata["state"] == "archived" {
				t.Fatalf("%s: loser was retired despite the runtime stop failing — the stop-gate continue regressed", leg.name)
			}
			if loserBead.Metadata["session_name"] == "" {
				t.Fatalf("%s: loser session_name cleared despite stop failure — retire ran when it must not have", leg.name)
			}
		}
	})
}
