package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/extmsg"
)

// extmsgTranscriptCheckOver is the check over store, or over a store open
// that fails with openErr.
func extmsgTranscriptCheckOver(store beads.Store, openErr error) *extmsgTranscriptDoctorCheck {
	return &extmsgTranscriptDoctorCheck{open: func() (beads.MailStore, func(), error) {
		if openErr != nil {
			return beads.MailStore{}, nil, openErr
		}
		return beadstest.Mail(store), func() {}, nil
	}}
}

// seedExtmsgTranscript appends one transcript entry to store, which creates
// the conversation's transcript state.
func seedExtmsgTranscript(t *testing.T, store beads.Store) {
	t.Helper()
	svc := extmsg.NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	if _, err := svc.Transcript.Append(context.Background(), extmsg.AppendTranscriptInput{
		Caller: extmsg.Caller{Kind: extmsg.CallerController, ID: "tester"},
		Conversation: extmsg.ConversationRef{
			ScopeID:        "city-1",
			Provider:       "slack",
			AccountID:      "acct-1",
			ConversationID: "c-1",
			Kind:           extmsg.ConversationDM,
		},
		Kind:              extmsg.TranscriptMessageInbound,
		ProviderMessageID: "msg-1",
		Text:              "hello",
		CreatedAt:         time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("seeding a transcript entry: %v", err)
	}
}

// transcriptListFailingStore fails every List; it hides every optional capability of
// the store it wraps.
type transcriptListFailingStore struct {
	beads.Store
}

func (transcriptListFailingStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list timed out")
}

func TestExtmsgTranscriptDoctorCheck(t *testing.T) {
	releasedBdWithTranscript := beadstest.NewFakeBd().NewBdStore()
	seedExtmsgTranscript(t, releasedBdWithTranscript)
	capableWithTranscript := beads.NewMemStore()
	seedExtmsgTranscript(t, capableWithTranscript)
	// A bd that advertises --if-revision but whose version cannot be read
	// passes the probe, yet refuses every pinned create when an append runs
	// it, so its appends run unguarded.
	unprovenBd := beadstest.NewFakeBd()
	unprovenBd.SetVersion("unknown")
	unprovenBd.AdvertiseConditionalWrites()
	unprovenBdWithTranscript := unprovenBd.NewBdStore()
	seedExtmsgTranscript(t, unprovenBdWithTranscript)

	for _, tc := range []struct {
		name    string
		check   *extmsgTranscriptDoctorCheck
		status  doctor.CheckStatus
		message []string
		hint    bool
	}{
		{
			name:    "a store that supports atomic allocation",
			check:   extmsgTranscriptCheckOver(capableWithTranscript, nil),
			status:  doctor.StatusOK,
			message: []string{"supports atomic transcript sequence allocation", "still runs unguarded"},
		},
		{
			name:    "a bd that passes the probe but refuses its pinned creates",
			check:   extmsgTranscriptCheckOver(unprovenBdWithTranscript, nil),
			status:  doctor.StatusOK,
			message: []string{"supports atomic transcript sequence allocation", "a bd whose version cannot be read", "still runs unguarded"},
		},
		{
			name:    "a released bd holding a transcript",
			check:   extmsgTranscriptCheckOver(releasedBdWithTranscript, nil),
			status:  doctor.StatusWarning,
			message: []string{"run unguarded", "--if-revision", "two entries one sequence"},
			hint:    true,
		},
		{
			name:    "a released bd holding no transcript",
			check:   extmsgTranscriptCheckOver(beadstest.NewFakeBd().NewBdStore(), nil),
			status:  doctor.StatusOK,
			message: []string{"holds no external-messaging transcript", "--if-revision"},
		},
		{
			name:    "a store whose transcripts cannot be read",
			check:   extmsgTranscriptCheckOver(transcriptListFailingStore{beads.NewMemStore()}, nil),
			status:  doctor.StatusWarning,
			message: []string{"run unguarded", "could not be read", "list timed out"},
			hint:    true,
		},
		{
			name:    "a city store that does not open",
			check:   extmsgTranscriptCheckOver(nil, errors.New("dolt is down")),
			status:  doctor.StatusWarning,
			message: []string{"not checked", "dolt is down"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.check.Run(&doctor.CheckContext{})
			if r.Name != "extmsg-transcript-sequencing" || r.Status != tc.status {
				t.Fatalf("result = %s %v %q, want extmsg-transcript-sequencing %v", r.Name, r.Status, r.Message, tc.status)
			}
			for _, want := range tc.message {
				if !strings.Contains(r.Message, want) {
					t.Errorf("message %q does not contain %q", r.Message, want)
				}
			}
			if got := r.FixHint == extmsgTranscriptSequencingRemedy; got != tc.hint {
				t.Errorf("fix hint = %q, want the remedy: %v", r.FixHint, tc.hint)
			}
		})
	}
}
