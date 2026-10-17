package beadmail

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// bdOnCloseError mirrors bd's validation.ValidateCloseReason, which a city
// armed with validation.on-close=error runs on every `bd close`: an empty or
// default reason is refused, and so is one under 20 characters.
func bdOnCloseError(reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || strings.EqualFold(reason, "closed") {
		return errors.New("close reason is empty or default; provide a summary of what was done")
	}
	if len(reason) < 20 {
		return fmt.Errorf("close reason is terse (%d chars); aim for 20+ characters describing what was done", len(reason))
	}
	return nil
}

// onCloseValidatingStore closes the way BdStore does against a city running
// validation.on-close=error: Close forwards metadata.close_reason as the bd
// reason, and bd refuses a close whose reason fails the validator.
type onCloseValidatingStore struct {
	*beads.MemStore
}

func (s onCloseValidatingStore) Close(id string) error {
	b, err := s.Get(id)
	if err != nil {
		return err
	}
	if err := bdOnCloseError(b.Metadata["close_reason"]); err != nil {
		return fmt.Errorf("closing bead %q: exit status 1: %w", id, err)
	}
	return s.MemStore.Close(id)
}

func TestArchiveCloseReasonPassesBdOnCloseValidation(t *testing.T) {
	if err := bdOnCloseError(ArchiveCloseReason); err != nil {
		t.Fatalf("ArchiveCloseReason = %q fails bd's on-close validation: %v", ArchiveCloseReason, err)
	}
	if ArchiveCloseReason == RetentionSweepCloseReason {
		t.Fatal("ArchiveCloseReason must differ from RetentionSweepCloseReason: an archived message is user-removed, not system-aged")
	}
}

func TestArchiveSucceedsUnderOnCloseValidation(t *testing.T) {
	base := beads.NewMemStore()
	p := New(onCloseValidatingStore{MemStore: base})

	msg, err := p.Send("human", "mayor", "subject", "body")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Archive(msg.ID); err != nil {
		t.Fatalf("Archive(%s) under validation.on-close=error: %v", msg.ID, err)
	}

	b, err := base.Get(msg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Fatalf("status = %q, want closed", b.Status)
	}
	if got := b.Metadata["close_reason"]; got != ArchiveCloseReason {
		t.Fatalf("metadata.close_reason = %q, want %q", got, ArchiveCloseReason)
	}
	// An archived message stays user-removed: every read path still hides it.
	if !isRemovedMessageBead(b) {
		t.Fatal("archived message reads as not removed; the archive reason must not change read-path visibility")
	}
}

func TestArchiveMatchingSucceedsUnderOnCloseValidation(t *testing.T) {
	base := beads.NewMemStore()
	p := New(onCloseValidatingStore{MemStore: base})

	a, err := p.Send("human", "human", "Dolt health one", "first")
	if err != nil {
		t.Fatal(err)
	}
	b, err := p.Send("human", "human", "Dolt health two", "second")
	if err != nil {
		t.Fatal(err)
	}

	_, results, err := p.ArchiveMatching(ArchiveFilter{
		Recipients:    []string{"human"},
		SubjectPrefix: "Dolt health",
		Limit:         10,
	})
	if err != nil {
		t.Fatalf("ArchiveMatching: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("ArchiveMatching returned %d results, want 2", len(results))
	}
	for i, r := range results {
		if r.Err != nil {
			t.Fatalf("results[%d].Err = %v", i, r.Err)
		}
	}
	for _, id := range []string{a.ID, b.ID} {
		got, err := base.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, got.Status)
		}
		if reason := got.Metadata["close_reason"]; reason != ArchiveCloseReason {
			t.Fatalf("%s metadata.close_reason = %q, want %q", id, reason, ArchiveCloseReason)
		}
	}
}

// TestArchiveBdStoreForwardsReasonToBdClose drives Archive through a real
// BdStore whose runner enforces bd's validation.on-close=error on `bd close`.
// Before the cure the archive ran `bd close --force --json <id>` with no
// reason and bd refused it.
func TestArchiveBdStoreForwardsReasonToBdClose(t *testing.T) {
	const id = "gc-wisp-a1"
	status := "open"
	metadata := map[string]string{}
	var closeArgs []string

	show := func() []byte {
		out, _ := json.Marshal([]map[string]any{{
			"id":         id,
			"title":      "subject",
			"status":     status,
			"issue_type": "message",
			"assignee":   "mayor",
			"created_at": "2026-10-07T18:00:00Z",
			"metadata":   metadata,
		}})
		return out
	}
	runner := func(_, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			return nil, fmt.Errorf("unexpected command name: %s", name)
		}
		switch {
		case len(args) == 3 && args[0] == "show" && args[1] == "--json" && args[2] == id:
			return show(), nil
		case len(args) == 5 && args[0] == "update" && args[1] == "--json" && args[2] == id && args[3] == "--set-metadata":
			k, v, _ := strings.Cut(args[4], "=")
			metadata[k] = v
			return show(), nil
		case len(args) >= 4 && args[0] == "close" && args[len(args)-1] == id:
			closeArgs = append([]string(nil), args...)
			reason := ""
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--reason" {
					reason = args[i+1]
				}
			}
			if err := bdOnCloseError(reason); err != nil {
				return nil, fmt.Errorf("exit status 1: %w", err)
			}
			status = "closed"
			return show(), nil
		default:
			return nil, fmt.Errorf("unexpected command: bd %s", strings.Join(args, " "))
		}
	}
	p := New(beads.NewBdStore("/city", runner))

	if err := p.Archive(id); err != nil {
		t.Fatalf("Archive(%s) through BdStore under validation.on-close=error: %v", id, err)
	}
	want := []string{"close", "--force", "--json", "--reason", ArchiveCloseReason, id}
	if fmt.Sprint(closeArgs) != fmt.Sprint(want) {
		t.Fatalf("bd close args = %v, want %v", closeArgs, want)
	}
	if status != "closed" {
		t.Fatalf("status = %q, want closed", status)
	}
}
