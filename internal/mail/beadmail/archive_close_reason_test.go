package beadmail

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// bdValidateCloseReason mirrors bd's ValidateCloseReason under
// validation.on-close: error (beads internal/validation/close_reason.go, not
// importable from this module): a trimmed reason that is empty or the default
// "closed" is refused, and so is one under 20 characters.
func bdValidateCloseReason(reason string) error {
	trimmed := strings.TrimSpace(reason)
	if trimmed == "" || strings.EqualFold(trimmed, "closed") {
		return errors.New("close reason is empty or default; provide a summary of what was done")
	}
	if n := len([]rune(trimmed)); n < 20 {
		return fmt.Errorf("close reason is terse (%d chars); provide a summary of what was done", n)
	}
	return nil
}

// onCloseValidatingStore mirrors a bd store configured with
// validation.on-close: error: BdStore.Close forwards metadata.close_reason as
// bd close --reason, and bd refuses a close whose reason fails validation.
type onCloseValidatingStore struct {
	*beads.MemStore
}

func (s onCloseValidatingStore) Close(id string) error {
	b, err := s.Get(id)
	if err != nil {
		return err
	}
	if err := bdValidateCloseReason(b.Metadata["close_reason"]); err != nil {
		return fmt.Errorf("closing bead %q: %w", id, err)
	}
	return s.MemStore.Close(id)
}

func assertArchivedWithReason(t *testing.T, store beads.Store, id string) {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	if b.Status != "closed" {
		t.Errorf("%s status = %q, want closed", id, b.Status)
	}
	if got := b.Metadata["close_reason"]; got != ArchiveCloseReason {
		t.Errorf("%s close_reason = %q, want %q", id, got, ArchiveCloseReason)
	}
}

// TestArchiveStampsCloseReasonForOnCloseValidation covers AC-1: Archive
// closes read and unread messages through a store that enforces bd's on-close
// validation, stamping ArchiveCloseReason first.
func TestArchiveStampsCloseReasonForOnCloseValidation(t *testing.T) {
	t.Run("Archive unread", func(t *testing.T) {
		store := onCloseValidatingStore{MemStore: beads.NewMemStore()}
		p := newTestProvider(store)
		m, err := p.Send("sender", "recipient", "archive me", "body")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := p.Archive(m.ID); err != nil {
			t.Fatalf("Archive(%s) = %v; want nil (close must carry a close_reason)", m.ID, err)
		}
		assertArchivedWithReason(t, store, m.ID)
	})

	t.Run("Archive read", func(t *testing.T) {
		store := onCloseValidatingStore{MemStore: beads.NewMemStore()}
		p := newTestProvider(store)
		m, err := p.Send("sender", "recipient", "archive me", "body")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := p.MarkRead(m.ID); err != nil {
			t.Fatalf("MarkRead: %v", err)
		}
		if err := p.Archive(m.ID); err != nil {
			t.Fatalf("Archive(%s) = %v; want nil (close must carry a close_reason)", m.ID, err)
		}
		assertArchivedWithReason(t, store, m.ID)
	})
}
