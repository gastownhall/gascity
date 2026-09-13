package session

import (
	"strings"
	"testing"
)

// leaseInfo builds a session Info carrying just the fields the pending-create
// lease reads: closed (derived from status), raw state, identity tokens, and
// the pending_create_claim bool. It is the typed-fixture analog of the raw
// session bead the pre-migration lease was constructed from.
func leaseInfo(status, state, tok, gen, claim string) Info {
	return Info{
		Closed:             strings.TrimSpace(status) == "closed",
		MetadataState:      state,
		InstanceToken:      tok,
		Generation:         gen,
		PendingCreateClaim: strings.TrimSpace(claim) == "true",
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
			prepared := LeaseFromInfo(Info{InstanceToken: tt.preparedToken, Generation: tt.preparedGen})
			current := LeaseFromInfo(Info{InstanceToken: tt.currentToken, Generation: tt.currentGen})
			if got := prepared.SameIdentity(current); got != tt.want {
				t.Errorf("SameIdentity = %v, want %v", got, tt.want)
			}
		})
	}
}

// oldStillCurrent and oldCleanupAllowed reproduce the legacy boolean helpers
// verbatim (reading the same typed Info fields the pre-refactor asyncStart*
// helpers read) so the parity of CommitVerdict is proven against the
// pre-refactor semantics, not against itself.
func oldIdentityMatches(prepared, current Info) bool {
	preparedToken := strings.TrimSpace(prepared.InstanceToken)
	if preparedToken != "" {
		return strings.TrimSpace(current.InstanceToken) == preparedToken
	}
	preparedGeneration := strings.TrimSpace(prepared.Generation)
	if preparedGeneration == "" {
		return true
	}
	return strings.TrimSpace(current.Generation) == preparedGeneration
}

func oldClaim(i Info) bool { return i.PendingCreateClaim }

func oldStillCurrent(prepared, current Info) bool {
	if current.Closed {
		return false
	}
	if !oldIdentityMatches(prepared, current) {
		return false
	}
	currentState := State(strings.TrimSpace(current.MetadataState))
	if currentState == StateAwake || currentState == StateActive {
		return true
	}
	if oldClaim(prepared) && !oldClaim(current) {
		return false
	}
	return oldConfirm(string(currentState))
}

func oldCleanupAllowed(prepared, current Info) bool {
	if current.Closed {
		return true
	}
	if !oldIdentityMatches(prepared, current) {
		return true
	}
	currentState := State(strings.TrimSpace(current.MetadataState))
	if oldClaim(prepared) && !oldClaim(current) {
		return currentState != StateAwake && currentState != StateActive
	}
	return !oldConfirm(string(currentState)) &&
		currentState != StateAwake &&
		currentState != StateActive
}

func oldConfirm(currentState string) bool {
	switch State(strings.TrimSpace(currentState)) {
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
									prepared := leaseInfo(pStatus, pState, pTok, "", pClaim)
									current := leaseInfo(cStatus, cState, cTok, "", cClaim)
									pl := LeaseFromInfo(prepared)
									cl := LeaseFromInfo(current)
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
	t.Run("#1542 commit-anyway on awake even with claim cleared", func(t *testing.T) {
		prepared := LeaseFromInfo(Info{InstanceToken: "tok-a", MetadataState: "creating", PendingCreateClaim: true})
		current := LeaseFromInfo(Info{InstanceToken: "tok-a", MetadataState: "awake"}) // claim cleared
		if v := prepared.CommitVerdict(current); v != LeaseCommit {
			t.Fatalf("want Commit, got %v", v)
		}
	})
	t.Run("#1542 generation drift with matching token commits", func(t *testing.T) {
		prepared := LeaseFromInfo(Info{InstanceToken: "tok-a", Generation: "1", MetadataState: "creating"})
		current := LeaseFromInfo(Info{InstanceToken: "tok-a", Generation: "99", MetadataState: "creating"})
		if v := prepared.CommitVerdict(current); v != LeaseCommit {
			t.Fatalf("want Commit, got %v", v)
		}
	})
	t.Run("#2073 claim-cleared-from-under-us discards + stops runtime", func(t *testing.T) {
		prepared := LeaseFromInfo(Info{InstanceToken: "tok-a", MetadataState: "creating", PendingCreateClaim: true})
		current := LeaseFromInfo(Info{InstanceToken: "tok-a", MetadataState: "creating"}) // claim cleared, not awake/active
		if v := prepared.CommitVerdict(current); v != LeaseDiscardStopRuntime {
			t.Fatalf("want DiscardStopRuntime, got %v", v)
		}
	})
	t.Run("closed current discards", func(t *testing.T) {
		prepared := LeaseFromInfo(Info{InstanceToken: "tok-a", MetadataState: "creating"})
		current := LeaseFromInfo(Info{InstanceToken: "tok-a", MetadataState: "creating"})
		current.Closed = true
		if v := prepared.CommitVerdict(current); v != LeaseDiscardStopRuntime {
			t.Fatalf("want DiscardStopRuntime, got %v", v)
		}
	})
}
