package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestLogDeclinedBeadRewakeIfDetected expresses ga-5tdflo's exit-contract
// acceptance criteria for the declined-bead re-wake loop detector (gm-6a5dt):
// a role re-wakes on a bead it just declined, forever, because the wake path
// only reads gc.routed_to and never checks who authored the newest note.
// logDeclinedBeadRewakeIfDetected and noteAuthorLookup do not exist yet — this
// file fails to compile until the GREEN step adds them.
func TestLogDeclinedBeadRewakeIfDetected(t *testing.T) {
	tests := []struct {
		name        string
		bead        beads.Bead
		lookupActor string
		lookupErr   error
		wantLog     bool
		wantSubstr  string
	}{
		{
			// Criterion 1: routed_to == newest-note-author -> log fires.
			name: "routed_to matches newest note author logs the loop signature",
			bead: beads.Bead{
				ID:       "ga-test1",
				Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "gascity/builder"},
			},
			lookupActor: "gascity/builder",
			wantLog:     true,
			wantSubstr:  "ga-test1",
		},
		{
			// Criterion 3: different author -> no log (this is the common,
			// healthy case and must stay silent).
			name: "routed_to differs from newest note author stays silent",
			bead: beads.Bead{
				ID:       "ga-test2",
				Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "gascity/builder"},
			},
			lookupActor: "gascity/reviewer",
			wantLog:     false,
		},
		{
			// Criterion 4: lookup failure degrades to silent skip, never
			// blocks or panics the caller (the session-start path).
			name: "history lookup failure is a silent skip",
			bead: beads.Bead{
				ID:       "ga-test3",
				Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "gascity/builder"},
			},
			lookupErr: errors.New("bd history: subprocess failed"),
			wantLog:   false,
		},
		{
			name: "no routed_to metadata stays silent",
			bead: beads.Bead{
				ID: "ga-test4",
			},
			lookupActor: "gascity/builder",
			wantLog:     false,
		},
		{
			// A lookup with no error but also no resolved author (e.g. bead
			// has no notes-changing history event yet) must not be treated
			// as a match against a non-empty routed_to.
			name: "empty actor from lookup stays silent",
			bead: beads.Bead{
				ID:       "ga-test5",
				Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "gascity/builder"},
			},
			lookupActor: "",
			wantLog:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logged []string
			lookup := func(_ string) (string, error) {
				if tt.lookupErr != nil {
					return "", tt.lookupErr
				}
				return tt.lookupActor, nil
			}
			logf := func(format string, args ...any) {
				logged = append(logged, fmt.Sprintf(format, args...))
			}

			logDeclinedBeadRewakeIfDetected(tt.bead, lookup, logf)

			if tt.wantLog && len(logged) == 0 {
				t.Fatalf("expected a log line, got none")
			}
			if !tt.wantLog && len(logged) != 0 {
				t.Fatalf("expected no log line, got %v", logged)
			}
			if tt.wantSubstr != "" {
				found := false
				for _, l := range logged {
					if strings.Contains(l, tt.wantSubstr) {
						found = true
					}
				}
				if !found {
					t.Fatalf("expected log to contain %q, got %v", tt.wantSubstr, logged)
				}
			}
		})
	}
}

// TestLogDeclinedBeadRewakeIfDetectedNeverMutatesBead pins criterion 2: the
// detector only observes (bead + injected lookup) and has no store handle, so
// it is structurally incapable of mutating or rerouting the bead. This guards
// against a future signature change smuggling in a store parameter.
func TestLogDeclinedBeadRewakeIfDetectedNeverMutatesBead(t *testing.T) {
	b := beads.Bead{
		ID:       "ga-test6",
		Assignee: "gascity/builder",
		Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "gascity/builder"},
	}
	before := b
	lookup := func(string) (string, error) { return "gascity/builder", nil }

	logDeclinedBeadRewakeIfDetected(b, lookup, func(string, ...any) {})

	if b.ID != before.ID || b.Assignee != before.Assignee ||
		b.Metadata[beadmeta.RoutedToMetadataKey] != before.Metadata[beadmeta.RoutedToMetadataKey] {
		t.Fatalf("bead was mutated: before=%+v after=%+v", before, b)
	}
}
