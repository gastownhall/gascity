package beadmail

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

const (
	redStagedHandoffLabel       = "gc:handoff-staged"
	redStagedHandoffMetadataKey = "mail.staged"
	redStagedForTokenKey        = "mail.staged_for_token"
)

func newStagedHandoff(t *testing.T) (*Provider, *beads.MemStore, mail.Message) {
	t.Helper()
	provider, store, message := newLabelOnlyStagedHandoff(t)
	if err := store.SetMetadataBatch(message.ID, map[string]string{
		redStagedHandoffMetadataKey: "true",
		redStagedForTokenKey:        "origin-instance",
	}); err != nil {
		t.Fatalf("stage handoff metadata: %v", err)
	}
	return provider, store, message
}

// newLabelOnlyStagedHandoff creates a staged brief as the create alone leaves
// it: the staged label is on the bead, but the separate write that sets
// mail.staged has not landed yet.
func newLabelOnlyStagedHandoff(t *testing.T) (*Provider, *beads.MemStore, mail.Message) {
	t.Helper()
	store := beads.NewMemStore()
	provider := newTestProvider(store)
	message, _, err := provider.SendHandoff(mail.HandoffIntent{
		From:     "worker",
		To:       "worker",
		Subject:  "context cycle",
		Body:     "resume from the staged brief",
		ThreadID: "handoff-thread",
		ExtraLabels: []string{
			mail.AutoHandoffLabel,
			mail.ArchiveAfterInjectLabel,
			redStagedHandoffLabel,
		},
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}
	return provider, store, message
}

func TestStagedHandoffIsInvisibleAcrossEveryMailReadSurface(t *testing.T) {
	tests := []struct {
		name  string
		check func(t *testing.T, provider *Provider, message mail.Message)
	}{
		{
			name: "inbox",
			check: func(t *testing.T, provider *Provider, _ mail.Message) {
				messages, err := provider.Inbox("worker")
				assertNoStagedMessages(t, "Inbox", messages, err)
			},
		},
		{
			name: "multi-recipient inbox",
			check: func(t *testing.T, provider *Provider, _ mail.Message) {
				messages, err := provider.InboxRecipients([]string{"worker"})
				assertNoStagedMessages(t, "InboxRecipients", messages, err)
			},
		},
		{
			name: "check",
			check: func(t *testing.T, provider *Provider, _ mail.Message) {
				messages, err := provider.Check("worker")
				assertNoStagedMessages(t, "Check", messages, err)
			},
		},
		{
			name: "startup auto-handoff injection",
			check: func(t *testing.T, provider *Provider, _ mail.Message) {
				messages, err := provider.CheckAutoHandoffs([]string{"worker"})
				assertNoStagedMessages(t, "CheckAutoHandoffs", messages, err)
			},
		},
		{
			name: "all",
			check: func(t *testing.T, provider *Provider, _ mail.Message) {
				messages, err := provider.All("worker")
				assertNoStagedMessages(t, "All", messages, err)
			},
		},
		{
			name: "thread",
			check: func(t *testing.T, provider *Provider, message mail.Message) {
				messages, err := provider.Thread(message.ID)
				assertNoStagedMessages(t, "Thread", messages, err)
			},
		},
		{
			name: "count",
			check: func(t *testing.T, provider *Provider, _ mail.Message) {
				total, unread, err := provider.Count("worker")
				if err != nil {
					t.Fatalf("Count: %v", err)
				}
				if total != 0 || unread != 0 {
					t.Fatalf("Count = (%d, %d), want (0, 0) while handoff is staged", total, unread)
				}
			},
		},
		{
			name: "multi-recipient count",
			check: func(t *testing.T, provider *Provider, _ mail.Message) {
				total, unread, err := provider.CountRecipients([]string{"worker"})
				if err != nil {
					t.Fatalf("CountRecipients: %v", err)
				}
				if total != 0 || unread != 0 {
					t.Fatalf("CountRecipients = (%d, %d), want (0, 0) while handoff is staged", total, unread)
				}
			},
		},
		{
			name: "get by id",
			check: func(t *testing.T, provider *Provider, message mail.Message) {
				if _, err := provider.Get(message.ID); !errors.Is(err, mail.ErrNotFound) {
					t.Fatalf("Get error = %v, want mail.ErrNotFound while handoff is staged", err)
				}
			},
		},
		{
			name: "read by id",
			check: func(t *testing.T, provider *Provider, message mail.Message) {
				if _, err := provider.Read(message.ID); !errors.Is(err, mail.ErrNotFound) {
					t.Fatalf("Read error = %v, want mail.ErrNotFound while handoff is staged", err)
				}
			},
		},
	}

	shapes := []struct {
		name  string
		stage func(t *testing.T) (*Provider, *beads.MemStore, mail.Message)
	}{
		{name: "label and metadata", stage: newStagedHandoff},
		{name: "label before metadata", stage: newLabelOnlyStagedHandoff},
	}
	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					provider, _, message := shape.stage(t)
					test.check(t, provider, message)
				})
			}
		})
	}
}

func TestReleasedStagedHandoffIsVisible(t *testing.T) {
	provider, store, message := newStagedHandoff(t)
	if err := store.Update(message.ID, beads.UpdateOpts{
		RemoveLabels: []string{redStagedHandoffLabel},
		Metadata:     map[string]string{redStagedHandoffMetadataKey: "false"},
	}); err != nil {
		t.Fatalf("release handoff: %v", err)
	}

	checked, err := provider.Check("worker")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if len(checked) != 1 || checked[0].ID != message.ID {
		t.Fatalf("Check = %#v, want released handoff %q", checked, message.ID)
	}
	auto, err := provider.CheckAutoHandoffs([]string{"worker"})
	if err != nil {
		t.Fatalf("CheckAutoHandoffs: %v", err)
	}
	if len(auto) != 1 || auto[0].ID != message.ID {
		t.Fatalf("CheckAutoHandoffs = %#v, want released handoff %q", auto, message.ID)
	}
}

func assertNoStagedMessages(t *testing.T, surface string, messages []mail.Message, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", surface, err)
	}
	if len(messages) != 0 {
		t.Fatalf("%s exposed staged handoff %#v", surface, messages)
	}
}

func TestExistingOrdinaryAutoHandoffRemainsVisible(t *testing.T) {
	store := beads.NewMemStore()
	provider := newTestProvider(store)
	message, _, err := provider.SendHandoff(mail.HandoffIntent{
		From:     "worker",
		To:       "worker",
		Subject:  "legacy context cycle",
		ThreadID: "legacy-thread",
		ExtraLabels: []string{
			mail.AutoHandoffLabel,
			mail.ArchiveAfterInjectLabel,
		},
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}

	messages, err := provider.CheckAutoHandoffs([]string{"worker"})
	if err != nil {
		t.Fatalf("CheckAutoHandoffs: %v", err)
	}
	if len(messages) != 1 || messages[0].ID != message.ID {
		t.Fatalf("CheckAutoHandoffs = %#v, want existing ordinary handoff %q", messages, message.ID)
	}
	total, unread, err := provider.Count("worker")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if total != 1 || unread != 1 {
		t.Fatalf("Count = (%d, %d), want (1, 1) for existing ordinary handoff", total, unread)
	}
}

func TestBulkArchiveLeavesAStagedHandoffOpen(t *testing.T) {
	provider, store, message := newStagedHandoff(t)
	filter := ArchiveFilter{Recipients: []string{"worker"}, IncludeRead: true}

	candidates, err := provider.ArchiveCandidates(filter)
	if err != nil {
		t.Fatalf("ArchiveCandidates: %v", err)
	}
	if len(candidates) != 0 {
		t.Fatalf("ArchiveCandidates offered staged handoff %#v", candidates)
	}
	archived, _, err := provider.ArchiveMatching(filter)
	if err != nil {
		t.Fatalf("ArchiveMatching: %v", err)
	}
	if len(archived) != 0 {
		t.Fatalf("ArchiveMatching archived staged handoff %#v", archived)
	}
	b, err := store.Get(message.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if b.Status != "open" {
		t.Fatalf("staged handoff status = %q after bulk archive, want open", b.Status)
	}
}
