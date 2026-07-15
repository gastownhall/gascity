package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// TestWakeFairnessInfoTwinCharacterization is the #2574-class regression guard for
// WI-6 W5 (start-execution feed typing). The per-tick wake budget is spent
// least-recently-woken first (wakeFairnessTime → sortCandidatesByWakeFairness). A
// same-tick sleep→re-wake (max-age kill / idle kill) clears last_woke_at via
// SleepPatch BEFORE the startCandidate is appended, so the re-woken session must
// sort by its cleared-fallback key (CreatedAt), competing fairly instead of
// jumping the queue on a stale last_woke_at.
//
// This pins two things across the W5 A→B read cutover:
//  1. wakeFairnessTime returns the right key for every coupling-mirror scenario
//     (cleared last_woke_at → CreatedAt fallback; valid last_woke_at honored; both
//     empty → zero) and sorts accordingly.
//  2. The captured Info twin agrees with the raw pointer for that key — a fairness
//     time computed off session.Info equals wakeFairnessTime over the same bead.
//     In Commit A wakeFairnessTime still reads the raw pointer, so this catches a
//     twin projection drift; in Commit B it reads Info, so the scenario coverage in
//     (1) stays load-bearing.
func TestWakeFairnessInfoTwinCharacterization(t *testing.T) {
	base := time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)

	// infoFairnessTime mirrors wakeFairnessTime's rule off the typed twin: parse
	// last_woke_at, else fall back to CreatedAt, else zero. Kept local so the pin
	// stays honest even after wakeFairnessTime itself moves onto Info.
	infoFairnessTime := func(i session.Info) time.Time {
		if t, err := time.Parse(time.RFC3339, i.LastWokeAt); err == nil {
			return t
		}
		if !i.CreatedAt.IsZero() {
			return i.CreatedAt
		}
		return time.Time{}
	}

	// candidateFor builds a startCandidate the way the reconciler append site does:
	// the raw bead plus the coherent Info twin projected from it.
	candidateFor := func(bead beads.Bead) startCandidate {
		return startCandidate{info: sessiontest.SeedBead(t, bead)}
	}

	beadWithMeta := func(id string, created time.Time, meta map[string]string) beads.Bead {
		return beads.Bead{
			ID:        id,
			Type:      session.BeadType,
			Title:     "worker",
			Labels:    []string{session.LabelSession},
			CreatedAt: created,
			Metadata:  meta,
		}
	}

	// applySleep mirrors the max-age / idle-kill coupling: SleepPatch clears
	// last_woke_at onto the bead metadata, exactly what the reconciler folds before
	// the append (session_reconciler.go).
	applySleep := func(bead beads.Bead) beads.Bead {
		for k, v := range session.SleepPatch(base, "idle") {
			bead.Metadata[k] = v
		}
		return bead
	}

	valid := base.Add(-30 * time.Minute).Format(time.RFC3339)

	scenarios := []struct {
		name string
		bead beads.Bead
		want time.Time
	}{
		{
			name: "max-age-kill-clears-last-woke-at-falls-back-to-created",
			bead: applySleep(beadWithMeta("ga-maxage", base.Add(-2*time.Hour), map[string]string{
				"template": "worker", "last_woke_at": valid,
			})),
			want: base.Add(-2 * time.Hour),
		},
		{
			name: "idle-kill-clears-last-woke-at-falls-back-to-created",
			bead: applySleep(beadWithMeta("ga-idle", base.Add(-90*time.Minute), map[string]string{
				"template": "worker", "last_woke_at": valid, "sleep_reason": "idle",
			})),
			want: base.Add(-90 * time.Minute),
		},
		{
			name: "valid-last-woke-at-honored",
			bead: beadWithMeta("ga-valid", base.Add(-3*time.Hour), map[string]string{
				"template": "worker", "last_woke_at": valid,
			}),
			want: mustParseRFC3339(t, valid),
		},
		{
			name: "empty-last-woke-at-created-fallback",
			bead: beadWithMeta("ga-created", base.Add(-45*time.Minute), map[string]string{
				"template": "worker",
			}),
			want: base.Add(-45 * time.Minute),
		},
		{
			name: "both-empty-zero-time",
			bead: beadWithMeta("ga-zero", time.Time{}, map[string]string{
				"template": "worker",
			}),
			want: time.Time{},
		},
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			cand := candidateFor(sc.bead)
			got := wakeFairnessTime(cand)
			if !got.Equal(sc.want) {
				t.Errorf("wakeFairnessTime = %v, want %v", got, sc.want)
			}
			// Twin coherence: the Info-derived key equals the wakeFairnessTime output
			// (the coupling mirrors all fold onto the captured Info before append).
			if twin := infoFairnessTime(cand.info); !twin.Equal(got) {
				t.Errorf("info-derived fairness time %v != wakeFairnessTime %v (twin drift)", twin, got)
			}
		})
	}

	// Same-tick re-wake ordering (#2574): two sessions slept THIS tick (last_woke_at
	// cleared) sort by CreatedAt among themselves and ahead of one that still carries
	// a newer valid last_woke_at. sortCandidatesByWakeFairness must rotate the budget
	// onto the longest-waiting sessions rather than defer them on a stale key.
	oldSlept := candidateFor(applySleep(beadWithMeta("ga-old", base.Add(-3*time.Hour), map[string]string{
		"template": "worker", "last_woke_at": valid,
	})))
	newSlept := candidateFor(applySleep(beadWithMeta("ga-new", base.Add(-1*time.Hour), map[string]string{
		"template": "worker", "last_woke_at": valid,
	})))
	recentlyWoken := candidateFor(beadWithMeta("ga-recent", base.Add(-4*time.Hour), map[string]string{
		"template": "worker", "last_woke_at": base.Add(-10 * time.Minute).Format(time.RFC3339),
	}))

	cands := []startCandidate{recentlyWoken, newSlept, oldSlept}
	sortCandidatesByWakeFairness(cands)
	gotOrder := []string{cands[0].info.ID, cands[1].info.ID, cands[2].info.ID}
	wantOrder := []string{"ga-old", "ga-new", "ga-recent"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Fatalf("fairness sort order = %v, want %v", gotOrder, wantOrder)
		}
	}
}

func mustParseRFC3339(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing %q: %v", s, err)
	}
	return parsed
}
