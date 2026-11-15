package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// nudgeSeed builds a seed Bead for NewMemStoreFrom representing an open nudge bead.
// id must be unique within the seed slice.
func nudgeSeed(id, nudgeID string, createdAt time.Time) beads.Bead {
	return beads.Bead{
		ID:     id,
		Type:   nudgeBeadType,
		Status: "open",
		Labels: []string{nudgeBeadLabel, "nudge:" + nudgeID},
		Metadata: map[string]string{
			"nudge_id": nudgeID,
			"state":    "queued",
		},
		CreatedAt: createdAt,
	}
}

// mailSeed builds a seed Bead for NewMemStoreFrom representing an open read mail bead.
// id must be unique within the seed slice.
func mailSeed(id string, createdAt time.Time) beads.Bead {
	return beads.Bead{
		ID:        id,
		Type:      "message",
		Status:    "open",
		Labels:    []string{"read"},
		CreatedAt: createdAt,
	}
}

// unreadMailSeed builds a seed Bead for NewMemStoreFrom representing an open
// message bead that has never been marked read (no "read" label) — the
// gastownhall/gascity#5240 regression case. id must be unique within the seed slice.
func unreadMailSeed(id string, createdAt time.Time) beads.Bead {
	return beads.Bead{
		ID:        id,
		Type:      "message",
		Status:    "open",
		CreatedAt: createdAt,
	}
}

func TestSweepStaleNudgeMail_TTLBoundaries(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute
	mailTTL := 30 * time.Minute // intentionally different from the default 60min

	// Nudge beads: one just past TTL (should sweep), one not yet past (should skip).
	// Mail beads: one just past TTL (should sweep), one not yet past (should skip).
	seed := []beads.Bead{
		nudgeSeed("nudge-old", "nudge-old", now.Add(-nudgeTTL-time.Second)),
		nudgeSeed("nudge-fresh", "nudge-fresh", now.Add(-nudgeTTL+time.Second)),
		mailSeed("mail-old", now.Add(-mailTTL-time.Second)),
		mailSeed("mail-fresh", now.Add(-mailTTL+time.Second)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, mailTTL, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeClosed != 1 {
		t.Errorf("NudgeClosed = %d, want 1", result.NudgeClosed)
	}
	if result.MailClosed != 1 {
		t.Errorf("MailClosed = %d, want 1", result.MailClosed)
	}
}

// TestSweepStaleNudgeMail_UnreadMailPastTTL pins the gastownhall/gascity#5240
// regression: a message bead that is never marked "read" never matches the
// read-mail phase's Label:"read" candidate query, so without the unread-mail
// phase it stays open past unreadMailTTL. This seeds one unread bead just past
// unreadMailTTL (must sweep) and one just short of it (must not).
func TestSweepStaleNudgeMail_UnreadMailPastTTL(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute
	mailTTL := 30 * time.Minute
	unreadMailTTL := 7 * 24 * time.Hour

	seed := []beads.Bead{
		unreadMailSeed("unread-old", now.Add(-unreadMailTTL-time.Second)),
		unreadMailSeed("unread-fresh", now.Add(-unreadMailTTL+time.Second)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, mailTTL, unreadMailTTL, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.MailClosed != 1 {
		t.Errorf("MailClosed = %d, want 1 (only unread-old should sweep)", result.MailClosed)
	}

	got, err := store.Get("unread-old")
	if err != nil {
		t.Fatalf("Get(unread-old): %v", err)
	}
	if got.Status != "closed" {
		t.Errorf("unread-old Status = %q, want closed", got.Status)
	}
	if got.Metadata["close_reason"] != beadmail.UnreadRetentionSweepCloseReason {
		t.Errorf("unread-old close_reason = %q, want %q", got.Metadata["close_reason"], beadmail.UnreadRetentionSweepCloseReason)
	}

	fresh, err := store.Get("unread-fresh")
	if err != nil {
		t.Fatalf("Get(unread-fresh): %v", err)
	}
	if fresh.Status != "open" {
		t.Errorf("unread-fresh Status = %q, want open (not yet past unreadMailTTL)", fresh.Status)
	}
}

// TestSweepStaleNudgeMail_UnreadMailNeverSweptByReadPhaseAlone documents why a
// separate phase is required: an unread bead sitting well past the much
// shorter read-mail TTL must NOT be swept by the read-mail phase (it lacks the
// "read" label), only by the unread-mail phase once it also passes the longer
// unreadMailTTL.
func TestSweepStaleNudgeMail_UnreadMailNeverSweptByReadPhaseAlone(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute
	mailTTL := 30 * time.Minute
	unreadMailTTL := 7 * 24 * time.Hour

	// Well past mailTTL, but not past the far longer unreadMailTTL.
	seed := []beads.Bead{
		unreadMailSeed("unread-mid-age", now.Add(-mailTTL-time.Hour)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, mailTTL, unreadMailTTL, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.MailClosed != 0 {
		t.Errorf("MailClosed = %d, want 0 (unread bead has not reached unreadMailTTL yet)", result.MailClosed)
	}
}

func TestSweepStaleNudgeMail_PendingExclusion(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute

	const pendingID = "nudge-pending"
	const safeID = "nudge-safe"
	seed := []beads.Bead{
		nudgeSeed("bead-pending", pendingID, now.Add(-nudgeTTL-time.Second)),
		nudgeSeed("bead-safe", safeID, now.Add(-nudgeTTL-time.Second)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	// pendingID is in the queue's Pending list — must NOT be swept.
	state := &nudgequeue.State{
		Pending: []nudgequeue.Item{{ID: pendingID}},
	}

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, state, now, nudgeTTL, time.Hour, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeClosed != 1 {
		t.Errorf("NudgeClosed = %d, want 1 (only the non-pending nudge)", result.NudgeClosed)
	}

	// Confirm pendingID's bead is still open.
	open, _ := store.ListOpen()
	for _, b := range open {
		if b.Metadata["nudge_id"] == pendingID {
			return // still open — correct
		}
	}
	t.Errorf("pending nudge bead should remain open but was swept")
}

func TestSweepStaleNudgeMail_InFlightExclusion(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute

	const inFlightID = "nudge-inflight"
	seed := []beads.Bead{
		nudgeSeed("bead-inflight", inFlightID, now.Add(-nudgeTTL-time.Second)),
		nudgeSeed("bead-safe", "nudge-safe", now.Add(-nudgeTTL-time.Second)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	// inFlightID is in InFlight — must NOT be swept.
	state := &nudgequeue.State{
		InFlight: []nudgequeue.Item{{ID: inFlightID}},
	}

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, state, now, nudgeTTL, time.Hour, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeClosed != 1 {
		t.Errorf("NudgeClosed = %d, want 1 (only the non-in-flight nudge)", result.NudgeClosed)
	}

	// Confirm in-flight bead is still open.
	open, _ := store.ListOpen()
	for _, b := range open {
		if b.Metadata["nudge_id"] == inFlightID {
			return // still open — correct
		}
	}
	t.Errorf("in-flight nudge bead was swept; it should be skipped")
}

func TestSweepStaleNudgeMail_OpenStatusFilter(t *testing.T) {
	// AC2: already-closed mail beads do not produce a false failure.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	mailTTL := 60 * time.Minute

	seed := []beads.Bead{
		// Pre-closed mail bead (already archived) — should not appear in the query.
		{
			ID:        "mail-pre-closed",
			Type:      "message",
			Status:    "closed",
			Labels:    []string{"read"},
			CreatedAt: now.Add(-mailTTL - time.Second),
		},
		// Open mail bead — should be swept.
		mailSeed("mail-open", now.Add(-mailTTL-time.Second)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, time.Minute, mailTTL, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.MailClosed != 1 {
		t.Errorf("MailClosed = %d, want 1 (only the open mail bead)", result.MailClosed)
	}
}

func TestSweepStaleNudgeMail_BudgetCap(t *testing.T) {
	// AC3: budget cap of N stops closes after N total (nudge + mail combined).
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute
	mailTTL := 60 * time.Minute
	const budget = 3

	// Create 2 stale nudge beads and 3 stale mail beads (total 5 candidates).
	seed := make([]beads.Bead, 0, 5)
	for i := 0; i < 2; i++ {
		seed = append(seed, nudgeSeed(fmt.Sprintf("nudge-%d", i), fmt.Sprintf("nudge-%d", i), now.Add(-nudgeTTL-time.Second)))
	}
	for i := 0; i < 3; i++ {
		seed = append(seed, mailSeed(fmt.Sprintf("mail-%d", i), now.Add(-mailTTL-time.Second)))
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, mailTTL, nudgeMailSweepDefaultUnreadMailTTL, budget)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	total := result.NudgeClosed + result.MailClosed
	if total != budget {
		t.Errorf("total closed = %d, want %d (budget cap)", total, budget)
	}
}

func TestSweepStaleNudgeMail_BudgetZeroMeansUnlimited(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute
	mailTTL := 60 * time.Minute

	seed := make([]beads.Bead, 0, 20)
	for i := 0; i < 10; i++ {
		seed = append(seed, nudgeSeed(fmt.Sprintf("nudge-%d", i), fmt.Sprintf("nudge-%d", i), now.Add(-nudgeTTL-time.Second)))
	}
	for i := 0; i < 10; i++ {
		seed = append(seed, mailSeed(fmt.Sprintf("mail-%d", i), now.Add(-mailTTL-time.Second)))
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, mailTTL, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeClosed != 10 {
		t.Errorf("NudgeClosed = %d, want 10", result.NudgeClosed)
	}
	if result.MailClosed != 10 {
		t.Errorf("MailClosed = %d, want 10", result.MailClosed)
	}
}

// nudgeSweepFailingClose wraps MemStore and forces Close to fail for specific bead IDs.
type nudgeSweepFailingClose struct {
	*beads.MemStore
	failIDs map[string]bool
}

func (s *nudgeSweepFailingClose) Close(id string) error {
	if s.failIDs[id] {
		return fmt.Errorf("store returned ErrConflict for %s", id)
	}
	return s.MemStore.Close(id)
}

func TestSweepStaleNudgeMail_PerBeadCloseFailureContinues(t *testing.T) {
	// AC4: individual close conflicts are reported and do not abort remaining candidates.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute

	// Three stale nudge beads; the middle one will fail to close.
	const id1, id2 = "bead-1", "bead-2"
	seed := []beads.Bead{
		nudgeSeed(id1, "nudge-1", now.Add(-nudgeTTL-time.Second)),
		nudgeSeed(id2, "nudge-2", now.Add(-nudgeTTL-time.Second)),
		nudgeSeed("bead-3", "nudge-3", now.Add(-nudgeTTL-time.Second)),
	}
	mem := beads.NewMemStoreFrom(100, seed, nil)
	store := &nudgeSweepFailingClose{
		MemStore: mem,
		failIDs:  map[string]bool{id2: true},
	}

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, time.Hour, nudgeMailSweepDefaultUnreadMailTTL, 0)

	// The sweep should report the error for the failing bead.
	if err == nil {
		t.Fatal("expected non-nil error for close failure")
	}
	if !strings.Contains(err.Error(), id2) {
		t.Errorf("error should mention failed bead ID %s, got: %v", id2, err)
	}

	// The sweep should still close the beads that did not fail.
	if result.NudgeClosed != 2 {
		t.Errorf("NudgeClosed = %d, want 2 (sweep continued past failure)", result.NudgeClosed)
	}

	// id1 and bead-3 should be closed; id2 should remain open.
	open, _ := mem.ListOpen()
	openIDs := make(map[string]bool)
	for _, b := range open {
		openIDs[b.ID] = true
	}
	if !openIDs[id2] {
		t.Errorf("bead %s (close failed) should still be open", id2)
	}
	if openIDs[id1] {
		t.Errorf("bead %s should be closed after successful sweep", id1)
	}
}

// nudgeSweepFailingMeta wraps MemStore and forces SetMetadataBatch to fail for specific IDs.
type nudgeSweepFailingMeta struct {
	*beads.MemStore
	failIDs map[string]bool
}

func (s *nudgeSweepFailingMeta) SetMetadataBatch(id string, kvs map[string]string) error {
	if s.failIDs[id] {
		return fmt.Errorf("metadata conflict for %s", id)
	}
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func TestSweepStaleNudgeMail_PerBeadMetadataFailureContinues(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute

	const id2 = "bead-2"
	seed := []beads.Bead{
		nudgeSeed("bead-1", "nudge-1", now.Add(-nudgeTTL-time.Second)),
		nudgeSeed(id2, "nudge-2", now.Add(-nudgeTTL-time.Second)),
		nudgeSeed("bead-3", "nudge-3", now.Add(-nudgeTTL-time.Second)),
	}
	mem := beads.NewMemStoreFrom(100, seed, nil)
	store := &nudgeSweepFailingMeta{
		MemStore: mem,
		failIDs:  map[string]bool{id2: true},
	}

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, time.Hour, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err == nil {
		t.Fatal("expected non-nil error for metadata failure")
	}
	if !strings.Contains(err.Error(), id2) {
		t.Errorf("error should mention failed bead ID %s, got: %v", id2, err)
	}
	if result.NudgeClosed != 2 {
		t.Errorf("NudgeClosed = %d, want 2 (sweep continued past metadata failure)", result.NudgeClosed)
	}
}

func TestSweepStaleNudgeMail_NudgeTerminalMetadata(t *testing.T) {
	// AC4: nudge candidates record terminal metadata before close.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute

	const beadID = "nudge-abc"
	seed := []beads.Bead{nudgeSeed(beadID, "nudge-abc", now.Add(-nudgeTTL-time.Second))}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, time.Hour, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeClosed != 1 {
		t.Fatalf("NudgeClosed = %d, want 1", result.NudgeClosed)
	}

	// Retrieve closed bead and check terminal metadata.
	b, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("get bead: %v", err)
	}
	if b.Metadata["state"] != "gc-swept" {
		t.Errorf("state = %q, want %q", b.Metadata["state"], "gc-swept")
	}
	if b.Metadata["terminal_reason"] != "gc-swept-stale" {
		t.Errorf("terminal_reason = %q, want %q", b.Metadata["terminal_reason"], "gc-swept-stale")
	}
	if b.Metadata["terminal_at"] == "" {
		t.Error("terminal_at should be set")
	}
	if b.Metadata["close_reason"] != nudgeMailSweepNudgeCloseReason {
		t.Errorf("close_reason = %q, want %q", b.Metadata["close_reason"], nudgeMailSweepNudgeCloseReason)
	}
	if b.Status != "closed" {
		t.Errorf("status = %q, want closed", b.Status)
	}
}

func TestSweepStaleNudgeMail_NilNudgeStateTreatsAllAsSafe(t *testing.T) {
	// When nudgeState is nil, all stale nudge beads should be swept (no live ID set).
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute

	seed := make([]beads.Bead, 3)
	for i := range seed {
		seed[i] = nudgeSeed(fmt.Sprintf("nudge-%d", i), fmt.Sprintf("nudge-%d", i), now.Add(-nudgeTTL-time.Second))
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, time.Hour, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeClosed != 3 {
		t.Errorf("NudgeClosed = %d, want 3", result.NudgeClosed)
	}
}

func TestSweepStaleNudgeMail_BudgetSplitNudgeThenMail(t *testing.T) {
	// Budget should be applied across nudge + mail phases combined.
	// With budget=3 and 2 nudge + 5 mail: expect 2 nudge + 1 mail = 3 total.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute
	mailTTL := 60 * time.Minute

	seed := make([]beads.Bead, 0, 7)
	for i := 0; i < 2; i++ {
		seed = append(seed, nudgeSeed(fmt.Sprintf("nudge-%d", i), fmt.Sprintf("nudge-%d", i), now.Add(-nudgeTTL-time.Second)))
	}
	for i := 0; i < 5; i++ {
		seed = append(seed, mailSeed(fmt.Sprintf("mail-%d", i), now.Add(-mailTTL-time.Second)))
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, mailTTL, nudgeMailSweepDefaultUnreadMailTTL, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeClosed != 2 {
		t.Errorf("NudgeClosed = %d, want 2", result.NudgeClosed)
	}
	if result.MailClosed != 1 {
		t.Errorf("MailClosed = %d, want 1", result.MailClosed)
	}
}

// TestSweepStaleNudgeMail_BudgetSplitNudgeThenMailThenUnread extends the
// two-phase budget split to phase 3: the unread-mail phase must consume only
// what the nudge and read-mail phases left over, not restart the budget.
func TestSweepStaleNudgeMail_BudgetSplitNudgeThenMailThenUnread(t *testing.T) {
	// With budget=5 and 2 nudge + 2 read mail + 3 unread mail: expect
	// 2 nudge + 2 read + 1 unread, so MailClosed (read + unread) = 3.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute
	mailTTL := 60 * time.Minute
	unreadMailTTL := 7 * 24 * time.Hour

	seed := make([]beads.Bead, 0, 7)
	for i := 0; i < 2; i++ {
		seed = append(seed, nudgeSeed(fmt.Sprintf("nudge-%d", i), fmt.Sprintf("nudge-%d", i), now.Add(-nudgeTTL-time.Second)))
	}
	for i := 0; i < 2; i++ {
		seed = append(seed, mailSeed(fmt.Sprintf("mail-%d", i), now.Add(-mailTTL-time.Second)))
	}
	for i := 0; i < 3; i++ {
		seed = append(seed, unreadMailSeed(fmt.Sprintf("unread-%d", i), now.Add(-unreadMailTTL-time.Second)))
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, mailTTL, unreadMailTTL, 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeClosed != 2 {
		t.Errorf("NudgeClosed = %d, want 2", result.NudgeClosed)
	}
	if result.MailClosed != 3 {
		t.Errorf("MailClosed = %d, want 3 (2 read + 1 unread under the remaining budget)", result.MailClosed)
	}

	// Exactly one of the three unread beads should have been closed.
	unreadClosed := 0
	for i := 0; i < 3; i++ {
		got, err := store.Get(fmt.Sprintf("unread-%d", i))
		if err != nil {
			t.Fatalf("Get(unread-%d): %v", i, err)
		}
		if got.Status == "closed" {
			unreadClosed++
		}
	}
	if unreadClosed != 1 {
		t.Errorf("closed unread beads = %d, want 1 (budget had 1 left for phase 3)", unreadClosed)
	}
}

func TestSweepStaleNudgeMail_MultiplePerBeadErrors(t *testing.T) {
	// Multiple per-bead errors should all be joined and returned.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := 10 * time.Minute

	const id1, id2 = "bead-1", "bead-2"
	seed := []beads.Bead{
		nudgeSeed(id1, "nudge-1", now.Add(-nudgeTTL-time.Second)),
		nudgeSeed(id2, "nudge-2", now.Add(-nudgeTTL-time.Second)),
	}
	mem := beads.NewMemStoreFrom(100, seed, nil)
	store := &nudgeSweepFailingClose{
		MemStore: mem,
		failIDs:  map[string]bool{id1: true, id2: true},
	}

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, time.Hour, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err == nil {
		t.Fatal("expected non-nil error when all beads fail")
	}
	// Both IDs should appear in the error.
	errText := err.Error()
	if !strings.Contains(errText, id1) {
		t.Errorf("error should mention %s, got: %v", id1, err)
	}
	if !strings.Contains(errText, id2) {
		t.Errorf("error should mention %s, got: %v", id2, err)
	}
	if result.NudgeClosed != 0 {
		t.Errorf("NudgeClosed = %d, want 0 (all failed)", result.NudgeClosed)
	}

	// Verify errors.Join gives us individual unwrappable errors.
	var errs []error
	if unwrap, ok := err.(interface{ Unwrap() []error }); ok {
		errs = unwrap.Unwrap()
	}
	if len(errs) < 2 {
		t.Errorf("expected at least 2 joined errors, got %d", len(errs))
	}
}

// --- CLI output format tests ---

func TestCmdOrderSweepNudgeMailRun_NothingToClose(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(100, nil, nil)

	var stdout, stderr bytes.Buffer
	cmdOrderSweepNudgeMailRun(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "nothing to close") {
		t.Errorf("expected 'nothing to close' message, got: %q", stdout.String())
	}
}

func TestCmdOrderSweepNudgeMailRun_NormalOutput(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seed := []beads.Bead{
		nudgeSeed("n1", "nudge-1", now.Add(-nudgeMailSweepDefaultNudgeTTL-time.Second)),
		mailSeed("m1", now.Add(-nudgeMailSweepDefaultMailTTL-time.Second)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	var stdout, stderr bytes.Buffer
	cmdOrderSweepNudgeMailRun(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr)
	out := stdout.String()
	if !strings.Contains(out, "nudge-mail-sweep: closed") {
		t.Errorf("expected 'nudge-mail-sweep: closed' in output, got: %q", out)
	}
	if !strings.Contains(out, "[budget:") {
		t.Errorf("expected budget line in output, got: %q", out)
	}
	if !strings.Contains(out, "/50 used]") {
		t.Errorf("expected budget fraction out of 50, got: %q", out)
	}
}

func TestCmdOrderSweepNudgeMailRun_CapReachedMessage(t *testing.T) {
	// When all budget slots are used, output shows "cap reached".
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seed := make([]beads.Bead, nudgeMailSweepCloseBudget)
	for i := range seed {
		seed[i] = nudgeSeed(fmt.Sprintf("nudge-%d", i), fmt.Sprintf("id-%d", i), now.Add(-nudgeMailSweepDefaultNudgeTTL-time.Second))
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	var stdout, stderr bytes.Buffer
	cmdOrderSweepNudgeMailRun(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "cap reached") {
		t.Errorf("expected 'cap reached' in output when budget is full, got: %q", stdout.String())
	}
}

func TestCmdOrderSweepNudgeMailRun_PerBeadErrorPrintedToStderr(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	const failID = "nudge-fail"
	seed := []beads.Bead{
		nudgeSeed(failID, "nudge-x", now.Add(-nudgeMailSweepDefaultNudgeTTL-time.Second)),
		nudgeSeed("nudge-ok", "nudge-y", now.Add(-nudgeMailSweepDefaultNudgeTTL-time.Second)),
	}
	mem := beads.NewMemStoreFrom(100, seed, nil)
	store := &nudgeSweepFailingClose{MemStore: mem, failIDs: map[string]bool{failID: true}}

	var stdout, stderr bytes.Buffer
	cmdOrderSweepNudgeMailRun(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "ERROR") {
		t.Errorf("expected ERROR line on stderr for failing bead, got: %q", stderr.String())
	}
	// The successful bead should still be counted.
	if !strings.Contains(stdout.String(), "nudge-mail-sweep: closed") {
		t.Errorf("expected success summary on stdout despite per-bead error, got: %q", stdout.String())
	}
}

func TestCmdOrderSweepNudgeMailRun_QuietSuppressesOutput(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(100, nil, nil)

	var stdout, stderr bytes.Buffer
	cmdOrderSweepNudgeMailRun(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, true, &stdout, &stderr)
	if stdout.String() != "" {
		t.Errorf("expected empty stdout with --quiet, got: %q", stdout.String())
	}
}

func TestCmdOrderSweepNudgeMailDryRun_NothingToClose(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(100, nil, nil)

	var stdout, stderr bytes.Buffer
	cmdOrderSweepNudgeMailDryRun(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr)
	if !strings.Contains(stdout.String(), "nothing to close") {
		t.Errorf("expected 'nothing to close' for empty store dry-run, got: %q", stdout.String())
	}
}

func TestCmdOrderSweepNudgeMailDryRun_ShowsWouldClose(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seed := []beads.Bead{
		nudgeSeed("n1", "nudge-1", now.Add(-nudgeMailSweepDefaultNudgeTTL-time.Second)),
		mailSeed("m1", now.Add(-nudgeMailSweepDefaultMailTTL-time.Second)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	var stdout, stderr bytes.Buffer
	cmdOrderSweepNudgeMailDryRun(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr)
	out := stdout.String()
	if !strings.HasPrefix(out, "[DRY RUN]") {
		t.Errorf("expected '[DRY RUN]' prefix, got: %q", out)
	}
	if !strings.Contains(out, "would close") {
		t.Errorf("expected 'would close' in output, got: %q", out)
	}
	if !strings.Contains(out, "no changes made") {
		t.Errorf("expected 'no changes made' suffix, got: %q", out)
	}
}

func TestCmdOrderSweepNudgeMailDryRun_NoBeadsClosed(t *testing.T) {
	// Dry-run must not close any beads.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seed := []beads.Bead{
		nudgeSeed("n1", "nudge-1", now.Add(-nudgeMailSweepDefaultNudgeTTL-time.Second)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	var stdout, stderr bytes.Buffer
	cmdOrderSweepNudgeMailDryRun(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr)

	// The bead should remain open.
	open, _ := store.ListOpen()
	if len(open) != 1 {
		t.Errorf("dry-run closed a bead; want 1 open bead, got %d", len(open))
	}
}

// nudgeSweepFailingList wraps MemStore and forces List to fail, simulating an
// unreadable/unavailable store during a candidate listing.
type nudgeSweepFailingList struct {
	*beads.MemStore
}

func (s *nudgeSweepFailingList) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return nil, fmt.Errorf("store unavailable")
}

func TestCmdOrderSweepNudgeMailDryRun_ListErrorReturnsNonZero(t *testing.T) {
	// A failed candidate listing in --dry-run must surface the error and signal
	// failure (non-zero), not report "nothing to close".
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &nudgeSweepFailingList{MemStore: beads.NewMemStoreFrom(100, nil, nil)}

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepNudgeMailDryRun(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit code on list error, got %d", code)
	}
	if !strings.Contains(stderr.String(), "gc order sweep-nudge-mail:") {
		t.Errorf("expected error on stderr, got: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "nothing to close") {
		t.Errorf("must not report 'nothing to close' when the listing failed, got: %q", stdout.String())
	}
}

func TestCmdOrderSweepNudgeMailRun_ListErrorReturnsNonZero(t *testing.T) {
	// A fatal candidate-listing failure on the normal (non-dry-run) path must
	// surface the error and signal failure (non-zero), not print a success
	// summary — the symmetric counterpart of the dry-run list-error test.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &nudgeSweepFailingList{MemStore: beads.NewMemStoreFrom(100, nil, nil)}

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepNudgeMailRun(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit code on list error, got %d", code)
	}
	if !strings.Contains(stderr.String(), "gc order sweep-nudge-mail:") {
		t.Errorf("expected error on stderr, got: %q", stderr.String())
	}
	if strings.Contains(stdout.String(), "closed") {
		t.Errorf("must not print a success summary when the listing failed, got: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "nothing to close") {
		t.Errorf("must not report 'nothing to close' when the listing failed, got: %q", stdout.String())
	}
}

func TestCountStaleNudgeMail_ListErrorPropagates(t *testing.T) {
	// countStaleNudgeMail must return the underlying list error rather than a
	// silent zero count, so callers can fail closed.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := &nudgeSweepFailingList{MemStore: beads.NewMemStoreFrom(100, nil, nil)}

	_, err := countStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err == nil {
		t.Fatal("expected non-nil error when the store listing fails")
	}
}

// --- Watchdog tests ---

func TestRunNudgeMailSweepWatchdog_ClosesStaleBeads(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seed := []beads.Bead{
		nudgeSeed("nudge-stale", "nudge-s", now.Add(-nudgeMailSweepDefaultNudgeTTL-time.Second)),
		mailSeed("mail-stale", now.Add(-nudgeMailSweepDefaultMailTTL-time.Second)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, nudgeMailSweepWatchdogCloseBudget)
	if err != nil {
		t.Fatalf("watchdog sweep: %v", err)
	}
	if result.NudgeClosed != 1 {
		t.Errorf("watchdog: NudgeClosed = %d, want 1", result.NudgeClosed)
	}
	if result.MailClosed != 1 {
		t.Errorf("watchdog: MailClosed = %d, want 1", result.MailClosed)
	}
}

func TestRunNudgeMailSweepWatchdog_RespectsWatchdogInterval(t *testing.T) {
	// Simulate CityRuntime watchdog interval guard by checking that the second
	// call within the interval would be skipped (tests the interval constant).
	if nudgeMailSweepWatchdogInterval <= 0 {
		t.Fatal("nudgeMailSweepWatchdogInterval must be positive")
	}
	// The watchdog fires when now.Sub(last) >= nudgeMailSweepWatchdogInterval.
	last := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nowJustBefore := last.Add(nudgeMailSweepWatchdogInterval - time.Second)
	nowJustAfter := last.Add(nudgeMailSweepWatchdogInterval)

	if nowJustBefore.Sub(last) >= nudgeMailSweepWatchdogInterval {
		t.Error("interval guard: should not fire just before deadline")
	}
	if nowJustAfter.Sub(last) < nudgeMailSweepWatchdogInterval {
		t.Error("interval guard: should fire at or after deadline")
	}
}

func TestCountStaleNudgeMail_MatchesSweepCounts(t *testing.T) {
	// countStaleNudgeMail should return the same counts that sweepStaleNudgeMail closes.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := nudgeMailSweepDefaultNudgeTTL
	mailTTL := nudgeMailSweepDefaultMailTTL

	seed := []beads.Bead{
		nudgeSeed("n1", "nudge-1", now.Add(-nudgeTTL-time.Second)),
		nudgeSeed("n2", "nudge-2", now.Add(-nudgeTTL-time.Second)),
		mailSeed("m1", now.Add(-mailTTL-time.Second)),
		nudgeSeed("n-fresh", "nudge-fresh", now.Add(-nudgeTTL+time.Second)), // fresh, should not count
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	counts, err := countStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, mailTTL, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts.NudgeClosed != 2 {
		t.Errorf("count: NudgeClosed = %d, want 2", counts.NudgeClosed)
	}
	if counts.MailClosed != 1 {
		t.Errorf("count: MailClosed = %d, want 1", counts.MailClosed)
	}
}

// --- #5877: cfg.Mail.RetentionTTL wiring ---

func TestSweepStaleNudgeMail_ZeroMailTTLSkipsMailPhase(t *testing.T) {
	// mailTTL <= 0 must skip the mail-close phase entirely rather than being
	// treated as a zero-length window (which would close every read mail bead
	// immediately, regardless of age).
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := nudgeMailSweepDefaultNudgeTTL
	seed := []beads.Bead{
		nudgeSeed("n1", "nudge-1", now.Add(-nudgeTTL-time.Second)),
		mailSeed("m-old", now.Add(-24*time.Hour)),
		mailSeed("m-new", now),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, 0, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.NudgeClosed != 1 {
		t.Errorf("NudgeClosed = %d, want 1 (mail-ttl=0 must not affect the nudge phase)", result.NudgeClosed)
	}
	if result.MailClosed != 0 {
		t.Errorf("MailClosed = %d, want 0 (mail-ttl=0 must skip the mail phase)", result.MailClosed)
	}
	open, _ := store.ListOpen()
	if len(open) != 2 {
		t.Errorf("expected both mail beads to remain open, got %d open beads", len(open))
	}
}

func TestCountStaleNudgeMail_ZeroMailTTLSkipsMailPhase(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := nudgeMailSweepDefaultNudgeTTL
	seed := []beads.Bead{
		mailSeed("m-old", now.Add(-24*time.Hour)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	counts, err := countStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, 0, nudgeMailSweepDefaultUnreadMailTTL, 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts.MailClosed != 0 {
		t.Errorf("MailClosed = %d, want 0 with mail-ttl=0", counts.MailClosed)
	}
}

func TestNudgeMailSweepMailTTLForConfig_UnsetUsesDefault(t *testing.T) {
	// A city that has never touched [mail] must keep today's 60-minute default.
	got := nudgeMailSweepMailTTLForConfig(&config.City{}, io.Discard)
	if got != nudgeMailSweepDefaultMailTTL {
		t.Errorf("got %s, want default %s", got, nudgeMailSweepDefaultMailTTL)
	}
	// A nil cfg (no config loaded) must behave the same way.
	got = nudgeMailSweepMailTTLForConfig(nil, io.Discard)
	if got != nudgeMailSweepDefaultMailTTL {
		t.Errorf("nil cfg: got %s, want default %s", got, nudgeMailSweepDefaultMailTTL)
	}
}

func TestNudgeMailSweepMailTTLForConfig_CustomDurationUsed(t *testing.T) {
	cfg := &config.City{Mail: config.MailConfig{RetentionTTL: "15m"}}
	got := nudgeMailSweepMailTTLForConfig(cfg, io.Discard)
	if got != 15*time.Minute {
		t.Errorf("got %s, want 15m", got)
	}
}

func TestNudgeMailSweepMailTTLForConfig_ExplicitZeroDisables(t *testing.T) {
	cfg := &config.City{Mail: config.MailConfig{RetentionTTL: "0"}}
	got := nudgeMailSweepMailTTLForConfig(cfg, io.Discard)
	if got != 0 {
		t.Errorf("got %s, want 0 (disabled)", got)
	}
}

func TestNudgeMailSweepMailTTLForConfig_InvalidFallsBackToDefault(t *testing.T) {
	var stderr bytes.Buffer
	cfg := &config.City{Mail: config.MailConfig{RetentionTTL: "not-a-duration"}}
	got := nudgeMailSweepMailTTLForConfig(cfg, &stderr)
	if got != nudgeMailSweepDefaultMailTTL {
		t.Errorf("got %s, want default %s on invalid config", got, nudgeMailSweepDefaultMailTTL)
	}
	if !strings.Contains(stderr.String(), "not-a-duration") {
		t.Errorf("expected stderr note about the invalid value, got: %q", stderr.String())
	}
}

func TestNudgeMailSweepMailTTLForCity_MissingConfigIsSilent(t *testing.T) {
	// A city with no city.toml is a legitimate no-config case: fall back to the
	// caller's value without nagging the operator every 5 minutes.
	var stderr bytes.Buffer
	got := nudgeMailSweepMailTTLForCity(t.TempDir(), nudgeMailSweepDefaultMailTTL, &stderr)
	if got != nudgeMailSweepDefaultMailTTL {
		t.Errorf("got %s, want fallback %s", got, nudgeMailSweepDefaultMailTTL)
	}
	if stderr.String() != "" {
		t.Errorf("expected no stderr note for a missing city.toml, got: %q", stderr.String())
	}
}

func TestNudgeMailSweepMailTTLForCity_UnparseableConfigWarnsAndFallsBack(t *testing.T) {
	// A config that exists and will not parse is the case that leaves a
	// configured retention_ttl looking applied when it is not, so it must be
	// reported -- but it must not stop the sweep.
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("this is not valid toml\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	var stderr bytes.Buffer
	got := nudgeMailSweepMailTTLForCity(cityPath, nudgeMailSweepDefaultMailTTL, &stderr)
	if got != nudgeMailSweepDefaultMailTTL {
		t.Errorf("got %s, want fallback %s", got, nudgeMailSweepDefaultMailTTL)
	}
	if !strings.Contains(stderr.String(), "nudge-mail-sweep:") {
		t.Errorf("expected a stderr note about the unloadable config, got: %q", stderr.String())
	}
}

func TestNudgeMailSweepMailTTLForCity_ConfiguredValueUsed(t *testing.T) {
	cityPath := t.TempDir()
	cityTOML := "[workspace]\nname = \"test-city\"\n\n[mail]\nretention_ttl = \"15m\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	var stderr bytes.Buffer
	got := nudgeMailSweepMailTTLForCity(cityPath, nudgeMailSweepDefaultMailTTL, &stderr)
	if got != 15*time.Minute {
		t.Errorf("got %s, want 15m from [mail] retention_ttl", got)
	}
	if stderr.String() != "" {
		t.Errorf("expected no stderr note for a valid config, got: %q", stderr.String())
	}
}

func TestValidateNudgeMailSweepFlags(t *testing.T) {
	cases := []struct {
		name                  string
		nudgeTTL              time.Duration
		mailTTL               time.Duration
		unreadMailTTL         time.Duration
		mailTTLExplicit       bool
		unreadMailTTLExplicit bool
		wantErr               bool
	}{
		{"defaults ok", nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, false, false},
		{"nudge-ttl zero rejected", 0, nudgeMailSweepDefaultMailTTL, 0, false, false, true},
		{"nudge-ttl negative rejected", -time.Minute, nudgeMailSweepDefaultMailTTL, 0, false, false, true},
		{"explicit mail-ttl zero accepted", nudgeMailSweepDefaultNudgeTTL, 0, 0, true, false, false},
		{"explicit mail-ttl negative rejected", nudgeMailSweepDefaultNudgeTTL, -time.Minute, 0, true, false, true},
		{"unset mail-ttl zero not validated here", nudgeMailSweepDefaultNudgeTTL, 0, 0, false, false, false},
		{"explicit unread-mail-ttl zero accepted", nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, 0, false, true, false},
		{"explicit unread-mail-ttl positive accepted", nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, 7 * 24 * time.Hour, false, true, false},
		{"explicit unread-mail-ttl negative rejected", nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, -time.Minute, false, true, true},
		{"unset unread-mail-ttl not validated here", nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, -time.Minute, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateNudgeMailSweepFlags(tc.nudgeTTL, tc.mailTTL, tc.unreadMailTTL, tc.mailTTLExplicit, tc.unreadMailTTLExplicit)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateNudgeMailSweepFlags(%s, %s, %s, %v, %v) err = %v, wantErr %v", tc.nudgeTTL, tc.mailTTL, tc.unreadMailTTL, tc.mailTTLExplicit, tc.unreadMailTTLExplicit, err, tc.wantErr)
			}
		})
	}
}

// --- #5240: opt-in unread-mail phase ---

func TestSweepStaleNudgeMail_ZeroUnreadMailTTLSkipsUnreadPhase(t *testing.T) {
	// unreadMailTTL <= 0 must skip the unread-mail phase entirely rather than
	// being treated as a zero-length window (which would close every unread
	// mail bead immediately, regardless of age).
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	nudgeTTL := nudgeMailSweepDefaultNudgeTTL
	seed := []beads.Bead{
		nudgeSeed("n1", "nudge-1", now.Add(-nudgeTTL-time.Second)),
		unreadMailSeed("unread-old", now.Add(-30*24*time.Hour)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeTTL, nudgeMailSweepDefaultMailTTL, 0, 0)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.NudgeClosed != 1 {
		t.Errorf("NudgeClosed = %d, want 1 (unread-mail-ttl=0 must not affect the nudge phase)", result.NudgeClosed)
	}
	if result.MailClosed != 0 {
		t.Errorf("MailClosed = %d, want 0 (unread-mail-ttl=0 must skip the unread phase)", result.MailClosed)
	}
	got, err := store.Get("unread-old")
	if err != nil {
		t.Fatalf("Get(unread-old): %v", err)
	}
	if got.Status != "open" {
		t.Errorf("unread-old status = %q, want open", got.Status)
	}
}

func TestCountStaleNudgeMail_ZeroUnreadMailTTLSkipsUnreadPhase(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seed := []beads.Bead{
		unreadMailSeed("unread-old", now.Add(-30*24*time.Hour)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	counts, err := countStaleNudgeMail(beads.NudgesStore{Store: store}, beads.MailStore{Store: store}, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, 0, 0)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if counts.MailClosed != 0 {
		t.Errorf("MailClosed = %d, want 0 with unread-mail-ttl=0", counts.MailClosed)
	}
	got, err := store.Get("unread-old")
	if err != nil {
		t.Fatalf("Get(unread-old): %v", err)
	}
	if got.Status != "open" {
		t.Errorf("unread-old status = %q, want open", got.Status)
	}
}

func TestNudgeMailSweepUnreadMailTTLForConfig_UnsetDisables(t *testing.T) {
	// The unread-mail phase is opt-in: a city that has never touched
	// [mail] unread_retention_ttl must not close unread mail.
	if got := nudgeMailSweepUnreadMailTTLForConfig(&config.City{}, io.Discard); got != 0 {
		t.Errorf("got %s, want 0 (disabled)", got)
	}
	if got := nudgeMailSweepUnreadMailTTLForConfig(nil, io.Discard); got != 0 {
		t.Errorf("nil cfg: got %s, want 0 (disabled)", got)
	}
}

func TestNudgeMailSweepUnreadMailTTLForConfig_CustomDurationUsed(t *testing.T) {
	cfg := &config.City{Mail: config.MailConfig{UnreadRetentionTTL: "168h"}}
	if got := nudgeMailSweepUnreadMailTTLForConfig(cfg, io.Discard); got != 168*time.Hour {
		t.Errorf("got %s, want 168h", got)
	}
}

func TestNudgeMailSweepUnreadMailTTLForConfig_ExplicitZeroDisables(t *testing.T) {
	cfg := &config.City{Mail: config.MailConfig{UnreadRetentionTTL: "0"}}
	if got := nudgeMailSweepUnreadMailTTLForConfig(cfg, io.Discard); got != 0 {
		t.Errorf("got %s, want 0 (disabled)", got)
	}
}

func TestNudgeMailSweepUnreadMailTTLForConfig_InvalidDisablesWithNote(t *testing.T) {
	var stderr bytes.Buffer
	cfg := &config.City{Mail: config.MailConfig{UnreadRetentionTTL: "not-a-duration"}}
	if got := nudgeMailSweepUnreadMailTTLForConfig(cfg, &stderr); got != 0 {
		t.Errorf("got %s, want 0 (disabled) on invalid config", got)
	}
	if !strings.Contains(stderr.String(), "not-a-duration") {
		t.Errorf("expected stderr note about the invalid value, got: %q", stderr.String())
	}
}

func TestNudgeMailSweepUnreadMailTTLForCity_MissingConfigIsSilent(t *testing.T) {
	var stderr bytes.Buffer
	got := nudgeMailSweepUnreadMailTTLForCity(t.TempDir(), nudgeMailSweepDefaultUnreadMailTTL, &stderr)
	if got != 0 {
		t.Errorf("got %s, want fallback 0", got)
	}
	if stderr.String() != "" {
		t.Errorf("expected no stderr note for a missing city.toml, got: %q", stderr.String())
	}
}

func TestNudgeMailSweepUnreadMailTTLForCity_ConfiguredValueUsed(t *testing.T) {
	cityPath := t.TempDir()
	cityTOML := "[workspace]\nname = \"test-city\"\n\n[mail]\nunread_retention_ttl = \"168h\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	var stderr bytes.Buffer
	got := nudgeMailSweepUnreadMailTTLForCity(cityPath, nudgeMailSweepDefaultUnreadMailTTL, &stderr)
	if got != 168*time.Hour {
		t.Errorf("got %s, want 168h from [mail] unread_retention_ttl", got)
	}
	if stderr.String() != "" {
		t.Errorf("expected no stderr note for a valid config, got: %q", stderr.String())
	}
}

func TestRunNudgeMailSweepWatchdog_UnreadMailOptIn(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		unreadTTL  string
		wantStatus string
	}{
		{"unset leaves unread mail open", "", "open"},
		{"configured closes aged unread mail", "168h", "closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seed := []beads.Bead{
				unreadMailSeed("unread-old", now.Add(-30*24*time.Hour)),
			}
			store := beads.NewMemStoreFrom(100, seed, nil)

			cr := &CityRuntime{
				cityName: "test-city",
				cfg: &config.City{
					Workspace: config.Workspace{Name: "test-city"},
					Mail:      config.MailConfig{UnreadRetentionTTL: tc.unreadTTL},
				},
				standaloneCityStore: store,
				stdout:              io.Discard,
				stderr:              io.Discard,
				logPrefix:           "gc test",
			}
			cr.runNudgeMailSweepWatchdog(cr.cfg, now)

			got, err := store.Get("unread-old")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("unread-old status = %s, want %s", got.Status, tc.wantStatus)
			}
		})
	}
}

func TestRunNudgeMailSweepWatchdog_UnsetConfigUsesDefaultMailTTL(t *testing.T) {
	// Regression guard: a city that has never touched [mail] must keep
	// closing read mail at the 60-minute default via the watchdog.
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seed := []beads.Bead{
		mailSeed("mail-stale", now.Add(-nudgeMailSweepDefaultMailTTL-time.Second)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	cr := &CityRuntime{
		cityName:            "test-city",
		cfg:                 &config.City{Workspace: config.Workspace{Name: "test-city"}},
		standaloneCityStore: store,
		stdout:              io.Discard,
		stderr:              io.Discard,
		logPrefix:           "gc test",
	}
	cr.runNudgeMailSweepWatchdog(cr.cfg, now)

	got, err := store.Get("mail-stale")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Errorf("mail-stale status = %s, want closed (default 60m TTL should still apply)", got.Status)
	}
}

func TestRunNudgeMailSweepWatchdog_CustomMailRetentionTTLUsed(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	seed := []beads.Bead{
		mailSeed("mail-15m", now.Add(-20*time.Minute)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	cr := &CityRuntime{
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Mail:      config.MailConfig{RetentionTTL: "15m"},
		},
		standaloneCityStore: store,
		stdout:              io.Discard,
		stderr:              io.Discard,
		logPrefix:           "gc test",
	}
	cr.runNudgeMailSweepWatchdog(cr.cfg, now)

	got, err := store.Get("mail-15m")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Errorf("mail-15m status = %s, want closed (a mail bead older than the configured 15m TTL should be swept)", got.Status)
	}
}

func TestRunNudgeMailSweepWatchdog_ExplicitZeroDisablesMailSweep(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// The stale nudge bead is the witness that the watchdog body actually ran:
	// without it, "mail-old stayed open" would also hold if the sweep no-opped
	// for an unrelated reason (interval gate, empty routed store, an early
	// return) and never reached the mailTTL > 0 guard this test names.
	seed := []beads.Bead{
		nudgeSeed("nudge-stale", "nudge-1", now.Add(-nudgeMailSweepDefaultNudgeTTL-time.Second)),
		mailSeed("mail-old", now.Add(-24*time.Hour)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	cr := &CityRuntime{
		cityName: "test-city",
		cfg: &config.City{
			Workspace: config.Workspace{Name: "test-city"},
			Mail:      config.MailConfig{RetentionTTL: "0"},
		},
		standaloneCityStore: store,
		stdout:              io.Discard,
		stderr:              io.Discard,
		logPrefix:           "gc test",
	}
	cr.runNudgeMailSweepWatchdog(cr.cfg, now)

	gotNudge, err := store.Get("nudge-stale")
	if err != nil {
		t.Fatalf("Get nudge-stale: %v", err)
	}
	if gotNudge.Status != "closed" {
		t.Fatalf("nudge-stale status = %s, want closed (retention_ttl=0 must not disable the nudge phase; the watchdog body did not run)", gotNudge.Status)
	}

	got, err := store.Get("mail-old")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Errorf("mail-old status = %s, want open (explicit retention_ttl=0 must disable the mail-close phase)", got.Status)
	}
}

// --- Rig-store coverage (ga-efp2w7) ---
//
// A rig's nudge shadows are born in the rig's own work store, so a sweep that
// reads only the city store leaves them open forever. The sweep takes the
// nudges-class store first and then every rig store (nudgeSweepScope) under
// one shared close budget.

// nudgeScopeStale returns n stale open nudge shadows whose bead ids and nudge
// ids both start with prefix, so a bead in one store can never be mistaken for
// another's.
func nudgeScopeStale(prefix string, n int, now time.Time) []beads.Bead {
	seed := make([]beads.Bead, n)
	for i := range seed {
		id := fmt.Sprintf("%s-%d", prefix, i)
		seed[i] = nudgeSeed(id, id, now.Add(-nudgeMailSweepDefaultNudgeTTL-time.Second))
	}
	return seed
}

func nudgeScopeOf(label string, store beads.Store) nudgeSweepScope {
	return nudgeSweepScope{Label: label, Store: beads.NudgesStore{Store: store}}
}

func mustBeadStatus(t *testing.T, store beads.Store, id string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b.Status
}

func TestSweepStaleNudgeMail_ClosesStaleShadowInEveryRigStore(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	city := beads.NewMemStoreFrom(100, nudgeScopeStale("gm", 1, now), nil)
	rigA := beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 2, now), nil)
	rigB := beads.NewMemStoreFrom(100, nudgeScopeStale("gb", 1, now), nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: city}, beads.MailStore{Store: city}, nil, now,
		nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, nudgeMailSweepWatchdogCloseBudget,
		nudgeScopeOf(`rig "a"`, rigA), nudgeScopeOf(`rig "b"`, rigB))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.NudgeClosed != 4 {
		t.Fatalf("NudgeClosed = %d, want 4 (1 city + 2 rig a + 1 rig b)", result.NudgeClosed)
	}
	for store, ids := range map[beads.Store][]string{city: {"gm-0"}, rigA: {"ga-0", "ga-1"}, rigB: {"gb-0"}} {
		for _, id := range ids {
			b, err := store.Get(id)
			if err != nil {
				t.Fatalf("Get(%s): %v", id, err)
			}
			if b.Status != "closed" || b.Metadata["state"] != "gc-swept" || b.Metadata["terminal_reason"] != "gc-swept-stale" ||
				b.Metadata["close_reason"] != nudgeMailSweepNudgeCloseReason {
				t.Errorf("%s: status=%q state=%q terminal_reason=%q close_reason=%q, want the gc-swept vocabulary",
					id, b.Status, b.Metadata["state"], b.Metadata["terminal_reason"], b.Metadata["close_reason"])
			}
		}
	}
	want := []nudgeSweepScopeClosed{{Label: `rig "a"`, Closed: 2}, {Label: `rig "b"`, Closed: 1}}
	if fmt.Sprint(result.ScopeNudgeClosed) != fmt.Sprint(want) {
		t.Errorf("ScopeNudgeClosed = %v, want %v", result.ScopeNudgeClosed, want)
	}
}

func TestSweepStaleNudgeMail_LiveRigNudgeIDsAreNotClosedInAnyStore(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	city := beads.NewMemStoreFrom(100, nudgeScopeStale("gm", 1, now), nil)
	rigA := beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 2, now), nil)
	rigB := beads.NewMemStoreFrom(100, nudgeScopeStale("gb", 2, now), nil)
	// ga-0 is Pending and gb-0 is InFlight: each lives in a rig store, and the
	// live set is the city's flock queue, so it has to protect them there.
	state := &nudgequeue.State{
		Pending:  []nudgequeue.Item{{ID: "ga-0"}},
		InFlight: []nudgequeue.Item{{ID: "gb-0"}},
	}

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: city}, beads.MailStore{Store: city}, state, now,
		nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, nudgeMailSweepWatchdogCloseBudget,
		nudgeScopeOf(`rig "a"`, rigA), nudgeScopeOf(`rig "b"`, rigB))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.NudgeClosed != 3 {
		t.Errorf("NudgeClosed = %d, want 3 (gm-0, ga-1, gb-1)", result.NudgeClosed)
	}
	if got := mustBeadStatus(t, rigA, "ga-0"); got != "open" {
		t.Errorf("Pending rig nudge ga-0 status = %q, want open", got)
	}
	if got := mustBeadStatus(t, rigB, "gb-0"); got != "open" {
		t.Errorf("InFlight rig nudge gb-0 status = %q, want open", got)
	}
	if got := mustBeadStatus(t, rigA, "ga-1"); got != "closed" {
		t.Errorf("non-live rig nudge ga-1 status = %q, want closed", got)
	}
}

func TestSweepStaleNudgeMail_RigStoresShareOneBudgetCityFirst(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	city := beads.NewMemStoreFrom(100, append(nudgeScopeStale("gm", 3, now), mailSeed("mail-1", now.Add(-nudgeMailSweepDefaultMailTTL-time.Second))), nil)
	rigA := beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 3, now), nil)
	rigB := beads.NewMemStoreFrom(100, nudgeScopeStale("gb", 3, now), nil)

	const budget = 4
	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: city}, beads.MailStore{Store: city}, nil, now,
		nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, budget,
		nudgeScopeOf(`rig "a"`, rigA), nudgeScopeOf(`rig "b"`, rigB))
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := result.NudgeClosed + result.MailClosed; got != budget {
		t.Fatalf("total closes = %d, want exactly the shared budget %d (a per-store budget would close more)", got, budget)
	}
	closedIn := func(store beads.Store, prefix string, n int) int {
		c := 0
		for i := 0; i < n; i++ {
			if mustBeadStatus(t, store, fmt.Sprintf("%s-%d", prefix, i)) == "closed" {
				c++
			}
		}
		return c
	}
	if got := closedIn(city, "gm", 3); got != 3 {
		t.Errorf("city closed %d of 3, want all 3 (the city is swept first)", got)
	}
	if got := closedIn(rigA, "ga", 3); got != 1 {
		t.Errorf("rig a closed %d, want the 1 remaining budget slot", got)
	}
	if got := closedIn(rigB, "gb", 3); got != 0 {
		t.Errorf("rig b closed %d, want 0 once the budget is spent", got)
	}
	if result.MailClosed != 0 {
		t.Errorf("MailClosed = %d, want 0 once the nudge phase spent the budget", result.MailClosed)
	}
}

// nudgeSweepListFailsOnLabel fails only the stale-shadow listing, so a store
// that can be reached for everything else still reads as down for this sweep.
type nudgeSweepListFailsOnLabel struct {
	*beads.MemStore
}

func (s *nudgeSweepListFailsOnLabel) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Label == nudgeBeadLabel {
		return nil, fmt.Errorf("rig store offline")
	}
	return s.MemStore.List(q)
}

func TestSweepStaleNudgeMail_FailingRigStoreDoesNotStopTheOthers(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	city := beads.NewMemStoreFrom(100, nudgeScopeStale("gm", 1, now), nil)
	rigA := &nudgeSweepListFailsOnLabel{MemStore: beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 1, now), nil)}
	rigB := beads.NewMemStoreFrom(100, nudgeScopeStale("gb", 1, now), nil)

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: city}, beads.MailStore{Store: city}, nil, now,
		nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, nudgeMailSweepWatchdogCloseBudget,
		nudgeScopeOf(`rig "a"`, rigA), nudgeScopeOf(`rig "b"`, rigB))
	if err == nil {
		t.Fatal("a rig store that cannot be listed must surface in the returned error")
	}
	if !strings.Contains(err.Error(), `rig "a"`) || !strings.Contains(err.Error(), "rig store offline") {
		t.Errorf("error = %q, want it to name the failing store and carry the cause", err)
	}
	// A joined error is the CLI's "report each, keep going" shape; a bare error
	// would read as fatal there and fail the whole run.
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		t.Errorf("error %T does not unwrap to a []error, so the CLI would treat one rig failure as fatal", err)
	}
	if result.NudgeClosed != 2 {
		t.Errorf("NudgeClosed = %d, want 2 (city and rig b still swept)", result.NudgeClosed)
	}
	if got := mustBeadStatus(t, rigB, "gb-0"); got != "closed" {
		t.Errorf("rig b after rig a failed: gb-0 status = %q, want closed", got)
	}
	if got := mustBeadStatus(t, city, "gm-0"); got != "closed" {
		t.Errorf("city: gm-0 status = %q, want closed", got)
	}
}

func TestSweepStaleNudgeMail_RigStoreEqualToNudgesStoreIsSweptOnce(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// A relocated-nudges city hands the sweep the relocated store first. A rig
	// scope that resolves to that same store, or the same rig listed twice,
	// must not be listed and closed a second time.
	relocated := beads.NewMemStoreFrom(100, nudgeScopeStale("gn", 2, now), nil)
	rig := beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 1, now), nil)
	scopes := []nudgeSweepScope{nudgeScopeOf("city store", relocated), nudgeScopeOf(`rig "a"`, rig), nudgeScopeOf(`rig "a" again`, rig)}

	result, err := sweepStaleNudgeMail(beads.NudgesStore{Store: relocated}, beads.MailStore{Store: relocated}, nil, now,
		nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, nudgeMailSweepWatchdogCloseBudget, scopes...)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.NudgeClosed != 3 {
		t.Errorf("NudgeClosed = %d, want 3 (each bead closed once)", result.NudgeClosed)
	}
	want := []nudgeSweepScopeClosed{{Label: `rig "a"`, Closed: 1}}
	if fmt.Sprint(result.ScopeNudgeClosed) != fmt.Sprint(want) {
		t.Errorf("ScopeNudgeClosed = %v, want %v", result.ScopeNudgeClosed, want)
	}
}

func TestCountStaleNudgeMail_RigStoresMatchSweepAndSecondRunClosesNothing(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	city := beads.NewMemStoreFrom(100, nudgeScopeStale("gm", 2, now), nil)
	rigA := beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 3, now), nil)
	rigB := beads.NewMemStoreFrom(100, nudgeScopeStale("gb", 1, now), nil)
	scopes := []nudgeSweepScope{nudgeScopeOf(`rig "a"`, rigA), nudgeScopeOf(`rig "b"`, rigB)}
	args := func() (beads.NudgesStore, beads.MailStore) {
		return beads.NudgesStore{Store: city}, beads.MailStore{Store: city}
	}

	n, m := args()
	counted, err := countStaleNudgeMail(n, m, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, nudgeMailSweepWatchdogCloseBudget, scopes...)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got := mustBeadStatus(t, rigA, "ga-0"); got != "open" {
		t.Fatalf("dry-run closed a rig bead (status %q)", got)
	}
	swept, err := sweepStaleNudgeMail(n, m, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, nudgeMailSweepWatchdogCloseBudget, scopes...)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if counted.NudgeClosed != 6 || counted.NudgeClosed != swept.NudgeClosed {
		t.Errorf("dry-run counted %d, sweep closed %d, want both 6", counted.NudgeClosed, swept.NudgeClosed)
	}
	if fmt.Sprint(counted.ScopeNudgeClosed) != fmt.Sprint(swept.ScopeNudgeClosed) {
		t.Errorf("dry-run per-store counts %v differ from the sweep's %v", counted.ScopeNudgeClosed, swept.ScopeNudgeClosed)
	}
	again, err := sweepStaleNudgeMail(n, m, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, nudgeMailSweepWatchdogCloseBudget, scopes...)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if again.NudgeClosed != 0 || len(again.ScopeNudgeClosed) != 0 {
		t.Errorf("second sweep closed %d (%v), want 0", again.NudgeClosed, again.ScopeNudgeClosed)
	}
}

func TestCountStaleNudgeMail_FailingRigStoreIsIsolatedLikeTheSweep(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	city := beads.NewMemStoreFrom(100, nudgeScopeStale("gm", 1, now), nil)
	rigA := &nudgeSweepListFailsOnLabel{MemStore: beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 1, now), nil)}
	rigB := beads.NewMemStoreFrom(100, nudgeScopeStale("gb", 1, now), nil)

	counted, err := countStaleNudgeMail(beads.NudgesStore{Store: city}, beads.MailStore{Store: city}, nil, now,
		nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, nudgeMailSweepWatchdogCloseBudget,
		nudgeScopeOf(`rig "a"`, rigA), nudgeScopeOf(`rig "b"`, rigB))
	if err == nil || !strings.Contains(err.Error(), `rig "a"`) {
		t.Fatalf("dry-run error = %v, want one naming rig a", err)
	}
	if counted.NudgeClosed != 2 {
		t.Errorf("dry-run counted %d, want 2 (city and rig b)", counted.NudgeClosed)
	}
}

func TestCmdOrderSweepNudgeMailRun_CityOnlyOutputIsByteStable(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	newCity := func() *beads.MemStore {
		return beads.NewMemStoreFrom(100, []beads.Bead{
			nudgeSeed("n1", "nudge-1", now.Add(-nudgeMailSweepDefaultNudgeTTL-time.Second)),
			mailSeed("m1", now.Add(-nudgeMailSweepDefaultMailTTL-time.Second)),
		}, nil)
	}
	const want = "nudge-mail-sweep: closed 1 nudge bead(s), 1 mail bead(s)  [budget: 2/50 used]\n"

	run := func(scopes ...nudgeSweepScope) string {
		city := newCity()
		var stdout, stderr bytes.Buffer
		if code := cmdOrderSweepNudgeMailRun(beads.NudgesStore{Store: city}, beads.MailStore{Store: city}, nil, now,
			nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr, scopes...); code != 0 {
			t.Fatalf("exit %d, stderr %q", code, stderr.String())
		}
		return stdout.String()
	}
	if got := run(); got != want {
		t.Errorf("city-only output = %q, want %q", got, want)
	}
	// A healthy rig with nothing stale must not change a byte either.
	quietRig := beads.NewMemStoreFrom(100, nil, nil)
	if got := run(nudgeScopeOf(`rig "a"`, quietRig)); got != want {
		t.Errorf("output with a clean rig = %q, want the city-only %q", got, want)
	}
}

func TestCmdOrderSweepNudgeMailRun_ReportsRigClosesPerStore(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	city := beads.NewMemStoreFrom(100, nudgeScopeStale("gm", 1, now), nil)
	rigA := beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 2, now), nil)
	rigB := beads.NewMemStoreFrom(100, nil, nil)

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepNudgeMailRun(beads.NudgesStore{Store: city}, beads.MailStore{Store: city}, nil, now,
		nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr,
		nudgeScopeOf(`rig "a"`, rigA), nudgeScopeOf(`rig "b"`, rigB))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "nudge-mail-sweep: closed 3 nudge bead(s), 0 mail bead(s)  [budget: 3/50 used]\n") {
		t.Errorf("summary line missing or wrong (the total must include rig closes): %q", out)
	}
	if !strings.Contains(out, `rig "a": closed 2 nudge bead(s)`) {
		t.Errorf("no per-store line for rig a: %q", out)
	}
	if strings.Contains(out, `rig "b"`) {
		t.Errorf("rig b closed nothing and must not be reported: %q", out)
	}
}

func TestCmdOrderSweepNudgeMailRun_FailingRigStoreReportsAndExitsZero(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	city := beads.NewMemStoreFrom(100, nudgeScopeStale("gm", 1, now), nil)
	rigA := &nudgeSweepListFailsOnLabel{MemStore: beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 1, now), nil)}

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepNudgeMailRun(beads.NudgesStore{Store: city}, beads.MailStore{Store: city}, nil, now,
		nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr,
		nudgeScopeOf(`rig "a"`, rigA))
	if code != 0 {
		t.Errorf("exit %d, want 0: one unreadable rig must not fail the run (same as a per-bead error)", code)
	}
	if !strings.Contains(stderr.String(), `rig "a"`) || !strings.Contains(stderr.String(), "ERROR") {
		t.Errorf("stderr = %q, want an ERROR line naming rig a", stderr.String())
	}
	if !strings.Contains(stdout.String(), "closed 1 nudge bead(s)") {
		t.Errorf("stdout = %q, want the city close still reported", stdout.String())
	}
}

func TestCmdOrderSweepNudgeMailDryRun_CountsRigStores(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	city := beads.NewMemStoreFrom(100, nudgeScopeStale("gm", 1, now), nil)
	rigA := beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 2, now), nil)

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepNudgeMailDryRun(beads.NudgesStore{Store: city}, beads.MailStore{Store: city}, nil, now,
		nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepDefaultUnreadMailTTL, false, &stdout, &stderr,
		nudgeScopeOf(`rig "a"`, rigA))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "[DRY RUN] nudge-mail-sweep: would close 3 nudge bead(s), 0 mail bead(s)  (no changes made)\n") {
		t.Errorf("dry-run summary must count rig stores: %q", out)
	}
	if !strings.Contains(out, `rig "a": would close 2 nudge bead(s)`) {
		t.Errorf("no per-store dry-run line for rig a: %q", out)
	}
	if got := mustBeadStatus(t, rigA, "ga-0"); got != "open" {
		t.Errorf("dry-run closed a rig bead (status %q)", got)
	}
}

// writeNudgeSweepRigCity lays out a file-backed city whose city.toml registers
// the named rigs, in order, and returns the city path and each rig's path.
func writeNudgeSweepRigCity(t *testing.T, rigNames ...string) (string, map[string]string) {
	t.Helper()
	cityDir := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_EVENTS", "")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_CITY_ROOT", cityDir)
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Chdir(cityDir)

	toml := "[workspace]\nname = \"test-city\"\n"
	paths := map[string]string{}
	for _, name := range rigNames {
		rigDir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		paths[name] = rigDir
		toml += fmt.Sprintf("\n[[rigs]]\nname = %q\npath = %q\n", name, rigDir)
	}
	writeFile(t, filepath.Join(cityDir, "city.toml"), toml)
	return cityDir, paths
}

// createStaleNudgeShadow opens the store at scopeRoot and files one open
// gc:nudge shadow in it.
func createStaleNudgeShadow(t *testing.T, scopeRoot, cityDir, nudgeID string) (beads.Store, string) {
	t.Helper()
	store, err := openStoreAtForCity(scopeRoot, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(%s): %v", scopeRoot, err)
	}
	b, err := store.Create(beads.Bead{
		Type:     nudgeBeadType,
		Labels:   []string{nudgeBeadLabel, "nudge:" + nudgeID},
		Metadata: map[string]string{"nudge_id": nudgeID, "state": "queued"},
	})
	if err != nil {
		t.Fatalf("Create nudge shadow in %s: %v", scopeRoot, err)
	}
	return store, b.ID
}

func TestCmdOrderSweepNudgeMail_ClosesStaleShadowsInRigStores(t *testing.T) {
	cityDir, rigs := writeNudgeSweepRigCity(t, "alpha", "beta")
	cityStore, cityBead := createStaleNudgeShadow(t, cityDir, cityDir, "city-1")
	alphaStore, alphaBead := createStaleNudgeShadow(t, rigs["alpha"], cityDir, "alpha-1")
	betaStore, betaBead := createStaleNudgeShadow(t, rigs["beta"], cityDir, "beta-1")
	time.Sleep(20 * time.Millisecond) // age the shadows past the 1ms TTL below

	var stdout, stderr bytes.Buffer
	code := cmdOrderSweepNudgeMail(time.Millisecond, 0, 0, true, true, false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderSweepNudgeMail = %d; stderr: %s", code, stderr.String())
	}
	for name, probe := range map[string]struct {
		store beads.Store
		id    string
	}{"city": {cityStore, cityBead}, "rig alpha": {alphaStore, alphaBead}, "rig beta": {betaStore, betaBead}} {
		if got := mustBeadStatus(t, probe.store, probe.id); got != "closed" {
			t.Errorf("%s shadow %s status = %q, want closed", name, probe.id, got)
		}
	}
	if !strings.Contains(stdout.String(), "closed 3 nudge bead(s)") {
		t.Errorf("stdout = %q, want a total of 3 nudge closes", stdout.String())
	}
	for _, label := range []string{`rig "alpha"`, `rig "beta"`} {
		if !strings.Contains(stdout.String(), label+": closed 1 nudge bead(s)") {
			t.Errorf("stdout = %q, want a per-store line for %s", stdout.String(), label)
		}
	}

	// A second run is a no-op, and the dry-run twin enumerates the same stores.
	stdout.Reset()
	stderr.Reset()
	if code := cmdOrderSweepNudgeMail(time.Millisecond, 0, 0, true, true, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("dry-run = %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "nothing to close") {
		t.Errorf("dry-run after a sweep = %q, want nothing to close", stdout.String())
	}
}

func TestCmdOrderSweepNudgeMail_DryRunSeesRigStores(t *testing.T) {
	cityDir, rigs := writeNudgeSweepRigCity(t, "alpha")
	_, _ = createStaleNudgeShadow(t, cityDir, cityDir, "city-1")
	alphaStore, alphaBead := createStaleNudgeShadow(t, rigs["alpha"], cityDir, "alpha-1")
	time.Sleep(20 * time.Millisecond)

	var stdout, stderr bytes.Buffer
	if code := cmdOrderSweepNudgeMail(time.Millisecond, 0, 0, true, true, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("dry-run = %d; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "would close 2 nudge bead(s)") {
		t.Errorf("dry-run stdout = %q, want 2 candidates (city + rig alpha)", stdout.String())
	}
	if got := mustBeadStatus(t, alphaStore, alphaBead); got != "open" {
		t.Errorf("dry-run closed the rig shadow (status %q)", got)
	}
}

func TestRunNudgeMailSweepWatchdog_ClosesStaleShadowsInRigStores(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cityPath := t.TempDir()
	city := beads.NewMemStoreFrom(100, nudgeScopeStale("gm", 1, now), nil)
	alpha := beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 2, now), nil)
	beta := beads.NewMemStoreFrom(100, nudgeScopeStale("gb", 1, now), nil)

	var stderr bytes.Buffer
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: filepath.Join(cityPath, "alpha")},
			{Name: "beta", Path: filepath.Join(cityPath, "beta")},
		},
	}
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test-city",
		cfg:                 cfg,
		standaloneCityStore: city,
		standaloneRigStores: map[string]beads.Store{"alpha": alpha, "beta": beta},
		stdout:              io.Discard,
		stderr:              &stderr,
		logPrefix:           "gc test",
	}
	cr.runNudgeMailSweepWatchdog(cfg, now)

	for store, ids := range map[beads.Store][]string{city: {"gm-0"}, alpha: {"ga-0", "ga-1"}, beta: {"gb-0"}} {
		for _, id := range ids {
			if got := mustBeadStatus(t, store, id); got != "closed" {
				t.Errorf("watchdog left %s %q, want closed", id, got)
			}
		}
	}
	if !strings.Contains(stderr.String(), "closed 4 nudge bead(s)") {
		t.Errorf("watchdog log = %q, want a total of 4 nudge closes", stderr.String())
	}
	if !strings.Contains(stderr.String(), `rig "alpha": closed 2 nudge bead(s)`) {
		t.Errorf("watchdog log = %q, want a per-store line for rig alpha", stderr.String())
	}
}

func TestRunNudgeMailSweepWatchdog_UnreadableRigDoesNotBlockTheRest(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cityPath := t.TempDir()
	city := beads.NewMemStoreFrom(100, nudgeScopeStale("gm", 1, now), nil)
	bad := &nudgeSweepListFailsOnLabel{MemStore: beads.NewMemStoreFrom(100, nudgeScopeStale("ga", 1, now), nil)}
	good := beads.NewMemStoreFrom(100, nudgeScopeStale("gb", 1, now), nil)

	var stderr bytes.Buffer
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "bad", Path: filepath.Join(cityPath, "bad")},
			{Name: "good", Path: filepath.Join(cityPath, "good")},
		},
	}
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test-city",
		cfg:                 cfg,
		standaloneCityStore: city,
		standaloneRigStores: map[string]beads.Store{"bad": bad, "good": good},
		stdout:              io.Discard,
		stderr:              &stderr,
		logPrefix:           "gc test",
	}
	cr.runNudgeMailSweepWatchdog(cfg, now)

	if got := mustBeadStatus(t, good, "gb-0"); got != "closed" {
		t.Errorf("good rig shadow status = %q, want closed despite the bad rig", got)
	}
	if !strings.Contains(stderr.String(), `rig "bad"`) {
		t.Errorf("watchdog log = %q, want the failing rig named", stderr.String())
	}
}
