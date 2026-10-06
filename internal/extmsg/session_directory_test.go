package extmsg

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

var errTestSessionDirectoryUnavailable = errors.New("session directory unavailable")

type switchableSessionDirectory struct {
	delegate session.AddressDirectory
	err      error
}

func (d *switchableSessionDirectory) ResolveAddress(selector string, includeClosed bool) (session.Info, error) {
	if d.err != nil {
		return session.Info{}, d.err
	}
	return d.delegate.ResolveAddress(selector, includeClosed)
}

func (d *switchableSessionDirectory) ResolveMailboxAddress(selector string, includeClosed bool) (session.Info, error) {
	if d.err != nil {
		return session.Info{}, d.err
	}
	return d.delegate.ResolveMailboxAddress(selector, includeClosed)
}

func (d *switchableSessionDirectory) ListAddresses(includeClosed bool) ([]session.Info, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.delegate.ListAddresses(includeClosed)
}

func TestSessionDirectorySplitKeepsExtmsgRecordsInMessaging(t *testing.T) {
	freezeTestClock(t)
	messaging := beads.NewMemStore()
	sessionsStore := beads.NewMemStore()
	directory := session.NewStore(beads.SessionStore{Store: sessionsStore})
	fabric, err := NewServicesWithSessionDirectory(messaging, directory)
	if err != nil {
		t.Fatalf("NewServicesWithSessionDirectory: %v", err)
	}

	oldID := makeSessionBead(t, sessionsStore, "gc-split")
	ref := testConversationRef()
	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    oldID,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	newID := respawn(t, sessionsStore, oldID, "gc-split")

	stats, err := ReapStaleBindingsWithSessionDirectory(context.Background(), messaging, directory, testNow())
	if err != nil {
		t.Fatalf("ReapStaleBindingsWithSessionDirectory: %v", err)
	}
	if stats.Reassigned != 1 || stats.Cleared != 0 {
		t.Fatalf("binding stats = %+v, want reassigned live binding", stats)
	}
	got, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil || got == nil || got.SessionID != newID {
		t.Fatalf("ResolveByConversation = %#v, %v; want session %q", got, err, newID)
	}
	requireNoSessionRows(t, messaging)
}

func TestSessionDirectorySplitHealsParticipantsInMessaging(t *testing.T) {
	freezeTestClock(t)
	messaging := beads.NewMemStore()
	sessionsStore := beads.NewMemStore()
	directory := session.NewStore(beads.SessionStore{Store: sessionsStore})
	fabric, err := NewServicesWithSessionDirectory(messaging, directory)
	if err != nil {
		t.Fatalf("NewServicesWithSessionDirectory: %v", err)
	}
	oldID := makeSessionBead(t, sessionsStore, "gc-participant")
	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: testConversationRef(),
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	participant, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: oldID,
	})
	if err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	newID := respawn(t, sessionsStore, oldID, "gc-participant")

	stats, err := ReapStaleParticipantsWithSessionDirectory(context.Background(), messaging, directory)
	if err != nil {
		t.Fatalf("ReapStaleParticipantsWithSessionDirectory: %v", err)
	}
	if stats.Scanned != 1 || stats.Reassigned != 1 {
		t.Fatalf("participant stats = %+v, want scanned and reassigned", stats)
	}
	stored, err := messaging.Get(participant.ID)
	if err != nil {
		t.Fatalf("load messaging participant: %v", err)
	}
	if stored.Metadata["session_id"] != newID {
		t.Fatalf("participant session_id = %q, want %q", stored.Metadata["session_id"], newID)
	}
	requireNoSessionRows(t, messaging)
}

func TestNewServicesWithSessionDirectoryRejectsTypedNil(t *testing.T) {
	var directory *session.Store
	if _, err := NewServicesWithSessionDirectory(beads.NewMemStore(), directory); err == nil {
		t.Fatal("NewServicesWithSessionDirectory accepted typed-nil directory")
	}
}

func TestSessionDirectorySameStorePreservesBindingReap(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	directory := session.NewStore(beads.SessionStore{Store: store})
	fabric, err := NewServicesWithSessionDirectory(store, directory)
	if err != nil {
		t.Fatalf("NewServicesWithSessionDirectory: %v", err)
	}
	oldID := makeSessionBead(t, store, "gc-same-store")
	ref := testConversationRef()
	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    oldID,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	newID := respawn(t, store, oldID, "gc-same-store")
	stats, err := ReapStaleBindingsWithSessionDirectory(context.Background(), store, directory, testNow())
	if err != nil {
		t.Fatalf("ReapStaleBindingsWithSessionDirectory: %v", err)
	}
	if stats.Reassigned != 1 {
		t.Fatalf("stats = %+v, want one reassignment", stats)
	}
	got, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil || got == nil || got.SessionID != newID {
		t.Fatalf("ResolveByConversation = %#v, %v; want %q", got, err, newID)
	}
}

func TestBindPropagatesSessionDirectoryFailureWithoutMessagingMutation(t *testing.T) {
	freezeTestClock(t)
	messaging := beads.NewMemStore()
	const sensitiveSelector = "sensitive-bind-selector"
	directory := &switchableSessionDirectory{err: unsafeSessionDirectoryError(sensitiveSelector)}
	fabric, err := NewServicesWithSessionDirectory(messaging, directory)
	if err != nil {
		t.Fatalf("NewServicesWithSessionDirectory: %v", err)
	}
	before := snapshotMessagingRows(t, messaging)

	_, err = fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: testConversationRef(),
		SessionID:    sensitiveSelector,
		Now:          testNow(),
	})

	requireSessionDirectoryFailure(t, err, "resolve binding session address", sensitiveSelector)
	requireMessagingRowsUnchanged(t, messaging, before)
}

func TestUpsertParticipantPropagatesSessionDirectoryFailureWithoutMessagingMutation(t *testing.T) {
	freezeTestClock(t)
	messaging := beads.NewMemStore()
	directory := &switchableSessionDirectory{
		delegate: session.NewStore(beads.SessionStore{Store: beads.NewMemStore()}),
	}
	fabric, err := NewServicesWithSessionDirectory(messaging, directory)
	if err != nil {
		t.Fatalf("NewServicesWithSessionDirectory: %v", err)
	}
	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: testConversationRef(),
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	before := snapshotMessagingRows(t, messaging)
	const sensitiveSelector = "sensitive-participant-selector"
	directory.err = unsafeSessionDirectoryError(sensitiveSelector)

	_, err = fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sensitiveSelector,
	})

	requireSessionDirectoryFailure(t, err, "resolve participant session address", sensitiveSelector)
	requireMessagingRowsUnchanged(t, messaging, before)
}

func TestLiveSessionOverlayPropagatesSessionDirectoryFailure(t *testing.T) {
	t.Run("binding", func(t *testing.T) {
		freezeTestClock(t)
		messaging := beads.NewMemStore()
		sessionsStore := beads.NewMemStore()
		directory := &switchableSessionDirectory{
			delegate: session.NewStore(beads.SessionStore{Store: sessionsStore}),
		}
		fabric, err := NewServicesWithSessionDirectory(messaging, directory)
		if err != nil {
			t.Fatalf("NewServicesWithSessionDirectory: %v", err)
		}
		const sensitiveName = "sensitive-binding-name"
		sessionID := makeSessionBead(t, sessionsStore, sensitiveName)
		if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
			Conversation: testConversationRef(),
			SessionID:    sessionID,
			Now:          testNow(),
		}); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		before := snapshotMessagingRows(t, messaging)
		directory.err = unsafeSessionDirectoryError(sensitiveName, sessionID)

		_, err = fabric.Bindings.ResolveByConversation(context.Background(), testConversationRef())

		requireSessionDirectoryFailure(t, err, "resolve binding live session", sensitiveName, sessionID)
		requireMessagingRowsUnchanged(t, messaging, before)
	})

	t.Run("group inbound", func(t *testing.T) {
		fabric, messaging, directory, sessionID, sensitiveName := sessionDirectoryGroupFixture(t)
		before := snapshotMessagingRows(t, messaging)
		directory.err = unsafeSessionDirectoryError(sensitiveName, sessionID)

		_, err := fabric.Groups.ResolveInbound(context.Background(), ExternalInboundMessage{
			Conversation: testConversationRef(),
		})

		requireSessionDirectoryFailure(t, err, "resolve inbound participant live session", sensitiveName, sessionID)
		requireMessagingRowsUnchanged(t, messaging, before)
	})

	t.Run("group outbound", func(t *testing.T) {
		fabric, messaging, directory, sessionID, sensitiveName := sessionDirectoryGroupFixture(t)
		before := snapshotMessagingRows(t, messaging)
		directory.err = unsafeSessionDirectoryError(sensitiveName, sessionID)

		_, err := fabric.Groups.ResolveOutbound(context.Background(), testConversationRef(), sessionID)

		requireSessionDirectoryFailure(t, err, "resolve outbound participant live session", sensitiveName, sessionID)
		requireMessagingRowsUnchanged(t, messaging, before)
	})
}

func TestBindingReaperPropagatesSessionDirectoryFailureWithoutMessagingMutation(t *testing.T) {
	freezeTestClock(t)
	messaging := beads.NewMemStore()
	sessionsStore := beads.NewMemStore()
	directory := &switchableSessionDirectory{
		delegate: session.NewStore(beads.SessionStore{Store: sessionsStore}),
	}
	fabric, err := NewServicesWithSessionDirectory(messaging, directory)
	if err != nil {
		t.Fatalf("NewServicesWithSessionDirectory: %v", err)
	}
	const sensitiveName = "sensitive-reaper-binding-name"
	sessionID := makeSessionBead(t, sessionsStore, sensitiveName)
	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: testConversationRef(),
		SessionID:    sessionID,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	before := snapshotMessagingRows(t, messaging)
	directory.err = unsafeSessionDirectoryError(sensitiveName, sessionID)

	_, err = ReapStaleBindingsWithSessionDirectory(context.Background(), messaging, directory, testNow())

	requireSessionDirectoryFailure(t, err, "resolve stale binding live session", sensitiveName, sessionID)
	requireMessagingRowsUnchanged(t, messaging, before)
}

func TestParticipantReaperPropagatesSessionDirectoryFailureWithoutMessagingMutation(t *testing.T) {
	_, messaging, directory, sessionID, sensitiveName := sessionDirectoryGroupFixture(t)
	before := snapshotMessagingRows(t, messaging)
	directory.err = unsafeSessionDirectoryError(sensitiveName, sessionID)

	_, err := ReapStaleParticipantsWithSessionDirectory(context.Background(), messaging, directory)

	requireSessionDirectoryFailure(t, err, "resolve stale participant live session", sensitiveName, sessionID)
	requireMessagingRowsUnchanged(t, messaging, before)
}

func TestReapersRejectMissingSessionDirectoryWithoutMessagingMutation(t *testing.T) {
	directories := []struct {
		name      string
		directory session.AddressDirectory
	}{
		{name: "nil"},
		{name: "typed nil", directory: (*session.Store)(nil)},
	}
	for _, test := range directories {
		t.Run("binding/"+test.name, func(t *testing.T) {
			freezeTestClock(t)
			messaging := beads.NewMemStore()
			sessionsStore := beads.NewMemStore()
			healthy := session.NewStore(beads.SessionStore{Store: sessionsStore})
			fabric, err := NewServicesWithSessionDirectory(messaging, healthy)
			if err != nil {
				t.Fatalf("NewServicesWithSessionDirectory: %v", err)
			}
			sessionID := makeSessionBead(t, sessionsStore, "missing-directory-binding")
			if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
				Conversation: testConversationRef(),
				SessionID:    sessionID,
				Now:          testNow(),
			}); err != nil {
				t.Fatalf("Bind: %v", err)
			}
			before := snapshotMessagingRows(t, messaging)

			_, err = ReapStaleBindingsWithSessionDirectory(context.Background(), messaging, test.directory, testNow())

			if err == nil || !strings.Contains(err.Error(), "sessions directory") {
				t.Fatalf("ReapStaleBindingsWithSessionDirectory() error = %v, want missing Sessions authority", err)
			}
			requireMessagingRowsUnchanged(t, messaging, before)
		})

		t.Run("participant/"+test.name, func(t *testing.T) {
			_, messaging, _, _, _ := sessionDirectoryGroupFixture(t)
			before := snapshotMessagingRows(t, messaging)

			_, err := ReapStaleParticipantsWithSessionDirectory(context.Background(), messaging, test.directory)

			if err == nil || !strings.Contains(err.Error(), "sessions directory") {
				t.Fatalf("ReapStaleParticipantsWithSessionDirectory() error = %v, want missing Sessions authority", err)
			}
			requireMessagingRowsUnchanged(t, messaging, before)
		})
	}
}

func TestReapersRedactWrappedPersistenceFailures(t *testing.T) {
	t.Run("binding reassignment", func(t *testing.T) {
		freezeTestClock(t)
		base := beads.NewMemStore()
		messaging := &failingExtmsgUpdateStore{Store: base}
		sessionsStore := beads.NewMemStore()
		directory := session.NewStore(beads.SessionStore{Store: sessionsStore})
		fabric, err := NewServicesWithSessionDirectory(messaging, directory)
		if err != nil {
			t.Fatalf("NewServicesWithSessionDirectory: %v", err)
		}
		const sensitiveName = "sensitive-persistence-binding-name"
		oldID := makeSessionBead(t, sessionsStore, sensitiveName)
		if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
			Conversation: testConversationRef(),
			SessionID:    oldID,
			Now:          testNow(),
		}); err != nil {
			t.Fatalf("Bind: %v", err)
		}
		newID := respawn(t, sessionsStore, oldID, sensitiveName)
		messaging.err = unsafeMessagingPersistenceError(sensitiveName, oldID, newID)

		_, err = ReapStaleBindingsWithSessionDirectory(context.Background(), messaging, directory, testNow())

		requireRedactedExtmsgError(t, err, errTestMessagingPersistence, "reassign stale session bindings", sensitiveName, oldID, newID)
	})

	t.Run("participant reassignment", func(t *testing.T) {
		messaging, sessionsStore, directory, oldID, sensitiveName := persistenceParticipantFixture(t)
		newID := respawn(t, sessionsStore, oldID, sensitiveName)
		messaging.err = unsafeMessagingPersistenceError(sensitiveName, oldID, newID)

		_, err := ReapStaleParticipantsWithSessionDirectory(context.Background(), messaging, directory)

		requireRedactedExtmsgError(t, err, errTestMessagingPersistence, "reassign stale session participants", sensitiveName, oldID, newID)
	})

	t.Run("pending participant cleanup", func(t *testing.T) {
		messaging, sessionsStore, directory, oldID, sensitiveName := persistenceParticipantFixture(t)
		newID := respawn(t, sessionsStore, oldID, sensitiveName)
		items, err := messaging.List(beads.ListQuery{Label: labelGroupParticipantBase})
		if err != nil || len(items) != 1 {
			t.Fatalf("list participant fixture = %#v, %v", items, err)
		}
		if err := messaging.Store.Update(items[0].ID, beads.UpdateOpts{Metadata: map[string]string{
			"session_id":                          newID,
			"previous_session_id_pending_cleanup": oldID,
		}}); err != nil {
			t.Fatalf("seed pending participant cleanup: %v", err)
		}
		messaging.err = unsafeMessagingPersistenceError(sensitiveName, oldID, newID)

		_, err = ReapStaleParticipantsWithSessionDirectory(context.Background(), messaging, directory)

		requireRedactedExtmsgError(t, err, errTestMessagingPersistence, "finish pending stale session participant cleanup", sensitiveName, oldID, newID)
	})
}

var errTestMessagingPersistence = errors.New("messaging persistence unavailable")

type failingExtmsgUpdateStore struct {
	beads.Store
	err error
}

func (s *failingExtmsgUpdateStore) Update(id string, opts beads.UpdateOpts) error {
	if s.err != nil {
		return s.err
	}
	return s.Store.Update(id, opts)
}

func persistenceParticipantFixture(t *testing.T) (*failingExtmsgUpdateStore, *beads.MemStore, session.AddressDirectory, string, string) {
	t.Helper()
	freezeTestClock(t)
	messaging := &failingExtmsgUpdateStore{Store: beads.NewMemStore()}
	sessionsStore := beads.NewMemStore()
	directory := session.NewStore(beads.SessionStore{Store: sessionsStore})
	fabric, err := NewServicesWithSessionDirectory(messaging, directory)
	if err != nil {
		t.Fatalf("NewServicesWithSessionDirectory: %v", err)
	}
	const sensitiveName = "sensitive-persistence-participant-name"
	oldID := makeSessionBead(t, sessionsStore, sensitiveName)
	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: testConversationRef(),
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: oldID,
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	return messaging, sessionsStore, directory, oldID, sensitiveName
}

func unsafeSessionDirectoryError(sensitiveValues ...string) error {
	return fmt.Errorf("%w: leaked session addresses %s", errTestSessionDirectoryUnavailable, strings.Join(sensitiveValues, ","))
}

func unsafeMessagingPersistenceError(sensitiveValues ...string) error {
	return fmt.Errorf("%w: leaked persistence targets %s", errTestMessagingPersistence, strings.Join(sensitiveValues, ","))
}

func sessionDirectoryGroupFixture(t *testing.T) (Services, *beads.MemStore, *switchableSessionDirectory, string, string) {
	t.Helper()
	freezeTestClock(t)
	messaging := beads.NewMemStore()
	sessionsStore := beads.NewMemStore()
	directory := &switchableSessionDirectory{
		delegate: session.NewStore(beads.SessionStore{Store: sessionsStore}),
	}
	fabric, err := NewServicesWithSessionDirectory(messaging, directory)
	if err != nil {
		t.Fatalf("NewServicesWithSessionDirectory: %v", err)
	}
	const sensitiveName = "sensitive-participant-name"
	sessionID := makeSessionBead(t, sessionsStore, sensitiveName)
	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: testConversationRef(),
		Mode:             GroupModeLauncher,
		DefaultHandle:    "alpha",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: sessionID,
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}
	return fabric, messaging, directory, sessionID, sensitiveName
}

func snapshotMessagingRows(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()
	rows, err := store.List(beads.ListQuery{IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("snapshot messaging rows: %v", err)
	}
	return rows
}

func requireMessagingRowsUnchanged(t *testing.T, store beads.Store, before []beads.Bead) {
	t.Helper()
	after := snapshotMessagingRows(t, store)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("messaging rows changed after failed session lookup:\nbefore: %#v\nafter:  %#v", before, after)
	}
}

func requireSessionDirectoryFailure(t *testing.T, err error, context string, sensitiveValues ...string) {
	t.Helper()
	requireRedactedExtmsgError(t, err, errTestSessionDirectoryUnavailable, context, sensitiveValues...)
}

func requireRedactedExtmsgError(t *testing.T, err, cause error, context string, sensitiveValues ...string) {
	t.Helper()
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want wrapped %v", err, cause)
	}
	if !strings.Contains(err.Error(), context) {
		t.Fatalf("error = %q, want contextual operation %q", err, context)
	}
	for _, sensitive := range sensitiveValues {
		if sensitive != "" && strings.Contains(err.Error(), sensitive) {
			t.Fatalf("error %q leaks session identifier %q", err, sensitive)
		}
	}
}

func requireNoSessionRows(t *testing.T, store beads.Store) {
	t.Helper()
	rows, err := store.List(beads.ListQuery{Type: session.BeadType, IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list messaging session rows: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("messaging store contains session rows: %#v", rows)
	}
}
