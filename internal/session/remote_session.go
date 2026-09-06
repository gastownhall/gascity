package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/runtime"
)

// RemoteCheckpointMetadataKey is the one canonical session-bead metadata
// record for provider-native remote identity and reconciliation cursors.
const RemoteCheckpointMetadataKey = beadmeta.RemoteSessionCheckpointMetadataKey

// ErrRemoteCheckpointFence reports that a stale session incarnation attempted
// to replace the provider-native remote checkpoint.
var ErrRemoteCheckpointFence = errors.New("remote checkpoint ownership fence mismatch")

const (
	remoteCheckpointSchemaVersion = 1
	maxRemoteCheckpointBytes      = 64 * 1024
)

// RemoteCheckpoint is the durable, content-free projection of a remote
// provider session. Provider messages, prompts, transcripts, credentials,
// account data, and billing/quota amounts are deliberately absent.
type RemoteCheckpoint struct {
	Ref           runtime.RemoteSessionRef   `json:"ref"`
	Phase         runtime.RemoteSessionPhase `json:"phase"`
	Failure       runtime.RemoteFailureKind  `json:"failure,omitempty"`
	EventCursor   string                     `json:"event_cursor,omitempty"`
	ReceiptCursor string                     `json:"receipt_cursor,omitempty"`
	Handoff       []runtime.RemoteGitHandoff `json:"handoff,omitempty"`
	UpdatedAt     time.Time                  `json:"updated_at"`
}

type remoteCheckpointEnvelope struct {
	SchemaVersion int `json:"schema_version"`
	RemoteCheckpoint
}

// Validate checks the durable checkpoint using the runtime snapshot contract.
func (c RemoteCheckpoint) Validate() error {
	return (runtime.RemoteSessionSnapshot{
		Ref:       c.Ref,
		Phase:     c.Phase,
		Failure:   c.Failure,
		Handoff:   c.Handoff,
		UpdatedAt: c.UpdatedAt,
	}).Validate()
}

// PersistRemoteCheckpoint atomically replaces the single remote checkpoint
// only when expectedInstanceToken still owns this session incarnation.
func (m *Manager) PersistRemoteCheckpoint(id, expectedInstanceToken string, checkpoint RemoteCheckpoint) error {
	if strings.TrimSpace(expectedInstanceToken) == "" {
		return fmt.Errorf("persisting remote checkpoint: expected instance token is required")
	}
	if err := checkpoint.Validate(); err != nil {
		return fmt.Errorf("persisting remote checkpoint: %w", err)
	}
	raw, err := json.Marshal(remoteCheckpointEnvelope{
		SchemaVersion:    remoteCheckpointSchemaVersion,
		RemoteCheckpoint: checkpoint,
	})
	if err != nil {
		return fmt.Errorf("persisting remote checkpoint: encoding: %w", err)
	}
	if len(raw) > maxRemoteCheckpointBytes {
		return fmt.Errorf("persisting remote checkpoint: encoded record is %d bytes, limit is %d", len(raw), maxRemoteCheckpointBytes)
	}

	return withSessionMutationLock(id, func() error {
		bead, err := m.store.Get(id)
		if err != nil {
			return fmt.Errorf("persisting remote checkpoint: getting session: %w", err)
		}
		if !IsSessionBeadOrRepairable(bead) {
			return fmt.Errorf("persisting remote checkpoint: %w: bead %s (type=%q)", ErrNotSession, id, bead.Type)
		}
		if bead.Status == "closed" {
			return fmt.Errorf("persisting remote checkpoint: %w: %s", ErrSessionClosed, id)
		}
		if bead.Metadata["instance_token"] != expectedInstanceToken {
			return fmt.Errorf("%w: session %s", ErrRemoteCheckpointFence, id)
		}
		if err := m.store.SetMetadata(id, RemoteCheckpointMetadataKey, string(raw)); err != nil {
			return fmt.Errorf("persisting remote checkpoint: %w", err)
		}
		return nil
	})
}

// RemoteCheckpoint reads the canonical content-free remote checkpoint. An
// absent record returns ok=false. Unknown schema versions fail closed.
func (m *Manager) RemoteCheckpoint(id string) (checkpoint RemoteCheckpoint, ok bool, err error) {
	bead, err := m.store.Get(id)
	if err != nil {
		return RemoteCheckpoint{}, false, fmt.Errorf("reading remote checkpoint: getting session: %w", err)
	}
	if !IsSessionBeadOrRepairable(bead) {
		return RemoteCheckpoint{}, false, fmt.Errorf("reading remote checkpoint: %w: bead %s (type=%q)", ErrNotSession, id, bead.Type)
	}
	raw := bead.Metadata[RemoteCheckpointMetadataKey]
	if raw == "" {
		return RemoteCheckpoint{}, false, nil
	}
	if len(raw) > maxRemoteCheckpointBytes {
		return RemoteCheckpoint{}, false, fmt.Errorf("reading remote checkpoint: encoded record is %d bytes, limit is %d", len(raw), maxRemoteCheckpointBytes)
	}
	var envelope remoteCheckpointEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return RemoteCheckpoint{}, false, fmt.Errorf("reading remote checkpoint: decoding: %w", err)
	}
	if envelope.SchemaVersion != remoteCheckpointSchemaVersion {
		return RemoteCheckpoint{}, false, fmt.Errorf("reading remote checkpoint: unsupported schema_version %d", envelope.SchemaVersion)
	}
	if err := envelope.Validate(); err != nil {
		return RemoteCheckpoint{}, false, fmt.Errorf("reading remote checkpoint: %w", err)
	}
	return envelope.RemoteCheckpoint, true, nil
}
