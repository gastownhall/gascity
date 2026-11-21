package beadmail

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
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

// TestArchiveStampsCloseReasonForOnCloseValidation covers AC-1 and AC-2:
// Archive and ArchiveMatching close read and unread messages through a store
// that enforces bd's on-close validation, stamping ArchiveCloseReason first.
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

	t.Run("ArchiveMatching read and unread", func(t *testing.T) {
		store := onCloseValidatingStore{MemStore: beads.NewMemStore()}
		p := newTestProvider(store)
		unread, err := p.Send("sender", "recipient", "bulk: unread", "body")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		read, err := p.Send("sender", "recipient", "bulk: read", "body")
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := p.MarkRead(read.ID); err != nil {
			t.Fatalf("MarkRead: %v", err)
		}
		_, results, err := p.ArchiveMatching(ArchiveFilter{
			Recipients:    []string{"recipient"},
			SubjectPrefix: "bulk:",
			IncludeRead:   true,
		})
		if err != nil {
			t.Fatalf("ArchiveMatching: %v", err)
		}
		if len(results) != 2 {
			t.Fatalf("ArchiveMatching results = %d; want 2", len(results))
		}
		for _, r := range results {
			if r.Err != nil {
				t.Errorf("ArchiveMatching(%s) err = %v; want nil (close must carry a close_reason)", r.ID, r.Err)
			}
		}
		for _, id := range []string{unread.ID, read.ID} {
			assertArchivedWithReason(t, store, id)
		}
	})
}

// fakeBdMessage is the one message bead a fakeBdOnCloseRunner holds, encoded
// the way bd show --json reports an issue.
type fakeBdMessage struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Status      string            `json:"status"`
	IssueType   string            `json:"issue_type"`
	Assignee    string            `json:"assignee"`
	From        string            `json:"from"`
	Labels      []string          `json:"labels"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	CloseReason string            `json:"close_reason,omitempty"`
}

// fakeBdOnCloseRunner emulates a bd binary holding one message bead with
// validation.on-close: error. It answers show, applies update's
// --set-metadata and --add-label flags, and refuses bd close unless --reason
// passes bdValidateCloseReason. Every close argv is recorded.
type fakeBdOnCloseRunner struct {
	msg        fakeBdMessage
	closeCalls [][]string
}

func (f *fakeBdOnCloseRunner) run(_ string, name string, args ...string) ([]byte, error) {
	if name != "bd" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command: %s %v", name, args)
	}
	switch args[0] {
	case "show":
		if args[len(args)-1] != f.msg.ID {
			return nil, fmt.Errorf("bd show: unexpected id in %v", args)
		}
		return json.Marshal([]fakeBdMessage{f.msg})
	case "update":
		return f.update(args)
	case "close":
		return f.close(args)
	}
	return nil, fmt.Errorf("unexpected bd subcommand: %v", args)
}

func (f *fakeBdOnCloseRunner) update(args []string) ([]byte, error) {
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--set-metadata":
			i++
			k, v, ok := strings.Cut(args[i], "=")
			if !ok {
				return nil, fmt.Errorf("bd update: malformed --set-metadata %q", args[i])
			}
			if f.msg.Metadata == nil {
				f.msg.Metadata = map[string]string{}
			}
			f.msg.Metadata[k] = v
		case "--add-label":
			i++
			if !slices.Contains(f.msg.Labels, args[i]) {
				f.msg.Labels = append(f.msg.Labels, args[i])
			}
		case "--json", f.msg.ID:
		default:
			return nil, fmt.Errorf("bd update: unexpected arg %q in %v", args[i], args)
		}
	}
	return []byte(`{}`), nil
}

func (f *fakeBdOnCloseRunner) close(args []string) ([]byte, error) {
	f.closeCalls = append(f.closeCalls, slices.Clone(args))
	reason := ""
	if i := slices.Index(args, "--reason"); i >= 0 && i+1 < len(args) {
		reason = args[i+1]
	}
	if err := bdValidateCloseReason(reason); err != nil {
		return nil, fmt.Errorf("exit status 1: %w", err)
	}
	f.msg.Status = "closed"
	f.msg.CloseReason = reason
	return []byte(`{}`), nil
}

// TestArchiveThroughBdStoreSendsCloseReason covers AC-3: through a real
// BdStore, Archive's bd close carries --reason ArchiveCloseReason, which bd's
// on-close validation accepts.
func TestArchiveThroughBdStoreSendsCloseReason(t *testing.T) {
	const id = "bd-msg1"
	runner := &fakeBdOnCloseRunner{msg: fakeBdMessage{
		ID:        id,
		Title:     "archive me",
		Status:    "open",
		IssueType: "message",
		Assignee:  "recipient",
		From:      "sender",
		Labels:    []string{},
		CreatedAt: time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC),
	}}
	p := newTestProvider(beads.NewBdStore(t.TempDir(), runner.run))

	if err := p.Archive(id); err != nil {
		t.Fatalf("Archive(%s) = %v; want nil (bd close must carry a valid --reason)", id, err)
	}
	want := []string{"close", "--force", "--json", "--reason", ArchiveCloseReason, id}
	if len(runner.closeCalls) != 1 || !slices.Equal(runner.closeCalls[0], want) {
		t.Fatalf("bd close calls = %q; want exactly one %q", runner.closeCalls, want)
	}
	if runner.msg.Status != "closed" {
		t.Errorf("bead status = %q, want closed", runner.msg.Status)
	}
}

// TestArchivedMessageReadsAsUserRemoved covers AC-4: a message Archive closes
// with ArchiveCloseReason is no longer addressable by ID, unlike retention-swept
// mail, and the reason itself satisfies bd's on-close validation.
func TestArchivedMessageReadsAsUserRemoved(t *testing.T) {
	if err := bdValidateCloseReason(ArchiveCloseReason); err != nil {
		t.Fatalf("ArchiveCloseReason %q fails bd on-close validation: %v", ArchiveCloseReason, err)
	}
	if ArchiveCloseReason == RetentionSweepCloseReason || ArchiveCloseReason == UnreadRetentionSweepCloseReason {
		t.Fatalf("ArchiveCloseReason %q must differ from the retention-sweep reasons", ArchiveCloseReason)
	}
	if !isRemovedMessageBead(beads.Bead{
		Type:     messageBeadType,
		Status:   "closed",
		Metadata: map[string]string{"close_reason": ArchiveCloseReason},
	}) {
		t.Errorf("isRemovedMessageBead = false for a message closed with ArchiveCloseReason, want true")
	}

	store := onCloseValidatingStore{MemStore: beads.NewMemStore()}
	p := newTestProvider(store)
	m, err := p.Send("sender", "recipient", "archive me", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := p.Archive(m.ID); err != nil {
		t.Fatalf("Archive(%s): %v", m.ID, err)
	}
	if _, err := p.Get(m.ID); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("Get(archived) = %v, want mail.ErrNotFound", err)
	}
	if _, err := p.Read(m.ID); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("Read(archived) = %v, want mail.ErrNotFound", err)
	}
	if err := p.MarkRead(m.ID); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("MarkRead(archived) = %v, want mail.ErrNotFound", err)
	}
	if _, err := p.Reply(m.ID, "recipient", "re", "body"); !errors.Is(err, mail.ErrNotFound) {
		t.Errorf("Reply(archived) = %v, want mail.ErrNotFound", err)
	}
}

// TestArchiveKeepsRetentionSweepReason guards the order in Archive: a message
// the retention sweep already closed is reported as already archived and keeps
// its addressable sweep reason rather than being re-stamped.
func TestArchiveKeepsRetentionSweepReason(t *testing.T) {
	store := beads.NewMemStore()
	p := newTestProvider(store)
	m, err := p.Send("sender", "recipient", "swept", "body")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := p.MarkRead(m.ID); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if _, _, err := SweepReadMessagesBefore(
		beads.MailStore{Store: store}, time.Now().Add(time.Hour), 0, RetentionSweepCloseReason); err != nil {
		t.Fatalf("SweepReadMessagesBefore: %v", err)
	}
	if err := p.Archive(m.ID); !errors.Is(err, mail.ErrAlreadyArchived) {
		t.Fatalf("Archive(swept) = %v, want mail.ErrAlreadyArchived", err)
	}
	b, err := store.Get(m.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got := b.Metadata["close_reason"]; got != RetentionSweepCloseReason {
		t.Errorf("close_reason after archiving a swept message = %q, want %q", got, RetentionSweepCloseReason)
	}
}
