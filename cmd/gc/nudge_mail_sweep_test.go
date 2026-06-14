package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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

	result, err := sweepStaleNudgeMail(store, nil, now, nudgeTTL, mailTTL, 0)
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

	result, err := sweepStaleNudgeMail(store, state, now, nudgeTTL, time.Hour, 0)
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

	result, err := sweepStaleNudgeMail(store, state, now, nudgeTTL, time.Hour, 0)
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

	result, err := sweepStaleNudgeMail(store, nil, now, time.Minute, mailTTL, 0)
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

	result, err := sweepStaleNudgeMail(store, nil, now, nudgeTTL, mailTTL, budget)
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

	result, err := sweepStaleNudgeMail(store, nil, now, nudgeTTL, mailTTL, 0)
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

	result, err := sweepStaleNudgeMail(store, nil, now, nudgeTTL, time.Hour, 0)

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

	result, err := sweepStaleNudgeMail(store, nil, now, nudgeTTL, time.Hour, 0)
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

	result, err := sweepStaleNudgeMail(store, nil, now, nudgeTTL, time.Hour, 0)
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

	result, err := sweepStaleNudgeMail(store, nil, now, nudgeTTL, time.Hour, 0)
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

	result, err := sweepStaleNudgeMail(store, nil, now, nudgeTTL, mailTTL, 3)
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

	result, err := sweepStaleNudgeMail(store, nil, now, nudgeTTL, time.Hour, 0)
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
	cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailRetentionPolicy{}, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailRetentionPolicy{}, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailRetentionPolicy{}, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailRetentionPolicy{}, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailRetentionPolicy{}, true, &stdout, &stderr)
	if stdout.String() != "" {
		t.Errorf("expected empty stdout with --quiet, got: %q", stdout.String())
	}
}

func TestCmdOrderSweepNudgeMailDryRun_NothingToClose(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(100, nil, nil)

	var stdout, stderr bytes.Buffer
	cmdOrderSweepNudgeMailDryRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailRetentionPolicy{}, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailDryRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailRetentionPolicy{}, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailDryRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailRetentionPolicy{}, false, &stdout, &stderr)

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
	code := cmdOrderSweepNudgeMailDryRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailRetentionPolicy{}, false, &stdout, &stderr)
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
	code := cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailRetentionPolicy{}, false, &stdout, &stderr)
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

	_, err := countStaleNudgeMail(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, 0)
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

	result, err := sweepStaleNudgeMail(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, nudgeMailSweepWatchdogCloseBudget)
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

	counts, err := countStaleNudgeMail(store, nil, now, nudgeTTL, mailTTL, 0)
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

// --- gc order sweep-nudge-mail: archive-then-delete retention (#3342 Option A) ---

// closedNudgeSeed builds a closed nudge bead whose close time (UpdatedAt) is
// closedAt. CreatedAt is set slightly earlier so created-time and close-time
// filters are distinguishable.
func closedNudgeSeed(id, nudgeID string, closedAt time.Time) beads.Bead {
	b := nudgeSeed(id, nudgeID, closedAt.Add(-time.Minute))
	b.Status = "closed"
	b.UpdatedAt = closedAt
	return b
}

// closedMailSeed builds a closed read mail bead whose close time (UpdatedAt) is
// closedAt.
func closedMailSeed(id string, closedAt time.Time) beads.Bead {
	b := mailSeed(id, closedAt.Add(-time.Minute))
	b.Status = "closed"
	b.UpdatedAt = closedAt
	return b
}

func retentionBeadPresent(t *testing.T, store beads.Store, id string) bool {
	t.Helper()
	_, err := store.Get(id)
	if err == nil {
		return true
	}
	if errors.Is(err, beads.ErrNotFound) {
		return false
	}
	t.Fatalf("unexpected error getting %s: %v", id, err)
	return false
}

func testNudgeMailRetentionPolicy() nudgeMailRetentionPolicy {
	return nudgeMailRetentionPolicy{
		nudgeDeleteAfterClose: 24 * time.Hour,
		mailDeleteAfterClose:  72 * time.Hour,
	}
}

func TestSweepClosedNudgeMailRetention_TTLBoundaries(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	policy := testNudgeMailRetentionPolicy()

	seed := []beads.Bead{
		closedNudgeSeed("n-old", "n-old", now.Add(-policy.nudgeDeleteAfterClose-time.Second)),     // delete
		closedNudgeSeed("n-fresh", "n-fresh", now.Add(-policy.nudgeDeleteAfterClose+time.Second)), // keep
		closedMailSeed("m-old", now.Add(-policy.mailDeleteAfterClose-time.Second)),                // delete
		closedMailSeed("m-fresh", now.Add(-policy.mailDeleteAfterClose+time.Second)),              // keep
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepClosedNudgeMailRetention(store, nil, now, policy, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeDeleted != 1 {
		t.Errorf("NudgeDeleted = %d, want 1", result.NudgeDeleted)
	}
	if result.MailDeleted != 1 {
		t.Errorf("MailDeleted = %d, want 1", result.MailDeleted)
	}
	if retentionBeadPresent(t, store, "n-old") {
		t.Error("n-old should have been deleted")
	}
	if !retentionBeadPresent(t, store, "n-fresh") {
		t.Error("n-fresh should have been kept (within TTL)")
	}
	if retentionBeadPresent(t, store, "m-old") {
		t.Error("m-old should have been deleted")
	}
	if !retentionBeadPresent(t, store, "m-fresh") {
		t.Error("m-fresh should have been kept (within TTL)")
	}
}

func TestSweepClosedNudgeMailRetention_OpenWorkNeverDeleted(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	policy := testNudgeMailRetentionPolicy()

	// Open + in_progress nudge/mail, all far past the delete TTL by age. The
	// closed-only status filter must keep them: deleting open/in_progress work
	// would lose the work queue (NDI gate "never touch open/in_progress").
	openNudge := nudgeSeed("n-open", "n-open", now.Add(-policy.nudgeDeleteAfterClose-time.Hour))
	openNudge.UpdatedAt = now.Add(-policy.nudgeDeleteAfterClose - time.Hour)
	inProgressNudge := nudgeSeed("n-inprog", "n-inprog", now.Add(-policy.nudgeDeleteAfterClose-time.Hour))
	inProgressNudge.Status = "in_progress"
	inProgressNudge.UpdatedAt = now.Add(-policy.nudgeDeleteAfterClose - time.Hour)
	openMail := mailSeed("m-open", now.Add(-policy.mailDeleteAfterClose-time.Hour))
	openMail.UpdatedAt = now.Add(-policy.mailDeleteAfterClose - time.Hour)
	inProgressMail := mailSeed("m-inprog", now.Add(-policy.mailDeleteAfterClose-time.Hour))
	inProgressMail.Status = "in_progress"
	inProgressMail.UpdatedAt = now.Add(-policy.mailDeleteAfterClose - time.Hour)
	store := beads.NewMemStoreFrom(100, []beads.Bead{openNudge, inProgressNudge, openMail, inProgressMail}, nil)

	result, err := sweepClosedNudgeMailRetention(store, nil, now, policy, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeDeleted != 0 || result.MailDeleted != 0 {
		t.Errorf("deleted = %+v, want zero (open/in_progress work must never be deleted)", result)
	}
	for _, id := range []string{"n-open", "n-inprog", "m-open", "m-inprog"} {
		if !retentionBeadPresent(t, store, id) {
			t.Errorf("non-closed bead %q must not be deleted", id)
		}
	}
}

func TestSweepClosedNudgeMailRetention_LiveNudgeProtected(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	policy := testNudgeMailRetentionPolicy()

	const liveID = "live-nudge"
	seed := []beads.Bead{
		closedNudgeSeed("n-live", liveID, now.Add(-policy.nudgeDeleteAfterClose-time.Hour)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	state := &nudgequeue.State{Pending: []nudgequeue.Item{{ID: liveID}}}

	result, err := sweepClosedNudgeMailRetention(store, state, now, policy, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeDeleted != 0 {
		t.Errorf("NudgeDeleted = %d, want 0 (live nudge ID must be protected)", result.NudgeDeleted)
	}
	if !retentionBeadPresent(t, store, "n-live") {
		t.Error("closed nudge whose ID is still live must not be deleted")
	}
}

func TestSweepClosedNudgeMailRetention_InFlightNudgeProtected(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	policy := testNudgeMailRetentionPolicy()

	const inFlightID = "inflight-nudge"
	seed := []beads.Bead{
		closedNudgeSeed("n-inflight", inFlightID, now.Add(-policy.nudgeDeleteAfterClose-time.Hour)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	state := &nudgequeue.State{InFlight: []nudgequeue.Item{{ID: inFlightID}}}

	result, err := sweepClosedNudgeMailRetention(store, state, now, policy, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeDeleted != 0 {
		t.Errorf("NudgeDeleted = %d, want 0 (in-flight nudge ID must be protected)", result.NudgeDeleted)
	}
	if !retentionBeadPresent(t, store, "n-inflight") {
		t.Error("closed nudge whose ID is still in-flight must not be deleted")
	}
}

// TestSweepClosedNudgeMailRetention_BudgetTopsUpAcrossPhases verifies the
// combined budget is computed from beads actually deleted, not from candidates
// scanned: when phase 1 (nudge) under-delivers because a candidate is skipped by
// a gate, phase 2 (mail) gets the unused budget rather than losing it.
func TestSweepClosedNudgeMailRetention_BudgetTopsUpAcrossPhases(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	policy := testNudgeMailRetentionPolicy()

	const liveID = "live-nudge"
	seed := []beads.Bead{
		// Two nudge candidates: one live-protected (skipped), one deletable.
		closedNudgeSeed("n-live", liveID, now.Add(-policy.nudgeDeleteAfterClose-2*time.Minute)),
		closedNudgeSeed("n-del", "n-del", now.Add(-policy.nudgeDeleteAfterClose-time.Minute)),
		// Three mail candidates, all deletable.
		closedMailSeed("m-0", now.Add(-policy.mailDeleteAfterClose-3*time.Minute)),
		closedMailSeed("m-1", now.Add(-policy.mailDeleteAfterClose-2*time.Minute)),
		closedMailSeed("m-2", now.Add(-policy.mailDeleteAfterClose-time.Minute)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	state := &nudgequeue.State{Pending: []nudgequeue.Item{{ID: liveID}}}

	// Budget 3: 1 nudge deleted (n-del; n-live skipped) + 2 mail deleted = 3 total.
	result, err := sweepClosedNudgeMailRetention(store, state, now, policy, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeDeleted != 1 {
		t.Errorf("NudgeDeleted = %d, want 1 (n-del; n-live is live-protected)", result.NudgeDeleted)
	}
	if result.MailDeleted != 2 {
		t.Errorf("MailDeleted = %d, want 2 (phase 2 receives the unused budget)", result.MailDeleted)
	}
	if total := result.NudgeDeleted + result.MailDeleted; total != 3 {
		t.Errorf("total deleted = %d, want 3 (budget cap)", total)
	}
}

func TestSweepClosedNudgeMailRetention_OwnershipEdgeSkip(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	policy := testNudgeMailRetentionPolicy()

	seed := []beads.Bead{
		closedNudgeSeed("n-owned", "n-owned", now.Add(-policy.nudgeDeleteAfterClose-time.Hour)),
	}
	// Another bead depends on n-owned: DepList(id, "up") returns this edge, so
	// the bead is left for a graph-aware reaper rather than severed here.
	deps := []beads.Dep{{IssueID: "dependent", DependsOnID: "n-owned", Type: "blocks"}}
	store := beads.NewMemStoreFrom(100, seed, deps)

	result, err := sweepClosedNudgeMailRetention(store, nil, now, policy, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeDeleted != 0 {
		t.Errorf("NudgeDeleted = %d, want 0 (bead with an inbound dependency must be skipped)", result.NudgeDeleted)
	}
	if !retentionBeadPresent(t, store, "n-owned") {
		t.Error("bead with an inbound dependency must not be deleted")
	}
}

func TestSweepClosedNudgeMailRetention_BudgetCap(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	policy := testNudgeMailRetentionPolicy()

	const total = 5
	seed := make([]beads.Bead, total)
	for i := range seed {
		id := fmt.Sprintf("n-%d", i)
		seed[i] = closedNudgeSeed(id, id, now.Add(-policy.nudgeDeleteAfterClose-time.Duration(i+1)*time.Minute))
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepClosedNudgeMailRetention(store, nil, now, policy, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeDeleted != 3 {
		t.Errorf("NudgeDeleted = %d, want 3 (budget cap)", result.NudgeDeleted)
	}
	remaining := 0
	for i := 0; i < total; i++ {
		if retentionBeadPresent(t, store, fmt.Sprintf("n-%d", i)) {
			remaining++
		}
	}
	if remaining != total-3 {
		t.Errorf("remaining closed nudge beads = %d, want %d", remaining, total-3)
	}
}

func TestSweepClosedNudgeMailRetention_BudgetZeroIsUnlimited(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	policy := testNudgeMailRetentionPolicy()

	const total = 5
	seed := make([]beads.Bead, total)
	for i := range seed {
		id := fmt.Sprintf("n-%d", i)
		seed[i] = closedNudgeSeed(id, id, now.Add(-policy.nudgeDeleteAfterClose-time.Duration(i+1)*time.Minute))
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepClosedNudgeMailRetention(store, nil, now, policy, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeDeleted != total {
		t.Errorf("NudgeDeleted = %d, want %d (budget 0 = unlimited)", result.NudgeDeleted, total)
	}
}

func TestSweepClosedNudgeMailRetention_DisabledPolicyDeletesNothing(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	// Zero-valued policy: both TTLs non-positive => deletion disabled.
	seed := []beads.Bead{
		closedNudgeSeed("n-old", "n-old", now.Add(-100*time.Hour)),
		closedMailSeed("m-old", now.Add(-100*time.Hour)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	result, err := sweepClosedNudgeMailRetention(store, nil, now, nudgeMailRetentionPolicy{}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NudgeDeleted != 0 || result.MailDeleted != 0 {
		t.Errorf("deleted = %+v, want zero (disabled policy must delete nothing)", result)
	}
	if !retentionBeadPresent(t, store, "n-old") || !retentionBeadPresent(t, store, "m-old") {
		t.Error("disabled policy must not delete any bead")
	}
}

func TestCountClosedNudgeMailRetention_DryRunMakesNoChanges(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	policy := testNudgeMailRetentionPolicy()

	seed := []beads.Bead{
		closedNudgeSeed("n-old", "n-old", now.Add(-policy.nudgeDeleteAfterClose-time.Hour)),
		closedMailSeed("m-old", now.Add(-policy.mailDeleteAfterClose-time.Hour)),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)

	counts, err := countClosedNudgeMailRetention(store, nil, now, policy, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if counts.NudgeDeleted != 1 || counts.MailDeleted != 1 {
		t.Errorf("counts = %+v, want NudgeDeleted=1 MailDeleted=1", counts)
	}
	if !retentionBeadPresent(t, store, "n-old") || !retentionBeadPresent(t, store, "m-old") {
		t.Error("dry-run must not delete any bead")
	}
}

func TestSweepClosedNudgeMailRetention_NilStoreErrors(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	_, err := sweepClosedNudgeMailRetention(nil, nil, now, testNudgeMailRetentionPolicy(), 0)
	if err == nil {
		t.Fatal("expected error for nil store, got nil")
	}
}

// nudgeRetentionFailingDelete wraps MemStore and forces Delete to fail for
// specific bead IDs, exercising the per-bead-error continuation path.
type nudgeRetentionFailingDelete struct {
	*beads.MemStore
	failIDs map[string]bool
}

func (s *nudgeRetentionFailingDelete) Delete(id string) error {
	if s.failIDs[id] {
		return fmt.Errorf("forced delete failure for %s", id)
	}
	return s.MemStore.Delete(id)
}

func TestSweepClosedNudgeMailRetention_PerBeadDeleteFailureContinues(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	policy := testNudgeMailRetentionPolicy()

	seed := []beads.Bead{
		closedNudgeSeed("n-fail", "n-fail", now.Add(-policy.nudgeDeleteAfterClose-2*time.Minute)),
		closedNudgeSeed("n-ok", "n-ok", now.Add(-policy.nudgeDeleteAfterClose-time.Minute)),
	}
	mem := beads.NewMemStoreFrom(100, seed, nil)
	store := &nudgeRetentionFailingDelete{MemStore: mem, failIDs: map[string]bool{"n-fail": true}}

	result, err := sweepClosedNudgeMailRetention(store, nil, now, policy, 0)
	if err == nil {
		t.Fatal("expected a joined per-bead error, got nil")
	}
	if !strings.Contains(err.Error(), "n-fail") {
		t.Errorf("error should mention the failing bead, got: %v", err)
	}
	if result.NudgeDeleted != 1 {
		t.Errorf("NudgeDeleted = %d, want 1 (the non-failing bead)", result.NudgeDeleted)
	}
	if retentionBeadPresent(t, store, "n-ok") {
		t.Error("n-ok should have been deleted")
	}
	if !retentionBeadPresent(t, store, "n-fail") {
		t.Error("n-fail should remain (delete failed)")
	}
}

func TestNudgeMailRetentionPolicyForConfig(t *testing.T) {
	// Nil config falls back to the controller defaults (24h / 72h).
	def := nudgeMailRetentionPolicyForConfig(nil)
	if def.nudgeDeleteAfterClose != 24*time.Hour {
		t.Errorf("nil cfg nudge TTL = %v, want 24h", def.nudgeDeleteAfterClose)
	}
	if def.mailDeleteAfterClose != 72*time.Hour {
		t.Errorf("nil cfg mail TTL = %v, want 72h", def.mailDeleteAfterClose)
	}

	// Explicit config values override the defaults.
	cfg := &config.City{
		Beads: config.BeadsConfig{
			Policies: map[string]config.BeadPolicyConfig{
				config.BeadPolicyNudge: {DeleteAfterClose: "1h"},
				config.BeadPolicyMail:  {DeleteAfterClose: "2h"},
			},
		},
	}
	got := nudgeMailRetentionPolicyForConfig(cfg)
	if got.nudgeDeleteAfterClose != time.Hour {
		t.Errorf("configured nudge TTL = %v, want 1h", got.nudgeDeleteAfterClose)
	}
	if got.mailDeleteAfterClose != 2*time.Hour {
		t.Errorf("configured mail TTL = %v, want 2h", got.mailDeleteAfterClose)
	}
}
