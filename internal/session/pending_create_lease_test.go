package session

import (
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// bead is a small helper to build a session bead with the metadata keys the
// lease reads.
func leaseBead(status string, meta map[string]string) beads.Bead {
	return beads.Bead{
		ID:        "gcs-1",
		Status:    status,
		Metadata:  meta,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestStateConfirmsPendingStart(t *testing.T) {
	// The frozen pending-start state set: "", start-pending, creating, asleep,
	// drained confirm; everything else does not.
	confirm := map[State]bool{
		"":                     true,
		StateStartPending:      true,
		StateCreating:          true,
		StateAsleep:            true,
		StateDrained:           true,
		StateAwake:             false,
		StateActive:            false,
		StateDraining:          false,
		StateArchived:          false,
		StateQuarantined:       false,
		StateFailedCreate:      false,
		StateSuspended:         false,
		State("garbage-state"): false,
	}
	for st, want := range confirm {
		if got := StateConfirmsPendingStart(st); got != want {
			t.Errorf("StateConfirmsPendingStart(%q) = %v, want %v", st, got, want)
		}
	}
}

func TestSameIdentity(t *testing.T) {
	tests := []struct {
		name          string
		preparedToken string
		preparedGen   string
		currentToken  string
		currentGen    string
		want          bool
	}{
		{"vacuous true: prepared has neither", "", "", "anything", "anything", true},
		{"vacuous true: prepared has neither, current empty", "", "", "", "", true},
		{"token match", "tok-a", "", "tok-a", "9", true},
		{"token match despite generation drift", "tok-a", "1", "tok-a", "99", true},
		{"token mismatch", "tok-a", "", "tok-b", "", false},
		{"token authoritative: current missing token", "tok-a", "", "", "1", false},
		{"generation fallback match (no prepared token)", "", "5", "", "5", true},
		{"generation fallback mismatch", "", "5", "", "6", false},
		{"generation fallback: current missing gen", "", "5", "", "", false},
		{"whitespace-padded token match", " tok-a ", "", "tok-a", "", true},
		{"whitespace-padded gen match", "", " 5 ", "", "5", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepared := LeaseFromBead(leaseBead("open", map[string]string{
				"instance_token": tt.preparedToken,
				"generation":     tt.preparedGen,
			}))
			current := LeaseFromBead(leaseBead("open", map[string]string{
				"instance_token": tt.currentToken,
				"generation":     tt.currentGen,
			}))
			if got := prepared.SameIdentity(current); got != tt.want {
				t.Errorf("SameIdentity = %v, want %v", got, tt.want)
			}
		})
	}
}

// oldStillCurrent and oldCleanupAllowed reproduce the legacy boolean helpers
// verbatim so the parity of CommitVerdict is proven against the pre-refactor
// semantics.
func oldIdentityMatches(prepared, current beads.Bead) bool {
	preparedToken := trimSpace(prepared.Metadata["instance_token"])
	if preparedToken != "" {
		return trimSpace(current.Metadata["instance_token"]) == preparedToken
	}
	preparedGeneration := trimSpace(prepared.Metadata["generation"])
	if preparedGeneration == "" {
		return true
	}
	return trimSpace(current.Metadata["generation"]) == preparedGeneration
}

func oldClaim(b beads.Bead) bool {
	return trimSpace(b.Metadata["pending_create_claim"]) == "true"
}

func oldStillCurrent(prepared, current beads.Bead) bool {
	if trimSpace(current.Status) == "closed" {
		return false
	}
	if !oldIdentityMatches(prepared, current) {
		return false
	}
	currentState := State(trimSpace(current.Metadata["state"]))
	if currentState == StateAwake || currentState == StateActive {
		return true
	}
	if oldClaim(prepared) && !oldClaim(current) {
		return false
	}
	return oldConfirm(string(currentState))
}

func oldCleanupAllowed(prepared, current beads.Bead) bool {
	if trimSpace(current.Status) == "closed" {
		return true
	}
	if !oldIdentityMatches(prepared, current) {
		return true
	}
	currentState := State(trimSpace(current.Metadata["state"]))
	if oldClaim(prepared) && !oldClaim(current) {
		return currentState != StateAwake && currentState != StateActive
	}
	return !oldConfirm(string(currentState)) &&
		currentState != StateAwake &&
		currentState != StateActive
}

func oldConfirm(currentState string) bool {
	switch State(trimSpace(currentState)) {
	case "", StateStartPending, StateCreating, StateAsleep, State("drained"):
		return true
	}
	return false
}

func TestCommitVerdict_ParityWithLegacyBooleans(t *testing.T) {
	// This grid is exhaustive over the token identity dimension. The generation
	// fallback branch of SameIdentity (empty instance_token + non-empty
	// generation) is delegated to TestSameIdentity and the "#1542 generation
	// drift" row in TestCommitVerdict_NamedInvariantRows; the identity
	// projection is shared with CommitVerdict, so re-crossing generation here
	// would only balloon the grid without adding coverage.
	statuses := []string{"open", "closed", "in_progress"}
	states := []string{"", "start-pending", "creating", "asleep", "drained", "awake", "active", "draining", "archived", "quarantined", "garbage"}
	tokens := []string{"", "tok-a", "tok-b"}
	claims := []string{"", "true", "yes"}

	for _, pStatus := range statuses {
		for _, pState := range states {
			for _, pTok := range tokens {
				for _, pClaim := range claims {
					for _, cStatus := range statuses {
						for _, cState := range states {
							for _, cTok := range tokens {
								for _, cClaim := range claims {
									prepared := leaseBead(pStatus, map[string]string{
										"state": pState, "instance_token": pTok, "pending_create_claim": pClaim,
									})
									current := leaseBead(cStatus, map[string]string{
										"state": cState, "instance_token": cTok, "pending_create_claim": cClaim,
									})
									pl := LeaseFromBead(prepared)
									cl := LeaseFromBead(current)
									verdict := pl.CommitVerdict(cl)

									wantCommit := oldStillCurrent(prepared, current)
									wantCleanup := oldCleanupAllowed(prepared, current)

									if (verdict == LeaseCommit) != wantCommit {
										t.Fatalf("CommitVerdict commit mismatch: prepared{status=%q state=%q tok=%q claim=%q} current{status=%q state=%q tok=%q claim=%q}: verdict=%v wantCommit=%v",
											pStatus, pState, pTok, pClaim, cStatus, cState, cTok, cClaim, verdict, wantCommit)
									}
									if (verdict == LeaseDiscardStopRuntime) != wantCleanup {
										t.Fatalf("CommitVerdict cleanup mismatch: prepared{status=%q state=%q tok=%q claim=%q} current{status=%q state=%q tok=%q claim=%q}: verdict=%v wantCleanup=%v",
											pStatus, pState, pTok, pClaim, cStatus, cState, cTok, cClaim, verdict, wantCleanup)
									}
									// Exactly one verdict, and commit/cleanup are exact complements.
									if wantCommit == wantCleanup {
										t.Fatalf("legacy booleans not complementary at prepared{state=%q claim=%q tok=%q status=%q} current{state=%q claim=%q tok=%q status=%q}: commit=%v cleanup=%v",
											pState, pClaim, pTok, pStatus, cState, cClaim, cTok, cStatus, wantCommit, wantCleanup)
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestCommitVerdict_NamedInvariantRows(t *testing.T) {
	withState := func(state string, extra map[string]string) beads.Bead {
		m := map[string]string{"instance_token": "tok-a", "state": state}
		for k, v := range extra {
			m[k] = v
		}
		return leaseBead("open", m)
	}

	t.Run("#1542 commit-anyway on awake even with claim cleared", func(t *testing.T) {
		prepared := LeaseFromBead(withState("creating", map[string]string{"pending_create_claim": "true"}))
		current := LeaseFromBead(withState("awake", nil)) // claim cleared
		if v := prepared.CommitVerdict(current); v != LeaseCommit {
			t.Fatalf("want Commit, got %v", v)
		}
	})
	t.Run("#1542 generation drift with matching token commits", func(t *testing.T) {
		prepared := LeaseFromBead(leaseBead("open", map[string]string{"instance_token": "tok-a", "generation": "1", "state": "creating"}))
		current := LeaseFromBead(leaseBead("open", map[string]string{"instance_token": "tok-a", "generation": "99", "state": "creating"}))
		if v := prepared.CommitVerdict(current); v != LeaseCommit {
			t.Fatalf("want Commit, got %v", v)
		}
	})
	t.Run("#2073 claim-cleared-from-under-us discards + stops runtime", func(t *testing.T) {
		prepared := LeaseFromBead(withState("creating", map[string]string{"pending_create_claim": "true"}))
		current := LeaseFromBead(withState("creating", nil)) // claim cleared, not awake/active
		if v := prepared.CommitVerdict(current); v != LeaseDiscardStopRuntime {
			t.Fatalf("want DiscardStopRuntime, got %v", v)
		}
	})
	t.Run("closed current discards", func(t *testing.T) {
		prepared := LeaseFromBead(withState("creating", nil))
		current := LeaseFromBead(withState("creating", nil))
		current.Closed = true
		if v := prepared.CommitVerdict(current); v != LeaseDiscardStopRuntime {
			t.Fatalf("want DiscardStopRuntime, got %v", v)
		}
	})
}

func trimSpace(s string) string { return strings.TrimSpace(s) }
