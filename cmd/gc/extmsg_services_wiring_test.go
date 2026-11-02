package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

// TestCityExtMsgServicesCannotReachTheRefusalPath verifies the claim
// newCityExtMsgServices makes about its own error arm.
//
// extmsg refuses exactly one thing — a nil session address directory — and this
// caller cannot hand it one: session.NewStore returns a non-nil *session.Store
// whatever it wraps, so even a nil inner store produces a non-nil directory.
// The subtlety worth pinning is that nilAddressDirectory reflects on the
// INTERFACE VALUE, so a typed non-nil pointer around nothing passes while an
// untyped nil does not. Without the control at the end this test would pass on
// a build where extmsg had stopped checking at all.
func TestCityExtMsgServicesCannotReachTheRefusalPath(t *testing.T) {
	work := splittest.NewWorkStore(t, "hq")
	if _, err := extmsg.NewServicesWithSessionDirectory(work, session.NewStore(beads.SessionStore{Store: work})); err != nil {
		t.Fatalf("the directory this caller builds was refused: %v", err)
	}
	// The store a class resolves to is never consulted for nil-ness, so the
	// degenerate case reaches the same verdict — which is what makes the arm
	// unreachable rather than merely unreached today.
	if _, err := extmsg.NewServicesWithSessionDirectory(work, session.NewStore(beads.SessionStore{Store: nil})); err != nil {
		t.Fatalf("a directory wrapping no store was refused, so the fallback arm IS reachable: %v", err)
	}
	if _, err := extmsg.NewServicesWithSessionDirectory(work, nil); err == nil {
		t.Fatal("extmsg accepted a nil session directory; the pin above proves nothing")
	}
}

// TestCityExtMsgServicesRouteEachClassToItsOwnStore pins WHICH store each half
// of the split reaches, on a city that relocates messaging and sessions to two
// different ledgers.
//
// The test above pins only that a call of this shape succeeds, and that is not
// the property the caller depends on: newCityExtMsgServices could hand extmsg
// the work store for both halves and every assertion there would still pass.
// That edit is precisely the residency bug the function's own doc comment
// describes in prose — bindings and transcripts written where the messaging
// class's readers never look, identity resolved out of a ledger holding no
// session beads — so it is the one thing worth pinning here.
//
// The fixture gives the two classes genuinely different stores and reads the
// answer off the single record a bind produces: the binding bead's id carries
// the mint prefix of whichever store created it, and SessionName is non-empty
// only if the directory found the session bead, which exists in the sessions
// store alone. cfg, cityPath and rec are the arguments resolveClassStore
// documents as unread, and are passed empty here to keep that visible.
func TestCityExtMsgServicesRouteEachClassToItsOwnStore(t *testing.T) {
	work := splittest.NewWorkStore(t, "hq")
	messaging := splittest.NewClassStore(t, config.BeadClassMessaging)
	sessions := splittest.NewClassStore(t, config.BeadClassSessions)
	routes := &storageRoutes{stores: map[coordclass.Class]beads.Store{
		coordclass.ClassMessaging: messaging,
		coordclass.ClassSessions:  sessions,
	}}

	const sessionName = "gc-extmsg-pin"
	sessionBead, err := sessions.Create(beads.Bead{
		Title:    "session " + sessionName,
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_name": sessionName},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	svc := newCityExtMsgServices(routes, work, nil, "", nil)
	record, err := svc.Bindings.Bind(context.Background(), extmsg.Caller{Kind: extmsg.CallerController, ID: "tester"}, extmsg.BindInput{
		Conversation: extmsg.ConversationRef{
			ScopeID:        "city-1",
			Provider:       "discord",
			AccountID:      "acct-1",
			ConversationID: "thread-1",
			Kind:           extmsg.ConversationThread,
		},
		SessionID: sessionBead.ID,
		Now:       time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Bind through the city services: %v", err)
	}

	msgPrefix, ok := config.ReservedClassPrefix(config.BeadClassMessaging)
	if !ok {
		t.Fatal("the messaging class declares no reserved prefix; the id assertions below would compare nothing")
	}
	if !strings.HasPrefix(record.ID, msgPrefix+"-") {
		t.Errorf("the binding minted as %q, want a %q- id; extmsg persisted outside the messaging class's store", record.ID, msgPrefix)
	}
	if _, err := messaging.Get(record.ID); err != nil {
		t.Errorf("the messaging store does not hold binding %q: %v", record.ID, err)
	}
	if _, err := work.Get(record.ID); err == nil {
		t.Errorf("the work store holds binding %q; a relocated class's records must not land in the work ledger", record.ID)
	}
	// The sessions half. An unresolvable selector is NOT an error — extmsg keeps
	// legacy pure-ID behavior for it — so a directory pointed at the wrong ledger
	// reports a successful bind carrying an empty stable name, and nothing but
	// this assertion notices.
	if record.SessionName != sessionName {
		t.Errorf("the binding recorded SessionName %q, want %q; the session directory read a ledger that does not hold the session bead", record.SessionName, sessionName)
	}
}

// TestRefusedExtMsgServicesDoNotAnswerFromTheWorkLedger pins what the
// unreachable arm does if a later edit makes it reachable.
//
// Returning services backed by the work store was the original fallback, and on
// a city that relocated the messaging class it would have written bindings and
// transcripts into the ledger the class's own readers never open: external
// messaging would look wired and deliver nothing. The refusal has to reach the
// caller, so the assertion is that an ordinary read carries the cause.
func TestRefusedExtMsgServicesDoNotAnswerFromTheWorkLedger(t *testing.T) {
	boom := errors.New("the messaging binding is unreachable")
	svc := refusedExtMsgServices(boom)

	_, err := svc.Transcript.List(context.Background(), extmsg.ListTranscriptInput{
		Caller: extmsg.Caller{Kind: extmsg.CallerController, ID: "tester"},
		Conversation: extmsg.ConversationRef{
			ScopeID:        "scope-1",
			Provider:       "slack",
			AccountID:      "acct-1",
			ConversationID: "c-1",
			Kind:           extmsg.ConversationDM,
		},
	})
	if err == nil {
		t.Fatal("a refused messaging service answered a transcript read; the caller cannot tell wiring failure from an empty conversation")
	}
	if !errors.Is(err, boom) {
		t.Errorf("the refusal reads %v, want the wiring cause carried through", err)
	}
}
