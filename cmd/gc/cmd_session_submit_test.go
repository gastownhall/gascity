package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

func TestEmitSessionSubmitResultFollowUpQueued(t *testing.T) {
	var stdout bytes.Buffer
	emitSessionSubmitResult(&stdout, "mayor", session.SubmitIntentFollowUp, true)
	if got := stdout.String(); !strings.Contains(got, "Queued follow-up for mayor") {
		t.Fatalf("stdout = %q, want queued confirmation", got)
	}
}

func TestEmitSessionSubmitResultFollowUpImmediate(t *testing.T) {
	var stdout bytes.Buffer
	emitSessionSubmitResult(&stdout, "mayor", session.SubmitIntentFollowUp, false)
	got := stdout.String()
	if strings.Contains(got, "Queued") {
		t.Fatalf("stdout = %q, should not say Queued when message was delivered immediately", got)
	}
	if !strings.Contains(got, "Submitted follow-up to mayor") {
		t.Fatalf("stdout = %q, want submitted follow-up confirmation", got)
	}
}

func TestParseSessionSubmitIntentAcceptsLegacySpellings(t *testing.T) {
	cases := []struct {
		raw  string
		want session.SubmitIntent
	}{
		{raw: "", want: session.SubmitIntentDefault},
		{raw: "default", want: session.SubmitIntentDefault},
		{raw: "follow_up", want: session.SubmitIntentFollowUp},
		{raw: "follow-up", want: session.SubmitIntentFollowUp},
		{raw: "interrupt_now", want: session.SubmitIntentInterruptNow},
		{raw: "interrupt-now", want: session.SubmitIntentInterruptNow},
	}

	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := parseSessionSubmitIntent(tc.raw)
			if err != nil {
				t.Fatalf("parseSessionSubmitIntent(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Fatalf("parseSessionSubmitIntent(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}

	if _, err := parseSessionSubmitIntent("later"); err == nil {
		t.Fatal("parseSessionSubmitIntent(later) unexpectedly succeeded")
	}
}
