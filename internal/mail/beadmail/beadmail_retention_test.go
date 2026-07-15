package beadmail

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/mail"
)

// readMailSeed builds a seed Bead for NewMemStoreFrom representing an open read
// message bead created at createdAt. opts mutate the bead (e.g. drop the "read"
// label or mark it closed) so a single helper covers every candidate variant.
func readMailSeed(id string, createdAt time.Time, opts ...func(*beads.Bead)) beads.Bead {
	b := beads.Bead{
		ID:        id,
		Type:      "message",
		Status:    "open",
		Labels:    []string{"read"},
		CreatedAt: createdAt,
	}
	for _, opt := range opts {
		opt(&b)
	}
	return b
}

// closeErrStore errors on Close for the configured IDs, exercising the
// per-bead (non-fatal) error path of the retention sweep.
type closeErrStore struct {
	*beads.MemStore
	failClose map[string]error
}

func (s closeErrStore) Close(id string) error {
	if err, ok := s.failClose[id]; ok {
		return err
	}
	return s.MemStore.Close(id)
}

// listErrStore errors on any message-typed List, exercising the fatal
// candidate-listing error path of the retention sweep.
type listErrStore struct {
	*beads.MemStore
	err error
}

func (s listErrStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == "message" {
		return nil, s.err
	}
	return s.MemStore.List(query)
}

// deleteTrackStore records every successful Delete and can be told to fail a
// specific ID, mirroring the wisp-GC test double for the purge path.
type deleteTrackStore struct {
	*beads.MemStore
	failDelete map[string]error
	deleted    []string
}

func (s *deleteTrackStore) Delete(id string) error {
	if err, ok := s.failDelete[id]; ok {
		return err
	}
	if err := s.MemStore.Delete(id); err != nil {
		return err
	}
	s.deleted = append(s.deleted, id)
	return nil
}

func TestSweepReadMessagesBefore_ClosesAgedReadMailWithReason(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now
	old := now.Add(-time.Minute)
	fresh := now.Add(time.Minute)

	seed := []beads.Bead{
		readMailSeed("old-1", old),
		readMailSeed("old-2", old),
		readMailSeed("fresh", fresh),
		readMailSeed("unread", old, func(b *beads.Bead) { b.Labels = nil }),
		readMailSeed("already-closed", old, func(b *beads.Bead) { b.Status = "closed" }),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	const reason = "mail gc-swept: test retention reason padded to length"
	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, cutoff, 0, reason)
	if listErr != nil {
		t.Fatalf("unexpected list error: %v", listErr)
	}
	if len(closeErrs) != 0 {
		t.Fatalf("unexpected per-bead errors: %v", closeErrs)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2", closed)
	}

	for _, id := range []string{"old-1", "old-2"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Errorf("%s status = %q, want closed", id, b.Status)
		}
		if got := b.Metadata["close_reason"]; got != reason {
			t.Errorf("%s close_reason = %q, want %q", id, got, reason)
		}
	}

	for _, id := range []string{"fresh", "unread"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "open" {
			t.Errorf("%s status = %q, want open (must not be swept)", id, b.Status)
		}
		if _, ok := b.Metadata["close_reason"]; ok {
			t.Errorf("%s unexpectedly stamped close_reason", id)
		}
	}
}

func TestSweepReadMessagesBefore_LimitCapsCloses(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)

	seed := []beads.Bead{
		readMailSeed("old-1", old),
		readMailSeed("old-2", old),
		readMailSeed("old-3", old),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 2, "reason padded to twenty plus characters")
	if listErr != nil || len(closeErrs) != 0 {
		t.Fatalf("unexpected errors: list=%v perBead=%v", listErr, closeErrs)
	}
	if closed != 2 {
		t.Fatalf("closed = %d, want 2 (limit)", closed)
	}

	openCount := 0
	all, err := store.List(beads.ListQuery{Type: "message", Label: "read", TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if b.Status == "open" {
			openCount++
		}
	}
	if openCount != 1 {
		t.Fatalf("open read beads = %d, want 1 (limit left one)", openCount)
	}
}

func TestSweepReadMessagesBefore_PerBeadCloseErrorIsCollected(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)

	// good is older so the created_asc sweep visits it first; both are aged.
	seed := []beads.Bead{
		readMailSeed("good", old.Add(-time.Minute)),
		readMailSeed("bad", old),
	}
	base := beads.NewMemStoreFrom(100, seed, nil)
	store := closeErrStore{MemStore: base, failClose: map[string]error{"bad": errors.New("close boom")}}
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 0, "reason padded to twenty plus characters")
	if listErr != nil {
		t.Fatalf("unexpected list error: %v", listErr)
	}
	if closed != 1 {
		t.Fatalf("closed = %d, want 1 (good only)", closed)
	}
	if len(closeErrs) != 1 {
		t.Fatalf("closeErrs = %v, want exactly one", closeErrs)
	}
	if got := closeErrs[0].Error(); !strings.Contains(got, "bad") || !strings.Contains(got, "close boom") {
		t.Fatalf("closeErrs[0] = %q, want it to name the bead and the close failure", got)
	}
}

func TestSweepReadMessagesBefore_ListErrorIsFatal(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	store := listErrStore{MemStore: beads.NewMemStore(), err: errors.New("store down")}
	mailStore := beads.MailStore{Store: store}

	closed, closeErrs, listErr := SweepReadMessagesBefore(mailStore, now, 0, "reason padded to twenty plus characters")
	if listErr == nil {
		t.Fatal("expected fatal list error")
	}
	if closed != 0 || len(closeErrs) != 0 {
		t.Fatalf("closed=%d closeErrs=%v, want zero on list failure", closed, closeErrs)
	}
}

func TestCountReadMessagesBefore_CountsWithoutMutating(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)
	fresh := now.Add(time.Minute)

	seed := []beads.Bead{
		readMailSeed("old-1", old),
		readMailSeed("old-2", old),
		readMailSeed("fresh", fresh),
		readMailSeed("unread", old, func(b *beads.Bead) { b.Labels = nil }),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	count, err := CountReadMessagesBefore(mailStore, now, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2", count)
	}

	// No mutation: every seeded bead is still open.
	for _, id := range []string{"old-1", "old-2", "fresh", "unread"} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "open" {
			t.Errorf("%s status = %q, count must not mutate", id, b.Status)
		}
	}
}

func TestCountReadMessagesBefore_LimitCapsCount(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Minute)
	seed := []beads.Bead{
		readMailSeed("old-1", old),
		readMailSeed("old-2", old),
		readMailSeed("old-3", old),
	}
	store := beads.NewMemStoreFrom(100, seed, nil)
	mailStore := beads.MailStore{Store: store}

	count, err := CountReadMessagesBefore(mailStore, now, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Fatalf("count = %d, want 2 (limit)", count)
	}
}

func TestPurgeReadMessageWisps_DeletesAgedReadWisps(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-time.Hour)
	aged := now.Add(-2 * time.Hour)
	recent := now.Add(-30 * time.Minute)

	wisp := func(id string, createdAt time.Time, meta map[string]string) beads.Bead {
		return beads.Bead{ID: id, Type: "message", Status: "open", CreatedAt: createdAt, Metadata: meta, Ephemeral: true}
	}
	seed := []beads.Bead{
		wisp("read-old", aged, map[string]string{mail.ReadMetadataKey: "true"}),
		wisp("unread-old", aged, map[string]string{mail.ReadMetadataKey: "false"}),
		wisp("unset-old", aged, nil),
		wisp("read-recent", recent, map[string]string{mail.ReadMetadataKey: "true"}),
		// Main-tier read message: excluded by the TierWisps query.
		{ID: "read-main", Type: "message", Status: "open", CreatedAt: aged, Metadata: map[string]string{mail.ReadMetadataKey: "true"}},
		// Wisp-tier but not a message bead: excluded by Type=message.
		{ID: "read-task-wisp", Type: "task", Status: "open", CreatedAt: aged, Metadata: map[string]string{mail.ReadMetadataKey: "true"}, Ephemeral: true},
	}
	store := &deleteTrackStore{MemStore: beads.NewMemStoreFrom(100, seed, nil), failDelete: map[string]error{}}
	mailStore := beads.MailStore{Store: store}

	purged, err := PurgeReadMessageWisps(mailStore, cutoff)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1", purged)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "read-old" {
		t.Fatalf("deleted = %v, want [read-old]", store.deleted)
	}
	for _, id := range []string{"unread-old", "unset-old", "read-recent", "read-main", "read-task-wisp"} {
		if _, err := store.Get(id); err != nil {
			t.Errorf("%s should be preserved: %v", id, err)
		}
	}
}

func TestPurgeReadMessageWisps_DeleteErrorSurfacedAndContinues(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cutoff := now.Add(-time.Hour)
	aged := now.Add(-2 * time.Hour)

	wisp := func(id string) beads.Bead {
		return beads.Bead{ID: id, Type: "message", Status: "open", CreatedAt: aged, Metadata: map[string]string{mail.ReadMetadataKey: "true"}, Ephemeral: true}
	}
	store := &deleteTrackStore{
		MemStore:   beads.NewMemStoreFrom(100, []beads.Bead{wisp("bad"), wisp("good")}, nil),
		failDelete: map[string]error{"bad": errors.New("delete boom")},
	}
	mailStore := beads.MailStore{Store: store}

	purged, err := PurgeReadMessageWisps(mailStore, cutoff)
	if err == nil {
		t.Fatal("expected delete error to be surfaced")
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 (good deleted)", purged)
	}
	if !contains(store.deleted, "good") {
		t.Fatalf("deleted = %v, want to include good", store.deleted)
	}
}

func TestPurgeReadMessageWisps_ListErrorSurfaced(t *testing.T) {
	store := listErrStore{MemStore: beads.NewMemStore(), err: errors.New("store down")}
	mailStore := beads.MailStore{Store: store}
	purged, err := PurgeReadMessageWisps(mailStore, time.Now())
	if err == nil {
		t.Fatal("expected list error to be surfaced")
	}
	if purged != 0 {
		t.Fatalf("purged = %d, want 0", purged)
	}
}

func TestIsMessageBead(t *testing.T) {
	if !IsMessageBead(beads.Bead{Type: "message"}) {
		t.Error("Type=message must be a message bead")
	}
	if IsMessageBead(beads.Bead{Type: "task"}) {
		t.Error("Type=task must not be a message bead")
	}
	if IsMessageBead(beads.Bead{}) {
		t.Error("empty-type bead must not be a message bead")
	}

	// A message bead that also carries wisp metadata is still a message bead:
	// IsMessageBead is a bare Type check, deliberately NOT coordclass.Classify
	// (which would route the wisp-marked bead to ClassGraph). This preserves the
	// historical inline `b.Type == "message"` behavior at the order single-flight
	// gate.
	wispMsg := beads.Bead{Type: "message", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}}
	if !IsMessageBead(wispMsg) {
		t.Error("wisp-marked message bead must still report true")
	}
	if coordclass.Classify(wispMsg) != coordclass.ClassGraph {
		t.Fatalf("precondition: expected wisp-marked message to Classify as ClassGraph, got %v", coordclass.Classify(wispMsg))
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
