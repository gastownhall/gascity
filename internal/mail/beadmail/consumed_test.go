package beadmail

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

// Every successful read, mark-read or archive of a message, the auto-handoff
// archives included, reports it to the consumed hook (OnConsumed), and so
// does an archive of a message already archived, removed under it or already
// gone; a failed read, and a mark-unread, does not.
func TestOnConsumedReportsEachConsumedMessage(t *testing.T) {
	for _, tc := range []struct {
		name string
		do   func(p *Provider, id string) error
	}{
		{"read", func(p *Provider, id string) error { _, err := p.Read(id); return err }},
		{"mark read", func(p *Provider, id string) error { return p.MarkRead(id) }},
		{"archive", func(p *Provider, id string) error { return p.Archive(id) }},
		{"delete", func(p *Provider, id string) error { return p.Delete(id) }},
		{"archive many", func(p *Provider, id string) error { _, err := p.ArchiveMany([]string{id}); return err }},
		{"delete many", func(p *Provider, id string) error { _, err := p.DeleteMany([]string{id}); return err }},
		{"archive matching", func(p *Provider, _ string) error {
			_, _, err := p.ArchiveMatching(ArchiveFilter{From: "alice", IncludeRead: true})
			return err
		}},
		{"archive injected handoffs", func(p *Provider, id string) error { return p.ArchiveInjectedAutoHandoffs([]string{id}) }},
		{"archive injected handoffs from inbox", func(p *Provider, id string) error {
			inbox, err := p.InboxForRoutes([]string{"bob"})
			if err != nil {
				return err
			}
			return p.ArchiveInjectedAutoHandoffsFromInbox(inbox, []string{id}, nil)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			p := New(store)
			var consumed []string
			p.OnConsumed(func(id string) { consumed = append(consumed, id) })
			msg, err := store.Create(beads.Bead{
				Title: "hi", Type: messageBeadType, Assignee: "bob", From: "alice",
				Labels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.do(p, msg.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !slices.Equal(consumed, []string{msg.ID}) {
				t.Fatalf("consumed = %v, want [%s]", consumed, msg.ID)
			}
		})
	}
	t.Run("not consumed", func(t *testing.T) {
		store := beads.NewMemStore()
		p := New(store)
		var consumed []string
		p.OnConsumed(func(id string) { consumed = append(consumed, id) })
		msg, err := p.Send("alice", "bob", "hi", "body")
		if err != nil {
			t.Fatal(err)
		}
		if err := p.MarkUnread(msg.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Read("gc-missing"); err == nil {
			t.Fatal("Read of a missing message succeeded")
		}
		if len(consumed) != 0 {
			t.Fatalf("consumed = %v, want none", consumed)
		}
	})
	// A hook whose work failed after the first archive gets another chance
	// from the next, as from a repeated Read.
	t.Run("archived again", func(t *testing.T) {
		store := beads.NewMemStore()
		p := New(store)
		var consumed []string
		p.OnConsumed(func(id string) { consumed = append(consumed, id) })
		msg, err := p.Send("alice", "bob", "hi", "body")
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Archive(msg.ID); err != nil {
			t.Fatal(err)
		}
		if err := p.Archive(msg.ID); !errors.Is(err, mail.ErrAlreadyArchived) {
			t.Fatalf("second Archive = %v, want mail.ErrAlreadyArchived", err)
		}
		if !slices.Equal(consumed, []string{msg.ID, msg.ID}) {
			t.Fatalf("consumed = %v, want both archives' [%s %s]", consumed, msg.ID, msg.ID)
		}
	})
	// A message already gone (purged after its first archive's hook failed,
	// say) answers mail.ErrAlreadyArchived and is consumed all the same, so
	// the hook gets another chance.
	for _, tc := range []struct {
		name string
		do   func(p *Provider, id string) error
	}{
		{"archive", func(p *Provider, id string) error { return p.Archive(id) }},
		{"delete", func(p *Provider, id string) error { return p.Delete(id) }},
		{"archive many", func(p *Provider, id string) error {
			results, err := p.ArchiveMany([]string{id})
			if err != nil {
				return err
			}
			if len(results) != 1 || results[0].ID != id {
				return fmt.Errorf("results = %+v, want one for %s", results, id)
			}
			return results[0].Err
		}},
	} {
		t.Run(tc.name+" of a message already gone", func(t *testing.T) {
			store := beads.NewMemStore()
			p := New(store)
			var consumed []string
			p.OnConsumed(func(id string) { consumed = append(consumed, id) })
			msg, err := p.Send("alice", "bob", "hi", "body")
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Delete(msg.ID); err != nil {
				t.Fatal(err)
			}
			if err := tc.do(p, msg.ID); !errors.Is(err, mail.ErrAlreadyArchived) {
				t.Fatalf("%s = %v, want mail.ErrAlreadyArchived", tc.name, err)
			}
			if !slices.Equal(consumed, []string{msg.ID}) {
				t.Fatalf("consumed = %v, want [%s]", consumed, msg.ID)
			}
		})
	}
	// A message removed between an archive's read and its close answers
	// mail.ErrAlreadyArchived, and is consumed all the same.
	for _, tc := range []struct {
		name string
		do   func(p *Provider, id string) error
	}{
		{"archive", func(p *Provider, id string) error { return p.Archive(id) }},
		{"archive matching", func(p *Provider, id string) error {
			_, results, err := p.ArchiveMatching(ArchiveFilter{From: "alice", IncludeRead: true})
			if err != nil {
				return err
			}
			if len(results) != 1 || results[0].ID != id {
				return fmt.Errorf("results = %+v, want one for %s", results, id)
			}
			return results[0].Err
		}},
	} {
		t.Run("removed under the "+tc.name, func(t *testing.T) {
			p := New(removedOnCloseStore{beads.NewMemStore()})
			var consumed []string
			p.OnConsumed(func(id string) { consumed = append(consumed, id) })
			msg, err := p.Send("alice", "bob", "hi", "body")
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.do(p, msg.ID); !errors.Is(err, mail.ErrAlreadyArchived) {
				t.Fatalf("%s = %v, want mail.ErrAlreadyArchived", tc.name, err)
			}
			if !slices.Equal(consumed, []string{msg.ID}) {
				t.Fatalf("consumed = %v, want [%s]", consumed, msg.ID)
			}
		})
	}
	// An injected auto-handoff removed between the archive's read and its
	// close is consumed all the same; the archive reports no error for it.
	for _, tc := range []struct {
		name string
		do   func(p *Provider, id string) error
	}{
		{"archive injected handoffs", func(p *Provider, id string) error { return p.ArchiveInjectedAutoHandoffs([]string{id}) }},
		{"archive injected handoffs from inbox", func(p *Provider, id string) error {
			inbox, err := p.InboxForRoutes([]string{"bob"})
			if err != nil {
				return err
			}
			return p.ArchiveInjectedAutoHandoffsFromInbox(inbox, []string{id}, nil)
		}},
	} {
		t.Run("removed under the "+tc.name, func(t *testing.T) {
			store := removedOnCloseStore{beads.NewMemStore()}
			p := New(store)
			var consumed []string
			p.OnConsumed(func(id string) { consumed = append(consumed, id) })
			msg, err := store.Create(beads.Bead{
				Title: "hi", Type: messageBeadType, Assignee: "bob", From: "alice",
				Labels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.do(p, msg.ID); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !slices.Equal(consumed, []string{msg.ID}) {
				t.Fatalf("consumed = %v, want [%s]", consumed, msg.ID)
			}
		})
	}
}

// removedOnCloseStore answers every Close and CloseAll with beads.ErrNotFound,
// as a store does for a bead removed after it was read.
type removedOnCloseStore struct{ beads.Store }

func (s removedOnCloseStore) Close(id string) error {
	return fmt.Errorf("closing %s: %w", id, beads.ErrNotFound)
}

func (s removedOnCloseStore) CloseAll(ids []string, _ map[string]string) (int, error) {
	return 0, fmt.Errorf("closing %v: %w", ids, beads.ErrNotFound)
}
