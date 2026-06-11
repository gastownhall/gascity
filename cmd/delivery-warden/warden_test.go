package deliverywarden

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/delivery"
)

// FakeGitHub is a stub GitHub client for unit tests.
type FakeGitHub struct {
	OpenPRs   []PullRequest
	PRByURL   map[string]PullRequest
	SendCalls []string // records "owner/repo#N" for GetPR calls
	ListErr   error    // if non-nil, ListOpenPRs returns this error
}

func (f *FakeGitHub) ListOpenPRs(owner, repo string) ([]PullRequest, error) {
	if f.ListErr != nil {
		return nil, f.ListErr
	}
	var out []PullRequest
	for _, pr := range f.OpenPRs {
		if pr.Owner == owner && pr.Repo == repo {
			out = append(out, pr)
		}
	}
	return out, nil
}

func (f *FakeGitHub) GetPR(prURL string) (PullRequest, error) {
	pr, ok := f.PRByURL[prURL]
	if !ok {
		return PullRequest{}, fmt.Errorf("PR not found: %s", prURL)
	}
	return pr, nil
}

// FakeMail records escalation calls for assertion.
type FakeMail struct {
	Sent []MailMsg
}

type MailMsg struct {
	To, Subject, Body string
}

func (f *FakeMail) Send(_, to, subject, body string) error {
	f.Sent = append(f.Sent, MailMsg{To: to, Subject: subject, Body: body})
	return nil
}

// newDeliveryBead creates an open delivery bead with gc.pr_url and gc.phase set.
func newDeliveryBead(store beads.Store, prURL, phase string) beads.Bead {
	b, err := store.Create(beads.Bead{
		Title: "test delivery bead",
		Metadata: map[string]string{
			delivery.MetaKeyPRURL: prURL,
			delivery.MetaKeyPhase: phase,
		},
	})
	if err != nil {
		panic(err)
	}
	return b
}

// closeBead closes a bead via the store (simulating a reviewer closing it).
func closeBead(store beads.Store, id string) {
	if err := store.Close(id); err != nil {
		panic(err)
	}
}

func TestRepairOrphan(t *testing.T) {
	store := beads.NewMemStore()

	// Arrange: PR #42 exists on GitHub (open), head = gc/bead-abc
	prURL := "https://github.com/gastownhall/gascity/pull/42"
	pr := PullRequest{
		Owner:   "gastownhall",
		Repo:    "gascity",
		Number:  42,
		URL:     prURL,
		HeadRef: "gc/bead-abc",
		State:   "OPEN",
	}
	gh := &FakeGitHub{
		OpenPRs: []PullRequest{pr},
		PRByURL: map[string]PullRequest{prURL: pr},
	}
	mail := &FakeMail{}

	// Source bead for bead-abc was closed by the reviewer.
	sourceBead, err := store.Create(beads.Bead{
		Title:    "bead-abc source",
		Metadata: map[string]string{delivery.MetaKeyPRURL: prURL, delivery.MetaKeyPhase: delivery.PhaseMerged},
	})
	if err != nil {
		t.Fatalf("create source bead: %v", err)
	}
	closeBead(store, sourceBead.ID)

	// No decision bead exists (that's the orphan condition).
	w := NewWarden(store, gh, mail)

	if err := w.RepairOrphan("gastownhall", "gascity"); err != nil {
		t.Fatalf("RepairOrphan: %v", err)
	}

	// A decision bead should now exist with gc.merge_pr=42, gc.merge_repo, gc.merge_source.
	results, err := store.ListByMetadata(map[string]string{"gc.merge_pr": "42"}, 0)
	if err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 decision bead, got %d", len(results))
	}
	got := results[0]
	if got.Type != "decision" {
		t.Errorf("decision bead type: got %q, want %q", got.Type, "decision")
	}
	if got.Metadata["gc.merge_repo"] == "" {
		t.Error("gc.merge_repo not set on decision bead")
	}
	if got.Metadata["gc.merge_source"] == "" {
		t.Error("gc.merge_source not set on decision bead")
	}
}

func TestRepairOrphan_Idempotent(t *testing.T) {
	store := beads.NewMemStore()
	prURL := "https://github.com/gastownhall/gascity/pull/55"
	pr := PullRequest{
		Owner: "gastownhall", Repo: "gascity", Number: 55, URL: prURL,
		HeadRef: "gc/bead-xyz", State: "OPEN",
	}
	gh := &FakeGitHub{OpenPRs: []PullRequest{pr}, PRByURL: map[string]PullRequest{prURL: pr}}
	mail := &FakeMail{}

	// Source bead closed.
	src, _ := store.Create(beads.Bead{
		Title:    "bead-xyz source",
		Metadata: map[string]string{delivery.MetaKeyPRURL: prURL, delivery.MetaKeyPhase: delivery.PhaseMerged},
	})
	closeBead(store, src.ID)

	w := NewWarden(store, gh, mail)

	// First sweep creates the decision bead.
	if err := w.RepairOrphan("gastownhall", "gascity"); err != nil {
		t.Fatalf("first RepairOrphan: %v", err)
	}
	// Second sweep must not create a duplicate.
	if err := w.RepairOrphan("gastownhall", "gascity"); err != nil {
		t.Fatalf("second RepairOrphan: %v", err)
	}

	results, err := store.ListByMetadata(map[string]string{"gc.merge_pr": "55"}, 0)
	if err != nil {
		t.Fatalf("ListByMetadata: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("idempotency: want 1 decision bead, got %d", len(results))
	}
}

func TestRepairZombie_Merged(t *testing.T) {
	store := beads.NewMemStore()
	prURL := "https://github.com/gastownhall/gascity/pull/99"
	mergedAt := time.Now().Add(-10 * time.Minute)
	gh := &FakeGitHub{
		PRByURL: map[string]PullRequest{
			prURL: {URL: prURL, State: "MERGED", MergedAt: &mergedAt},
		},
	}
	mail := &FakeMail{}

	// Open delivery bead with gc.pr_url pointing to a merged PR.
	b := newDeliveryBead(store, prURL, delivery.PhaseReviewPending)
	w := NewWarden(store, gh, mail)

	if err := w.RepairZombie(); err != nil {
		t.Fatalf("RepairZombie: %v", err)
	}

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Status != "closed" {
		t.Errorf("bead status: got %q, want %q", got.Status, "closed")
	}
	if got.Metadata[delivery.MetaKeyPhase] != delivery.PhaseMerged {
		t.Errorf("bead phase: got %q, want %q", got.Metadata[delivery.MetaKeyPhase], delivery.PhaseMerged)
	}
}

func TestRepairZombie_Closed(t *testing.T) {
	store := beads.NewMemStore()
	prURL := "https://github.com/gastownhall/gascity/pull/77"
	gh := &FakeGitHub{
		PRByURL: map[string]PullRequest{
			prURL: {URL: prURL, State: "CLOSED"},
		},
	}
	mail := &FakeMail{}

	b := newDeliveryBead(store, prURL, delivery.PhaseCIPending)
	w := NewWarden(store, gh, mail)

	if err := w.RepairZombie(); err != nil {
		t.Fatalf("RepairZombie: %v", err)
	}

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Status != "closed" {
		t.Errorf("bead status: got %q, want %q", got.Status, "closed")
	}
	if got.Metadata[delivery.MetaKeyPhase] != delivery.PhaseAbandoned {
		t.Errorf("bead phase: got %q, want %q", got.Metadata[delivery.MetaKeyPhase], delivery.PhaseAbandoned)
	}
}

// newDeliveryBeadWithPhaseEnteredAt creates a delivery bead with gc.phase_entered_at set.
func newDeliveryBeadWithPhaseEnteredAt(store beads.Store, prURL, phase string, enteredAt time.Time) beads.Bead {
	b := newDeliveryBead(store, prURL, phase)
	if err := store.SetMetadata(b.ID, delivery.MetaKeyPhaseEnteredAt, enteredAt.UTC().Format(time.RFC3339)); err != nil {
		panic(err)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		panic(err)
	}
	return got
}

func TestPhaseDwellRetry(t *testing.T) {
	store := beads.NewMemStore()

	// Bead in review-pending for 65 min — past the 60-min budget.
	enteredAt := time.Now().Add(-65 * time.Minute)
	b := newDeliveryBeadWithPhaseEnteredAt(store, "https://github.com/org/repo/pull/10", delivery.PhaseReviewPending, enteredAt)

	gh := &FakeGitHub{}
	mail := &FakeMail{}
	w := NewWarden(store, gh, mail)

	if err := w.CheckPhaseDwell(); err != nil {
		t.Fatalf("CheckPhaseDwell: %v", err)
	}

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Metadata[delivery.MetaKeyWardenRetries] != "1" {
		t.Errorf("gc.warden_retries: got %q, want %q", got.Metadata[delivery.MetaKeyWardenRetries], "1")
	}
	if len(mail.Sent) == 0 {
		t.Error("want recovery nudge sent, got none")
	}
}

func TestPhaseDwellEscalate(t *testing.T) {
	store := beads.NewMemStore()

	// Bead in review-pending 65 min, already retried 3 times → escalation path.
	enteredAt := time.Now().Add(-65 * time.Minute)
	b := newDeliveryBeadWithPhaseEnteredAt(store, "https://github.com/org/repo/pull/20", delivery.PhaseReviewPending, enteredAt)
	if err := store.SetMetadata(b.ID, delivery.MetaKeyWardenRetries, "3"); err != nil {
		t.Fatalf("set retries: %v", err)
	}

	gh := &FakeGitHub{}
	mail := &FakeMail{}
	w := NewWarden(store, gh, mail)

	if err := w.CheckPhaseDwell(); err != nil {
		t.Fatalf("first CheckPhaseDwell: %v", err)
	}

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Metadata[delivery.MetaKeyWardenEscalated] == "" {
		t.Error("gc.warden_escalated not set after escalation")
	}
	if len(mail.Sent) != 1 {
		t.Errorf("want 1 escalation mail, got %d", len(mail.Sent))
	}

	// Second run must be a no-op.
	mail.Sent = nil
	if err := w.CheckPhaseDwell(); err != nil {
		t.Fatalf("second CheckPhaseDwell: %v", err)
	}
	if len(mail.Sent) != 0 {
		t.Errorf("second run: want 0 mails (idempotent), got %d", len(mail.Sent))
	}
}

func TestGlobalLifetime(t *testing.T) {
	store := beads.NewMemStore()

	// Bead just created (MemStore stamps CreatedAt = now). The warden clock
	// runs 25 h in the future so the bead appears past the 24 h max-lifetime.
	prURL := "https://github.com/org/repo/pull/30"
	b := newDeliveryBead(store, prURL, delivery.PhaseReviewPending)

	gh := &FakeGitHub{}
	mail := &FakeMail{}
	w := NewWarden(store, gh, mail)
	w.clock = func() time.Time { return time.Now().Add(25 * time.Hour) }

	// First run: escalation fires.
	if err := w.CheckGlobalLifetime(); err != nil {
		t.Fatalf("first CheckGlobalLifetime: %v", err)
	}

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if got.Metadata[delivery.MetaKeyWardenEscalated] != "global" {
		t.Errorf("gc.warden_escalated: got %q, want %q", got.Metadata[delivery.MetaKeyWardenEscalated], "global")
	}
	if len(mail.Sent) != 1 {
		t.Errorf("want 1 escalation mail, got %d", len(mail.Sent))
	}

	// Second run: idempotent — no additional mails.
	mail.Sent = nil
	if err := w.CheckGlobalLifetime(); err != nil {
		t.Fatalf("second CheckGlobalLifetime: %v", err)
	}
	if len(mail.Sent) != 0 {
		t.Errorf("second run: want 0 mails (idempotent), got %d", len(mail.Sent))
	}
}

func TestRepairZombie_OpenPRNotTouched(t *testing.T) {
	store := beads.NewMemStore()
	prURL := "https://github.com/gastownhall/gascity/pull/11"
	gh := &FakeGitHub{
		PRByURL: map[string]PullRequest{
			prURL: {URL: prURL, State: "OPEN"},
		},
	}
	mail := &FakeMail{}
	b := newDeliveryBead(store, prURL, delivery.PhaseReviewPending)
	w := NewWarden(store, gh, mail)

	if err := w.RepairZombie(); err != nil {
		t.Fatalf("RepairZombie: %v", err)
	}

	got, _ := store.Get(b.ID)
	if got.Status == "closed" {
		t.Error("open-PR bead must not be closed by zombie repair")
	}
	if !strings.HasPrefix(got.Status, "open") && got.Status != "in_progress" {
		t.Errorf("bead status: got %q, want open or in_progress", got.Status)
	}
}

func TestWardenNoop(t *testing.T) {
	// Empty store + empty GH PR list → sweep exits cleanly, heartbeat written,
	// no store mutations.
	store := beads.NewMemStore()
	gh := &FakeGitHub{}
	mail := &FakeMail{}
	w := NewWarden(store, gh, mail)

	heartbeatFile := t.TempDir() + "/heartbeat"
	repos := [][2]string{{"gastownhall", "gascity"}}

	if err := w.Sweep(repos, heartbeatFile); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Heartbeat file must exist and contain a unix timestamp.
	data, err := os.ReadFile(heartbeatFile)
	if err != nil {
		t.Fatalf("heartbeat not written: %v", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		t.Error("heartbeat file is empty")
	}

	// No store mutations — the store should still be empty.
	open, err := store.ListOpen()
	if err != nil {
		t.Fatalf("store.ListOpen: %v", err)
	}
	if len(open) != 0 {
		t.Errorf("want 0 open beads, got %d", len(open))
	}

	// No mail sent.
	if len(mail.Sent) != 0 {
		t.Errorf("want 0 mails, got %d", len(mail.Sent))
	}
}

// TestSweepHeartbeatWrittenOnError verifies that the heartbeat file is written
// even when an earlier sweep step returns an error — catches the
// heartbeat-gated-on-zero-errors regression.
func TestSweepHeartbeatWrittenOnError(t *testing.T) {
	store := beads.NewMemStore()
	// Make ListOpenPRs fail so RepairOrphan returns an error.
	gh := &FakeGitHub{ListErr: fmt.Errorf("github unavailable")}
	mail := &FakeMail{}
	w := NewWarden(store, gh, mail)

	heartbeatFile := t.TempDir() + "/heartbeat"
	repos := [][2]string{{"gastownhall", "gascity"}}

	// Sweep should return a non-nil error (GitHub failed).
	err := w.Sweep(repos, heartbeatFile)
	if err == nil {
		t.Fatal("want error from Sweep when GitHub fails, got nil")
	}

	// Despite the error, the heartbeat must have been written.
	if _, statErr := os.Stat(heartbeatFile); statErr != nil {
		t.Errorf("heartbeat not written despite warden running: %v", statErr)
	}
}

// TestRepairZombie_LogsGetPRError verifies that a GetPR failure is written to
// the warden's log writer rather than silently swallowed — catches the
// zombie-repair-no-logging regression.
func TestRepairZombie_LogsGetPRError(t *testing.T) {
	store := beads.NewMemStore()
	// Bead whose PR URL is not registered in FakeGitHub → GetPR returns error.
	prURL := "https://github.com/org/repo/pull/50"
	newDeliveryBead(store, prURL, delivery.PhaseReviewPending)

	gh := &FakeGitHub{PRByURL: map[string]PullRequest{}} // URL absent → error
	mail := &FakeMail{}
	w := NewWarden(store, gh, mail)

	var logBuf strings.Builder
	w.log = &logBuf

	if err := w.RepairZombie(); err != nil {
		t.Fatalf("RepairZombie: %v", err)
	}

	if logBuf.Len() == 0 {
		t.Error("want log output for GetPR error, got none")
	}
}

// TestCheckPhaseDwell_BootstrapsRFC3339 verifies that CheckPhaseDwell bootstraps
// gc.phase_entered_at in RFC3339 format so delivery.PhaseEnteredAt reads the
// same timestamp back correctly. Catches the phase_entered_at-format-mismatch
// regression (warden wrote Unix epoch, doctor read RFC3339).
func TestCheckPhaseDwell_BootstrapsRFC3339(t *testing.T) {
	store := beads.NewMemStore()
	prURL := "https://github.com/org/repo/pull/99"
	b := newDeliveryBead(store, prURL, delivery.PhaseReviewPending)

	gh := &FakeGitHub{}
	mail := &FakeMail{}
	w := NewWarden(store, gh, mail)

	// First call: warden bootstraps gc.phase_entered_at and skips this tick.
	if err := w.CheckPhaseDwell(); err != nil {
		t.Fatalf("CheckPhaseDwell bootstrap: %v", err)
	}

	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}

	raw := got.Metadata[delivery.MetaKeyPhaseEnteredAt]
	if raw == "" {
		t.Fatal("gc.phase_entered_at not set after bootstrap")
	}

	// Value must be parseable as RFC3339 — the canonical format that delivery.PhaseEnteredAt reads first.
	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		t.Errorf("gc.phase_entered_at is not RFC3339: %q: %v", raw, err)
	}

	// delivery.PhaseAge must report near-zero (just bootstrapped).
	if age := delivery.PhaseAge(got); age < 0 || age > 2*time.Second {
		t.Errorf("PhaseAge after RFC3339 bootstrap: got %v, want ≤ 2s", age)
	}
}

// TestPhaseDwellConflicted verifies that a bead stuck in the "conflicted" phase
// past its 60-minute budget triggers a recovery nudge. Catches the
// phase_conflicted-missing-from-dwell-budgets regression.
func TestPhaseDwellConflicted(t *testing.T) {
	store := beads.NewMemStore()

	// Bead in conflicted for 70 min — past the 60-min budget.
	enteredAt := time.Now().Add(-70 * time.Minute)
	newDeliveryBeadWithPhaseEnteredAt(store, "https://github.com/org/repo/pull/40", delivery.PhaseConflicted, enteredAt)

	gh := &FakeGitHub{}
	mail := &FakeMail{}
	w := NewWarden(store, gh, mail)

	if err := w.CheckPhaseDwell(); err != nil {
		t.Fatalf("CheckPhaseDwell: %v", err)
	}

	if len(mail.Sent) == 0 {
		t.Error("want recovery nudge sent for conflicted phase, got none")
	}
}
