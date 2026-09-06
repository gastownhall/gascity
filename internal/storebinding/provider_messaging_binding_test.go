package storebinding

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/session"
)

func TestOpenBindingDefersMessagingSessionsJoinAcrossIndependentProviders(t *testing.T) {
	messagingStore := beads.NewMemStore()
	sessionsStore := beads.NewMemStore()
	sessions := newBeadsSessionsAdapter(beads.SessionStore{Store: sessionsStore})
	created, err := sessions.CreateSessionInfo(session.CreateSpec{
		ID:    "split-session",
		Title: "split session",
		Metadata: map[string]string{
			"alias":        "split-alias",
			"session_name": "split-canonical",
		},
	})
	if err != nil {
		t.Fatalf("CreateSessionInfo(): %v", err)
	}

	messagingDescriptor := testDescriptor(t, ProviderID("messaging-provider"), PhysicalIdentity("messaging-physical"), coordclass.ClassMessaging)
	sessionsDescriptor := testDescriptor(t, ProviderID("sessions-provider"), PhysicalIdentity("sessions-physical"), coordclass.ClassSessions)
	messagingBinder, err := BindBeadsMessaging(messagingStore)
	if err != nil {
		t.Fatalf("BindBeadsMessaging(): %v", err)
	}
	var messagingCloseCalls atomic.Int32
	messagingPhysical, err := NewOpenedBinding(OpenedBindingParts{
		Descriptor:   messagingDescriptor,
		Capabilities: messagingDescriptor.Capabilities,
		Messaging:    messagingBinder,
		Handles: []ComponentHandle{{
			Component: ComponentID("component"),
			Close: func() error {
				messagingCloseCalls.Add(1)
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewOpenedBinding(messaging): %v", err)
	}
	var sessionsCloseCalls atomic.Int32
	sessionsPhysical, err := NewOpenedBinding(OpenedBindingParts{
		Descriptor:   sessionsDescriptor,
		Capabilities: sessionsDescriptor.Capabilities,
		Sessions:     sessions,
		Handles: []ComponentHandle{{
			Component: ComponentID("component"),
			Close: func() error {
				sessionsCloseCalls.Add(1)
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewOpenedBinding(sessions): %v", err)
	}
	messagingProvider := &recordingProvider{opened: messagingPhysical}
	sessionsProvider := &recordingProvider{opened: sessionsPhysical}

	openedMessaging, err := OpenBinding(context.Background(), messagingProvider, completeOpenRequest(t, messagingDescriptor, messagingDescriptor.Classes()))
	if err != nil {
		t.Fatalf("OpenBinding(messaging): %v", err)
	}
	openedSessions, err := OpenBinding(context.Background(), sessionsProvider, completeOpenRequest(t, sessionsDescriptor, sessionsDescriptor.Classes()))
	if err != nil {
		t.Fatalf("OpenBinding(sessions): %v", err)
	}
	deferred, ok := openedMessaging.Messaging()
	if !ok {
		t.Fatal("Messaging() did not return the already-open persistence binder")
	}
	selectedSessions, ok := openedSessions.Sessions()
	if !ok {
		t.Fatal("Sessions() did not return the selected address/liveness directory")
	}

	fronts, err := deferred.BindSessions(selectedSessions)
	if err != nil {
		t.Fatalf("BindSessions(): %v", err)
	}
	message, err := fronts.Mail.Send("split-alias", created.ID, "subject", "body")
	if err != nil {
		t.Fatalf("Mail.Send(): %v", err)
	}
	if _, err := messagingStore.Get(message.ID); err != nil {
		t.Fatalf("message not persisted in Messaging binding: %v", err)
	}
	sessionMessages, err := sessionsStore.List(beads.ListQuery{Type: "message", IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("listing Sessions message rows: %v", err)
	}
	if len(sessionMessages) != 0 {
		t.Fatalf("message rows escaped into Sessions binding: %#v", sessionMessages)
	}
	if messagingProvider.openCalls != 1 || sessionsProvider.openCalls != 1 {
		t.Fatalf("provider opens after deferred bind = messaging:%d sessions:%d, want 1:1", messagingProvider.openCalls, sessionsProvider.openCalls)
	}
	if _, err := deferred.BindSessions(selectedSessions); !errors.Is(err, ErrMessagingAlreadyBound) {
		t.Fatalf("duplicate BindSessions() error = %v, want ErrMessagingAlreadyBound", err)
	}

	for index := 0; index < 2; index++ {
		if err := openedMessaging.Close(); err != nil {
			t.Fatalf("openedMessaging.Close() #%d: %v", index+1, err)
		}
		if err := openedSessions.Close(); err != nil {
			t.Fatalf("openedSessions.Close() #%d: %v", index+1, err)
		}
	}
	if messagingCloseCalls.Load() != 1 || sessionsCloseCalls.Load() != 1 {
		t.Fatalf("physical close calls = messaging:%d sessions:%d, want 1:1", messagingCloseCalls.Load(), sessionsCloseCalls.Load())
	}
}

func TestMessagingFrontDoorBinderRejectsTypedNilDuplicateAndPartialBinding(t *testing.T) {
	messagingStore := beads.NewMemStore()
	binder, err := BindBeadsMessaging(messagingStore)
	if err != nil {
		t.Fatalf("BindBeadsMessaging(): %v", err)
	}
	var typedNilSessions *beadsSessionsAdapter
	if _, err := binder.BindSessions(typedNilSessions); !errors.Is(err, ErrInvalidMessagingBinding) {
		t.Fatalf("BindSessions(typed nil) error = %v, want ErrInvalidMessagingBinding", err)
	}
	sessions := newBeadsSessionsAdapter(beads.SessionStore{Store: beads.NewMemStore()})
	if _, err := binder.BindSessions(sessions); err != nil {
		t.Fatalf("BindSessions() after rejected typed nil: %v", err)
	}
	if _, err := binder.BindSessions(sessions); !errors.Is(err, ErrMessagingAlreadyBound) {
		t.Fatalf("duplicate BindSessions() error = %v, want ErrMessagingAlreadyBound", err)
	}

	partial := &recordingMessagingBinder{
		bind: func(SessionsAddressDirectory) (MessagingFrontDoors, error) {
			return MessagingFrontDoors{}, nil
		},
	}
	managed, err := newManagedMessagingFrontDoorBinder(partial)
	if err != nil {
		t.Fatalf("newManagedMessagingFrontDoorBinder(): %v", err)
	}
	if _, err := managed.BindSessions(sessions); !errors.Is(err, ErrInvalidMessagingBinding) {
		t.Fatalf("partial BindSessions() error = %v, want ErrInvalidMessagingBinding", err)
	}
	if partial.calls != 1 {
		t.Fatalf("partial binder calls = %d, want 1", partial.calls)
	}
}

func TestNewOpenedBindingRejectsTypedNilMessagingBinderAndClosesHandle(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("typed-nil-messaging-provider"), PhysicalIdentity("typed-nil-messaging"), coordclass.ClassMessaging)
	var typedNil *recordingMessagingBinder
	var closeCalls atomic.Int32

	_, err := NewOpenedBinding(OpenedBindingParts{
		Descriptor:   descriptor,
		Capabilities: descriptor.Capabilities,
		Messaging:    typedNil,
		Handles: []ComponentHandle{{
			Component: ComponentID("component"),
			Close: func() error {
				closeCalls.Add(1)
				return nil
			},
		}},
	})
	if !errors.Is(err, ErrInvalidMessagingBinding) {
		t.Fatalf("NewOpenedBinding() error = %v, want ErrInvalidMessagingBinding", err)
	}
	if closeCalls.Load() != 1 {
		t.Fatalf("rejected Messaging persistence close calls = %d, want 1", closeCalls.Load())
	}
}

func TestOpenBindingClosesMessagingPersistenceOnceForNonNilErrorResult(t *testing.T) {
	descriptor := testDescriptor(t, ProviderID("partial-messaging-provider"), PhysicalIdentity("partial-messaging"), coordclass.ClassMessaging)
	binder, err := BindBeadsMessaging(beads.NewMemStore())
	if err != nil {
		t.Fatalf("BindBeadsMessaging(): %v", err)
	}
	var closeCalls atomic.Int32
	physical, err := NewOpenedBinding(OpenedBindingParts{
		Descriptor:   descriptor,
		Capabilities: descriptor.Capabilities,
		Messaging:    binder,
		Handles: []ComponentHandle{{
			Component: ComponentID("component"),
			Close: func() error {
				closeCalls.Add(1)
				return nil
			},
		}},
	})
	if err != nil {
		t.Fatalf("NewOpenedBinding(): %v", err)
	}
	providerErr := errors.New("messaging open failed after persistence acquisition")

	_, err = OpenBinding(context.Background(), &recordingProvider{opened: physical, openErr: providerErr}, completeOpenRequest(t, descriptor, descriptor.Classes()))
	if !errors.Is(err, providerErr) {
		t.Fatalf("OpenBinding() error = %v, want provider error", err)
	}
	if closeCalls.Load() != 1 {
		t.Fatalf("partial Messaging persistence close calls = %d, want 1", closeCalls.Load())
	}
	if err := physical.Close(); err != nil {
		t.Fatalf("physical.Close() after rejection: %v", err)
	}
	if closeCalls.Load() != 1 {
		t.Fatalf("partial Messaging persistence close calls after idempotent close = %d, want 1", closeCalls.Load())
	}
}

type recordingMessagingBinder struct {
	bind  func(SessionsAddressDirectory) (MessagingFrontDoors, error)
	calls int
}

func (b *recordingMessagingBinder) BindSessions(sessions SessionsAddressDirectory) (MessagingFrontDoors, error) {
	b.calls++
	return b.bind(sessions)
}
