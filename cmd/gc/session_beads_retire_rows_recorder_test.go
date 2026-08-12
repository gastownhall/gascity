package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestRetireDuplicateRows_RecordsTypedRetirementForShadow pins council finding 6:
// the typed duplicate-retire path (retireDuplicateConfiguredNamedSessionRows) must
// feed its canonical-identity clears to the S19 converge recorder, exactly as the
// raw sibling (retireDuplicateConfiguredNamedSessionBeads) does. RetireNamedSessionPatch
// clears the compared keys canonical_instance_name and canonical_pool_slot; without
// a recorder entry, a GC_CONVERGE_SHADOW soak sees that owned-key delta with no
// recorded write and false-classifies it as a foreign_write. The file-level
// write-site guard misses this because session_beads.go has other recorder calls,
// so this path-specific test is the real assertion.
func TestRetireDuplicateRows_RecordsTypedRetirementForShadow(t *testing.T) {
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

	store := beads.NewMemStore()
	mkSession := func(gen, sessName string, canonical string) string {
		b, err := store.Create(beads.Bead{
			Type:   session.BeadType,
			Status: "open",
			Labels: []string{session.LabelSession},
			Metadata: map[string]string{
				"template":                            "mayor",
				"configured_named_session":            "true",
				"configured_named_identity":           "mayor",
				"generation":                          gen,
				"session_name":                        sessName,
				session.CanonicalInstanceNameMetadata: canonical,
				session.CanonicalPoolSlotMetadata:     "1",
			},
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return b.ID
	}
	winner := mkSession("5", spec.SessionName, spec.SessionName)         // canonical name + higher generation → wins
	loser := mkSession("3", spec.SessionName, spec.SessionName+"-stale") // retired duplicate

	// Install a recorder directly (no env var needed: recordLegacyCompareWrites is
	// gated only on an attached recorder).
	rec := &legacyWriteRecorder{}
	convergeGlobalRecorder.Store(rec)
	t.Cleanup(func() { convergeGlobalRecorder.Store(nil) })

	rows := []session.ReconcileSession{
		{Info: sessiontest.SeedBead(t, mustGet(t, store, winner))},
		{Info: sessiontest.SeedBead(t, mustGet(t, store, loser))},
	}

	retireDuplicateConfiguredNamedSessionRows(store, nil, runtime.NewFake(), cfg, cityName, rows, now, nil)

	// The loser's typed retirement must have recorded the compared canonical-identity
	// clears; the winner must not have been retired.
	loserWrites := rec.forSession(loser)
	if len(loserWrites) == 0 {
		t.Fatalf("typed retirement recorded NO compared-key writes for the loser — shadow soak would see a foreign_write")
	}
	sawInstance, sawSlot := false, false
	for _, w := range loserWrites {
		switch w.key {
		case session.CanonicalInstanceNameMetadata:
			sawInstance = true
			if w.value != "" {
				t.Errorf("recorded %s = %q, want cleared", w.key, w.value)
			}
		case session.CanonicalPoolSlotMetadata:
			sawSlot = true
			if w.value != "" {
				t.Errorf("recorded %s = %q, want cleared", w.key, w.value)
			}
		}
	}
	if !sawInstance || !sawSlot {
		t.Errorf("recorder missing canonical clears (instance=%v slot=%v); writes=%#v", sawInstance, sawSlot, loserWrites)
	}
	if got := rec.forSession(winner); len(got) != 0 {
		t.Errorf("winner recorded %d writes, want 0 (winner is not retired)", len(got))
	}
}

func mustGet(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}
