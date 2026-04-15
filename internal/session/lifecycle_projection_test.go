package session

import (
	"testing"
	"time"
)

func TestProjectLifecycleNormalizesCompatibilityStates(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name      string
		metadata  map[string]string
		wantBase  BaseState
		wantState State
	}{
		{
			name: "legacy awake state behaves as active",
			metadata: map[string]string{
				"state":        "awake",
				"session_name": "s-worker",
			},
			wantBase:  BaseStateActive,
			wantState: StateActive,
		},
		{
			name: "stored drained state remains a distinct projected base state",
			metadata: map[string]string{
				"state":        "drained",
				"session_name": "s-worker",
			},
			wantBase:  BaseStateDrained,
			wantState: StateAsleep,
		},
		{
			name: "asleep with drained sleep reason projects as drained",
			metadata: map[string]string{
				"state":        "asleep",
				"sleep_reason": "drained",
				"session_name": "s-worker",
			},
			wantBase:  BaseStateDrained,
			wantState: StateAsleep,
		},
		{
			name: "closed bead status wins over stale active metadata",
			metadata: map[string]string{
				"state":        "active",
				"session_name": "s-worker",
			},
			wantBase:  BaseStateClosed,
			wantState: State("closed"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := "open"
			if tt.wantBase == BaseStateClosed {
				status = "closed"
			}
			view := ProjectLifecycle(LifecycleInput{
				Status:   status,
				Metadata: tt.metadata,
				Now:      now,
			})

			if view.BaseState != tt.wantBase {
				t.Fatalf("BaseState = %q, want %q", view.BaseState, tt.wantBase)
			}
			if view.CompatState != tt.wantState {
				t.Fatalf("CompatState = %q, want %q", view.CompatState, tt.wantState)
			}
		})
	}
}

func TestProjectLifecycleDesiredStateAndBlockers(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	future := now.Add(30 * time.Minute).Format(time.RFC3339)

	tests := []struct {
		name         string
		input        LifecycleInput
		wantDesired  DesiredState
		wantBlockers []LifecycleBlocker
		wantCauses   []WakeCause
	}{
		{
			name: "pending create claim is a one-shot wake cause",
			input: LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":                "creating",
					"session_name":         "s-worker",
					"pending_create_claim": "true",
				},
				Now: now,
			},
			wantDesired: DesiredStateRunning,
			wantCauses:  []WakeCause{WakeCausePendingCreate},
		},
		{
			name: "future hold blocks an otherwise runnable create claim",
			input: LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":                "creating",
					"session_name":         "s-worker",
					"pending_create_claim": "true",
					"held_until":           future,
				},
				Now: now,
			},
			wantDesired:  DesiredStateBlocked,
			wantBlockers: []LifecycleBlocker{BlockerHeld},
			wantCauses:   []WakeCause{WakeCausePendingCreate},
		},
		{
			name: "future quarantine blocks pin wake",
			input: LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":               "archived",
					"session_name":        "s-worker",
					"pin_awake":           "true",
					"quarantined_until":   future,
					"continuity_eligible": "true",
				},
				Now: now,
			},
			wantDesired:  DesiredStateBlocked,
			wantBlockers: []LifecycleBlocker{BlockerQuarantined},
			wantCauses:   []WakeCause{WakeCausePinned},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := ProjectLifecycle(tt.input)
			if view.DesiredState != tt.wantDesired {
				t.Fatalf("DesiredState = %q, want %q", view.DesiredState, tt.wantDesired)
			}
			for _, blocker := range tt.wantBlockers {
				if !view.HasBlocker(blocker) {
					t.Fatalf("HasBlocker(%q) = false, blockers = %v", blocker, view.Blockers)
				}
			}
			for _, cause := range tt.wantCauses {
				if !view.HasWakeCause(cause) {
					t.Fatalf("HasWakeCause(%q) = false, causes = %v", cause, view.WakeCauses)
				}
			}
		})
	}
}

func TestProjectLifecycleNamedIdentityProjection(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name         string
		input        LifecycleInput
		wantIdentity IdentityProjection
		wantDesired  DesiredState
		wantBlocker  LifecycleBlocker
	}{
		{
			name: "configured named identity without a bead is reserved but not desired",
			input: LifecycleInput{
				NamedIdentity: NamedIdentityInput{
					Identity:         "worker",
					Configured:       true,
					HasCanonicalBead: false,
				},
				Now: now,
			},
			wantIdentity: IdentityReservedUnmaterialized,
			wantDesired:  DesiredStateUndesired,
		},
		{
			name: "always named identity without a bead is desired running",
			input: LifecycleInput{
				NamedIdentity: NamedIdentityInput{
					Identity:         "worker",
					Configured:       true,
					HasCanonicalBead: false,
				},
				WakeCauses: []WakeCause{WakeCauseNamedAlways},
				Now:        now,
			},
			wantIdentity: IdentityReservedUnmaterialized,
			wantDesired:  DesiredStateRunning,
		},
		{
			name: "configured named conflict blocks materialization",
			input: LifecycleInput{
				NamedIdentity: NamedIdentityInput{
					Identity:         "worker",
					Configured:       true,
					HasCanonicalBead: false,
					Conflict:         true,
				},
				WakeCauses: []WakeCause{WakeCauseNamedAlways},
				Now:        now,
			},
			wantIdentity: IdentityConflict,
			wantDesired:  DesiredStateBlocked,
			wantBlocker:  BlockerIdentityConflict,
		},
		{
			name: "materialized continuity eligible named bead is canonical",
			input: LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":                     "asleep",
					"session_name":              "s-worker",
					"configured_named_identity": "worker",
					"continuity_eligible":       "true",
				},
				PreserveIdentity: true,
				Now:              now,
			},
			wantIdentity: IdentityCanonical,
			wantDesired:  DesiredStateAsleep,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := ProjectLifecycle(tt.input)
			if view.Identity != tt.wantIdentity {
				t.Fatalf("Identity = %q, want %q", view.Identity, tt.wantIdentity)
			}
			if view.DesiredState != tt.wantDesired {
				t.Fatalf("DesiredState = %q, want %q", view.DesiredState, tt.wantDesired)
			}
			if tt.wantBlocker != "" && !view.HasBlocker(tt.wantBlocker) {
				t.Fatalf("HasBlocker(%q) = false, blockers = %v", tt.wantBlocker, view.Blockers)
			}
		})
	}
}

func TestProjectLifecycleConflictIsBlockerOverlay(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		namedInput  NamedIdentityInput
		wantBlocker LifecycleBlocker
	}{
		{
			name: "canonical named bead with a live conflicting claimant",
			namedInput: NamedIdentityInput{
				Identity:         "worker",
				Configured:       true,
				HasCanonicalBead: true,
				Conflict:         true,
			},
			wantBlocker: BlockerIdentityConflict,
		},
		{
			name: "canonical named bead with duplicate open canonical bead",
			namedInput: NamedIdentityInput{
				Identity:           "worker",
				Configured:         true,
				HasCanonicalBead:   true,
				DuplicateCanonical: true,
			},
			wantBlocker: BlockerDuplicateCanonical,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := ProjectLifecycle(LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":                     "asleep",
					"session_name":              "s-worker",
					"configured_named_identity": "worker",
					"continuity_eligible":       "true",
				},
				NamedIdentity: tt.namedInput,
				WakeCauses:    []WakeCause{WakeCauseNamedAlways},
				Now:           now,
			})

			if view.Identity != IdentityCanonical {
				t.Fatalf("Identity = %q, want canonical ownership with blocker overlay", view.Identity)
			}
			if !view.HasBlocker(tt.wantBlocker) {
				t.Fatalf("HasBlocker(%q) = false, blockers = %v", tt.wantBlocker, view.Blockers)
			}
			if view.DesiredState != DesiredStateBlocked {
				t.Fatalf("DesiredState = %q, want blocked", view.DesiredState)
			}
		})
	}
}

func TestProjectLifecycleRuntimeLivenessProjection(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name                string
		input               LifecycleInput
		wantRuntime         RuntimeProjection
		wantReconciledState State
		wantReset           bool
	}{
		{
			name: "alive runtime heals advisory state to awake",
			input: LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":        "asleep",
					"session_name": "s-worker",
				},
				Runtime: RuntimeFacts{Observed: true, Alive: true},
				Now:     now,
			},
			wantRuntime:         RuntimeProjectionAlive,
			wantReconciledState: StateAwake,
		},
		{
			name: "dead active runtime heals to asleep and resets stale resume identity",
			input: LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":               "active",
					"session_name":        "s-worker",
					"session_key":         "old-provider-conversation",
					"started_config_hash": "old-config",
				},
				Runtime: RuntimeFacts{Observed: true, Alive: false},
				Now:     now,
			},
			wantRuntime:         RuntimeProjectionMissing,
			wantReconciledState: StateAsleep,
			wantReset:           true,
		},
		{
			name: "fresh creating state stays creating after restart",
			input: LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":        "creating",
					"session_name": "s-worker",
				},
				Runtime:            RuntimeFacts{Observed: true, Alive: false},
				CreatedAt:          now.Add(-30 * time.Second),
				StaleCreatingAfter: time.Minute,
				Now:                now,
			},
			wantRuntime:         RuntimeProjectionFreshCreating,
			wantReconciledState: StateCreating,
		},
		{
			name: "stale creating state heals to asleep and resets stale resume identity",
			input: LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":        "creating",
					"session_name": "s-worker",
					"session_key":  "old-provider-conversation",
				},
				Runtime:            RuntimeFacts{Observed: true, Alive: false},
				CreatedAt:          now.Add(-2 * time.Minute),
				StaleCreatingAfter: time.Minute,
				Now:                now,
			},
			wantRuntime:         RuntimeProjectionStaleCreating,
			wantReconciledState: StateAsleep,
			wantReset:           true,
		},
		{
			name: "pending create claim keeps stale creating state in creating",
			input: LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":                "creating",
					"session_name":         "s-worker",
					"pending_create_claim": "true",
				},
				Runtime:            RuntimeFacts{Observed: true, Alive: false},
				CreatedAt:          now.Add(-2 * time.Minute),
				StaleCreatingAfter: time.Minute,
				Now:                now,
			},
			wantRuntime:         RuntimeProjectionStartRequested,
			wantReconciledState: StateCreating,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := ProjectLifecycle(tt.input)
			if view.RuntimeProjection != tt.wantRuntime {
				t.Fatalf("RuntimeProjection = %q, want %q", view.RuntimeProjection, tt.wantRuntime)
			}
			if view.ReconciledState != tt.wantReconciledState {
				t.Fatalf("ReconciledState = %q, want %q", view.ReconciledState, tt.wantReconciledState)
			}
			if view.ResetContinuation != tt.wantReset {
				t.Fatalf("ResetContinuation = %v, want %v", view.ResetContinuation, tt.wantReset)
			}
		})
	}
}

func TestProjectLifecycleMissingConfigBlocksWake(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		input LifecycleInput
	}{
		{
			name: "orphaned continuity eligible named bead keeps identity but blocks wake",
			input: LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":                     "orphaned",
					"session_name":              "s-worker",
					"configured_named_identity": "worker",
					"continuity_eligible":       "true",
					"pin_awake":                 "true",
				},
				Now: now,
			},
		},
		{
			name: "known missing config blocks otherwise active materialized identity",
			input: LifecycleInput{
				Status: "open",
				Metadata: map[string]string{
					"state":        "asleep",
					"session_name": "s-worker",
					"pin_awake":    "true",
				},
				ConfigMissing: true,
				Now:           now,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			view := ProjectLifecycle(tt.input)
			if !view.HasBlocker(BlockerMissingConfig) {
				t.Fatalf("HasBlocker(%q) = false, blockers = %v", BlockerMissingConfig, view.Blockers)
			}
			if view.DesiredState != DesiredStateBlocked {
				t.Fatalf("DesiredState = %q, want blocked", view.DesiredState)
			}
		})
	}
}
