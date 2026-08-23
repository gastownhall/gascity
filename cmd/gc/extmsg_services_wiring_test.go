package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
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
