package beadmail

import (
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
)

// sendInjectableAutoHandoff creates an unread auto-handoff message bead carrying
// both the AutoHandoffLabel and ArchiveAfterInjectLabel — the exact shape
// sessionStartAutoHandoffInjection fetches and hands to ArchiveInjectedAutoHandoffs.
func sendInjectableAutoHandoff(t *testing.T, p *Provider, subject string) mail.Message {
	t.Helper()
	msg, err := p.SendHandoff(mail.HandoffIntent{
		From:     "worker",
		To:       "worker",
		Subject:  subject,
		Body:     "continue durable work — carries a GO",
		ThreadID: "thread-" + subject,
		ExtraLabels: []string{
			mail.AutoHandoffLabel,
			mail.ArchiveAfterInjectLabel,
		},
	})
	if err != nil {
		t.Fatalf("SendHandoff(%s): %v", subject, err)
	}
	return msg
}

// TestArchiveInjectedAutoHandoffMarksReadRetainsAddressable is the golden TDD
// pin for dip-6ov51a: ArchiveInjectedAutoHandoffs must MARK-READ + CLOSE
// (retain-addressable), NOT hard-delete. An injected-but-unconsumed auto-handoff
// (the write returned nil, but nothing proves the recycled agent consumed the
// GO it carries) must survive as a recoverable, addressable bead — never
// permanently lost. Covers spec acceptance 1 (marked read + closed but still
// addressable), 2 (a second SessionStart does not re-inject it), and 5 (no new
// hard-Delete of any message bead in the inject path).
func TestArchiveInjectedAutoHandoffMarksReadRetainsAddressable(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	auto := sendInjectableAutoHandoff(t, p, "context-cycle")

	if err := p.ArchiveInjectedAutoHandoffs([]string{auto.ID}); err != nil {
		t.Fatalf("ArchiveInjectedAutoHandoffs: %v", err)
	}

	// Acceptance 5: NOT hard-deleted — the bead still exists at the store level.
	raw, err := store.Get(auto.ID)
	if err != nil {
		t.Fatalf("store.Get after archive = %v; injected handoff was hard-deleted (the dip-6ov51a data loss)", err)
	}

	// Acceptance 1: marked read + closed, stamped with the retention marker that
	// keeps it addressable (mirrors the read-gated TTL sweep).
	if raw.Status != "closed" {
		t.Errorf("raw.Status = %q, want closed", raw.Status)
	}
	if !hasLabel(raw.Labels, "read") {
		t.Errorf("raw.Labels = %#v, missing %q (must be marked read)", raw.Labels, "read")
	}
	if got := raw.Metadata[mail.ReadMetadataKey]; got != "true" {
		t.Errorf("raw.Metadata[%q] = %q, want %q", mail.ReadMetadataKey, got, "true")
	}
	if got := raw.Metadata["close_reason"]; got != RetentionSweepCloseReason {
		t.Errorf("raw close_reason = %q, want %q (retain-addressable marker)", got, RetentionSweepCloseReason)
	}

	// Acceptance 1: still addressable through the Provider surface (recoverable
	// via gc mail show / bd show), exactly like retention-swept read mail — not
	// treated as user-removed.
	if _, err := p.Get(auto.ID); err != nil {
		t.Errorf("p.Get(injected auto-handoff) = %v, want addressable", err)
	}
	if _, err := p.Read(auto.ID); err != nil {
		t.Errorf("p.Read(injected auto-handoff) = %v, want addressable", err)
	}

	// Acceptance 2: a second SessionStart must not re-inject it. CheckAutoHandoffs
	// fetches only UNREAD open handoffs; the read+closed bead is excluded.
	again, err := p.CheckAutoHandoffs([]string{"worker"})
	if err != nil {
		t.Fatalf("CheckAutoHandoffs after archive: %v", err)
	}
	for _, m := range again {
		if m.ID == auto.ID {
			t.Errorf("CheckAutoHandoffs re-surfaced archived handoff %q — would re-inject", auto.ID)
		}
	}
}

// TestArchiveInjectedAutoHandoffReclaimedByReadGatedTTLSweep proves the unified
// retention path: after inject-archive marks the handoff read, the already-correct
// read-gated TTL sweep (PurgeReadMessageWisps) reclaims it once it ages past the
// retention window — one retention path, no special-case delete. It also pins the
// unread-not-swept regression guard (spec acceptance 3 and 4): an auto-handoff
// that was never injected stays unread and MUST survive the same sweep, so an
// unconsumed-and-unarchived handoff is never reclaimed out from under a recycle.
func TestArchiveInjectedAutoHandoffReclaimedByReadGatedTTLSweep(t *testing.T) {
	store := beads.NewMemStore()
	p := New(store)

	injected := sendInjectableAutoHandoff(t, p, "injected-cycle")
	neverInjected := sendInjectableAutoHandoff(t, p, "never-injected-cycle")

	if err := p.ArchiveInjectedAutoHandoffs([]string{injected.ID}); err != nil {
		t.Fatalf("ArchiveInjectedAutoHandoffs: %v", err)
	}

	// Acceptance 3: immediately after inject the handoff is still recoverable —
	// it survives the inject, it is not gone.
	if _, err := p.Get(injected.ID); err != nil {
		t.Fatalf("p.Get(injected) right after archive = %v, want recoverable", err)
	}

	// The read-gated wisp sweep reclaims read mail aged past the cutoff. Both
	// handoffs are wisp-tier (SendHandoff sets Ephemeral); only the read
	// (injected) one is a candidate.
	cutoff := time.Now().Add(time.Hour)
	purged, err := PurgeReadMessageWisps(beads.MailStore{Store: store}, cutoff)
	if err != nil {
		t.Fatalf("PurgeReadMessageWisps: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (only the read injected handoff)", purged)
	}

	// The injected+read handoff is reclaimed by the TTL sweep (its recoverable
	// window has elapsed) — the one unified retention path.
	if _, err := store.Get(injected.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("store.Get(injected) after purge = %v, want ErrNotFound (reclaimed)", err)
	}

	// Acceptance 4 (read-gate regression): the never-injected handoff stayed
	// UNREAD and must NOT be swept — an unconsumed, unarchived handoff is never
	// reclaimed by the read-gated sweep.
	survivor, err := store.Get(neverInjected.ID)
	if err != nil {
		t.Fatalf("store.Get(neverInjected) after purge = %v; unread handoff must not be swept", err)
	}
	if survivor.Status != "open" {
		t.Errorf("neverInjected.Status = %q, want open (unread handoff untouched)", survivor.Status)
	}
	if hasLabel(survivor.Labels, "read") {
		t.Errorf("neverInjected must not be marked read; labels = %#v", survivor.Labels)
	}
}
