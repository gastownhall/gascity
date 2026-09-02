package main

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/testutil"
)

var errStalledBeadNotImplemented = errors.New("stalled-bead alarm is not implemented")

func TestBeadsCommandRegistersStallCheck(t *testing.T) {
	t.Parallel()

	cmd := newBeadsCmd(&bytes.Buffer{}, &bytes.Buffer{})
	found, _, err := cmd.Find([]string{"stall-check"})
	if err != nil {
		t.Fatalf("find gc beads stall-check: %v", err)
	}
	if found == cmd || found.Name() != "stall-check" {
		t.Fatalf("gc beads stall-check resolved to %q, want registered stall-check subcommand", found.CommandPath())
	}
}

func TestRunBeadsStallCheckScansAllInProgressPriorities(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(20, []beads.Bead{
		stalledCommandBead("ga-p0", "in_progress", intPointer(0), now.Add(-3*time.Hour)),
		stalledCommandBead("ga-p1", "in_progress", intPointer(1), now.Add(-7*time.Hour)),
		stalledCommandBead("ga-p2", "in_progress", intPointer(2), now.Add(-25*time.Hour)),
		stalledCommandBead("ga-p3", "in_progress", intPointer(3), now.Add(-73*time.Hour)),
		stalledCommandBead("ga-p4", "in_progress", intPointer(4), now.Add(-25*time.Hour)),
		stalledCommandBead("ga-default", "in_progress", nil, now.Add(-25*time.Hour)),
		stalledCommandBead("ga-fresh", "in_progress", intPointer(0), now.Add(-time.Hour)),
		stalledCommandBead("ga-open", "open", intPointer(0), now.Add(-100*time.Hour)),
		stalledCommandBead("ga-closed", "closed", intPointer(0), now.Add(-100*time.Hour)),
	}, nil)
	messenger := mail.NewFake()
	var log bytes.Buffer

	err := runBeadsStallCheck(stallCheckOptions{
		store:        store,
		mail:         messenger,
		now:          func() time.Time { return now },
		escalationTo: "gascity/operator",
		log:          &log,
	})
	if err != nil {
		t.Fatalf("stall-check: %v", err)
	}

	messages := messenger.Messages()
	if len(messages) != 6 {
		t.Fatalf("mail count = %d, want 6; messages=%+v", len(messages), messages)
	}
	gotRecipients := make([]string, 0, len(messages))
	for _, message := range messages {
		if message.To != "gascity/operator" {
			t.Errorf("mail recipient = %q, want gascity/operator", message.To)
		}
		if !strings.Contains(message.Subject+"\n"+message.Body, "threshold=") {
			t.Errorf("mail for %q omits threshold: subject=%q body=%q", message.ID, message.Subject, message.Body)
		}
		gotRecipients = append(gotRecipients, message.Subject+"\n"+message.Body)
	}
	for _, id := range []string{"ga-default", "ga-p0", "ga-p1", "ga-p2", "ga-p3", "ga-p4"} {
		if !strings.Contains(strings.Join(gotRecipients, "\n"), id) {
			t.Errorf("mail does not identify stalled bead %s", id)
		}
	}

	wantLines := []string{
		"STALL-ALARM bead=ga-default priority=P2 stalled_for=25h0m0s threshold=24h0m0s",
		"STALL-ALARM bead=ga-p0 priority=P0 stalled_for=3h0m0s threshold=2h0m0s",
		"STALL-ALARM bead=ga-p1 priority=P1 stalled_for=7h0m0s threshold=6h0m0s",
		"STALL-ALARM bead=ga-p2 priority=P2 stalled_for=25h0m0s threshold=24h0m0s",
		"STALL-ALARM bead=ga-p3 priority=P3 stalled_for=73h0m0s threshold=72h0m0s",
		"STALL-ALARM bead=ga-p4 priority=P4 stalled_for=25h0m0s threshold=24h0m0s",
	}
	gotLines := nonemptyLines(log.String())
	sort.Strings(gotLines)
	if !reflect.DeepEqual(gotLines, wantLines) {
		t.Fatalf("supervisor log lines = %#v, want %#v", gotLines, wantLines)
	}
}

func TestRunBeadsStallCheckEmitsOnceForContinuousEpisode(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	target := stalledCommandBead("ga-stale", "in_progress", intPointer(0), now.Add(-3*time.Hour))
	store := beads.NewMemStoreFrom(1, []beads.Bead{target}, nil)
	messenger := mail.NewFake()
	var log bytes.Buffer
	options := stallCheckOptions{
		store:        store,
		mail:         messenger,
		now:          func() time.Time { return now },
		escalationTo: "gascity/operator",
		log:          &log,
	}

	if err := runBeadsStallCheck(options); err != nil {
		t.Fatalf("first stall-check: %v", err)
	}
	if err := runBeadsStallCheck(options); err != nil {
		t.Fatalf("repeated stall-check: %v", err)
	}
	if got := len(messenger.Messages()); got != 1 {
		t.Fatalf("mail count after repeated sweep = %d, want 1", got)
	}
	wantLog := "STALL-ALARM bead=ga-stale priority=P0 stalled_for=3h0m0s threshold=2h0m0s\n"
	if got := log.String(); got != wantLog {
		t.Fatalf("supervisor log = %q, want %q", got, wantLog)
	}
	after, err := store.Get(target.ID)
	if err != nil {
		t.Fatalf("get target after stall-check: %v", err)
	}
	if !reflect.DeepEqual(after, target) {
		t.Fatalf("stall-check mutated target:\n before: %+v\n  after: %+v", target, after)
	}
}

func TestRunBeadsStallCheckEmptyRecipientIsLogOnly(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		stalledCommandBead("ga-stale", "in_progress", intPointer(0), now.Add(-3*time.Hour)),
	}, nil)
	var log bytes.Buffer
	err := runBeadsStallCheck(stallCheckOptions{
		store: store,
		mail:  mail.NewFailFake(),
		now:   func() time.Time { return now },
		log:   &log,
	})
	if err != nil {
		t.Fatalf("log-only stall-check: %v", err)
	}
	want := "STALL-ALARM bead=ga-stale priority=P0 stalled_for=3h0m0s threshold=2h0m0s\n"
	if got := log.String(); got != want {
		t.Fatalf("supervisor log = %q, want %q", got, want)
	}
}

func TestRunBeadsStallCheckSurfacesStoreAndMailFailures(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	t.Run("store list", func(t *testing.T) {
		sentinel := errors.New("ledger unavailable")
		err := runBeadsStallCheck(stallCheckOptions{
			store:        stallListFailStore{Store: beads.NewMemStore(), err: sentinel},
			mail:         mail.NewFake(),
			now:          func() time.Time { return now },
			escalationTo: "gascity/operator",
			log:          &bytes.Buffer{},
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("stall-check error = %v, want wrapped store error", err)
		}
	})

	t.Run("mail remains retryable", func(t *testing.T) {
		store := beads.NewMemStoreFrom(1, []beads.Bead{
			stalledCommandBead("ga-stale", "in_progress", intPointer(0), now.Add(-3*time.Hour)),
		}, nil)
		var log bytes.Buffer
		options := stallCheckOptions{
			store:        store,
			mail:         mail.NewFailFake(),
			now:          func() time.Time { return now },
			escalationTo: "gascity/operator",
			log:          &log,
		}
		if err := runBeadsStallCheck(options); err == nil || !strings.Contains(err.Error(), "mail provider unavailable") {
			t.Fatalf("mail failure = %v, want surfaced provider error", err)
		}
		if log.Len() != 0 {
			t.Fatalf("failed escalation logged as sent: %q", log.String())
		}

		healthy := mail.NewFake()
		options.mail = healthy
		if err := runBeadsStallCheck(options); err != nil {
			t.Fatalf("retry stall-check: %v", err)
		}
		if got := len(healthy.Messages()); got != 1 {
			t.Fatalf("retry mail count = %d, want 1", got)
		}
		if got := len(nonemptyLines(log.String())); got != 1 {
			t.Fatalf("retry log line count = %d, want 1; log=%q", got, log.String())
		}
	})
}

func TestRunBeadsStallCheckOverlappingSweepsDoNotDuplicate(t *testing.T) {
	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		stalledCommandBead("ga-stale", "in_progress", intPointer(0), now.Add(-3*time.Hour)),
	}, nil)
	messenger := &blockingMail{
		Fake:    mail.NewFake(),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	log := &stallLockedBuffer{}
	options := stallCheckOptions{
		store:        store,
		mail:         messenger,
		now:          func() time.Time { return now },
		escalationTo: "gascity/operator",
		log:          log,
	}
	results := make(chan error, 2)
	go func() { results <- runBeadsStallCheck(options) }()

	select {
	case <-messenger.entered:
	case err := <-results:
		t.Fatalf("first sweep returned before mail send: %v", err)
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("first sweep did not reach mail send")
	}
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		results <- runBeadsStallCheck(options)
	}()
	<-secondStarted
	close(messenger.release)

	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("overlapping stall-check: %v", err)
			}
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("overlapping stall-check did not return")
		}
	}
	if got := len(messenger.Messages()); got != 1 {
		t.Fatalf("mail count after overlapping sweeps = %d, want 1", got)
	}
	if got := len(nonemptyLines(log.String())); got != 1 {
		t.Fatalf("log line count after overlapping sweeps = %d, want 1; log=%q", got, log.String())
	}
}

func stalledCommandBead(id, status string, priority *int, updatedAt time.Time) beads.Bead {
	return beads.Bead{
		ID:          id,
		Title:       "work " + id,
		Status:      status,
		Type:        "task",
		Priority:    priority,
		CreatedAt:   updatedAt.Add(-time.Hour),
		UpdatedAt:   updatedAt,
		Assignee:    "gascity/worker",
		Description: "operator notes",
		Metadata: beads.StringMap{
			"session_id":   "session-42",
			"session_name": "worker-42",
		},
	}
}

func intPointer(value int) *int {
	return &value
}

func nonemptyLines(value string) []string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "\n")
}

type stallListFailStore struct {
	beads.Store
	err error
}

func (s stallListFailStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, fmt.Errorf("list in-progress beads: %w", s.err)
}

type blockingMail struct {
	*mail.Fake
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (m *blockingMail) Send(from, to, subject, body string) (mail.Message, error) {
	m.once.Do(func() { close(m.entered) })
	<-m.release
	return m.Fake.Send(from, to, subject, body)
}

type stallLockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *stallLockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(value)
}

func (b *stallLockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
