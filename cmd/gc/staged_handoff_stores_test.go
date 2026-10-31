package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// newDistinctHandoffStores returns a message store and a session store whose
// beads get disjoint IDs. The session store burns its first IDs on padding it
// then deletes, so the session row is not gc-1 and the message in the other
// store is: a read or write sent to the wrong store misses instead of landing
// on an unrelated bead that shares an ID.
func newDistinctHandoffStores(t *testing.T) (msgStore, sessStore beads.Store) {
	t.Helper()
	msgStore = beads.NewMemStore()
	sessStore = beads.NewMemStore()
	for i := 0; i < 3; i++ {
		padding, err := sessStore.Create(beads.Bead{Title: "padding"})
		if err != nil {
			t.Fatalf("pad session store: %v", err)
		}
		if err := sessStore.Delete(padding.ID); err != nil {
			t.Fatalf("remove padding %s: %v", padding.ID, err)
		}
	}
	return msgStore, sessStore
}

// stageHandoffAcrossStores seeds a session row in sessStore and a staged
// self-handoff brief for it in msgStore, the shape a city whose messaging and
// sessions classes live in different stores holds.
func stageHandoffAcrossStores(t *testing.T, msgStore, sessStore beads.Store, originToken, currentToken string, stagedAt time.Time) (beads.Bead, beads.Bead) {
	t.Helper()
	sessionBead := seedRestartableHandoffSession(t, sessStore, "worker", currentToken)
	message, _, err := beadmail.New(msgStore).SendHandoff(mail.HandoffIntent{
		From:        "worker",
		To:          "worker",
		Subject:     "context cycle",
		Body:        "resume exactly once",
		ThreadID:    "distinct-stores",
		ExtraLabels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel, redHandoffStagedLabel},
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}
	if err := msgStore.SetMetadataBatch(message.ID, map[string]string{
		redHandoffStagedMetadataKey: "true",
		redHandoffStagedForTokenKey: originToken,
	}); err != nil {
		t.Fatalf("stage handoff: %v", err)
	}
	if err := sessStore.SetMetadataBatch(sessionBead.ID, map[string]string{
		redHandoffStagedMessageIDKey:  message.ID,
		redHandoffStageCommittedAtKey: stagedAt.UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("record staged pointer: %v", err)
	}
	if sessionBead.ID == message.ID {
		t.Fatalf("session and message share ID %q; the stores are not distinguishable", sessionBead.ID)
	}
	stored, err := sessStore.Get(sessionBead.ID)
	if err != nil {
		t.Fatalf("get seeded session: %v", err)
	}
	staged, err := msgStore.Get(message.ID)
	if err != nil {
		t.Fatalf("get seeded message: %v", err)
	}
	return stored, staged
}

func assertAbsentFromStore(t *testing.T, store beads.Store, storeName, id string) {
	t.Helper()
	if _, err := store.Get(id); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("%s holds %s (err=%v); a read or write reached the wrong store", storeName, id, err)
	}
}

// TestReleaseStagedSelfHandoffsReadsEachSideFromItsOwnStore pins the release
// path to two typed handles: the brief is read and released through the
// message store, and the session row is cleared or stamped through the session
// store. A city whose messaging class moved to a binding keeps the two apart,
// and TestHandoffStagedArmLandsInTheBindingOnAMigratedCity's co-resident
// stores cannot tell the two sides apart.
func TestReleaseStagedSelfHandoffsReadsEachSideFromItsOwnStore(t *testing.T) {
	cfg := &config.City{}
	clk := &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)}

	t.Run("successor releases the brief", func(t *testing.T) {
		msgStore, sessStore := newDistinctHandoffStores(t)
		sessionBead, message := stageHandoffAcrossStores(t, msgStore, sessStore, "origin-instance", "successor-instance", clk.Now())
		rec := events.NewFake()

		rows := sessionpkg.ReconcileRowsFromBeads([]beads.Bead{sessionBead})
		released, failed := releaseStagedSelfHandoffs(beads.MailStore{Store: msgStore}, beads.SessionStore{Store: sessStore}, rows, cfg, clk, rec)
		if released != 1 || failed != 0 {
			t.Fatalf("releaseStagedSelfHandoffs = (released=%d, failed=%d), want (1, 0)", released, failed)
		}

		got, err := msgStore.Get(message.ID)
		if err != nil {
			t.Fatalf("get released brief: %v", err)
		}
		if hasString(got.Labels, redHandoffStagedLabel) || got.Metadata[redHandoffStagedMetadataKey] != "false" {
			t.Fatalf("brief was not released in the message store: labels=%v metadata=%v", got.Labels, got.Metadata)
		}
		session, err := sessStore.Get(sessionBead.ID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if session.Metadata[redHandoffStagedMessageIDKey] != "" || session.Metadata[redHandoffStageCommittedAtKey] != "" {
			t.Fatalf("staged markers not cleared in the session store: %v", session.Metadata)
		}
		assertAbsentFromStore(t, msgStore, "message store", sessionBead.ID)
		assertAbsentFromStore(t, sessStore, "session store", message.ID)
		assertEventCount(t, rec.Events, redSessionHandoffReleasedEvent, 1)
	})

	t.Run("timeout stamps the session row without touching the brief", func(t *testing.T) {
		msgStore, sessStore := newDistinctHandoffStores(t)
		stagedAt := clk.Now().Add(-controllerRestartTimeout(cfg) - time.Second)
		sessionBead, message := stageHandoffAcrossStores(t, msgStore, sessStore, "same-instance", "same-instance", stagedAt)
		rec := events.NewFake()

		rows := sessionpkg.ReconcileRowsFromBeads([]beads.Bead{sessionBead})
		released, failed := releaseStagedSelfHandoffs(beads.MailStore{Store: msgStore}, beads.SessionStore{Store: sessStore}, rows, cfg, clk, rec)
		if released != 0 || failed != 1 {
			t.Fatalf("releaseStagedSelfHandoffs = (released=%d, failed=%d), want (0, 1)", released, failed)
		}

		session, err := sessStore.Get(sessionBead.ID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if session.Metadata[redHandoffReleaseAttemptedAtKey] == "" {
			t.Fatalf("release-attempt marker missing from the session store: %v", session.Metadata)
		}
		assertHandoffProvisionalRecord(t, mustGet(t, msgStore, message.ID), "same-instance")
		assertAbsentFromStore(t, msgStore, "message store", sessionBead.ID)
		assertEventCount(t, rec.Events, redSessionHandoffFailedEvent, 1)
	})

	t.Run("a missing mail handle releases nothing", func(t *testing.T) {
		_, sessStore := newDistinctHandoffStores(t)
		sessionBead := seedRestartableHandoffSession(t, sessStore, "worker", "successor-instance")
		if err := sessStore.SetMetadataBatch(sessionBead.ID, map[string]string{redHandoffStagedMessageIDKey: "gc-missing"}); err != nil {
			t.Fatalf("record staged pointer: %v", err)
		}
		stored := mustGet(t, sessStore, sessionBead.ID)

		rows := sessionpkg.ReconcileRowsFromBeads([]beads.Bead{stored})
		released, failed := releaseStagedSelfHandoffs(beads.MailStore{}, beads.SessionStore{Store: sessStore}, rows, cfg, clk, events.NewFake())
		if released != 0 || failed != 0 {
			t.Fatalf("releaseStagedSelfHandoffs without a mail handle = (released=%d, failed=%d), want (0, 0) and no panic", released, failed)
		}
		if got := mustGet(t, sessStore, sessionBead.ID); got.Metadata[redHandoffStagedMessageIDKey] != "gc-missing" {
			t.Fatalf("staged pointer changed without a mail handle: %v", got.Metadata)
		}
	})
}

// TestReconcileReleasesStagedHandoffAcrossDistinctStores drives the same split
// through the reconciler tick: the sessions it holds are read from one store
// and the brief it releases lives in another, supplied by withHandoffMailStore.
func TestReconcileReleasesStagedHandoffAcrossDistinctStores(t *testing.T) {
	env, sessionBead := newHandoffReconcileSession(t, "successor-instance")
	// The reconciler env's store already holds the session row and the fixtures
	// around it, so burn a long run of the message store's IDs to keep the two
	// ID spaces disjoint.
	msgStore := beads.NewMemStore()
	for i := 0; i < 100; i++ {
		padding, err := msgStore.Create(beads.Bead{Title: "padding"})
		if err != nil {
			t.Fatalf("pad message store: %v", err)
		}
		if err := msgStore.Delete(padding.ID); err != nil {
			t.Fatalf("remove padding %s: %v", padding.ID, err)
		}
	}
	message, _, err := beadmail.New(msgStore).SendHandoff(mail.HandoffIntent{
		From:        "worker",
		To:          "worker",
		Subject:     "context cycle",
		Body:        "resume exactly once",
		ThreadID:    "reconcile-distinct-stores",
		ExtraLabels: []string{mail.AutoHandoffLabel, mail.ArchiveAfterInjectLabel, redHandoffStagedLabel},
	})
	if err != nil {
		t.Fatalf("SendHandoff: %v", err)
	}
	if err := msgStore.SetMetadataBatch(message.ID, map[string]string{
		redHandoffStagedMetadataKey: "true",
		redHandoffStagedForTokenKey: "origin-instance",
	}); err != nil {
		t.Fatalf("stage handoff: %v", err)
	}
	env.setSessionMetadata(&sessionBead, map[string]string{
		redHandoffStageCommittedAtKey: env.clk.Now().Format(time.RFC3339),
		redHandoffStagedMessageIDKey:  message.ID,
	})
	if sessionBead.ID == message.ID {
		t.Fatalf("session and message share ID %q; the stores are not distinguishable", message.ID)
	}
	env.startOptions = append(env.startOptions, withHandoffMailStore(beads.MailStore{Store: msgStore}))
	recorder := events.NewFake()
	env.rec = recorder

	env.reconcile([]beads.Bead{sessionBead})

	got := mustGet(t, msgStore, message.ID)
	if hasString(got.Labels, redHandoffStagedLabel) || got.Metadata[redHandoffStagedMetadataKey] != "false" {
		t.Fatalf("brief was not released in the message store: labels=%v metadata=%v", got.Labels, got.Metadata)
	}
	session := mustGet(t, env.store, sessionBead.ID)
	if session.Metadata[redHandoffStagedMessageIDKey] != "" || session.Metadata[redHandoffStageCommittedAtKey] != "" {
		t.Fatalf("staged markers not cleared in the session store: %v", session.Metadata)
	}
	assertAbsentFromStore(t, env.store, "session store", message.ID)
	assertEventCount(t, recorder.Events, redSessionHandoffReleasedEvent, 1)
}

// TestSelfHandoffRestartNotRecordedWording covers what a handoff tells the
// operator when its restart could not be recorded after the brief was written.
// A staged brief is held, undelivered, until a successor starts, so claiming
// "sent mail" would contradict the durable state; an unstaged brief really was
// sent.
func TestSelfHandoffRestartNotRecordedWording(t *testing.T) {
	restartErr := errors.New("restart rejected")
	tests := []struct {
		name          string
		instanceToken string
		wantStaged    bool
	}{
		{name: "staged brief is held", instanceToken: "origin-instance", wantStaged: true},
		{name: "unstaged brief is sent", instanceToken: "", wantStaged: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := beads.NewMemStore()
			seedPinnedHandoffSession(t, store, "worker", test.instanceToken)

			outcome, _, stderr := runSelfHandoff(store, events.NewFake(), func() error { return restartErr }, "resume from this brief")
			if outcome.code != 1 {
				t.Fatalf("outcome = %#v, want a failed restart", outcome)
			}
			message := onlyOpenHandoffMessage(t, store)
			lower := strings.ToLower(stderr)
			if test.wantStaged {
				if strings.Contains(lower, "sent mail") {
					t.Fatalf("stderr = %q, must not say the held brief was sent", stderr)
				}
				if !strings.Contains(lower, "staged") || !strings.Contains(stderr, message.ID) {
					t.Fatalf("stderr = %q, want it to name the staged brief %s", stderr, message.ID)
				}
			} else if !strings.Contains(stderr, "sent mail "+message.ID) {
				t.Fatalf("stderr = %q, want the unstaged brief reported as sent mail %s", stderr, message.ID)
			}
			if !strings.Contains(stderr, restartErr.Error()) {
				t.Fatalf("stderr = %q, want the restart failure %q", stderr, restartErr)
			}
		})
	}
}
