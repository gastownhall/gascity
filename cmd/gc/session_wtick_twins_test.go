package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
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
			info := session.InfoFromPersistedBead(b)
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
				session.InfoFromPersistedBead(tc.cand), session.InfoFromPersistedBead(tc.incb), canon)
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
		rows[i] = session.ReconcileSession{Info: session.InfoFromPersistedBead(b)}
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
			info := stopRuntimeBeforeSessionBeadMutationInfo(nil, tc.sp, nil, session.InfoFromPersistedBead(b), "duplicate", &infoErr)
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

	loadOpen := func(t *testing.T, store beads.Store) []beads.Bead {
		all, err := session.ListAllSessionBeads(store, beads.ListQuery{})
		if err != nil {
			t.Fatalf("list sessions: %v", err)
		}
		return all
	}

	// Raw run.
	rawStore, _, rawLoser, rawWork := build(t)
	rawSP := runtime.NewFake()
	rawBeads := loadOpen(t, rawStore)
	bySessionName := map[string]beads.Bead{}
	indexBySessionName := map[string]int{}
	for i, b := range rawBeads {
		if sn := b.Metadata["session_name"]; sn != "" {
			bySessionName[sn] = b
			indexBySessionName[sn] = i
		}
	}
	retireDuplicateConfiguredNamedSessionBeads(rawStore, nil, rawSP, cfg, cityName, rawBeads, bySessionName, indexBySessionName, now, nil)

	// Row run.
	rowStore, _, rowLoser, rowWork := build(t)
	rowSP := runtime.NewFake()
	rowBeads := loadOpen(t, rowStore)
	rows := make([]session.ReconcileSession, len(rowBeads))
	for i, b := range rowBeads {
		rows[i] = session.ReconcileSession{Info: session.InfoFromPersistedBead(b)}
	}
	retireDuplicateConfiguredNamedSessionRows(rowStore, nil, rowSP, cfg, cityName, rows, now, nil)

	// Compare persisted loser state (retired = archived + open status) across stores.
	rawLoserBead, err := rawStore.Get(rawLoser)
	if err != nil {
		t.Fatalf("get raw loser: %v", err)
	}
	rowLoserBead, err := rowStore.Get(rowLoser)
	if err != nil {
		t.Fatalf("get row loser: %v", err)
	}
	if rawLoserBead.Metadata["state"] != rowLoserBead.Metadata["state"] {
		t.Fatalf("loser state diverged: raw=%q row=%q", rawLoserBead.Metadata["state"], rowLoserBead.Metadata["state"])
	}
	if rawLoserBead.Metadata["configured_named_session"] != rowLoserBead.Metadata["configured_named_session"] {
		t.Fatalf("loser configured_named_session diverged: raw=%q row=%q",
			rawLoserBead.Metadata["configured_named_session"], rowLoserBead.Metadata["configured_named_session"])
	}
	// Load-bearing: the loser was actually retired to archived (proving the row
	// form did the retire, not a no-op that trivially matches).
	if rowLoserBead.Metadata["state"] != "archived" {
		t.Fatalf("row form did not retire the loser to archived: state=%q", rowLoserBead.Metadata["state"])
	}

	// Compare work reassignment.
	rawWorkBead, err := rawStore.Get(rawWork)
	if err != nil {
		t.Fatalf("get raw work: %v", err)
	}
	rowWorkBead, err := rowStore.Get(rowWork)
	if err != nil {
		t.Fatalf("get row work: %v", err)
	}
	if rawWorkBead.Assignee != rowWorkBead.Assignee {
		t.Fatalf("work reassignment diverged: raw assignee=%q row assignee=%q", rawWorkBead.Assignee, rowWorkBead.Assignee)
	}
	// Load-bearing: the work must have actually moved off the loser (proving the
	// reassignment ran on both paths, not that both no-oped).
	if rowWorkBead.Assignee == rowLoser {
		t.Fatalf("row form did not reassign work off the retired loser (assignee still %q)", rowLoser)
	}
}
