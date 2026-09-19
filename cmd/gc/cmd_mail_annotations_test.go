package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/mail"
	"github.com/gastownhall/gascity/internal/mail/beadmail"
)

// sweepRecorder is a provider that cannot carry metadata but can be asked to
// sweep, so each half of the annotation wiring is observable on its own.
type sweepRecorder struct {
	*mail.Fake
	got []string
	err error
}

func (s *sweepRecorder) ArchiveResolvedBlockers(recipients []string) ([]string, error) {
	s.got = append(s.got, recipients...)
	if s.err != nil {
		return nil, s.err
	}
	return recipients, nil
}

// A backend that cannot carry the supersede key must reject the flag. Dropping
// it would let the digests pile up again with no sign that anything failed.
func TestSendAnnotatedMailRejectsProviderWithoutMetadataSupport(t *testing.T) {
	var stderr bytes.Buffer
	_, err := sendAnnotatedMail(mail.NewFake(), "reviewer", "mayor", "digest", "state",
		mailSendAnnotations{supersede: "slack-alert-review"}, &stderr)
	if err == nil {
		t.Fatal("sendAnnotatedMail() = nil error, want rejection on a provider without metadata support")
	}
	if !strings.Contains(err.Error(), "--supersede") {
		t.Errorf("error = %q, want it to name the rejected flag", err)
	}
}

// Unannotated mail keeps the plain Send path, so ordinary senders are
// unaffected by a backend's metadata support.
func TestSendAnnotatedMailUsesPlainSendWithoutAnnotations(t *testing.T) {
	var stderr bytes.Buffer
	m, err := sendAnnotatedMail(mail.NewFake(), "reviewer", "mayor", "plain", "body", mailSendAnnotations{}, &stderr)
	if err != nil {
		t.Fatalf("sendAnnotatedMail(): %v", err)
	}
	if m.Subject != "plain" {
		t.Errorf("subject = %q, want %q", m.Subject, "plain")
	}
}

// The flags must reach the message as the keys the backend reads back.
func TestSendAnnotatedMailAttachesBothAnnotations(t *testing.T) {
	store := beads.NewMemStore()
	var stderr bytes.Buffer
	m, err := sendAnnotatedMail(beadmail.New(store), "worker", "mayor", "BLOCKED: merge PR 25", "waiting",
		mailSendAnnotations{supersede: "slack-alert-review", blockedOn: "sc-8fk2p"}, &stderr)
	if err != nil {
		t.Fatalf("sendAnnotatedMail(): %v", err)
	}
	b, err := store.Get(m.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", m.ID, err)
	}
	if got := b.Metadata[mail.SupersedeKeyMetadataKey]; got != "slack-alert-review" {
		t.Errorf("supersede key = %q, want %q", got, "slack-alert-review")
	}
	if got := b.Metadata[mail.BlockedOnMetadataKey]; got != "sc-8fk2p" {
		t.Errorf("blocked-on = %q, want %q", got, "sc-8fk2p")
	}
}

// The inbox sweep runs for every recipient route the target resolved to, and
// only for providers that can resolve a bead id.
func TestSweepResolvedBlockersPassesEveryRecipientRoute(t *testing.T) {
	var stderr bytes.Buffer
	rec := &sweepRecorder{Fake: mail.NewFake()}
	sweepResolvedBlockers(rec, resolvedMailTarget{display: "mayor", recipients: []string{"mayor", "gc-42"}}, &stderr)
	if strings.Join(rec.got, ",") != "mayor,gc-42" {
		t.Errorf("swept recipients = %v, want [mayor gc-42]", rec.got)
	}

	// A provider without the extension is skipped silently, not reported as an
	// error: an inbox listing must never fail because of the sweep.
	stderr.Reset()
	sweepResolvedBlockers(mail.NewFake(), resolvedMailTarget{display: "mayor", recipients: []string{"mayor"}}, &stderr)
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want silence for a provider without sweep support", stderr.String())
	}

	// A sweep failure is advisory. It warns and returns, because an inbox
	// listing must never fail on account of the cleanup.
	stderr.Reset()
	failing := &sweepRecorder{Fake: mail.NewFake(), err: errors.New("store unreachable")}
	sweepResolvedBlockers(failing, resolvedMailTarget{display: "mayor", recipients: []string{"mayor"}}, &stderr)
	if !strings.Contains(stderr.String(), "store unreachable") {
		t.Errorf("stderr = %q, want the sweep failure reported", stderr.String())
	}
}
