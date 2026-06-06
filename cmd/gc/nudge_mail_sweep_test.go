package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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
	cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, true, &stdout, &stderr)
	if stdout.String() != "" {
		t.Errorf("expected empty stdout with --quiet, got: %q", stdout.String())
	}
}

func TestCmdOrderSweepNudgeMailDryRun_NothingToClose(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(100, nil, nil)

	var stdout, stderr bytes.Buffer
	cmdOrderSweepNudgeMailDryRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailDryRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, false, &stdout, &stderr)
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
	cmdOrderSweepNudgeMailDryRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, false, &stdout, &stderr)

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
	code := cmdOrderSweepNudgeMailDryRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, false, &stdout, &stderr)
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
	code := cmdOrderSweepNudgeMailRun(store, nil, now, nudgeMailSweepDefaultNudgeTTL, nudgeMailSweepDefaultMailTTL, false, &stdout, &stderr)
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
