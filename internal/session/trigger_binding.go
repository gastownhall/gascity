package session

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TriggerBinding is the complete work provenance assigned to a reusable live
// session. Optional fields are cleared when empty; WorkID and StoreRef are
// required. WorkDir is written to both the canonical and compatibility keys.
type TriggerBinding struct {
	WorkID         string
	StoreRef       string
	BrainParentSID string
	Pack           string
	Workspace      string
	WorkDir        string
}

// Matches reports whether info already carries the complete normalized
// binding. It reads only the typed session projection.
func (b TriggerBinding) Matches(info Info) bool {
	b = b.normalized()
	return strings.TrimSpace(info.TriggerBeadID) == b.WorkID &&
		strings.TrimSpace(info.TriggerBeadStoreRef) == b.StoreRef &&
		strings.TrimSpace(info.BrainParentSID) == b.BrainParentSID &&
		strings.TrimSpace(info.Pack) == b.Pack &&
		strings.TrimSpace(info.PackWorkspace) == b.Workspace &&
		strings.TrimSpace(info.WorkDirCanonical) == b.WorkDir &&
		strings.TrimSpace(info.WorkDir) == b.WorkDir
}

// RebindTriggerIfMatch atomically replaces the complete trigger provenance
// cluster only when persisted.Revision still names info's exact persisted row.
// It never falls back to an unconditional write. On any error it returns info
// unchanged; an already-identical binding is a write-free no-op.
//
// It also returns the metadata patch the write committed — empty for the no-op
// replay — so the caller can name the exact durable image it just produced. The
// landed REVISION is deliberately not returned: UpdateIfMatch does not report
// the token the store minted, and the revision contract on
// beads.ConditionalWriter forbids deriving one ("callers may test it only for
// equality; arithmetic, ordering across beads, and gap inference are
// undefined"). A caller that needs the post-write revision re-reads the row and
// carries whatever token it finds (ga-f7v2ft.144).
func (s *Store) RebindTriggerIfMatch(info Info, persisted PersistedResponse, binding TriggerBinding) (Info, MetadataPatch, error) {
	if s == nil || s.store.Store == nil {
		return info, nil, fmt.Errorf("rebinding trigger for %q: session store is unavailable", info.ID)
	}
	binding = binding.normalized()
	if err := binding.validate(); err != nil {
		return info, nil, fmt.Errorf("rebinding trigger for %q: %w", info.ID, err)
	}
	if strings.TrimSpace(info.ID) == "" || strings.TrimSpace(info.ID) != info.ID {
		return info, nil, fmt.Errorf("rebinding trigger: session ID %q is not canonical", info.ID)
	}
	// beads.RevisionKnown, not a sign test: a revision is opaque and only zero
	// means "unavailable". Gating on `> 0` refused the rebind outright on the
	// negative half of every city's bd rows (ga-f7v2ft.141).
	if info.Closed || persisted.Status != "open" || !beads.RevisionKnown(persisted.Revision) {
		return info, nil, fmt.Errorf("rebinding trigger for %q: persisted session is not an open revisioned row", info.ID)
	}
	if !triggerPreimageMatchesPersisted(info, persisted.Metadata) {
		return info, nil, fmt.Errorf("rebinding trigger for %q: Info and persisted response are not the same preimage", info.ID)
	}
	patch := binding.patch(info)
	if len(patch) == 0 {
		return info, MetadataPatch{}, nil
	}
	writer, _, err := beads.ResolveConditionalWriter(s.store)
	if err != nil {
		return info, nil, fmt.Errorf("rebinding trigger for %q: resolving conditional writer: %w", info.ID, err)
	}
	if writer == nil {
		return info, nil, fmt.Errorf("rebinding trigger for %q: %w", info.ID, beads.ErrConditionalWriteUnsupported)
	}
	if err := writer.UpdateIfMatch(info.ID, persisted.Revision, beads.UpdateOpts{Metadata: map[string]string(patch)}); err != nil {
		return info, nil, fmt.Errorf("rebinding trigger for %q: %w", info.ID, err)
	}
	return info.ApplyPatch(patch), patch, nil
}

func (b TriggerBinding) normalized() TriggerBinding {
	b.WorkID = strings.TrimSpace(b.WorkID)
	b.StoreRef = strings.TrimSpace(b.StoreRef)
	b.BrainParentSID = strings.TrimSpace(b.BrainParentSID)
	b.Pack = strings.TrimSpace(b.Pack)
	b.Workspace = strings.TrimSpace(b.Workspace)
	b.WorkDir = strings.TrimSpace(b.WorkDir)
	return b
}

func (b TriggerBinding) validate() error {
	if b.WorkID == "" {
		return fmt.Errorf("work ID is required")
	}
	if b.StoreRef == "" {
		return fmt.Errorf("work store reference is required")
	}
	if b.WorkDir == "" {
		return fmt.Errorf("work directory is required")
	}
	return nil
}

func (b TriggerBinding) patch(info Info) MetadataPatch {
	patch := MetadataPatch{}
	for _, field := range []struct {
		key     string
		current string
		next    string
	}{
		{beadmeta.TriggerBeadIDMetadataKey, info.TriggerBeadID, b.WorkID},
		{beadmeta.TriggerBeadStoreRefMetadataKey, info.TriggerBeadStoreRef, b.StoreRef},
		{beadmeta.BrainParentSIDMetadataKey, info.BrainParentSID, b.BrainParentSID},
		{beadmeta.PackMetadataKey, info.Pack, b.Pack},
		{beadmeta.PackWorkspaceMetadataKey, info.PackWorkspace, b.Workspace},
		{beadmeta.WorkDirMetadataKey, info.WorkDirCanonical, b.WorkDir},
		{beadmeta.LegacyWorkDirMetadataKey, info.WorkDir, b.WorkDir},
	} {
		if strings.TrimSpace(field.current) != field.next {
			patch[field.key] = field.next
		}
	}
	return patch
}

func triggerPreimageMatchesPersisted(info Info, metadata map[string]string) bool {
	for _, field := range []struct {
		key   string
		value string
	}{
		{beadmeta.TriggerBeadIDMetadataKey, info.TriggerBeadID},
		{beadmeta.TriggerBeadStoreRefMetadataKey, info.TriggerBeadStoreRef},
		{beadmeta.BrainParentSIDMetadataKey, info.BrainParentSID},
		{beadmeta.PackMetadataKey, info.Pack},
		{beadmeta.PackWorkspaceMetadataKey, info.PackWorkspace},
		{beadmeta.WorkDirMetadataKey, info.WorkDirCanonical},
		{beadmeta.LegacyWorkDirMetadataKey, info.WorkDir},
	} {
		if metadata[field.key] != field.value {
			return false
		}
	}
	return true
}
