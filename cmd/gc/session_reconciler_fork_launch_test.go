package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// forkClaude is a resolved provider with full fork support, mirroring the
// claude builtin profile (--resume / --fork-session / --session-id).
func forkClaude() *config.ResolvedProvider {
	return &config.ResolvedProvider{
		Name:          "claude",
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		ForkFlag:      "--fork-session",
		SessionIDFlag: "--session-id",
	}
}

// TestResolveSessionCommand_ForkLaunch pins the command form emitted by the
// resolver across the fork / fresh / resume precedence on first and later wakes.
func TestResolveSessionCommand_ForkLaunch(t *testing.T) {
	tests := []struct {
		name       string
		parentSID  string
		rp         *config.ResolvedProvider
		firstStart bool
		forceFresh bool
		want       string
	}{
		{
			name:       "first start with parent forks off the brain",
			parentSID:  "brain-abc",
			rp:         forkClaude(),
			firstStart: true,
			want:       "claude --resume brain-abc --fork-session --session-id gc-key",
		},
		{
			name:       "no parent on first start is the unchanged fresh form",
			parentSID:  "",
			rp:         forkClaude(),
			firstStart: true,
			want:       "claude --session-id gc-key",
		},
		{
			name:       "later wake of a forked session resumes the child, not re-fork",
			parentSID:  "brain-abc",
			rp:         forkClaude(),
			firstStart: false,
			want:       "claude --resume gc-key",
		},
		{
			name:       "fresh wake mints a new conversation even with a parent",
			parentSID:  "brain-abc",
			rp:         forkClaude(),
			firstStart: false,
			forceFresh: true,
			want:       "claude --session-id gc-key",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSessionCommand("claude", "gc-key", tc.parentSID, tc.rp, tc.firstStart, tc.forceFresh)
			if got != tc.want {
				t.Errorf("resolveSessionCommand = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestValidateForkLaunch covers the four fail-loud paths and the no-op cases.
// Every failure must error, never silently allow a fresh downgrade.
func TestValidateForkLaunch(t *testing.T) {
	tests := []struct {
		name        string
		parentSID   string
		rp          *config.ResolvedProvider
		firstStart  bool
		forceFresh  bool
		parentStale bool
		wantErr     bool
		errContains string
	}{
		{
			name:       "no parent sid is not a fork launch",
			parentSID:  "",
			rp:         forkClaude(),
			firstStart: true,
		},
		{
			name:       "supported provider on first start passes",
			parentSID:  "brain-abc",
			rp:         forkClaude(),
			firstStart: true,
		},
		{
			name:        "unsupported provider fails loud",
			parentSID:   "brain-abc",
			rp:          &config.ResolvedProvider{Name: "codex", ResumeFlag: "resume"},
			firstStart:  true,
			wantErr:     true,
			errContains: "fork_flag",
		},
		{
			name:        "wake_mode fresh with a parent fails loud (Q2)",
			parentSID:   "brain-abc",
			rp:          forkClaude(),
			firstStart:  false,
			forceFresh:  true,
			wantErr:     true,
			errContains: "wake_mode=fresh",
		},
		{
			name:        "wake_mode fresh trips even on first start (Q2 hard guard)",
			parentSID:   "brain-abc",
			rp:          forkClaude(),
			firstStart:  true,
			forceFresh:  true,
			wantErr:     true,
			errContains: "wake_mode=fresh",
		},
		{
			name:        "stale parent brain fails loud, no fresh fallback",
			parentSID:   "brain-gone",
			rp:          forkClaude(),
			firstStart:  true,
			parentStale: true,
			wantErr:     true,
			errContains: "missing on disk",
		},
		{
			name:       "later wake with parent is a no-op (resumes the child)",
			parentSID:  "brain-abc",
			rp:         forkClaude(),
			firstStart: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateForkLaunch(tc.parentSID, tc.rp, tc.firstStart, tc.forceFresh, tc.parentStale)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateForkLaunch = nil, want error")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateForkLaunch = %v, want nil", err)
			}
		})
	}
}

// TestUnsupportedProviderErrorNamesProvider asserts the unsupported-provider
// error is actionable: it names the provider and both missing flags.
func TestUnsupportedProviderErrorNamesProvider(t *testing.T) {
	err := validateForkLaunch("brain-abc", &config.ResolvedProvider{Name: "codex"}, true, false, false)
	if err == nil {
		t.Fatal("expected error for fork on unsupported provider")
	}
	for _, want := range []string{"codex", "fork_flag", "session_id_flag", "brain-abc"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestPoolTriggerMetadata_StampsParentSID pins the work->session propagation:
// a request carrying a brain parent sid yields session-bead metadata with the
// gc.brain_parent_sid key, the value the launch path forks off of.
func TestPoolTriggerMetadata_StampsParentSID(t *testing.T) {
	req := SessionRequest{WorkBeadID: "wb-1", BrainParentSID: "brain-abc"}
	md := poolTriggerMetadata(nil, nil, "city/claude", req)
	if got := md[beadmeta.BrainParentSIDMetadataKey]; got != "brain-abc" {
		t.Errorf("%s = %q, want brain-abc", beadmeta.BrainParentSIDMetadataKey, got)
	}

	// No parent sid means no key — the fresh path is byte-for-byte unchanged.
	plain := poolTriggerMetadata(nil, nil, "city/claude", SessionRequest{WorkBeadID: "wb-1"})
	if _, ok := plain[beadmeta.BrainParentSIDMetadataKey]; ok {
		t.Errorf("plain request stamped %s, want absent", beadmeta.BrainParentSIDMetadataKey)
	}
}

// TestBindPoolSessionTriggerBead_ClearsParentOnReassign pins Q1: a re-pointed
// session must not silently inherit the prior fork's "warm" provenance.
func TestBindPoolSessionTriggerBead_ClearsParentOnReassign(t *testing.T) {
	t.Run("unassign clears the parent sid", func(t *testing.T) {
		session := beads.Bead{ID: "sess-1", Metadata: map[string]string{
			beadmeta.TriggerBeadIDMetadataKey:  "wb-A",
			beadmeta.BrainParentSIDMetadataKey: "brain-A",
		}}
		bound, err := bindPoolSessionTriggerBead(nil, nil, "city/claude", session, SessionRequest{WorkBeadID: ""})
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		if got := bound.Metadata[beadmeta.BrainParentSIDMetadataKey]; got != "" {
			t.Errorf("%s = %q, want cleared", beadmeta.BrainParentSIDMetadataKey, got)
		}
	})

	t.Run("reassign to different work without a parent clears it", func(t *testing.T) {
		session := beads.Bead{ID: "sess-1", Metadata: map[string]string{
			beadmeta.TriggerBeadIDMetadataKey:  "wb-A",
			beadmeta.BrainParentSIDMetadataKey: "brain-A",
		}}
		bound, err := bindPoolSessionTriggerBead(nil, nil, "city/claude", session, SessionRequest{WorkBeadID: "wb-B"})
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		if got := bound.Metadata[beadmeta.BrainParentSIDMetadataKey]; got != "" {
			t.Errorf("%s = %q, want cleared on reassign to non-warm work", beadmeta.BrainParentSIDMetadataKey, got)
		}
	})

	t.Run("reassign to different warm work re-stamps the new parent", func(t *testing.T) {
		session := beads.Bead{ID: "sess-1", Metadata: map[string]string{
			beadmeta.TriggerBeadIDMetadataKey:  "wb-A",
			beadmeta.BrainParentSIDMetadataKey: "brain-A",
		}}
		bound, err := bindPoolSessionTriggerBead(nil, nil, "city/claude", session, SessionRequest{WorkBeadID: "wb-B", BrainParentSID: "brain-B"})
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		if got := bound.Metadata[beadmeta.BrainParentSIDMetadataKey]; got != "brain-B" {
			t.Errorf("%s = %q, want brain-B", beadmeta.BrainParentSIDMetadataKey, got)
		}
	})

	t.Run("same work bead preserves the parent sid", func(t *testing.T) {
		session := beads.Bead{ID: "sess-1", Metadata: map[string]string{
			beadmeta.TriggerBeadIDMetadataKey:  "wb-A",
			beadmeta.BrainParentSIDMetadataKey: "brain-A",
		}}
		bound, err := bindPoolSessionTriggerBead(nil, nil, "city/claude", session, SessionRequest{WorkBeadID: "wb-A", BrainParentSID: "brain-A"})
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		if got := bound.Metadata[beadmeta.BrainParentSIDMetadataKey]; got != "brain-A" {
			t.Errorf("%s = %q, want brain-A preserved", beadmeta.BrainParentSIDMetadataKey, got)
		}
	})
}
